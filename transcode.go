package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 音轨转码（预转码模式）：只把浏览器解不了的音轨（EAC3/DTS/TrueHD…）转成 AAC，
// 输出一条独立的 .m4a，视频仍用原文件——浏览器本来就能解画面，缺的只是音频解码器。
//
// 为什么不连视频一起转：那会多占一份和原片等大的磁盘（4.2GB），而这里只要
// 一条音轨的体积（5.1 @ 384k，2 小时约 400MB）。播放时由页面把原视频（静音）
// 与这条音轨同步起来。
//
// 状态只存内存：转码是可重复的长任务，进程重启后未完成的任务视为从未开始
// （.part 残留文件在启动时清掉）；已完成的以缓存文件存在为准。

type tcJob struct {
	State    string  `json:"state"` // none / running / done / error
	Progress float64 `json:"progress"`
	Error    string  `json:"error,omitempty"`
}

type Transcoder struct {
	mu       sync.Mutex
	jobs     map[string]*tcJob
	cacheDir string
	cancel   context.CancelFunc
}

// NewTranscoder 建缓存目录并清掉上次中断留下的 .part 文件。
// 顺带清掉旧版本留下的整片转码缓存（<id>.mkv，每部 4GB）——现在只存音轨。
func NewTranscoder(cacheDir string) (*Transcoder, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建转码缓存目录失败: %w", err)
	}
	matches, _ := filepath.Glob(filepath.Join(cacheDir, "*.part"))
	for _, m := range matches {
		os.Remove(m)
	}
	if old, _ := filepath.Glob(filepath.Join(cacheDir, "*.mkv")); len(old) > 0 {
		for _, m := range old {
			os.Remove(m)
		}
		logf("已清理 %d 个旧版整片转码缓存（现在只缓存音轨）", len(old))
	}
	return &Transcoder{jobs: map[string]*tcJob{}, cacheDir: cacheDir}, nil
}

func (t *Transcoder) CachePath(id string) string {
	return filepath.Join(t.cacheDir, id+".m4a")
}

// Has 返回音轨缓存是否存在（播放页据此决定用「预转码」还是「实时转码」）。
func (t *Transcoder) Has(id string) bool {
	fi, err := os.Stat(t.CachePath(id))
	return err == nil && fi.Size() > 0
}

// Status 返回当前状态；内存里没有但缓存文件在 → done（服务重启后依然成立）。
func (t *Transcoder) Status(id string) tcJob {
	t.mu.Lock()
	defer t.mu.Unlock()
	if j, ok := t.jobs[id]; ok {
		return *j
	}
	if t.Has(id) {
		return tcJob{State: "done", Progress: 1}
	}
	return tcJob{State: "none"}
}

// Start 启动转码。同一 id：running/done 直接返回现状；error 允许重试；
// 缓存比源文件旧（源被替换/更新）也重转。同一时刻只允许一个任务（手机 CPU）。
// force=true 时丢掉已有缓存重转（音轨参数改过之后要靠它把旧产物换掉）。
func (t *Transcoder) Start(id, src string, force bool) (tcJob, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return tcJob{}, errors.New("ffmpeg 未安装——Termux 上执行 pkg install ffmpeg")
	}
	if _, err := os.Stat(src); err != nil {
		return tcJob{}, errors.New("源文件不存在")
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if force {
		if j, ok := t.jobs[id]; ok && j.State == "running" {
			return *j, nil // 正在转就别打断，等它自己结束
		}
		os.Remove(t.CachePath(id))
		delete(t.jobs, id)
	}
	if j, ok := t.jobs[id]; ok {
		switch j.State {
		case "running":
			return *j, nil
		case "done":
			// 源文件比缓存新 → 视为需要重转，否则命中缓存
			if fi, err := os.Stat(src); err == nil {
				if cf, err2 := os.Stat(t.CachePath(id)); err2 == nil && !fi.ModTime().After(cf.ModTime()) {
					return *j, nil
				}
			}
		}
	}
	if t.Has(id) {
		// 无记录但缓存文件在（服务重启后的 done 状态）：缓存与源同 mtime
		// （完成时对齐过）说明有效，恢复为 done；源更新过则落到下面重转
		if fi, err := os.Stat(src); err == nil {
			if cf, err2 := os.Stat(t.CachePath(id)); err2 == nil && !fi.ModTime().After(cf.ModTime()) {
				j := tcJob{State: "done", Progress: 1}
				t.jobs[id] = &j
				return j, nil
			}
		}
	}
	if t.cancel != nil {
		// 有任务在跑：找出是谁
		for jid, j := range t.jobs {
			if j.State == "running" && jid != id {
				return tcJob{}, errors.New("已有转码任务在进行，等它完成后再试")
			}
		}
	}

	fi, err := os.Stat(src)
	if err != nil {
		return tcJob{}, err
	}
	dur := probeDuration(src)

	ctx, cancel := context.WithCancel(context.Background())
	job := &tcJob{State: "running"}
	t.jobs[id] = job
	t.cancel = cancel
	go t.run(ctx, id, src, fi.ModTime(), dur)
	return *job, nil
}

func (t *Transcoder) run(ctx context.Context, id, src string, srcMod time.Time, durSec float64) {
	part := t.CachePath(id) + ".part"
	final := t.CachePath(id)
	os.Remove(part)

	args := []string{
		"-y", "-nostdin",
		"-i", src,
		"-vn",
		"-map", "0:a:0?",
		"-c:a", "aac", "-b:a", audioBitrate(),
		// -ac 必须显式给：5.1(side) 这类布局 ffmpeg 默认写 channelConfig=0（要读 PCE），
		// Chromium 的 mp4 解析器不认，<audio> 会直接放不出来。理由同 hls.go。
		"-ac", strconv.Itoa(audioPlaybackChannels),
		"-progress", "pipe:1", "-nostats",
		"-f", "mp4", part,
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.finish(id, tcJob{State: "error", Error: err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		t.finish(id, tcJob{State: "error", Error: err.Error()})
		return
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		if p := parseProgress(sc.Text(), durSec); p >= 0 {
			if p > 1 {
				p = 1
			}
			t.mu.Lock()
			if j, ok := t.jobs[id]; ok {
				j.Progress = p
			}
			t.mu.Unlock()
		}
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		t.finish(id, tcJob{State: "error", Error: "已取消"})
		return
	}
	if waitErr != nil {
		msg := waitErr.Error()
		if tail := errBuf.String(); len(tail) > 0 {
			lines := strings.Split(strings.TrimSpace(tail), "\n")
			if n := len(lines); n > 0 {
				msg = lines[n-1]
			}
		}
		os.Remove(part)
		t.finish(id, tcJob{State: "error", Error: msg})
		return
	}
	// 原子落位：完成后改回正式名字，部分文件绝不会冒充成品
	if err := os.Rename(part, final); err != nil {
		t.finish(id, tcJob{State: "error", Error: err.Error()})
		return
	}
	// 让缓存的 mtime 跟源文件对齐（用于「源更新过就重转」判断）
	os.Chtimes(final, srcMod, srcMod)
	t.finish(id, tcJob{State: "done", Progress: 1})
}

func (t *Transcoder) finish(id string, j tcJob) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.jobs[id] = &j
	if j.State != "running" {
		t.cancel = nil
	}
}

// probeDuration 用 ffprobe 拿总时长，供进度百分比计算；拿不到就返回 0
// （进度条显示不确定态，不影响转码本身）。
func probeDuration(src string) float64 {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", src).Output()
	if err != nil {
		return 0
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// parseProgress 从 -progress 输出里提取已转秒数（独立出来便于单测）。
func parseProgress(line string, durSec float64) float64 {
	k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
	if !ok || durSec <= 0 {
		return -1
	}
	switch k {
	case "out_time_us":
		if us, err := strconv.ParseFloat(v, 64); err == nil {
			return us / 1e6 / durSec
		}
	case "out_time_ms":
		if us, err := strconv.ParseFloat(v, 64); err == nil {
			// ffmpeg 的 out_time_ms 实际是微秒（字段名叫 ms 是历史坑），同 out_time_us
			return us / 1e6 / durSec
		}
	}
	return -1
}

// probeChannels 取第一条音轨的声道数，拿不到返回 0。
func probeChannels(src string) int {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=channels", "-of", "csv=p=0", src).Output()
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// probeVideoCodec 取第一条视频流的编码名（hevc/h264/av1…），拿不到返回空串。
func probeVideoCodec(src string) string {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", src).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// audioPlaybackChannels 是网页播放路径（预转码 / 实时转码）的输出声道数。
//
// 固定立体声，不是为省带宽，而是兼容性：网页播放要么走 <audio>、要么走 MSE，
// 多声道 AAC 能不能出声取决于机型的 AudioTrack 通道掩码——不少设备解码成功
// 却一点声音都不出，也不报错，症状和「音轨编码不支持」一模一样。下混到 2
// 声道是每个浏览器内核都稳定支持的路径。
//
// 要 5.1 原声走「mpv 播放」：它直接吃原文件，不经过这里。
const audioPlaybackChannels = 2

// audioBitrate 给 AAC 输出码率：立体声 192k，与主流媒体服务器同档。
func audioBitrate() string {
	return "192k"
}
