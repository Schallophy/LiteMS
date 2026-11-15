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
	"sync/atomic"
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
	hlsSegmentTime  = 4  // 每段秒数
	hlsReadRate     = 1  // 限速到实时：比这更快就会跑在播放进度前面，窗口滑过去后观众还在原地
	hlsInitialBurst = 16 // 开头先超速读这么多秒，把起播缓冲一次性备好
	hlsKeepSegs     = 45 // 保留最近多少段（约 3 分钟窗口，供回拖）
	hlsIdleStop     = 90 * time.Second
)

var hlsSegName = regexp.MustCompile(`^seg(\d{5})\.m4s$`)

type HLSSession struct {
	Token    string
	Item     string
	Track    int
	Src      string
	Start    float64
	Dir      string
	HasAudio bool
	VCodec   string // 源视频编码（探一次，Master 复用，避免重复 ffprobe）

	mu     sync.Mutex
	cmd    *exec.Cmd
	last   time.Time
	master string      // 惰性生成的 master.m3u8（带精确 CODECS）
	exited atomic.Bool // ffmpeg 已退出：会话不能再复用，也不会再有新分段
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
	// 每个进程用独立的实例目录：多个 serve 共用同一个数据库目录时，
	// 谁启动都不能清掉别人正在写、正在播的分段。崩溃残留下来的目录
	// （含旧版本直接放在根下的 token 目录）只清理一天以前的。
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				os.RemoveAll(filepath.Join(dir, e.Name()))
			}
		}
	}
	root := filepath.Join(dir, "inst-"+newToken())
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("创建实时转码实例目录失败: %w", err)
	}
	h := &HLSServer{dir: root, byItem: map[string]*HLSSession{}, byTok: map[string]*HLSSession{}}
	go h.janitor()
	return h, nil
}

// Close 结束所有会话并清掉本实例的分段目录。进程退出前必须调用，
// 否则 ffmpeg 会变成孤儿进程，继续转码、继续占 CPU。
func (h *HLSServer) Close() {
	h.mu.Lock()
	for _, s := range h.byTok {
		h.stopLocked(s)
	}
	root := h.dir
	h.mu.Unlock()
	os.RemoveAll(root)
}

// Start 为某条目开一个从 start 秒起、使用第 track 条音轨的会话。
// 同一时刻同一位置且同一条音轨重复调用复用现有会话；换轨或拖动会重启 ffmpeg。
func (h *HLSServer) Start(item, src string, start float64, track int) (*HLSSession, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, errors.New("ffmpeg 未安装——Termux 上执行 pkg install ffmpeg")
	}
	if _, err := os.Stat(src); err != nil {
		return nil, errors.New("源文件不存在")
	}
	if start < 0 {
		start = 0
	}
	// 起播点先对准关键帧：视频是 -c:v copy，只能从关键帧剪，实际起点会比请求
	// 的时刻早 1–2 秒。字幕是整片绝对时间轴，进度也是 base + currentTime，
	// 都得按真实起点算，否则字幕会整体偏那么多（实测 0.767 秒）。
	if start > 0 {
		start = keyframeAtOrBefore(src, start)
	}

	// 先做一次不跑 ffprobe 的快速复用检查：同一位置重复请求不该每次都探测
	h.mu.Lock()
	if s := h.byItem[item]; s != nil && !s.exited.Load() && s.Track == track {
		if d := s.Start - start; d > -2.5 && d < 2.5 {
			s.touch()
			h.mu.Unlock()
			return s, nil
		}
	}
	h.mu.Unlock()

	// ffprobe 在弱存储/网络挂载上可能等到 10 秒超时；不能持全局锁跑，
	// 否则 Lookup/Ping/其它起播请求全部被堵住。
	hasAudio := probeChannels(src, track) > 0
	vcodec := probeVideoCodec(src)

	h.mu.Lock()
	defer h.mu.Unlock()
	// 探测期间可能已经有人建好了会话：仍按“可复用则复用”处理
	if s := h.byItem[item]; s != nil {
		if !s.exited.Load() && s.Track == track {
			if d := s.Start - start; d > -2.5 && d < 2.5 {
				s.touch()
				return s, nil
			}
		}
		h.stopLocked(s)
	}

	tok := newToken()
	dir := filepath.Join(h.dir, tok)
	for _, sub := range []string{"video", "audio"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	s := &HLSSession{
		Token: tok, Item: item, Track: track, Src: src, Start: start,
		Dir: dir, HasAudio: hasAudio, VCodec: vcodec, last: time.Now(),
	}

	args := []string{
		"-y", "-nostdin",
		"-ss", strconv.FormatFloat(start, 'f', 3, 64),
		// -ss 配 -noaccurate_seek：默认的精确 seek 会解码并丢弃到目标时间点，
		// 但视频是 -c:v copy，丢不了包，只能从更早的关键帧开始；音频能精确丢，
		// 于是两条流内容起点错开（实测 0.75 秒），时间戳又都归零 —— 表现为
		// 声音稳定滞后画面。关掉精确 seek 后两者都从同一个关键帧起步。
		"-noaccurate_seek",
		// -readrate 是输入选项，必须在 -i 之前：按实时速度读，跟观众的播放进度同步；
		// 开头那 hlsInitialBurst 秒先全速读完，否则起播得干等十几秒。
		"-readrate", strconv.Itoa(hlsReadRate),
		"-readrate_initial_burst", strconv.Itoa(hlsInitialBurst),
		"-i", src,
		"-map", "0:v:0",
	}
	if hasAudio {
		args = append(args, "-map", fmt.Sprintf("0:a:%d", track))
	}
	args = append(args, "-c:v", "copy")
	// Chromium 的 MSE 只认 hvc1 这个 sample entry，ffmpeg 默认写 hev1，
	// 会直接 MEDIA_ERR_SRC_NOT_SUPPORTED。HEVC 源必须显式改成 hvc1。
	if vcodec == "hevc" {
		args = append(args, "-tag:v", "hvc1")
	}
	if hasAudio {
		args = append(args, "-c:a", "aac", "-b:a", audioBitrate(),
			// 必须显式给声道数。一是 5.1(side) 这类布局下 ffmpeg 默认会往
			// AudioSpecificConfig 里写 channelConfig=0（要求解码器读 PCE），
			// Chromium 的 MSE 解析不了，append 直接报 parsing failed；
			// 二是浏览器播放一律下混立体声（理由见 audioPlaybackChannels）。
			"-ac", strconv.Itoa(audioPlaybackChannels))
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
	go func() {
		_ = cmd.Wait()
		s.exited.Store(true)
	}()

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

// Ping 只刷新会话的活跃时间，不改变 ffmpeg 状态。暂停期间前端靠它续命，
// 否则 janitor 会把「没人请求」误判成没人看。
func (h *HLSServer) Ping(item, tok string) bool {
	return h.Lookup(item, tok) != nil
}

// Stop 立刻结束会话并清掉分段。切回直连 / 预转码后没必要等 janitor
// 再让 ffmpeg 空转 90 秒。
func (h *HLSServer) Stop(item, tok string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.byTok[tok]
	if s == nil || s.Item != item {
		return false
	}
	h.stopLocked(s)
	return true
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
	if s.master != "" {
		out := s.master
		s.mu.Unlock()
		return out
	}
	s.mu.Unlock()

	// 读 init 段与探测尺寸都不在锁内：ffprobe 可能等满 10 秒，
	// 持 s.mu 会连带堵住 janitor 的 idle 检查。
	var initSeg []byte
	if p := findInitFile(s.Dir); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			initSeg = b
		}
	}
	video := videoCodecString(s.VCodec, initSeg)
	codecs, codecAttr, audioAttr := "", "", ""
	if video != "" {
		codecs = video
		if s.HasAudio {
			codecs += ",mp4a.40.2"
		}
		codecAttr = ",CODECS=\"" + codecs + "\""
	}
	media := ""
	if s.HasAudio {
		media = "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"audio\",DEFAULT=YES,URI=\"audio/index.m3u8\"\n"
		audioAttr = ",AUDIO=\"aud\""
	}
	res := ""
	if w, h := probeSize(s.Src); w > 0 {
		res = fmt.Sprintf(",RESOLUTION=%dx%d", w, h)
	}
	master := "#EXTM3U\n#EXT-X-VERSION:7\n" + media +
		"#EXT-X-STREAM-INF:BANDWIDTH=8000000" + codecAttr + res + audioAttr + "\n" +
		"video/index.m3u8\n"

	s.mu.Lock()
	if s.master == "" {
		s.master = master
	}
	out := s.master
	s.mu.Unlock()
	return out
}

// videoCodecString 按源编码生成 RFC 6381 编码串。无法识别时返回空串，
// 让 master 省掉 CODECS 属性——宁可让 hls.js 从 init 段自行判断，
// 也不能把 AV1/VP9 谎报成 HEVC。
func videoCodecString(codec string, initSeg []byte) string {
	switch codec {
	case "hevc":
		if v := hevcCodecString(initSeg); v != "" {
			return v
		}
		return "hvc1.2.4.L150.B0"
	case "h264":
		if v := avcCodecString(initSeg); v != "" {
			return v
		}
		return "avc1.42E01E"
	case "av1":
		if v := av1CodecString(initSeg); v != "" {
			return v
		}
		return "av01.0.05M.08"
	case "vp9":
		return "vp09.00.10.08"
	case "mpeg4":
		return "mp4v.20.8"
	}
	return ""
}

// av1CodecString 从 av1C 盒构造 av01.P.LLT.DD。
func av1CodecString(b []byte) string {
	i := bytes.Index(b, []byte("av1C"))
	if i < 0 || i+4+3 > len(b) {
		return ""
	}
	p := b[i+4:]
	profile := p[1] >> 5
	level := p[1] & 0x1f
	tier := byte('M')
	if p[2]&0x80 != 0 {
		tier = 'H'
	}
	depth := 8
	if p[2]&0x40 != 0 {
		depth = 10
	}
	if p[2]&0x20 != 0 {
		depth = 12
	}
	return fmt.Sprintf("av01.%d.%02d%c.%02d", profile, level, tier, depth)
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

// keyframeAtOrBefore 返回 start 之前（含）最近一个视频关键帧的时间。
// 实时转码从关键帧起流，调用方拿它当「真实起点」用：字幕偏移、进度显示都靠它。
// 只读 start 前 8 秒的包，实测约 140ms；探不到就退回原值（宁可差一点也不卡住）。
func keyframeAtOrBefore(src string, start float64) float64 {
	if start <= 0 {
		return 0
	}
	from := start - 8
	if from < 0 {
		from = 0
	}
	out, err := runProbe("-v", "error", "-select_streams", "v:0",
		// 注意 Sprintf 里 %% 才能输出字面量 %
		"-read_intervals", fmt.Sprintf("%.3f%%%.3f", from, start),
		"-show_entries", "packet=pts_time,flags", "-of", "csv=p=0", src)
	if err != nil {
		return start
	}
	best := -1.0
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Split(strings.TrimSpace(line), ",")
		if len(parts) < 2 || !strings.Contains(parts[1], "K") {
			continue
		}
		t, err := strconv.ParseFloat(parts[0], 64)
		if err != nil || t > start+0.05 {
			continue
		}
		if t > best {
			best = t
		}
	}
	if best < 0 {
		return start
	}
	return best
}

// probeSize 取视频宽高（master 里的 RESOLUTION 用）。
func probeSize(src string) (int, int) {
	out, err := runProbe("-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=p=0", src)
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

func newToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// segmentGone 判断请求的分段是否已经不可能再出现：ffmpeg 已退出，或分段号
// 早于当前播放列表的窗口起点（滑动窗口已经删掉）。这两种情况都不值得再等
// 8 秒——请求堆着只会拖住后面的分段。
func (s *HLSSession) segmentGone(name string) bool {
	base := filepath.Base(name)
	m := hlsSegName.FindStringSubmatch(base)
	if m == nil {
		return false
	}
	full := filepath.Join(s.Dir, filepath.FromSlash(name))
	if fi, err := os.Stat(full); err == nil && fi.Size() > 0 {
		return false
	}
	if s.exited.Load() {
		return true
	}
	n, _ := strconv.Atoi(m[1])
	dir := filepath.Dir(filepath.FromSlash(name))
	pl, err := os.ReadFile(filepath.Join(s.Dir, dir, "index.m3u8"))
	if err != nil {
		return false
	}
	min := -1
	for _, line := range strings.Split(string(pl), "\n") {
		mm := hlsSegName.FindStringSubmatch(strings.TrimSpace(line))
		if mm == nil {
			continue
		}
		if v, err := strconv.Atoi(mm[1]); err == nil && (min < 0 || v < min) {
			min = v
		}
	}
	return min >= 0 && n < min
}

// waitInitFile 等视频 rendition 的 init 段落盘。ffmpeg 用 -var_stream_map 时
// 文件名叫 init_0.mp4，不能用固定文件名等。
func waitInitFile(sessionDir string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if p := findInitFile(sessionDir); p != "" {
			if fi, err := os.Stat(p); err == nil && fi.Size() > 0 {
				return true
			}
		}
		time.Sleep(80 * time.Millisecond)
	}
	return false
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
