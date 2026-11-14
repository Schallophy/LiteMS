package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 顶层条目目录改名后，poster/fanart 的相对路径也必须跟着换。
func TestRewriteDirOfTopLevel(t *testing.T) {
	cases := [][3]string{
		{"Movie (2019)/poster.jpg", "New Name (2019)", "New Name (2019)/poster.jpg"},
		{"Downloads/Torrent/Movie/poster.jpg", "New", "Downloads/Torrent/New/poster.jpg"},
		{"poster.jpg", "New", "poster.jpg"}, // 根目录裸文件没有条目目录
	}
	for _, c := range cases {
		if got := rewriteDirOf(c[0], c[1]); got != c[2] {
			t.Errorf("rewriteDirOf(%q,%q)=%q want %q", c[0], c[1], got, c[2])
		}
	}
}

// 字幕只能挂到自己的视频上：后缀必须是语言类标记，不能是任意词。
func TestSubMatchesVideo(t *testing.T) {
	cases := []struct {
		sub, video string
		want       bool
	}{
		{"Movie.2020.chs", "Movie.2020", true},
		{"Movie.2020.chs.forced", "Movie.2020", true},
		{"Movie.2020_eng", "Movie.2020", true},
		{"Movie.2020[chi]", "Movie.2020", true},
		{"Movie.2020.Extended.chs", "Movie.2020", false},
		{"Movie.20202.chs", "Movie.2020", false},
		{"Other.chs", "Movie.2020", false},
	}
	for _, c := range cases {
		if got := subMatchesVideo(c.sub, c.video); got != c.want {
			t.Errorf("subMatchesVideo(%q,%q)=%v want %v", c.sub, c.video, got, c.want)
		}
	}
}

// 媒体根被删除后，旧根下的条目与进度必须一起清掉。
func TestReconcileRootsDropsStaleItems(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rootA := filepath.Join(dir, "A")
	rootB := filepath.Join(dir, "B")
	if err := db.SyncRoots([]string{rootA, rootB}); err != nil {
		t.Fatal(err)
	}
	mk := func(root string, idx int) Item {
		return Item{ID: ItemID(root, "x.mkv"), RootIdx: idx, RelPath: "x.mkv",
			Kind: KindMovie, Title: "X", SizeBytes: 1, MtimeUnix: 1}
	}
	a, b := mk(rootA, 0), mk(rootB, 1)
	for _, it := range []*Item{&a, &b} {
		if err := db.UpsertItem(it); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetProgress(b.ID, 5000, 10000); err != nil {
		t.Fatal(err)
	}

	n, err := db.ReconcileRoots([]string{rootA})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("清理条目数 = %d, want 1", n)
	}
	if got, _ := db.GetItem(b.ID); got != nil {
		t.Error("已删除媒体根的条目仍在索引里")
	}
	if got, _ := db.GetProgress(b.ID); got != nil {
		t.Error("已删除条目的进度应随外键级联删除")
	}
	if got, _ := db.GetItem(a.ID); got == nil {
		t.Error("仍在配置里的媒体根条目被误删")
	}
}

// 下载目录与任何媒体根重叠都必须拒绝启动。
func TestConfigRejectsOverlappingDownloadDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Library.Root = "/media"
	cfg.Library.DownloadDir = "/media/Downloads"
	if err := cfg.validate(); err == nil {
		t.Error("下载目录在媒体根内时应报错")
	}
	cfg.Library.DownloadDir = "/downloads"
	if err := cfg.validate(); err != nil {
		t.Errorf("不相交的路径不该报错：%v", err)
	}
	cfg.Library.DownloadDir = "/"
	if err := cfg.validate(); err == nil {
		t.Error("下载目录包含媒体根时应报错")
	}
}

// 导入字幕不能占用内嵌字幕缓存的 embed- 前缀。
func TestSanitizeSubNameRejectsEmbedPrefix(t *testing.T) {
	if _, err := sanitizeSubName("embed-9.vtt", "eng"); err == nil {
		t.Error("embed- 前缀应被拒绝")
	}
	got, err := sanitizeSubName("film.vtt", "eng")
	if err != nil || got != "film.eng.vtt" {
		t.Errorf("sanitizeSubName = %q, %v", got, err)
	}
}

// 有 meta 时按其中记录的源 mtime 判断，不再依赖被对齐过的缓存 mtime。
func TestTranscoderUsableUsesMeta(t *testing.T) {
	dir := t.TempDir()
	tc, err := NewTranscoder(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "src.mkv")
	if err := os.WriteFile(src, []byte("src"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(src, now, now); err != nil {
		t.Fatal(err)
	}
	cache := tc.CachePath("0123456789abcdef", 0)
	if err := os.WriteFile(cache, []byte("cache"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 文件 mtime 故意做旧；meta 里记录源更新过 → 不可用
	if err := os.Chtimes(cache, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := writeCacheMeta(cache, cacheMeta{SrcMtimeUnixNano: now.Add(-time.Hour).UnixNano(), DoneAtUnixNano: now.UnixNano()}); err != nil {
		t.Fatal(err)
	}
	if tc.Usable("0123456789abcdef", 0, src) {
		t.Error("meta 显示源比缓存新时不应可用")
	}
	if err := writeCacheMeta(cache, cacheMeta{SrcMtimeUnixNano: now.UnixNano(), DoneAtUnixNano: now.UnixNano()}); err != nil {
		t.Fatal(err)
	}
	if !tc.Usable("0123456789abcdef", 0, src) {
		t.Error("meta 与源一致时应可用")
	}
}

// 请求已滑出窗口或 ffmpeg 已退出时，分段应立即判定为不存在。
func TestHLSSegmentGone(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "video"), 0o755); err != nil {
		t.Fatal(err)
	}
	pl := "#EXTM3U\n#EXTINF:4.0,\nseg00005.m4s\n#EXTINF:4.0,\nseg00006.m4s\n"
	if err := os.WriteFile(filepath.Join(dir, "video", "index.m3u8"), []byte(pl), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &HLSSession{Dir: dir}
	if !s.segmentGone("video/seg00001.m4s") {
		t.Error("早于播放列表窗口的分段应判定为已消失")
	}
	if s.segmentGone("video/seg00007.m4s") {
		t.Error("窗口之后、尚未生成的分段应继续等待")
	}
	if err := os.WriteFile(filepath.Join(dir, "video", "seg00002.m4s"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s.segmentGone("video/seg00002.m4s") {
		t.Error("已存在的分段不应判定为消失")
	}
	s.exited.Store(true)
	if !s.segmentGone("video/seg00007.m4s") {
		t.Error("ffmpeg 已退出时，缺失分段应立即判定为消失")
	}
}

// 刮削改名换 ID 后，所有音轨缓存（含 .meta）与导入字幕目录都要跟着搬走。
func TestRenameMovesAllTrackCaches(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "lib")
	dir := filepath.Join(root, "Old Name")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Movie.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := OpenDB(filepath.Join(base, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SyncRoots([]string{root}); err != nil {
		t.Fatal(err)
	}
	it := Item{ID: ItemID(root, "Old Name/Movie.mkv"), RootIdx: 0,
		RelPath: "Old Name/Movie.mkv", Kind: KindMovie, Title: "Movie"}
	if err := db.UpsertItem(&it); err != nil {
		t.Fatal(err)
	}

	tcDir := filepath.Join(base, "transcode")
	if err := os.MkdirAll(tcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldID := it.ID
	for _, n := range []string{oldID + ".m4a", oldID + ".m4a.meta",
		oldID + ".a1.m4a", oldID + ".a1.m4a.meta"} {
		if err := os.WriteFile(filepath.Join(tcDir, n), []byte("c"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	subDir := filepath.Join(root, ".litems", "subs", oldID)
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "a.chs.srt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Library.Root = root
	cfg.Database.Path = filepath.Join(base, "index.db")
	r := NewRunner(cfg, db, nil, false, false, "", 0)
	r.renameToScrapedName(context.Background(), &it, dir, &Meta{OriginalTitle: "The Matrix", Year: 1999})

	newID := ItemID(root, "The Matrix (1999)/Movie.mkv")
	if it.ID != newID {
		t.Fatalf("条目 ID = %s, want %s", it.ID, newID)
	}
	if _, err := os.Stat(filepath.Join(root, "The Matrix (1999)")); err != nil {
		t.Fatalf("目录未改名：%v", err)
	}
	for _, n := range []string{newID + ".m4a", newID + ".m4a.meta",
		newID + ".a1.m4a", newID + ".a1.m4a.meta"} {
		if _, err := os.Stat(filepath.Join(tcDir, n)); err != nil {
			t.Errorf("缓存未迁移 %s：%v", n, err)
		}
	}
	for _, n := range []string{oldID + ".m4a", oldID + ".a1.m4a"} {
		if _, err := os.Stat(filepath.Join(tcDir, n)); err == nil {
			t.Errorf("旧缓存 %s 应已搬走", n)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".litems", "subs", newID, "a.chs.srt")); err != nil {
		t.Errorf("导入字幕未迁移：%v", err)
	}
}
