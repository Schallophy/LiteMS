package main

import (
	"os"
	"os/exec"
	"regexp"
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

// probeAudioCodec 用 ffprobe 读第一条音轨的编码名（只读头部，很快）。
func probeAudioCodec(path string) (string, error) {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name", "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

type audioCodecEntry struct {
	mtime int64
	size  int64
	ok    bool
}

var audioCacheMu sync.Mutex
var audioCache = map[string]audioCodecEntry{}

// nativeAudioOK 报告条目的原生音轨能否直接播；探测失败时按「能播」处理，
// 宁可少禁一个入口，也不要把还能用的 AAC 选项锁掉。
func (s *Server) nativeAudioOK(it Item) bool {
	p := s.itemPath(it)
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	audioCacheMu.Lock()
	c, cached := audioCache[it.ID]
	audioCacheMu.Unlock()
	if cached && c.mtime == fi.ModTime().Unix() && c.size == fi.Size() {
		return c.ok
	}
	codec, err := probeAudioCodec(p)
	ok := err == nil && browserAudioOK(codec)
	audioCacheMu.Lock()
	audioCache[it.ID] = audioCodecEntry{mtime: fi.ModTime().Unix(), size: fi.Size(), ok: ok}
	audioCacheMu.Unlock()
	return ok
}
