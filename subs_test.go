package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeSubName(t *testing.T) {
	cases := []struct {
		in, lang, want string
		bad            bool
	}{
		{"film.srt", "", "film.srt", false},
		{"film.srt", "chs", "film.chs.srt", false},
		{"film.chs.srt", "", "film.chs.srt", false},   // 已带语言标记不重复加
		{"film.chs.srt", "cht", "film.chs.srt", false}, // 文件名优先，尊重已有的
		{"../evil.srt", "", "evil.srt", false},         // 去掉路径
		{"a\\b\\c.ass", "", "c.ass", false},
		{"film.txt", "", "", true},
		{"noext", "", "", true},
		{"", "", "", true},
		{"..", "", "", true},
	}
	for _, c := range cases {
		got, err := sanitizeSubName(c.in, c.lang)
		if c.bad {
			if err == nil {
				t.Errorf("sanitizeSubName(%q) 应报错，got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("sanitizeSubName(%q) err: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("sanitizeSubName(%q,%q) = %q, want %q", c.in, c.lang, got, c.want)
		}
	}
}

// 导入字幕落盘 → 列表出现 → 同名再导入自动加序号。
func TestSubImportRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := &Server{roots: []string{dir}}
	it := Item{ID: "00000000000000c1", RootIdx: 0, RelPath: "movie.mkv"}

	if _, err := s.saveUploadedSub(it, strings.NewReader("1\n00:00:01,000 --> 00:00:02,000\nhi\n"), "movie.chs.srt"); err != nil {
		t.Fatal(err)
	}
	subs := s.importedSubs(it)
	if len(subs) != 1 {
		t.Fatalf("导入后应有 1 条，got %d", len(subs))
	}
	if subs[0].Lang != "chs" || subs[0].Src != "imported" || !strings.Contains(subs[0].URL, "imp=") {
		t.Errorf("导入条目字段不对: %+v", subs[0])
	}

	// 同名再来一次：不能覆盖，要变成 -2 后缀
	final, err := s.saveUploadedSub(it, strings.NewReader("x"), "movie.chs.srt")
	if err != nil {
		t.Fatal(err)
	}
	if final == "movie.chs.srt" {
		t.Errorf("同名导入应改名为不同文件")
	}
	if len(s.importedSubs(it)) != 2 {
		t.Errorf("同名导入后应有 2 条")
	}

	// 内嵌抽取缓存不算导入字幕
	if err := os.WriteFile(filepath.Join(s.subsDir(it), "embed-2.vtt"), []byte("WEBVTT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(s.importedSubs(it)) != 2 {
		t.Errorf("embed-* 缓存不应出现在导入列表")
	}
}

// 内嵌字幕：探测 + 抽取成 VTT。无 ffmpeg 环境跳过。
func TestEmbedSubExtract(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("无 ffmpeg，跳过")
	}
	dir := t.TempDir()
	srt := filepath.Join(dir, "in.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,500\n你好，内嵌字幕\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(dir, "movie.mkv")
	gen := exec.Command("ffmpeg", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc=size=128x96:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-i", srt,
		"-t", "2",
		"-map", "0:v", "-map", "1:a", "-map", "2:s",
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-c:s", "srt",
		"-metadata:s:s:0", "language=chi",
		video)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("生成测试片失败: %v\n%s", err, out)
	}

	s := &Server{roots: []string{dir}}
	it := Item{ID: "00000000000000c2", RootIdx: 0, RelPath: "movie.mkv"}

	subs := s.embeddedSubs(it)
	if len(subs) != 1 {
		t.Fatalf("应探测到 1 条内嵌字幕，got %d (%+v)", len(subs), subs)
	}
	if subs[0].Src != "embed" || subs[0].URL == "" {
		t.Errorf("内嵌条目字段不对: %+v", subs[0])
	}

	idx := strings.TrimPrefix(subs[0].URL[strings.Index(subs[0].URL, "embed="):], "embed=")
	path, err := s.extractEmbedSub(it, atoiOr(t, idx))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.HasPrefix(body, "WEBVTT") {
		t.Errorf("抽取结果不是 WebVTT: %.20q", body)
	}
	if !strings.Contains(body, "内嵌字幕") {
		t.Errorf("抽取结果缺少字幕文本")
	}
	// 二次调用命中缓存（mtime 与源对齐）
	if p2, err := s.extractEmbedSub(it, atoiOr(t, idx)); err != nil || p2 != path {
		t.Errorf("二次抽取应命中缓存: %q err=%v", p2, err)
	}
}

func atoiOr(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("非法数字 %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
