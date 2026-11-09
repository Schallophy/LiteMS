package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 实时转码（Direct Stream）：视频原样复制、音轨现场转 AAC，输出 HLS 分段，
// 边转边发、看完即删——磁盘上只留一小段窗口，不缓存整片。
//
// 这是 Jellyfin/Plex 的「Direct Stream」做法（只转音频、视频不动），
// 与「预转码」的差别只在音轨是提前编好还是播放时编。
//
// 音视频分成两条 rendition（video/ 与 audio/）：Chrome 的 MSE 不接受把带音轨的
// muxed init 塞进单个 SourceBuffer——init 一 append 就报错，表现为
// MEDIA_ERR_SRC_NOT_SUPPORTED。
//
// 每个条目同一时刻只有一个会话；换起播点（拖动）就重启 ffmpeg，
// 会话 URL 里带 token，旧会话的分段不会串到新时间轴上。

const (
	hlsSegmentTime  = 4 // 每段秒数
	hlsReadRate     = 1 // 限速到实时：比这更快就会跑在播放进度前面，窗口滑过去后观众还在原地
	hlsInitialBurst = 16 // 开头先超速读这么多秒，把起播缓冲一次性备好
	hlsKeepSegs     = 45 // 保留最近多少段（约 3 分钟窗口，供回拖）
	hlsIdleStop     = 90 * time.Second
)

var hlsSegName = regexp.MustCompile(`^seg(\d{5})\.m4s$`)

type HLSSession struct {
	Token    string
	Item     string
	Src      string
	Start    float64
	Dir      string
	HasAudio bool

	mu     sync.Mutex
	cmd    *exec.Cmd
	last   time.Time
	master string // 惰性生成的 master.m3u8（带精确 CODECS）
}

func (s *HLSSession) touch() {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
}

func (s *HLSSession) idle() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.last)
}

type HLSServer struct {
	mu     sync.Mutex
	dir    string
	byItem map[string]*HLSSession
	byTok  map[string]*HLSSession
}

func NewHLSServer(dir string) (*HLSServer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建实时转码目录失败: %w", err)
	}
	// 启动时清掉上次残留的会话目录
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	h := &HLSServer{dir: dir, byItem: map[string]*HLSSession{}, byTok: map[string]*HLSSession{}}
	go h.janitor()
	return h, nil
}

// Start 为某条目开一个从 start 秒起的会话。同一位置重复调用复用现有会话。
func (h *HLSServer) Start(item, src string, start float64) (*HLSSession, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, errors.New("ffmpeg 未安装——Termux 上执行 pkg install ffmpeg")
	}
	if _, err := os.Stat(src); err != nil {
		return nil, errors.New("源文件不存在")
	}
	if start < 0 {
		start = 0
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.byItem[item]; s != nil {
		if d := s.Start - start; d > -2.5 && d < 2.5 {
			s.touch()
			return s, nil
		}
		h.stopLocked(s)
	}

	hasAudio := probeChannels(src) > 0
	tok := newToken()
	dir := filepath.Join(h.dir, tok)
	for _, sub := range []string{"video", "audio"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	s := &HLSSession{
		Token: tok, Item: item, Src: src, Start: start,
		Dir: dir, HasAudio: hasAudio, last: time.Now(),
	}

	args := []string{
		"-y", "-nostdin",
		"-ss", strconv.FormatFloat(start, 'f', 3, 64),
		// -readrate 是输入选项，必须在 -i 之前：按实时速度读，跟观众的播放进度同步；
		// 开头那 hlsInitialBurst 秒先全速读完，否则起播得干等十几秒。
		"-readrate", strconv.Itoa(hlsReadRate),
		"-readrate_initial_burst", strconv.Itoa(hlsInitialBurst),
		"-i", src,
		"-map", "0:v:0",
	}
	if hasAudio {
		args = append(args, "-map", "0:a:0")
	}
	args = append(args, "-c:v", "copy")
	// Chromium 的 MSE 只认 hvc1 这个 sample entry，ffmpeg 默认写 hev1，
	// 会直接 MEDIA_ERR_SRC_NOT_SUPPORTED。HEVC 源必须显式改成 hvc1。
	if probeVideoCodec(src) == "hevc" {
		args = append(args, "-tag:v", "hvc1")
	}
	if hasAudio {
		args = append(args, "-c:a", "aac", "-b:a", audioBitrate(src),
			// 必须显式给声道数。源是 5.1(side) 这类布局时，ffmpeg 默认会往
			// AudioSpecificConfig 里写 channelConfig=0（要求解码器读 PCE），
			// Chromium 的 MSE 解析不了，append 直接报 parsing failed。
			// 明确 -ac 后写的是标准 channelConfig，MSE 才收。
			"-ac", strconv.Itoa(audioChannels(src)))
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(hlsSegmentTime),
		"-hls_segment_type", "fmp4",
		// 滑动窗口。必须是滑动播放列表（不带 playlist_type event）：
		// event 类型会忽略 hls_list_size、把所有分段都留在列表里，删掉文件就是 404。
		"-hls_list_size", strconv.Itoa(hlsKeepSegs),
		"-hls_flags", "delete_segments",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_segment_filename", filepath.Join(dir, "%v", "seg%05d.m4s"),
		"-master_pl_name", "master.m3u8",
	)
	if hasAudio {
		args = append(args, "-var_stream_map",
			"v:0,agroup:aud,name:video a:0,agroup:aud,name:audio,default:yes")
	} else {
		args = append(args, "-var_stream_map", "v:0,name:video")
	}
	args = append(args, filepath.Join(dir, "%v", "index.m3u8"))

	cmd := exec.Command("ffmpeg", args...)
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	s.cmd = cmd
	go func() { _ = cmd.Wait() }()

	h.byItem[item] = s
	h.byTok[tok] = s
	return s, nil
}

// Lookup 按 token 取会话（同时刷新活跃时间）。
func (h *HLSServer) Lookup(item, tok string) *HLSSession {
	h.mu.Lock()
	s := h.byTok[tok]
	h.mu.Unlock()
	if s == nil || s.Item != item {
		return nil
	}
	s.touch()
	return s
}

// Pause 暂停/恢复会话里的 ffmpeg（SIGSTOP / SIGCONT）。
// 观众按暂停时如果 ffmpeg 照跑，实时窗口会一路滑过去，等回到播放时
// 当前位置的分段早被删了。停一下既省 CPU 也保住窗口。
func (h *HLSServer) Pause(item, tok string, pause bool) bool {
	s := h.Lookup(item, tok)
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return false
	}
	sig := syscall.SIGCONT
	if pause {
		sig = syscall.SIGSTOP
	}
	s.touch() // 暂停期间靠心跳续命，别被 janitor 回收
	return s.cmd.Process.Signal(sig) == nil
}

func (h *HLSServer) stopLocked(s *HLSSession) {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	delete(h.byItem, s.Item)
	delete(h.byTok, s.Token)
	os.RemoveAll(s.Dir)
}

// janitor 定期回收没人看的会话——ffmpeg 不能在看片结束后继续占着 CPU 写盘。
func (h *HLSServer) janitor() {
	for range time.Tick(20 * time.Second) {
		var dead []*HLSSession
		h.mu.Lock()
		for _, s := range h.byTok {
			if s.idle() > hlsIdleStop {
				dead = append(dead, s)
			}
		}
		for _, s := range dead {
			h.stopLocked(s)
		}
		h.mu.Unlock()
	}
}

// Master 生成 master.m3u8：ffmpeg 自带的 master 不带 CODECS 属性，而 hls.js
// 要靠它决定 MSE 的 mime——codecs 为空时 addSourceBuffer 直接失败。
// 编码串从 init 段的 hvcC/avcC 里解析，保证和码流自洽。
func (s *HLSSession) Master() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.master != "" {
		return s.master
	}

	video, audio := "", "mp4a.40.2"
	if p := findInitFile(s.Dir); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			switch probeVideoCodec(s.Src) {
			case "hevc":
				video = hevcCodecString(b)
			case "h264":
				video = avcCodecString(b)
			}
		}
	}
	if video == "" {
		video = "hvc1.2.4.L150.B0"
	}
	codecs := video
	media := ""
	if s.HasAudio {
		codecs += "," + audio
		media = "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"audio\",DEFAULT=YES,URI=\"audio/index.m3u8\"\n"
	}
	res := ""
	if w, h := probeSize(s.Src); w > 0 {
		res = fmt.Sprintf(",RESOLUTION=%dx%d", w, h)
	}
	audioAttr := ""
	if s.HasAudio {
		audioAttr = ",AUDIO=\"aud\""
	}
	s.master = "#EXTM3U\n#EXT-X-VERSION:7\n" + media +
		"#EXT-X-STREAM-INF:BANDWIDTH=8000000,CODECS=\"" + codecs + "\"" + res + audioAttr + "\n" +
		"video/index.m3u8\n"
	return s.master
}

// hevcCodecString 从 hvcC 盒构造 RFC 6381 的 HEVC 编码串（hvc1.2.4.L150.B0 这种）。
func hevcCodecString(b []byte) string {
	i := bytes.Index(b, []byte("hvcC"))
	if i < 0 || i+4+13 > len(b) {
		return ""
	}
	p := b[i+4:]
	space := p[1] >> 6
	tier := (p[1] >> 5) & 1
	idc := p[1] & 0x1f
	compat := binary.BigEndian.Uint32(p[2:6])
	level := p[12]

	var sb strings.Builder
	sb.WriteString("hvc1.")
	if space > 0 {
		sb.WriteByte(byte('A' + space - 1))
	}
	sb.WriteString(strconv.Itoa(int(idc)))
	sb.WriteByte('.')
	sb.WriteString(strings.ToUpper(strconv.FormatUint(uint64(reverseBits32(compat)), 16)))
	sb.WriteByte('.')
	if tier == 1 {
		sb.WriteByte('H')
	} else {
		sb.WriteByte('L')
	}
	sb.WriteString(strconv.Itoa(int(level)))
	// 约束字节：最多 6 个，尾部 0 不写
	cs := p[6:12]
	end := len(cs)
	for end > 0 && cs[end-1] == 0 {
		end--
	}
	if end > 0 {
		sb.WriteByte('.')
		sb.WriteString(strings.ToUpper(hex.EncodeToString(cs[:end])))
	}
	return sb.String()
}

func avcCodecString(b []byte) string {
	i := bytes.Index(b, []byte("avcC"))
	if i < 0 || i+4+4 > len(b) {
		return ""
	}
	p := b[i+4:]
	return fmt.Sprintf("avc1.%02X%02X%02X", p[1], p[2], p[3])
}

func reverseBits32(v uint32) uint32 {
	var r uint32
	for i := 0; i < 32; i++ {
		r = (r << 1) | (v & 1)
		v >>= 1
	}
	return r
}

// probeSize 取视频宽高（master 里的 RESOLUTION 用）。
func probeSize(src string) (int, int) {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=p=0", src).Output()
	if err != nil {
		return 0, 0
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(parts) != 2 {
		return 0, 0
	}
	w, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
	return w, h
}

// audioChannels 取输出声道数：源声道数，夹到 AAC 能标准表达的范围。
// 8 声道（7.1）在 AAC 里没有标准 channelConfig，硬塞会让 ffmpeg 退回 PCE，
// 所以统一降到 6；探不到就按立体声。
func audioChannels(src string) int {
	switch n := probeChannels(src); {
	case n <= 0:
		return 2
	case n > 6:
		return 6
	default:
		return n
	}
}

func newToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// waitFile 等 ffmpeg 把某个文件写出来（客户端可能比转码先到）。
func waitFile(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			return true
		}
		time.Sleep(80 * time.Millisecond)
	}
	return false
}

// ffmpegReady 判断设备上是否装了 ffmpeg（实时转码模式的先决条件）。
func ffmpegReady() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// findInitFile 找视频 rendition 的 init 段。ffmpeg 用 -var_stream_map 时会给
// 每条 rendition 的 init 加序号（init_0.mp4 / init_1.mp4），名字不固定。
func findInitFile(dir string) string {
	matches, _ := filepath.Glob(filepath.Join(dir, "video", "init*.mp4"))
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// safeHLSPath 把请求的相对路径（可能是 video/seg00001.m4s）拼到会话目录下，
// 只放行播放列表、init 段和媒体分段，拒绝路径穿越。
func safeHLSPath(dir, name string) (string, bool) {
	name = filepath.ToSlash(name)
	if strings.Contains(name, "..") {
		return "", false
	}
	base := filepath.Base(name)
	ok := base == "index.m3u8" || base == "init.mp4" || strings.HasPrefix(base, "init_") ||
		hlsSegName.MatchString(base)
	if !ok {
		return "", false
	}
	return filepath.Join(dir, filepath.FromSlash(name)), true
}
