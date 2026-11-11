package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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

	// raw 是字幕在磁盘上的路径；只给服务端估算偏移用，不对外。
	raw string
}

// ---------- 默认字幕偏移 ----------
//
// 手动试偏移很痛苦，这里估一个整体平移量当默认值：
// 逐条在参考轨里找时间上最近的 cue，把差值取中位数。不同语言字幕的断句
// 未必一一对应（本片中文 2054 条 / 内嵌英文 2067 条），会有一批错配，
// 中位数对这类离群值不敏感，剩下的就是真正的整体偏移。
//
// 参考轨用内嵌轨：它和视频在同一个容器里、共用一条时间基，是对齐最可靠的。

// vttStarts 取 WebVTT 里每条 cue 的起始秒数。
func vttStarts(b []byte) []float64 {
	var out []float64
	for _, line := range strings.Split(string(b), "\n") {
		i := strings.Index(line, "-->")
		if i < 0 {
			continue
		}
		if t, ok := vttTime(strings.TrimSpace(line[:i])); ok {
			out = append(out, t)
		}
	}
	return out
}

// vttTime 解析 WebVTT 时间戳，兼容 HH:MM:SS.mmm 与 MM:SS.mmm。
func vttTime(s string) (float64, bool) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	sec, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, false
	}
	min, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		return 0, false
	}
	total := float64(min)*60 + sec
	if len(parts) == 3 {
		h, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, false
		}
		total += float64(h) * 3600
	}
	return total, true
}

// embedIndex 从 /sub/<id>?embed=N 里取出 N，取不到返回 -1。
func embedIndex(u string) int {
	i := strings.Index(u, "embed=")
	if i < 0 {
		return -1
	}
	rest := u[i+len("embed="):]
	if j := strings.IndexAny(rest, "&"); j >= 0 {
		rest = rest[:j]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return -1
	}
	return n
}

// subStarts 读某条字幕的 cue 起始时间；内嵌轨按需抽取成 VTT（有磁盘缓存）。
func (s *Server) subStarts(it Item, d SubDTO) []float64 {
	path := d.raw
	if path == "" && d.Src == "embed" {
		if idx := embedIndex(d.URL); idx >= 0 {
			if p, err := s.extractEmbedSub(it, idx); err == nil {
				path = p
			}
		}
	}
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if ext := strings.ToLower(filepath.Ext(path)); ext != ".vtt" {
		v, err := toVTT(raw, ext, d.Lang)
		if err != nil {
			return nil
		}
		raw = v
	}
	return vttStarts(raw)
}

// medianOffset 用参考轨估目标轨的整体偏移（秒）。不可靠时返回 0：
// 条数太少、有效匹配不到一半、或者算出来本来就在 0.1 秒以内。
func medianOffset(target, ref []float64) float64 {
	if len(target) < 10 || len(ref) < 10 {
		return 0
	}
	sorted := append([]float64(nil), ref...)
	sort.Float64s(sorted)
	diffs := make([]float64, 0, len(target))
	for _, t := range target {
		i := sort.SearchFloat64s(sorted, t)
		best, bestD := 0.0, math.MaxFloat64
		for _, j := range []int{i - 1, i, i + 1} {
			if j < 0 || j >= len(sorted) {
				continue
			}
			if d := math.Abs(sorted[j] - t); d < bestD {
				best, bestD = sorted[j], d
			}
		}
		// 差出 12 秒以上的基本是匹配到别的句子了，直接丢掉
		if bestD < 12 {
			diffs = append(diffs, t-best)
		}
	}
	if len(diffs) < len(target)/2 {
		return 0
	}
	sort.Float64s(diffs)
	m := diffs[len(diffs)/2]
	if math.Abs(m) < 0.1 {
		return 0
	}
	return math.Round(m*10) / 10
}

// SubRefPlan 是一次偏移估算的输入与结果，直接当 /api/suboffset 的响应体。
type SubRefPlan struct {
	Ref     string             `json:"ref"`            // 实际用作基准的字幕 URL；空表示没找到可用基准
	Auto    bool               `json:"auto,omitempty"` // 用户指定的基准不可用，已回落成自动挑选
	Offsets map[string]float64 `json:"offsets"`        // 字幕 URL -> 相对基准的偏移秒数
}

// pickSubRef 选基准轨下标，第二个返回值表示「用户指定的那条没找到，用了自动」。
// 没指定时内嵌轨优先：它和视频在同一个容器里、共用一条时间基，对齐最可靠；
// 没有内嵌轨再用第一条网页能播的字幕。
func pickSubRef(subs []SubDTO, refURL string) (idx int, fellBack bool) {
	if refURL != "" {
		for i, d := range subs {
			if d.URL != "" && d.URL == refURL {
				return i, false
			}
		}
	}
	for i, d := range subs {
		if d.Src == "embed" && d.URL != "" {
			return i, refURL != ""
		}
	}
	for i, d := range subs {
		if d.URL != "" {
			return i, refURL != ""
		}
	}
	return -1, refURL != ""
}

// subOffsetsFor 给条目下每条非基准字幕算一个相对偏移，按 URL 索引。
// refURL 是用户指定的基准轨；为空或指向一条已不存在的字幕时按内嵌轨自动挑。
// 结果进程内缓存——解字幕文本不贵，但没必要每次播放页都重算。
func (s *Server) subOffsetsFor(it Item, refURL string) SubRefPlan {
	plan := SubRefPlan{Offsets: map[string]float64{}}
	subs := s.subsFor(it)
	refIdx, fellBack := pickSubRef(subs, refURL)
	plan.Auto = fellBack
	if refIdx < 0 {
		return plan
	}
	plan.Ref = subs[refIdx].URL
	ref := s.subStarts(it, subs[refIdx])
	if len(ref) == 0 {
		return plan
	}
	for i, d := range subs {
		if i == refIdx || d.URL == "" {
			continue
		}
		// 缓存键带上基准轨：同一条字幕换个参照物就是另一套数，不能串
		key := it.ID + "|" + d.URL + "@" + plan.Ref
		if v, ok := s.subOff.Load(key); ok {
			plan.Offsets[d.URL] = v.(float64)
			continue
		}
		off := medianOffset(s.subStarts(it, d), ref)
		s.subOff.Store(key, off)
		plan.Offsets[d.URL] = off
	}
	return plan
}

// handleSubOffset 返回每条字幕相对基准轨的偏移（秒）。
// ?ref=<字幕URL> 指定基准轨（校准用）；不带就自动挑内嵌轨。
// 播放页拿到后先套上，用户再想微调才用滑块，不用从 0 开始试。
func (s *Server) handleSubOffset(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/api/suboffset/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "id 必须是 16 位十六进制")
		return
	}
	it, err := s.db.GetItem(id)
	if err != nil || it == nil {
		writeErr(w, http.StatusNotFound, "条目不存在")
		return
	}
	writeJSON(w, http.StatusOK, s.subOffsetsFor(*it, strings.TrimSpace(r.URL.Query().Get("ref"))))
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
	itemDir := filepath.Dir(s.itemPath(it))
	for i, sp := range it.SubPaths {
		lang := NormalizeLang(sp.Lang)
		out = append(out, SubDTO{
			Lang:  lang,
			Label: langLabel(sp.Lang),
			Src:   "sidecar",
			URL:   "/sub/" + it.ID + "?sub=" + strconv.Itoa(i),
			raw:   filepath.Join(itemDir, filepath.FromSlash(sp.Path)),
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
			raw:   filepath.Join(s.subsDir(it), name),
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
