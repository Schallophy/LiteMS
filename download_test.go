package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 造一个假下载条目：目录型种子带两个文件（其中一个未完成）。
func makeFakeTorrent(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "movie.mkv"), []byte("video-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "movie.mkv.!qB"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPlanMirrorDir(t *testing.T) {
	src := makeFakeTorrent(t, t.TempDir(), "Foo.2024.1080p")
	linkDir := filepath.Join(t.TempDir(), "Downloads")

	items, err := planMirror(src, "Foo.2024.1080p", linkDir)
	if err != nil {
		t.Fatal(err)
	}
	// 未完成文件必须被跳过
	if len(items) != 1 {
		t.Fatalf("应只镜像 1 个文件，得到 %d：%v", len(items), items)
	}
	want := filepath.Join(linkDir, "Foo.2024.1080p", "movie.mkv")
	if items[0][1] != want {
		t.Errorf("落点 = %q，应为 %q", items[0][1], want)
	}
}

func TestPlanMirrorSingleFile(t *testing.T) {
	root := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "Downloads")

	// 文件名与条目名一致：同样「每片一目录」，目录名去掉扩展名
	p := filepath.Join(root, "movie.mkv")
	os.WriteFile(p, []byte("x"), 0o644)
	items, _ := planMirror(p, "movie.mkv", linkDir)
	want := filepath.Join(linkDir, "movie", "movie.mkv")
	if len(items) != 1 || items[0][1] != want {
		t.Errorf("单文件归档规则错误：%v，应为 %v", items, want)
	}

	// 条目名不带扩展名：目录名保持原样
	items, _ = planMirror(p, "Bar.2023", linkDir)
	want = filepath.Join(linkDir, "Bar.2023", "movie.mkv")
	if len(items) != 1 || items[0][1] != want {
		t.Errorf("归档规则错误：%v，应为 %v", items, want)
	}
}

func TestSyncDownloadsSymlinkIdempotent(t *testing.T) {
	home := t.TempDir()
	dl := filepath.Join(home, "Downloads")
	media := filepath.Join(home, "Media")
	os.MkdirAll(dl, 0o755)
	os.MkdirAll(media, 0o755)
	makeFakeTorrent(t, dl, "Foo.2024")

	cfg := &Config{}
	cfg.Library.Root = media
	cfg.Library.DownloadDir = dl
	cfg.Library.DownloadMode = "symlink"

	n, err := syncDownloads(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("首次应新建 1 条链接，得到 %d", n)
	}
	link := filepath.Join(media, "Downloads", "Foo.2024", "movie.mkv")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("链接不存在或不是符号链接：%v", err)
	}

	// 第二次：幂等，无新建
	n, err = syncDownloads(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("第二次不应新建，得到 %d", n)
	}
}

func TestCleanDangling(t *testing.T) {
	home := t.TempDir()
	dl := filepath.Join(home, "Downloads")
	media := filepath.Join(home, "Media")
	os.MkdirAll(dl, 0o755)
	os.MkdirAll(media, 0o755)
	cfg := &Config{}
	cfg.Library.Root = media
	cfg.Library.DownloadDir = dl
	cfg.Library.DownloadMode = "symlink"

	// 种子 A 正常做种；种子 B 下载后被删
	os.WriteFile(filepath.Join(dl, "A.mkv"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(dl, "B.mkv"), []byte("b"), 0o644)
	syncDownloads(cfg)

	os.Remove(filepath.Join(dl, "B.mkv"))
	n := cleanDangling(downloadLinkDir(cfg))
	if n != 1 {
		t.Fatalf("应清理 1 条悬空链接，得到 %d", n)
	}
	if _, err := os.Lstat(filepath.Join(media, "Downloads", "A", "A.mkv")); err != nil {
		t.Errorf("正常链接不应被清理：%v", err)
	}
	if _, err := os.Lstat(filepath.Join(media, "Downloads", "B", "B.mkv")); !os.IsNotExist(err) {
		t.Errorf("悬空链接应被删除")
	}
}

// 目录型种子整体删除：镜像树里的嵌套链接也要被清掉，空目录一并移除
func TestCleanDanglingNested(t *testing.T) {
	home := t.TempDir()
	dl := filepath.Join(home, "Downloads")
	media := filepath.Join(home, "Media")
	os.MkdirAll(dl, 0o755)
	os.MkdirAll(media, 0o755)
	src := makeFakeTorrent(t, dl, "Foo.2024")

	cfg := &Config{}
	cfg.Library.Root = media
	cfg.Library.DownloadDir = dl
	cfg.Library.DownloadMode = "symlink"
	if n, _ := syncDownloads(cfg); n != 1 {
		t.Fatalf("应新建 1 条链接，得到 %d", n)
	}
	link := filepath.Join(media, "Downloads", "Foo.2024", "movie.mkv")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("链接不存在：%v", err)
	}

	// 种子整个删掉，再放个旁观的正常种子
	os.RemoveAll(src)
	os.WriteFile(filepath.Join(dl, "keep.mkv"), []byte("k"), 0o644)
	syncDownloads(cfg)

	// scrape 的侧车文件不该挡住孤儿壳清理
	mirrorFoo := filepath.Join(media, "Downloads", "Foo.2024")
	os.WriteFile(filepath.Join(mirrorFoo, "movie.nfo"), []byte("<movie/>"), 0o644)
	os.WriteFile(filepath.Join(mirrorFoo, "poster.jpg"), []byte("img"), 0o644)
	syncDownloads(cfg)

	if _, err := os.Lstat(mirrorFoo); !os.IsNotExist(err) {
		t.Errorf("只剩侧车文件的孤儿目录应被移除：%v", err)
	}
	if _, err := os.Lstat(filepath.Join(media, "Downloads", "keep", "keep.mkv")); err != nil {
		t.Errorf("正常镜像不应被清理：%v", err)
	}
}

func TestSyncDownloadsCopyMode(t *testing.T) {
	home := t.TempDir()
	dl := filepath.Join(home, "Downloads")
	media := filepath.Join(home, "Media")
	os.MkdirAll(dl, 0o755)
	os.MkdirAll(media, 0o755)
	src := filepath.Join(dl, "movie.mkv")
	os.WriteFile(src, []byte("video-data"), 0o644)

	cfg := &Config{}
	cfg.Library.Root = media
	cfg.Library.DownloadDir = dl
	cfg.Library.DownloadMode = "copy"

	if n, _ := syncDownloads(cfg); n != 1 {
		t.Fatalf("首次应复制 1 个文件，得到 %d", n)
	}
	dst := filepath.Join(media, "Downloads", "movie", "movie.mkv")
	fi, err := os.Stat(dst)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("copy 模式应是实体文件：%v", err)
	}
	// mtime 必须与源一致：增量扫描按 mtime 判定
	si, _ := os.Stat(src)
	if !fi.ModTime().Equal(si.ModTime()) {
		t.Errorf("副本 mtime %v != 源 %v", fi.ModTime(), si.ModTime())
	}
	// 二次同步幂等
	if n, _ := syncDownloads(cfg); n != 0 {
		t.Errorf("第二次不应复制，得到 %d", n)
	}
}

func TestIsTempFile(t *testing.T) {
	cases := map[string]bool{
		"movie.mkv.!qB": true,
		"MOVIE.MKV.!QB": true,
		"movie.part":    true,
		"movie.mkv":     false,
		"part":          false,
	}
	for name, want := range cases {
		if got := isTempFile(name); got != want {
			t.Errorf("isTempFile(%q) = %v，应为 %v", name, got, want)
		}
	}
}

func TestDownloadModeValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`[library]
download_dir = "~/Downloads"
download_mode = "move"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "download_mode") {
		t.Errorf("应拒绝非法 mode，得到 %v", err)
	}
}

func TestLinkDirFromBasename(t *testing.T) {
	cfg := &Config{}
	cfg.Library.Root = "/data/media"
	cfg.Library.DownloadDir = "/home/u/Downloads"
	if got := downloadLinkDir(cfg); got != filepath.Join("/data/media", "Downloads") {
		t.Errorf("linkDir = %q", got)
	}
}
