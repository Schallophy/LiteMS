package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseProgress(t *testing.T) {
	cases := []struct {
		line string
		dur  float64
		want float64
	}{
		{"out_time_us=5000000", 10, 0.5},
		{"out_time_ms=5000000", 10, 0.5}, // ffmpeg 的 out_time_ms 实际是微秒
		{"out_time_us=10000000", 10, 1.0},
		{"progress=continue", 10, -1},
		{"progress=end", 10, -1},
		{"out_time_us=5000000", 0, -1}, // 时长未知不给进度
		{"garbage", 10, -1},
	}
	for _, c := range cases {
		if got := parseProgress(c.line, c.dur); got != c.want {
			t.Errorf("parseProgress(%q, %v) = %v, want %v", c.line, c.dur, got, c.want)
		}
	}
}

// 端到端：生成 2 秒 AC3 音轨测试片 → 转码 → 缓存落位、音轨变 AAC。
// 无 ffmpeg 的环境跳过。
func TestTranscodeEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("无 ffmpeg，跳过")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	gen := exec.Command("ffmpeg", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=128x96:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "2",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "ac3", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("生成测试片失败: %v\n%s", err, out)
	}

	tc, err := NewTranscoder(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	job, err := tc.Start("abc123", src, false)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "running" {
		t.Fatalf("Start 状态 = %s, want running", job.State)
	}

	deadline := time.Now().Add(90 * time.Second)
	for {
		j := tc.Status("abc123")
		if j.State == "done" {
			break
		}
		if j.State == "error" {
			t.Fatalf("转码失败: %s", j.Error)
		}
		if time.Now().After(deadline) {
			t.Fatal("转码超时")
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !tc.Has("abc123") {
		t.Fatal("完成后缓存文件不在")
	}
	// 音轨必须是 aac
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", tc.CachePath("abc123")).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "aac" {
		t.Errorf("输出音轨 = %s, want aac", got)
	}

	// 完成后再 Start：命中缓存直接返回 done
	job, err = tc.Start("abc123", src, false)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "done" {
		t.Errorf("二次 Start 状态 = %s, want done", job.State)
	}
}

// 无 ffmpeg 时 Start 报错而不是启动任务。
func TestTranscodeNoFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		t.Skip("有 ffmpeg，跳过")
	}
	dir := t.TempDir()
	tc, err := NewTranscoder(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.Start("abc123", filepath.Join(dir, "x.mkv"), false); err == nil {
		t.Fatal("无 ffmpeg 应该报错")
	}
}

// 服务重启后状态恢复：无内存记录、缓存文件在 → Status 返回 done。
func TestTranscodeStatusAfterRestart(t *testing.T) {
	dir := t.TempDir()
	tc, err := NewTranscoder(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tc.CachePath("abc123"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	if j := tc.Status("abc123"); j.State != "done" {
		t.Errorf("Status = %s, want done", j.State)
	}
	if j := tc.Status("nope"); j.State != "none" {
		t.Errorf("Status = %s, want none", j.State)
	}
}
