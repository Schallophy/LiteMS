package main

// 字幕的浏览器适配层：外挂字幕 → WebVTT。
//
// HTML5 <video> / ArtPlayer 只接受 VTT，而外挂字幕的现实是：
//   - 格式混杂：srt / ass / ssa / vtt
//   - 编码混杂：中文字幕大量是 GBK / Big5，浏览器只认 UTF-8
// mpv 两条都能自己消化（chardet + 原生渲染），所以 mpv 路径
// （--sub-file=/sub/<id>?lang=x）保持返回原始文件；只有 fmt=vtt
// 这个参数才走本文件的转换。转换是无状态的：每次请求现读现转，
// 字幕文件通常几十 KB，不值得缓存。

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// decodeSubtitle 把字幕文件统一转成 UTF-8 文本。
// langHint 用扫描阶段识别出的语言（chs/cht/…）：繁体优先按 Big5 解，
// 其余优先 GB18030（GBK/GB2312 的超集），能把猜错编码的概率压到最低。
func decodeSubtitle(raw []byte, langHint string) string {
	// BOM 检测：带 BOM 的一律按 BOM 来
	switch {
	case bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}):
		return string(raw[3:])
	case bytes.HasPrefix(raw, []byte{0xFF, 0xFE}):
		return utf16LEToString(raw[2:])
	case bytes.HasPrefix(raw, []byte{0xFE, 0xFF}):
		return utf16BEToString(raw[2:])
	}
	if utf8.Valid(raw) {
		return string(raw)
	}
	// 无 BOM 且不是合法 UTF-8 → 中文区最常见的 GBK/Big5
	if langHint == "cht" {
		if dec, err := traditionalchinese.Big5.NewDecoder().Bytes(raw); err == nil && looksReasonable(dec) {
			return string(dec)
		}
	}
	if dec, err := simplifiedchinese.GB18030.NewDecoder().Bytes(raw); err == nil {
		return string(dec)
	}
	// 都解不了就当 UTF-8 用，坏字让浏览器显示替换符，总比拒绝服务好
	return string(raw)
}

func utf16LEToString(b []byte) string {
	runes := make([]rune, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		runes = append(runes, rune(uint16(b[i])|uint16(b[i+1])<<8))
	}
	return string(runes)
}

func utf16BEToString(b []byte) string {
	runes := make([]rune, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		runes = append(runes, rune(uint16(b[i])<<8|uint16(b[i+1])))
	}
	return string(runes)
}

// looksReasonable 粗验解码结果：CJK 区文本里不该出现大量替换符或控制字符。
func looksReasonable(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	bad := 0
	for _, r := range string(b) {
		if r == utf8.RuneError || (r < 0x20 && r != '\n' && r != '\r' && r != '\t') {
			bad++
		}
	}
	return bad*20 <= len(b) // 坏字超过 5% 就认为猜错了编码
}

var (
	// srt 时间戳：00:00:01,600 --> 00:00:04,200（毫秒用逗号）
	srtTime = regexp.MustCompile(`(\d{2}):(\d{2}):(\d{2}),(\d{3})`)
	// ass 对白行：Dialogue: 层,起始,结束,样式,人名,左,右,垂直,效果,文本
	assDialogue = regexp.MustCompile(`(?i)^Dialogue:\s*([^,]*),([^,]*),([^,]*),`)
	// ASS 覆盖块 {\i1} {\an8} 等
	assTags = regexp.MustCompile(`\{[^}]*\}`)
)

// toVTT 把任意支持的字幕格式转成 WebVTT。
// srt/vtt 基本无损；ass/ssa 只保留对白文本和时间轴，样式（字体/特效/定位）丢弃。
func toVTT(raw []byte, ext, langHint string) ([]byte, error) {
	text := decodeSubtitle(raw, langHint)
	text = strings.ReplaceAll(text, "\r\n", "\n")

	var out strings.Builder
	out.WriteString("WEBVTT\n\n")

	switch strings.ToLower(ext) {
	case ".vtt":
		// 已经是 VTT：只确认头部存在，原样返回（去 BOM 后）
		if strings.HasPrefix(text, "WEBVTT") {
			return []byte(text), nil
		}
		return []byte("WEBVTT\n\n" + text), nil

	case ".srt":
		// 时间戳毫秒段的逗号换成点，其余原样保留
		converted := srtTime.ReplaceAllString(text, "$1:$2:$3.$4")
		return []byte("WEBVTT\n\n" + strings.TrimLeft(converted, "\ufeff")), nil

	case ".ass", ".ssa":
		return []byte("WEBVTT\n\n" + assToVTT(text)), nil
	}
	return nil, fmt.Errorf("不支持转 VTT 的字幕格式：%s", ext)
}

// assToVTT 抽取 [Events] 里的 Dialogue 行。
func assToVTT(text string) string {
	var out strings.Builder
	for _, line := range strings.Split(text, "\n") {
		m := assDialogue.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// 分组顺序：(层号)(起始)(结束)——起止时间是 m[2]/m[3]，
		// 用 m[1] 会拿到层号 "0"，时间解析失败后整行被跳过、输出为空
		start := assTimeToVTT(m[2])
		end := assTimeToVTT(m[3])
		if start == "" || end == "" {
			continue
		}
		// Dialogue 的字段从第 10 个才是文本：按逗号切 9 列后剩余部分原样拼回
		// （对白文本里可能带逗号，不能按 index 取）
		rest := strings.SplitN(line, ",", 10)
		if len(rest) < 10 {
			continue
		}
		body := rest[9]
		body = strings.ReplaceAll(body, "\\N", "\n")
		body = strings.ReplaceAll(body, "\\n", "\n")
		body = assTags.ReplaceAllString(body, "")
		body = strings.ReplaceAll(body, "\n\n", "\n")
		out.WriteString(start + " --> " + end + "\n" + strings.TrimSpace(body) + "\n\n")
	}
	return out.String()
}

// assTimeToVTT：H:MM:SS.cc（厘秒，两位）→ HH:MM:SS.mmm
func assTimeToVTT(t string) string {
	t = strings.TrimSpace(t)
	parts := strings.Split(t, ":")
	if len(parts) != 3 {
		return ""
	}
	var h, m int
	var sec float64
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return ""
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return ""
	}
	if _, err := fmt.Sscanf(parts[2], "%f", &sec); err != nil {
		return ""
	}
	d := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute +
		time.Duration(sec*1000)*time.Millisecond
	total := int64(d / time.Millisecond)
	return fmt.Sprintf("%02d:%02d:%02d.%03d", total/3600000, total%3600000/60000,
		total%60000/1000, total%1000)
}
