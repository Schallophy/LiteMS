package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// 字幕三源：外挂（视频旁边）、导入（.litems/subs/<id>/，用户上传）、
// 内嵌（容器里的字幕轨）。网页播放统一用 WebVTT；内嵌文本轨用 ffmpeg 抽取
// 并缓存成 vtt，图像字幕（PGS/VobSub）无法转文本，只列出来提示用 mpv。

// SubDTO 是页面与 JSON 共用的字幕条目。
type SubDTO struct {
	Lang  string `json:"lang"`
	Label string `json:"label"`
	Src   string `json:"src"`             // sidecar / imported / embed
	URL   string `json:"url,omitempty"`   // 为空表示网页播不了（图像字幕）
	File  string `json:"file,omitempty"`  // 导入字幕的文件名（删除用）
	Codec string `json:"codec,omitempty"` // 内嵌轨编码
}

// subExts 允许导入/识别的外挂字幕格式。
var subExts = map[string]bool{".srt": true, ".ass": true, ".ssa": true, ".vtt": true}

// maxSubBytes 单个字幕文件上限。
const maxSubBytes = 8 << 20

func langLabel(l string) string {
	switch NormalizeLang(l) {
	case "chs":
		return "简体中文"
	case "cht":
		return "繁體中文"
	case "eng":
		return "English"
	case "jpn":
		return "日本語"
	case "kor":
		return "한국어"
	case "fre", "fra":
		return "Français"
	case "ger", "deu":
		return "Deutsch"
	case "spa":
		return "Español"
	case "ita":
		return "Italiano"
	case "rus":
		return "Русский"
	case "por":
		return "Português"
	case "tha":
		return "ไทย"
	case "vie":
		return "Tiếng Việt"
	}
	if strings.TrimSpace(l) == "" {
		return "未标注"
	}
	return strings.ToUpper(l)
}

// subsDir 是某条目的导入字幕目录（在媒体根的 .litems 状态目录下，不进媒体库）。
func (s *Server) subsDir(it Item) string {
	return filepath.Join(s.roots[it.RootIdx], ".litems", "subs", it.ID)
}

func (s *Server) itemPath(it Item) string {
	return filepath.Join(s.roots[it.RootIdx], filepath.FromSlash(it.RelPath))
}

// subsFor 合并三种来源，供详情页 / 播放页 / 详情 JSON 使用。
func (s *Server) subsFor(it Item) []SubDTO {
	out := make([]SubDTO, 0, 6)
	for i, sp := range it.SubPaths {
		lang := NormalizeLang(sp.Lang)
		out = append(out, SubDTO{
			Lang:  lang,
			Label: langLabel(sp.Lang),
			Src:   "sidecar",
			URL:   "/sub/" + it.ID + "?sub=" + strconv.Itoa(i),
		})
	}
	out = append(out, s.importedSubs(it)...)
	out = append(out, s.embeddedSubs(it)...)
	// 按 [scrape].sub_langs 的偏好排，同优先级里内嵌优先：内嵌轨和视频在同一个
	// 容器里、共用一条时间基，天然严丝合缝；外挂和导入的字幕本身就可能带偏移，
	// 拿它当默认很容易让人以为是播放器对不齐。
	sort.SliceStable(out, func(i, j int) bool {
		ri := langRank(out[i].Lang, s.cfg.Scrape.SubLangs)
		rj := langRank(out[j].Lang, s.cfg.Scrape.SubLangs)
		if ri != rj {
			return ri < rj
		}
		return out[i].Src == "embedded" && out[j].Src != "embedded"
	})
	return out
}

// importedSubs 列出 .litems/subs/<id>/ 下用户导入的字幕。
func (s *Server) importedSubs(it Item) []SubDTO {
	ents, err := os.ReadDir(s.subsDir(it))
	if err != nil {
		return nil
	}
	out := make([]SubDTO, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// embed-*.vtt 是内嵌字幕的抽取缓存，不算导入
		if strings.HasPrefix(name, "embed-") {
			continue
		}
		if !subExts[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		lang := SubtitleLang(name)
		out = append(out, SubDTO{
			Lang:  NormalizeLang(lang),
			Label: langLabel(lang),
			Src:   "imported",
			URL:   "/sub/" + it.ID + "?imp=" + name,
			File:  name,
		})
	}
	return out
}

// subTrack 是 ffprobe 报告的一条字幕轨。
type subTrack struct {
	Index int    `json:"index"`
	Codec string `json:"codec_name"`
	Lang  string `json:"lang"`
	Title string `json:"title"`
}

// subCache 缓存每条目的内嵌轨探测结果，键为 id，值 embedCache。
type embedCache struct {
	mtime  int64
	size   int64
	tracks []subTrack
}

var subCacheMu sync.Mutex
var subCache = map[string]embedCache{}

// textSubCodecs 是能转成 WebVTT 的文本字幕编码。
var textSubCodecs = map[string]bool{
	"subrip": true, "srt": true, "ass": true, "ssa": true,
	"webvtt": true, "mov_text": true, "text": true,
}

// probeSubTracks 用 ffprobe 读容器里的字幕轨（读头部，秒级以内）。
func probeSubTracks(path string) []subTrack {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "s",
		"-show_entries", "stream=index,codec_name:stream_tags=language,title",
		"-of", "json", path).Output()
	if err != nil {
		return nil
	}
	var doc struct {
		Streams []struct {
			Index int    `json:"index"`
			Codec string `json:"codec_name"`
			Tags  struct {
				Language string `json:"language"`
				Title    string `json:"title"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil
	}
	tracks := make([]subTrack, 0, len(doc.Streams))
	for _, st := range doc.Streams {
		tracks = append(tracks, subTrack{Index: st.Index, Codec: st.Codec,
			Lang: st.Tags.Language, Title: st.Tags.Title})
	}
	return tracks
}

// embeddedSubs 列出内嵌字幕轨（结果按源文件 mtime+size 缓存，列表页不会反复探测）。
func (s *Server) embeddedSubs(it Item) []SubDTO {
	src := s.itemPath(it)
	fi, err := os.Stat(src)
	if err != nil {
		return nil
	}
	subCacheMu.Lock()
	cached, ok := subCache[it.ID]
	subCacheMu.Unlock()
	if !ok || cached.mtime != fi.ModTime().Unix() || cached.size != fi.Size() {
		tracks := probeSubTracks(src)
		cached = embedCache{mtime: fi.ModTime().Unix(), size: fi.Size(), tracks: tracks}
		subCacheMu.Lock()
		subCache[it.ID] = cached
		subCacheMu.Unlock()
	}
	out := make([]SubDTO, 0, len(cached.tracks))
	for _, t := range cached.tracks {
		label := langLabel(t.Lang)
		if t.Title != "" {
			label += " · " + t.Title
		}
		d := SubDTO{Lang: NormalizeLang(t.Lang), Label: label, Src: "embed", Codec: t.Codec}
		if textSubCodecs[strings.ToLower(t.Codec)] {
			d.URL = "/sub/" + it.ID + "?embed=" + strconv.Itoa(t.Index)
		} else {
			d.Label += "（图像字幕，仅 mpv）"
		}
		out = append(out, d)
	}
	return out
}

// embedCachePath 是内嵌字幕抽取后的 vtt 缓存路径。
func (s *Server) embedCachePath(it Item, idx int) string {
	return filepath.Join(s.subsDir(it), "embed-"+strconv.Itoa(idx)+".vtt")
}

// extractEmbedSub 把第 idx 条内嵌字幕轨抽成 VTT 并缓存；源文件更新过则重抽。
func (s *Server) extractEmbedSub(it Item, idx int) (string, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return "", errors.New("ffmpeg 未安装——Termux 上执行 pkg install ffmpeg")
	}
	src := s.itemPath(it)
	sfi, err := os.Stat(src)
	if err != nil {
		return "", errors.New("源文件不存在")
	}
	cache := s.embedCachePath(it, idx)
	if cfi, err := os.Stat(cache); err == nil && cfi.Size() > 0 &&
		!sfi.ModTime().After(cfi.ModTime()) {
		return cache, nil
	}
	if err := os.MkdirAll(s.subsDir(it), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(s.subsDir(it), "embed-*.part")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()

	cmd := exec.Command("ffmpeg", "-nostdin", "-y", "-i", src,
		"-map", "0:"+strconv.Itoa(idx), "-f", "webvtt", tmpPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("抽取内嵌字幕失败：%s", lastLine(out))
	}
	if err := os.Rename(tmpPath, cache); err != nil {
		os.Remove(tmpPath)
		return "", err
	}
	os.Chtimes(cache, sfi.ModTime(), sfi.ModTime()) // 与源对齐，便于失效判断
	return cache, nil
}

// sanitizeSubName 校验并规范导入字幕的文件名：去路径、限制扩展名、
// 按需补语言标记（如 film.ass + lang=chs → film.chs.ass）。
func sanitizeSubName(name, lang string) (string, error) {
	name = strings.TrimSpace(name)
	name = name[strings.LastIndexAny(name, "/\\")+1:]
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if clean == "" || clean == "." || clean == ".." {
		return "", errors.New("文件名无效")
	}
	ext := filepath.Ext(clean)
	if !subExts[strings.ToLower(ext)] {
		return "", errors.New("只支持 .srt / .ass / .ssa / .vtt")
	}
	stem := strings.TrimSuffix(clean, ext)
	l := NormalizeLang(lang)
	if l == "" {
		l = NormalizeLang(SubtitleLang(clean))
	}
	// 文件名里已带语言标记就不重复加（注意不能用 SubtitleLang 判断：
	// 它会把 "film.chs" 的 .chs 当扩展名剥掉）
	if l != "" && !hasLangMark(stem) {
		clean = stem + "." + l + ext
	}
	return clean, nil
}

// hasLangMark 判断不含扩展名的文件名里是否已有语言标记（film.chs / 电影.cht.forced）。
func hasLangMark(stem string) bool {
	parts := strings.Split(stem, ".")
	for i := len(parts) - 1; i >= 0 && i >= len(parts)-3; i-- {
		if subLangs[strings.ToLower(parts[i])] {
			return true
		}
	}
	return false
}

// uniqueInDir 在同目录下生成不冲突的文件名（a.srt → a-2.srt）。
func uniqueInDir(dir, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	cand := name
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(dir, cand)); os.IsNotExist(err) {
			return cand
		}
		cand = fmt.Sprintf("%s-%d%s", stem, i, ext)
	}
}

// saveUploadedSub 落盘一个导入字幕，返回最终文件名。
func (s *Server) saveUploadedSub(it Item, r io.Reader, name string) (string, error) {
	dir := s.subsDir(it)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	final := uniqueInDir(dir, name)
	dst := filepath.Join(dir, final)
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, io.LimitReader(r, maxSubBytes)); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return final, nil
}

// lastLine 取命令输出的最后一行，作为错误提示。
func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 0 {
		return "未知错误"
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// handleSubUpload 处理导入字幕：POST 上传（multipart field=file，可选 lang），
// DELETE 删除（?file=）。
func (s *Server) handleSubUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/api/sub/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "id 必须是 16 位十六进制")
		return
	}
	it, err := s.db.GetItem(id)
	if err != nil || it == nil {
		writeErr(w, http.StatusNotFound, "条目不存在")
		return
	}
	switch r.Method {
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxSubBytes+1<<20)
		if err := r.ParseMultipartForm(maxSubBytes); err != nil {
			writeErr(w, http.StatusRequestEntityTooLarge, "字幕文件过大（上限 8MB）")
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeErr(w, http.StatusBadRequest, "缺少 file 字段")
			return
		}
		defer file.Close()
		name, err := sanitizeSubName(hdr.Filename, r.FormValue("lang"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		final, err := s.saveUploadedSub(*it, file, name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "保存字幕失败："+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"file": final,
			"subs": s.subsFor(*it),
		})
	case http.MethodDelete:
		name := filepath.Base(r.URL.Query().Get("file"))
		if name == "" || name == "." || strings.HasPrefix(name, "embed-") ||
			!subExts[strings.ToLower(filepath.Ext(name))] {
			writeErr(w, http.StatusBadRequest, "文件名无效")
			return
		}
		if err := os.Remove(filepath.Join(s.subsDir(*it), name)); err != nil {
			writeErr(w, http.StatusNotFound, "字幕不存在")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"subs": s.subsFor(*it)})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "只支持 POST/DELETE")
	}
}
