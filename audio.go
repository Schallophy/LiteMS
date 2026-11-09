package main

import (
	"regexp"
	"strings"
)

// 浏览器内核（Chromium / Firefox / WebKit）自带的音频解码器只有 AAC、MP3、
// FLAC、Opus、Vorbis 这几族。网盘下载的片源常见 DTS / TrueHD / AC3 系音轨，
// 浏览器解不了时的表现恰好很迷惑：画面正常播放、没有任何报错，就是没声音。
//
// LiteMS 不做容器级嗅探（那要带 EBML/MP4 解析器，不值得），改从文件名与
// 容器名里识别发布组惯标的音频标签，在播放页据此分级提示：
//
//	bad    → DTS / TrueHD / Atmos / PCM：主流浏览器内核都不带解码器，无声基本是它
//	maybe  → AC3 / EAC3 / DDP：Chromium 不带、Edge 等带专有解码器的浏览器可解
//	""     → 没有标注（AAC 等通用编码一般不标），不提示
//
// 只做提示不做硬判断：标签是文件名自述，且是否可解因浏览器而异。

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
