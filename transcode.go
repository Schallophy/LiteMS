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

// 音轨转码：把浏览器解不了的音轨（EAC3/DTS/TrueHD…）转成 AAC，视频流原样
// 复制不重编码。手机 CPU 只需要扛音频编码，一部 2 小时的片几分钟就能转完。
//
// 结果缓存为 <cacheDir>/<id>.mkv，/stream/{id}?web=1 优先返回它——所以网页
// 播放器零改动地获得「转码后可拖进度条的完整文件」。缓存放数据库同目录下
// （$HOME 私有存储，不碰媒体库/下载目录，不会被扫描器当条目捡走）。
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
func NewTranscoder(cacheDir string) (*Transcoder, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建转码缓存目录失败: %w", err)
	}
	matches, _ := filepath.Glob(filepath.Join(cacheDir, "*.part"))
	for _, m := range matches {
		os.Remove(m)
	}
	return &Transcoder{jobs: map[string]*tcJob{}, cacheDir: cacheDir}, nil
}

func (t *Transcoder) CachePath(id string) string {
	return filepath.Join(t.cacheDir, id+".mkv")
}

// Has 返回缓存文件是否存在（serve ?web=1 与状态查询都以它为准）。
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
func (t *Transcoder) Start(id, src string) (tcJob, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return tcJob{}, errors.New("ffmpeg 未安装——Termux 上执行 pkg install ffmpeg")
	}
	if _, err := os.Stat(src); err != nil {
		return tcJob{}, errors.New("源文件不存在")
	}

	t.mu.Lock()
	defer t.mu.Unlock()
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
		"-map", "0:v:0", "-map", "0:a:0?", "-map", "0:s?",
		"-c:v", "copy", // 视频原样复制，画质无损、CPU 几乎为零
		"-c:a", "aac", "-b:a", "320k",
		"-c:s", "copy",
		"-progress", "pipe:1", "-nostats",
		"-f", "matroska", part,
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
