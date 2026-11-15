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
	job, err := tc.Start("abc123", 0, src, false)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "running" {
		t.Fatalf("Start 状态 = %s, want running", job.State)
	}

	deadline := time.Now().Add(90 * time.Second)
	for {
		j := tc.Status("abc123", 0)
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

	if !tc.Has("abc123", 0) {
		t.Fatal("完成后缓存文件不在")
	}
	// 音轨必须是 aac
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", tc.CachePath("abc123", 0)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "aac" {
		t.Errorf("输出音轨 = %s, want aac", got)
	}

	// 完成后再 Start：命中缓存直接返回 done
	job, err = tc.Start("abc123", 0, src, false)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "done" {
		t.Errorf("二次 Start 状态 = %s, want done", job.State)
	}
}

// 缓存列表只认成品文件名；.part 和陌生文件不进列表；Remove 删文件并清状态。
func TestTranscoderCachedAndRemove(t *testing.T) {
	dir := t.TempDir()
	tc, err := NewTranscoder(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef"
	if err := os.WriteFile(tc.CachePath(id, 0), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tc.CachePath(id, 2), []byte("bbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tc.CachePath(id, 1)+".part", []byte("part"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tc.cacheDir, "junk.m4a"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	files := tc.Cached()
	if len(files) != 2 {
		t.Fatalf("Cached 返回 %d 条，want 2: %+v", len(files), files)
	}
	sizes := map[string]int64{}
	for _, f := range files {
		sizes[tcKey(f.ID, f.Track)] = f.Size
	}
	if sizes[id+"@0"] != 3 || sizes[id+"@2"] != 4 {
		t.Errorf("缓存项不对: %+v", sizes)
	}

	if cancelled, removed := tc.Remove(id, 2); cancelled || !removed {
		t.Errorf("Remove(track 2) = cancelled=%v removed=%v, want false/true", cancelled, removed)
	}
	if tc.Has(id, 2) {
		t.Error("删除后缓存文件还在")
	}
	if got := tc.Status(id, 2); got.State != "none" {
		t.Errorf("删除后 Status = %s, want none", got.State)
	}
	if cancelled, removed := tc.Remove(id, 2); cancelled || removed {
		t.Errorf("重复 Remove = cancelled=%v removed=%v, want false/false", cancelled, removed)
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
	if _, err := tc.Start("abc123", 0, filepath.Join(dir, "x.mkv"), false); err == nil {
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
	if err := os.WriteFile(tc.CachePath("abc123", 0), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	if j := tc.Status("abc123", 0); j.State != "done" {
		t.Errorf("Status = %s, want done", j.State)
	}
	if j := tc.Status("nope", 0); j.State != "none" {
		t.Errorf("Status = %s, want none", j.State)
	}
}

// 多音轨：只转指定那条（0:a:1），产物与第 0 条分开缓存。
func TestTranscodeSecondTrack(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("无 ffmpeg，跳过")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	gen := exec.Command("ffmpeg", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=128x96:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-f", "lavfi", "-i", "sine=frequency=660",
		"-t", "2",
		"-map", "0:v", "-map", "1:a", "-map", "2:a",
		"-c:v", "libx264", "-preset", "ultrafast",
		"-c:a:0", "aac", "-c:a:1", "ac3", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("生成双音轨测试片失败: %v\n%s", err, out)
	}

	tc, err := NewTranscoder(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.Start("multi01", 1, src, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		j := tc.Status("multi01", 1)
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
	if !tc.Has("multi01", 1) {
		t.Fatal("第 2 条音轨缓存不在")
	}
	if tc.Has("multi01", 0) {
		t.Error("没转的第 1 条音轨不该有缓存")
	}
	if got, want := tc.CachePath("multi01", 1), filepath.Join(dir, "cache", "multi01.a1.m4a"); got != want {
		t.Errorf("缓存路径 = %s, want %s", got, want)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", tc.CachePath("multi01", 1)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "aac" {
		t.Errorf("输出音轨 = %s, want aac", got)
	}
}

// 取消转码：状态变 cancelled，正式文件和 .part 都不留，之后还能重新起任务。
func TestTranscodeCancel(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("无 ffmpeg，跳过")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	gen := exec.Command("ffmpeg", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=128x96:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "120",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "ac3", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("生成测试片失败: %v\n%s", err, out)
	}

	tc, err := NewTranscoder(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.Start("cancel1", 0, src, false); err != nil {
		t.Fatal(err)
	}
	if got := tc.Running(); len(got) != 1 || got[0].ID != "cancel1" || got[0].Track != 0 {
		t.Fatalf("Running = %+v, want cancel1@0", got)
	}
	// 等 ffmpeg 真正开始写产物，再取消（半成品名带任务序号）
	partPattern := tc.CachePath("cancel1", 0) + ".*.part"
	deadline := time.Now().Add(10 * time.Second)
	for {
		if m, _ := filepath.Glob(partPattern); len(m) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等 .part 出现超时")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if !tc.Cancel("cancel1", 0) {
		t.Fatal("Cancel 应返回 true")
	}

	deadline = time.Now().Add(10 * time.Second)
	for {
		st := tc.Status("cancel1", 0).State
		if st == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("取消后状态 = %s，want cancelled", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if tc.Has("cancel1", 0) {
		t.Error("取消后不该有正式缓存文件")
	}
	if m, _ := filepath.Glob(partPattern); len(m) > 0 {
		t.Errorf("取消后残留 .part: %v", m)
	}
	if got := tc.Running(); len(got) != 0 {
		t.Errorf("取消收尾后 Running = %+v, want 空", got)
	}
	// 取消后仍能重新起任务
	if _, err := tc.Start("cancel1", 0, src, false); err != nil {
		t.Fatalf("取消后重新启动失败: %v", err)
	}
	tc.Cancel("cancel1", 0) // 别让测试进程拖着 ffmpeg
	if tc.Cancel("cancel1", 0) {
		t.Error("没有任务时 Cancel 应返回 false")
	}
}

// Usable：缓存不比源旧才可用；源被替换（mtime 变新）后旧音轨必须作废。
func TestTranscoderUsable(t *testing.T) {
	dir := t.TempDir()
	tc, err := NewTranscoder(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	id := "0123456789abcdef"
	src := filepath.Join(dir, "src.mkv")
	if err := os.WriteFile(src, []byte("src"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tc.Usable(id, 0, src) {
		t.Error("没有缓存时 Usable 应为 false")
	}
	cache := tc.CachePath(id, 0)
	if err := os.WriteFile(cache, []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(cache, now, now); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !tc.Usable(id, 0, src) {
		t.Error("缓存比源新时应可用")
	}
	// 源被替换：mtime 比缓存新
	if err := os.Chtimes(src, now.Add(time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if tc.Usable(id, 0, src) {
		t.Error("源比缓存新时不应再用旧音轨")
	}
}

// 启动清扫要能认出带任务序号的半成品，同时不能碰另一个实例正在写的文件。
func TestTranscoderCleanupParts(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(cacheDir, "0123456789abcdef.a1.m4a.7.part")
	if err := os.WriteFile(part, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(cacheDir, "0123456789abcdef.a1.m4a.8.part")
	if err := os.WriteFile(fresh, []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(part, past, past); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTranscoder(cacheDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(part); err == nil {
		t.Error("一天前的半成品应被清掉")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("刚写的半成品可能属于另一个实例，不能清掉")
	}
}
