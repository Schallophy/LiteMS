package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteScrapeSettingsCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	vals := map[string]string{
		"provider":   "tmdb",
		"language":   "zh-CN",
		"tmdb_token": "eyJhbGciOiJIUzI1NiJ9.body.sig",
		"tmdb_key":   "",
		"omdb_key":   "",
	}
	if err := writeScrapeSettings(path, vals); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	cfg, exists, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("重新解析失败: %v", err)
	}
	if !exists {
		t.Fatal("文件应已创建")
	}
	if cfg.Scrape.TMDBToken != vals["tmdb_token"] {
		t.Errorf("token = %q, want %q", cfg.Scrape.TMDBToken, vals["tmdb_token"])
	}
	if cfg.Scrape.Provider != "tmdb" || cfg.Scrape.Language != "zh-CN" {
		t.Errorf("provider/language = %q/%q", cfg.Scrape.Provider, cfg.Scrape.Language)
	}

	// 含密钥的文件不能是所有人可读
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("权限 = %o, want 600", perm)
	}
}

// 关键行为：只动白名单里的键，注释、顺序和其它段一律保留。
func TestWriteScrapeSettingsPreservesRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := `# 手工写的注释
[server]
listen = "0.0.0.0:8080"

[library]
root = "/home/u/Media"   # 行尾注释

[scrape]
# 凭据在下面
provider    = "omdb"
omdb_key    = "old-omdb"
concurrency = 2          # 不许被改动
title_sim_min = 0.75

[ui]
title = "我的媒体库"
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	vals := map[string]string{
		"provider":   "tmdb",
		"language":   "zh-CN",
		"tmdb_token": "new-token",
		"tmdb_key":   "",
		"omdb_key":   "",
	}
	if err := writeScrapeSettings(path, vals); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)

	for _, want := range []string{
		"# 手工写的注释",                        // 文件头注释
		`root = "/home/u/Media"   # 行尾注释`, // 其它段原样
		"concurrency = 2          # 不许被改动",
		`provider = "tmdb"`, // 白名单内被更新
		`tmdb_token = "new-token"`,
		`omdb_key = ""`, // 显式清空
		"[scrape]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("缺少内容 %q\n--- 实际 ---\n%s", want, text)
		}
	}
	if strings.Contains(text, `provider    = "omdb"`) {
		t.Error("旧的 provider 没有被替换")
	}
	if strings.Contains(text, "old-omdb") {
		t.Error("旧的 omdb_key 没有被替换")
	}

	// 缺失的键应补在 [scrape] 段内，而不是追加到文件末尾
	idxScrape := strings.Index(text, "[scrape]")
	idxUI := strings.Index(text, "[ui]")
	idxLang := strings.Index(text, `language = "zh-CN"`)
	if !(idxScrape < idxLang && idxLang < idxUI) {
		t.Errorf("language 落在错误位置：scrape=%d lang=%d ui=%d", idxScrape, idxLang, idxUI)
	}
}

func TestWriteScrapeSettingsAppendsMissingSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[server]\nlisten = \":8080\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	vals := map[string]string{
		"provider": "tmdb", "language": "zh-CN",
		"tmdb_token": "t", "tmdb_key": "", "omdb_key": "",
	}
	if err := writeScrapeSettings(path, vals); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	cfg, _, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.Server.Listen != ":8080" {
		t.Errorf("原有配置被破坏: %q", cfg.Server.Listen)
	}
	if cfg.Scrape.TMDBToken != "t" {
		t.Errorf("token = %q", cfg.Scrape.TMDBToken)
	}
}

func TestMaskSecret(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"short", "•••••"},
		{"eyJhbGciOiJIUzI1NiJ9.abcdefg.hijklmn", "eyJh" + strings.Repeat("•", 6) + "klmn"},
	}
	for _, c := range cases {
		if got := maskSecret(c.in); got != c.want {
			t.Errorf("maskSecret(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWriteLibrarySettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := `# 手工写的注释
[server]
listen = ":7788"

[scrape]
provider = "tmdb"
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	vals := map[string]string{
		"root":                 tomlString("~/Media"),
		"extra_roots":          tomlArray([]string{"/mnt/a", "/mnt/b"}),
		"follow_file_symlinks": "true",
		"download_dir":         tomlString("~/Downloads"),
		"download_mode":        tomlString("symlink"),
	}
	if err := writeSectionKeys(path, "library", librarySettingKeys, vals); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 再写一次：段已存在时应原位更新，而不是再追加一段
	if err := writeSectionKeys(path, "library", librarySettingKeys, vals); err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}

	cfg, _, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// LoadConfig 会把 ~ 展开成绝对路径
	if want := ExpandPath("~/Media"); cfg.Library.Root != want {
		t.Errorf("root = %q，应为 %q", cfg.Library.Root, want)
	}
	if len(cfg.Library.ExtraRoots) != 2 || cfg.Library.ExtraRoots[0] != "/mnt/a" {
		t.Errorf("extra_roots = %v", cfg.Library.ExtraRoots)
	}
	if !cfg.Library.FollowFileSymlinks {
		t.Errorf("follow_file_symlinks 应为 true")
	}
	if cfg.Library.DownloadDir != ExpandPath("~/Downloads") {
		t.Errorf("download_dir = %q", cfg.Library.DownloadDir)
	}
	if cfg.Library.DownloadMode != "symlink" {
		t.Errorf("download_mode = %q", cfg.Library.DownloadMode)
	}
	if cfg.Scrape.Provider != "tmdb" {
		t.Errorf("scrape 段被破坏: %q", cfg.Scrape.Provider)
	}
	if cfg.Server.Listen != ":7788" {
		t.Errorf("server 段被破坏: %q", cfg.Server.Listen)
	}

	text, _ := os.ReadFile(path)
	if n := strings.Count(string(text), "[library]"); n != 1 {
		t.Errorf("[library] 段出现 %d 次，应为 1", n)
	}
	if !strings.Contains(string(text), "# 手工写的注释") {
		t.Errorf("文件头注释丢失")
	}
}

// 主题三个值原样保留，其它一律按 auto：手滑写个 "Dark"/"night" 也不能把整站卡住。
func TestNormalizeTheme(t *testing.T) {
	cases := map[string]string{
		"light":   "light",
		"dark":    "dark",
		"auto":    "auto",
		"Dark":    "dark",
		" light ": "light",
		"":        "auto",
		"night":   "auto",
	}
	for in, want := range cases {
		if got := NormalizeTheme(in); got != want {
			t.Errorf("NormalizeTheme(%q) = %q，想要 %q", in, got, want)
		}
	}
}

// 设置页保存主题：写进 [ui].theme，并且运行时值立刻切过去（不必重启）。
func TestWriteThemeSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := writeSectionKeys(path, "ui", uiSettingKeys, map[string]string{
		"theme": tomlString("dark"),
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	cfg, _, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("重新解析失败: %v", err)
	}
	if cfg.UI.Theme != "dark" {
		t.Errorf("[ui].theme = %q，想要 dark", cfg.UI.Theme)
	}
	// 再写一次 auto，确认是原位替换而不是追加重复键
	if err := writeSectionKeys(path, "ui", uiSettingKeys, map[string]string{
		"theme": tomlString("auto"),
	}); err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}
	text, _ := os.ReadFile(path)
	if strings.Count(string(text), `theme = `) != 1 {
		t.Errorf("[ui].theme 出现多次:\n%s", text)
	}
	cfg2, _, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("二次解析失败: %v", err)
	}
	if cfg2.UI.Theme != "auto" {
		t.Errorf("二次写入后 theme = %q，想要 auto", cfg2.UI.Theme)
	}
}
