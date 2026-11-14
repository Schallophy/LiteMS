package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// 浏览器内核（Chromium / Firefox / WebKit）自带的音频解码器只有 AAC、MP3、
// FLAC、Opus、Vorbis 这几族。网盘下载的片源常见 DTS / TrueHD / AC3 系音轨，
// 浏览器解不了时的表现恰好很迷惑：画面正常播放、没有任何报错，就是没声音。
//
// 文件名里的标签（audioHint）只用来提示；能不能直接播要看容器里真实的音轨
// 编码，播放页用 ffprobe 探一次（按文件 mtime+size 缓存）后据此禁用不必要的
// AAC 转码入口。

var (
	// 尾边界用 [^a-z] 而不是 [^a-z0-9]：DDP5.1 / DTS5.1 这类标签后面紧跟的是
	// 声道数字，按「非字母数字」判定边界会把最常见的写法整个漏掉。
	reAudioBad   = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(dtshd|dts-hd|dtsma|dts|truehd|thd|atmos|lpcm|pcm)(?:$|[^a-z])`)
	reAudioMaybe = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(e[-_. ]?ac3|ddp|dd[0-9]|ac3|dolby)(?:$|[^a-z])`)
)

// audioHint 识别文件名/容器名里的音频标签，返回提示等级。
func audioHint(names ...string) string {
	s := strings.ToLower(strings.Join(names, " "))
	s = strings.ReplaceAll(s, "dd+", "ddp") // 发布组常写作 DD+5.1
	if reAudioBad.MatchString(s) {
		return "bad"
	}
	if reAudioMaybe.MatchString(s) {
		return "maybe"
	}
	return ""
}

// browserAudioOK 判断音轨编码能否被主流浏览器内核直接解码。
// 空编码表示容器里没有音轨，也就没有要转的东西，同样算「不需要转码」。
func browserAudioOK(codec string) bool {
	switch strings.ToLower(codec) {
	case "", "aac", "mp3", "opus", "vorbis", "flac":
		return true
	}
	return false
}

type audioTrack struct {
	N        int    `json:"n"`     // ffmpeg -map 0:a:N 的序号（按音轨出现顺序，0 起）
	Index    int    `json:"index"` // 容器里的 stream index
	Codec    string `json:"codec"`
	Lang     string `json:"lang"`
	Title    string `json:"title,omitempty"`
	Channels int    `json:"channels"`
	Default  bool   `json:"default"` // 容器标记的默认轨：原生直连播的就是它
}

// probeAudioTracks 用 ffprobe 列出容器内所有音轨（只读头部）。
func probeAudioTracks(path string) ([]audioTrack, error) {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-select_streams", "a",
		"-show_entries", "stream=index,codec_name,channels:stream_tags=language,title:stream_disposition=default",
		"-of", "json", path).Output()
	if err != nil {
		return nil, err
	}
	var doc struct {
		Streams []struct {
			Index    int    `json:"index"`
			Codec    string `json:"codec_name"`
			Channels int    `json:"channels"`
			Tags     struct {
				Language string `json:"language"`
				Title    string `json:"title"`
			} `json:"tags"`
			Disposition struct {
				Default int `json:"default"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	tracks := make([]audioTrack, 0, len(doc.Streams))
	for i, st := range doc.Streams {
		tracks = append(tracks, audioTrack{
			N: i, Index: st.Index, Codec: st.Codec,
			Lang: st.Tags.Language, Title: st.Tags.Title,
			Channels: st.Channels, Default: st.Disposition.Default == 1,
		})
	}
	return tracks, nil
}

// defaultAudioTrack 返回容器标记的默认音轨；没有标记就用第一条。
func defaultAudioTrack(tracks []audioTrack) (audioTrack, bool) {
	for _, t := range tracks {
		if t.Default {
			return t, true
		}
	}
	if len(tracks) > 0 {
		return tracks[0], true
	}
	return audioTrack{}, false
}

type audioProbeEntry struct {
	mtime  int64
	size   int64
	probed bool
	tracks []audioTrack
}

var audioCacheMu sync.Mutex
var audioCache = map[string]audioProbeEntry{}

// audioTracks 返回条目容器里的音轨列表；probed=false 表示探测失败（不缓存失败）。
func (s *Server) audioTracks(it Item) (tracks []audioTrack, probed bool) {
	p := s.itemPath(it)
	fi, err := os.Stat(p)
	if err != nil {
		return nil, false
	}
	audioCacheMu.Lock()
	c, cached := audioCache[it.ID]
	audioCacheMu.Unlock()
	if cached && c.mtime == fi.ModTime().Unix() && c.size == fi.Size() {
		return c.tracks, true
	}
	tracks, err = probeAudioTracks(p)
	if err != nil {
		return nil, false
	}
	if tracks == nil {
		tracks = []audioTrack{}
	}
	audioCacheMu.Lock()
	audioCache[it.ID] = audioProbeEntry{mtime: fi.ModTime().Unix(), size: fi.Size(), probed: true, tracks: tracks}
	audioCacheMu.Unlock()
	return tracks, true
}

// nativeAudioOK 报告默认音轨能否直接播。探测失败时按「能播」处理，
// 宁可少禁一个入口，也不要把还能用的 AAC 选项锁掉。
func (s *Server) nativeAudioOK(it Item) bool {
	tracks, probed := s.audioTracks(it)
	if !probed {
		return false
	}
	if len(tracks) == 0 {
		return true // 没有音轨，也就没有要转的
	}
	t, ok := defaultAudioTrack(tracks)
	return ok && browserAudioOK(t.Codec)
}

func audioCodecLabel(codec string) string {
	switch strings.ToLower(codec) {
	case "aac":
		return "AAC"
	case "ac3":
		return "AC3"
	case "eac3":
		return "EAC3"
	case "dts":
		return "DTS"
	case "truehd":
		return "TrueHD"
	case "flac":
		return "FLAC"
	case "opus":
		return "Opus"
	case "vorbis":
		return "Vorbis"
	case "mp3":
		return "MP3"
	case "pcm_s16le", "pcm_s24le", "pcm_s32le", "pcm_f32le":
		return "PCM"
	}
	return strings.ToUpper(codec)
}

func audioChannelLabel(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "单声道"
	case 2:
		return "立体声"
	case 6:
		return "5.1"
	case 8:
		return "7.1"
	}
	return strconv.Itoa(n) + " 声道"
}

// audioTrackLabel 拼设置面板里的音轨名：语言 · 编码 声道 · 标题。
func audioTrackLabel(t audioTrack) string {
	name := audioCodecLabel(t.Codec)
	if ch := audioChannelLabel(t.Channels); ch != "" {
		name += " " + ch
	}
	parts := []string{langLabel(t.Lang), name}
	if t.Title != "" && t.Title != name {
		parts = append(parts, t.Title)
	}
	return strings.Join(parts, " · ")
}
