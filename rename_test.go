package main

import (
	"path/filepath"
	"testing"
)

func TestMostlyLatin(t *testing.T) {
	cases := map[string]bool{
		"The Odyssey":       true,
		"La La Land":        true,
		"Le Fabuleux Destin": true, // 全拉丁（法语）也算
		"流浪地球2":             false,
		"해리 포터":             false,
		"":                  false,
		"123":               false, // 没有字母
	}
	for in, want := range cases {
		if got := mostlyLatin(in); got != want {
			t.Errorf("mostlyLatin(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSanitizeDirName(t *testing.T) {
	cases := map[string]string{
		"The Odyssey":            "The Odyssey",
		"The: Odyssey  Part II":  "The Odyssey Part II", // 非法字符换空格后压掉多余空白
		"  spaced..  ":           "spaced",              // 首尾空白与点都剥掉
		"a/b\\c:d*e?f\"g<h>i|j": "a b c d e f g h i j",
		"":                       "",
	}
	for in, want := range cases {
		if got := sanitizeDirName(in); got != want {
			t.Errorf("sanitizeDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRewriteDirOf(t *testing.T) {
	if got := rewriteDirOf("Downloads/old/stem-poster.jpg", "New (2026)"); got != filepath.ToSlash("Downloads/New (2026)/stem-poster.jpg") {
		t.Errorf("rewriteDirOf = %q", got)
	}
	if got := rewriteDirOf("", "New"); got != "" {
		t.Errorf("空路径应原样返回，得到 %q", got)
	}
	if got := rewriteDirOf("poster.jpg", "New"); got != "poster.jpg" {
		t.Errorf("无目录段应原样返回，得到 %q", got)
	}
}

func TestRekeyItem(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// items.root_idx 有外键指向 roots 表，必须先注册媒体根
	if err := db.SyncRoots([]string{"/tmp/fake-root"}); err != nil {
		t.Fatal(err)
	}

	it := &Item{
		ID: "aaaaaaaaaaaaaaaa", RootIdx: 0, RelPath: "Downloads/Old.Dir/movie.mkv",
		Kind: KindMovie, Title: "某片", OriginalTitle: "Some Film",
		Year: 2026, PosterPath: "Downloads/Old.Dir/poster.jpg",
		ScrapeStatus: "ok",
	}
	if err := db.UpsertItem(it); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProgress(it.ID, 14700, 8505000); err != nil {
		t.Fatal(err)
	}

	err = db.RekeyItem("aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb",
		"Downloads/New (2026)/movie.mkv", "Downloads/New (2026)/poster.jpg", "",
		"某片", "Some Film")
	if err != nil {
		t.Fatal(err)
	}

	if old, _ := db.GetItem("aaaaaaaaaaaaaaaa"); old != nil {
		t.Error("旧 ID 应该不存在了")
	}
	got, err := db.GetItem("bbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatalf("新 ID 查不到：%v", err)
	}
	if got.RelPath != "Downloads/New (2026)/movie.mkv" {
		t.Errorf("rel_path 未更新：%q", got.RelPath)
	}
	if got.PosterPath != "Downloads/New (2026)/poster.jpg" {
		t.Errorf("poster_path 未更新：%q", got.PosterPath)
	}
	if got.Title != "某片" {
		t.Errorf("title 被改动了：%q", got.Title)
	}
	p, err := db.GetProgress("bbbbbbbbbbbbbbbb")
	if err != nil || p == nil {
		t.Fatalf("progress 未迁移：%v", err)
	}
	if p.PositionMS != 14700 {
		t.Errorf("progress 位置丢了：%d", p.PositionMS)
	}
	if old, _ := db.GetProgress("aaaaaaaaaaaaaaaa"); old != nil {
		t.Error("旧 progress 应该不在了")
	}
}
