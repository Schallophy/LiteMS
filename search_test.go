package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// 折叠搜索：忽略空格/标点/大小写；标题、原名、文件名三者都参与匹配。
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
		{ID: "00000000000000a1", RootIdx: 0,
			RelPath: "La La Land (2016) [1080p]/La.La.Land.2016.1080p.BluRay.x264-[YTS.AG].mp4",
			Kind:    KindMovie, Title: "爱乐之城", OriginalTitle: "La La Land", Year: 2016,
			SizeBytes: 1, MtimeUnix: 1},
		// 国产片：original_title 也是中文，英文名只存在于文件名
		{ID: "00000000000000a2", RootIdx: 0,
			RelPath: "The Shadow's Edge (2025) (1080p WEB-DL x265 SAMPA)/The Shadow's Edge (2025) (1080p WEB-DL x265 SAMPA).mkv",
			Kind:    KindMovie, Title: "捕风追影", OriginalTitle: "捕风追影", Year: 2025,
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
		{"lala", 1},         // 原名去空格
		{"lalaland", 1},     // 全去掉分隔
		{"La La Land", 1},   // 原名原样
		{"la-la land", 1},   // 连字符也算噪音
		{"爱乐", 1},          // 中文标题
		{"爱 乐", 1},         // 中文带空格
		{"shadow", 1},       // 英文国际名只在文件名里
		{"the", 1},          // 同上（首个单词）
		{"SHADOWS EDGE", 1}, // 大小写与撇号
		{"sampa", 1},        // 发布组标记也命中，可接受
		{"zzz", 0},
		{"捕风", 1},
		{"不存在", 0},
		// 拼音：全拼、首字母、多音字变体
		{"ailezhicheng", 1},
		{"ALZC", 1},
		{"aile", 1},        // 前缀命中
		{"aiyue", 1},       // 乐 的第二读音 yuè
		{"aiyuezhicheng", 1},
		{"ayzc", 1},
		{"bufengzhuiying", 1},
		{"bfzy", 1},
		{"ai le zhi cheng", 1},
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

// 老库迁移：键格式升版（v2 起含文件名）后要全量重算，不能只看空值。
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
	if err := db.UpsertItem(&Item{ID: "00000000000000b1", RootIdx: 0,
		RelPath: "The.Shadow's.Edge.2025.mkv", Kind: KindMovie,
		Title: "捕风追影", SizeBytes: 1, MtimeUnix: 1}); err != nil {
		t.Fatal(err)
	}
	// 模拟旧版键：只有标题折叠，没有文件名；并清掉版本标记
	if _, err := db.sql.Exec(`UPDATE items SET search_key='捕风追影' WHERE id='00000000000000b1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`DELETE FROM kv WHERE k='search_key_ver'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	var key string
	if err := db2.sql.QueryRow(`SELECT search_key FROM items WHERE id='00000000000000b1'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(key, "shadow") {
		t.Errorf("迁移后 search_key 应含文件名片段，got %q", key)
	}
	if _, total, err := db2.ListItems(ItemFilter{Q: "shadow"}); err != nil || total != 1 {
		t.Errorf("迁移后按文件名搜索失败: total=%d err=%v", total, err)
	}
	db2.Close()

	// 版本标记写入后再次打开应保持幂等
	db3, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()
	var ver string
	if err := db3.sql.QueryRow(`SELECT v FROM kv WHERE k='search_key_ver'`).Scan(&ver); err != nil || ver != searchKeyVersion {
		t.Errorf("search_key_ver = %q err=%v", ver, err)
	}
}

// SearchKey 的形态：各段去重后以 | 连接（标题、原名、文件名、拼音全拼、首字母）。
func TestSearchKeyShape(t *testing.T) {
	// 乐 有 lè/yuè、之 有 zhī/zhū 两读：全拼与首字母按笛卡尔积展开，
	// 主题读音（ailezhicheng/alzc）与 aiyue 变体都要在键里
	got := SearchKey("爱乐之城", "La La Land", "La.La.Land.2016.mkv")
	want := "爱乐之城|lalaland|lalaland2016mkv|ailezhicheng|ailezhucheng|aiyuezhicheng|aiyuezhucheng|alzc|ayzc"
	if got != want {
		t.Errorf("SearchKey = %q, want %q", got, want)
	}
	// 追 有 zhuī/duī/tuī 三读：只校验包含，不锁死全量顺序
	got = SearchKey("捕风追影", "捕风追影", "shadow.mkv")
	for _, part := range []string{"捕风追影", "shadowmkv", "bufengzhuiying", "bfzy"} {
		if !strings.Contains(got, part) {
			t.Errorf("SearchKey 缺少 %q: %q", part, got)
		}
	}
	if strings.Count(got, "捕风追影") != 1 {
		t.Errorf("标题段应去重: %q", got)
	}
	// 纯英文标题不产首字母噪音
	if got := SearchKey("Some Film", "", ""); got != "somefilm" {
		t.Errorf("纯英文不应有首字母段: %q", got)
	}
	if got := SearchKey("某 片", "", ""); got != "某片|meipan|meipian|moupan|moupian|mp" {
		t.Errorf("SearchKey = %q", got)
	}
}
