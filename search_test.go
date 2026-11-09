package main

import (
	"path/filepath"
	"testing"
)

// 折叠搜索：忽略空格/标点/大小写，标题与原名都能命中。
func TestSearchFolded(t *testing.T) {
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SyncRoots([]string{dir}); err != nil {
		t.Fatal(err)
	}
	items := []*Item{
		{ID: "00000000000000a1", RootIdx: 0, RelPath: "La.La.Land.mkv",
			Kind: KindMovie, Title: "爱乐之城", OriginalTitle: "La La Land", Year: 2016,
			SizeBytes: 1, MtimeUnix: 1},
		{ID: "00000000000000a2", RootIdx: 0, RelPath: "shadow.mkv",
			Kind: KindMovie, Title: "捕风追影", Year: 2025,
			SizeBytes: 1, MtimeUnix: 2},
	}
	for _, it := range items {
		if err := db.UpsertItem(it); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		q    string
		want int
	}{
		{"lala", 1},        // 去空格
		{"lalaland", 1},    // 全去掉分隔
		{"La La Land", 1},  // 原名原样
		{"la-la land", 1},  // 连字符也算噪音
		{"爱乐", 1},         // 中文标题
		{"爱 乐", 1},        // 中文带空格
		{"shadow", 0},      // 文件名不入搜索键
		{"zzz", 0},
		{"捕风", 1},
	}
	for _, c := range cases {
		got, total, err := db.ListItems(ItemFilter{Q: c.q})
		if err != nil {
			t.Fatalf("q=%q err: %v", c.q, err)
		}
		if total != c.want || len(got) != c.want {
			t.Errorf("q=%q 命中 %d/%d，want %d", c.q, total, len(got), c.want)
		}
	}
}

// 老库迁移：没有 search_key 列的库打开后自动补列并回填。
func TestSearchKeyMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SyncRoots([]string{dir}); err != nil {
		t.Fatal(err)
	}
	// 模拟老库：写入一行但把 search_key 清空
	if err := db.UpsertItem(&Item{ID: "00000000000000b1", RootIdx: 0, RelPath: "x.mkv",
		Kind: KindMovie, Title: "Fight Club", SizeBytes: 1, MtimeUnix: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE items SET search_key='' WHERE id='00000000000000b1'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// 重新打开：migrateSearchKey 应回填
	db2, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	var key string
	if err := db2.sql.QueryRow(`SELECT search_key FROM items WHERE id='00000000000000b1'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "fightclub" {
		t.Errorf("回填 search_key = %q, want fightclub", key)
	}
	if _, total, err := db2.ListItems(ItemFilter{Q: "fightclub"}); err != nil || total != 1 {
		t.Errorf("迁移后按折叠键搜索失败: total=%d err=%v", total, err)
	}
}

// SearchKey 本身的形态：标题与原名不同才拼接。
func TestSearchKeyShape(t *testing.T) {
	if got := SearchKey("爱乐之城", "La La Land"); got != "爱乐之城|lalaland" {
		t.Errorf("SearchKey = %q", got)
	}
	if got := SearchKey("捕风追影", "捕风追影"); got != "捕风追影" {
		t.Errorf("同名不应拼接: %q", got)
	}
	if got := SearchKey("某 片", ""); got != "某片" {
		t.Errorf("SearchKey = %q", got)
	}
}
