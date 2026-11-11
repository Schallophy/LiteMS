package main

import (
	"fmt"
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
		{"film.chs.srt", "", "film.chs.srt", false},    // 已带语言标记不重复加
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

// 基准轨挑选：用户指定且可用就照办；指定的失效时回落自动；自动时内嵌轨优先。
func TestPickSubRef(t *testing.T) {
	subs := []SubDTO{
		{Lang: "chs", Src: "sidecar", URL: "/sub/x?sub=0"},
		{Lang: "eng", Src: "embed", URL: "/sub/x?embed=2"},
		{Lang: "cht", Src: "sidecar", URL: "/sub/x?sub=1"},
		{Lang: "jpn", Src: "embed"}, // 图像字幕，没 URL，不能当基准
	}
	cases := []struct {
		name, ref string
		wantIdx   int
		wantBack  bool
	}{
		{"不指定就用内嵌", "", 1, false},
		{"指定外挂", "/sub/x?sub=0", 0, false},
		{"指定内嵌", "/sub/x?embed=2", 1, false},
		{"指定的字幕没了", "/sub/x?imp=gone.srt", 1, true},
		{"指向图像字幕也算失效", "/sub/x?embed=9", 1, true},
	}
	for _, c := range cases {
		idx, back := pickSubRef(subs, c.ref)
		if idx != c.wantIdx || back != c.wantBack {
			t.Errorf("%s: pickSubRef(%q) = (%d,%v)，想要 (%d,%v)",
				c.name, c.ref, idx, back, c.wantIdx, c.wantBack)
		}
	}

	// 没有内嵌轨时，自动挑第一条网页能播的
	idx, back := pickSubRef([]SubDTO{subs[0], subs[2]}, "")
	if idx != 0 || back {
		t.Errorf("无内嵌轨应挑第一条，got (%d,%v)", idx, back)
	}
	if idx, _ := pickSubRef([]SubDTO{subs[3]}, ""); idx != -1 {
		t.Errorf("全都没 URL 应返回 -1，got %d", idx)
	}
}

// 换基准轨后同一条字幕的偏移要换符号：B 比 A 晚 2 秒，
// 那 B 相对 A 是 +2，A 相对 B 是 -2。
func TestSubOffsetsWithRef(t *testing.T) {
	dir := t.TempDir()
	writeSRT := func(name string, start int) {
		var b strings.Builder
		for i := 0; i < 20; i++ { // medianOffset 要求至少 10 条
			s, e := start+i*30, start+i*30+2
			fmt.Fprintf(&b, "%d\n%02d:%02d:%02d,000 --> %02d:%02d:%02d,000\nline %d\n\n",
				i+1, s/3600, s%3600/60, s%60, e/3600, e%3600/60, e%60, i+1)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeSRT("a.chs.srt", 0)
	writeSRT("b.eng.srt", 2)

	s := &Server{roots: []string{dir}, cfg: &Config{}}
	it := Item{ID: "00000000000000d1", RootIdx: 0, RelPath: "movie.mkv",
		SubPaths: []SubEntry{{Lang: "chs", Path: "a.chs.srt"}, {Lang: "eng", Path: "b.eng.srt"}}}

	urlA := "/sub/" + it.ID + "?sub=0"
	urlB := "/sub/" + it.ID + "?sub=1"

	// 没有内嵌轨 → 自动拿第一条（A）当基准，B 晚 2 秒
	plan := s.subOffsetsFor(it, "")
	if plan.Ref != urlA || plan.Auto {
		t.Fatalf("自动基准应为 A，got ref=%q auto=%v", plan.Ref, plan.Auto)
	}
	if got := plan.Offsets[urlB]; got != 2 {
		t.Errorf("B 相对 A 应为 +2，got %v", got)
	}

	// 用户指定 B 当基准：A 反过来变成 -2，基准轨自己不出现在结果里
	plan = s.subOffsetsFor(it, urlB)
	if plan.Ref != urlB {
		t.Fatalf("基准应变成 B，got %q", plan.Ref)
	}
	if got := plan.Offsets[urlA]; got != -2 {
		t.Errorf("A 相对 B 应为 -2，got %v", got)
	}
	if _, ok := plan.Offsets[urlB]; ok {
		t.Errorf("基准轨自己不该有偏移")
	}

	// 指定的基准失效：回落自动挑选，并且如实告诉前端（auto=true）
	plan = s.subOffsetsFor(it, "/sub/"+it.ID+"?imp=gone.srt")
	if plan.Ref != urlA || !plan.Auto {
		t.Errorf("失效基准应回落 A 且标 auto，got ref=%q auto=%v", plan.Ref, plan.Auto)
	}
}
