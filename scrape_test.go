package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTMDB 起一个假 TMDB，喂固定 JSON。
// 这样整条链路（搜索 → 匹配 → 详情 → 落盘 → 回填索引）都能在无 key、无网络的情况下验证。
func fakeTMDB(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/t/p/"):
			// 图片：给一段够大的字节，满足 >512 的健全性检查
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(bytes.Repeat([]byte{0x42}, 4096))

		case r.URL.Path == "/3/search/movie":
			json.NewEncoder(w).Encode(map[string]any{
				"page": 1,
				"results": []map[string]any{
					{
						"id": 157336, "title": "星际穿越", "original_title": "Interstellar",
						"release_date": "2014-11-05", "poster_path": "/p.jpg",
						"popularity": 120.5, "vote_average": 8.4, "vote_count": 38000,
					},
					{
						// 同名但年份差很远，应被硬门槛淘汰
						"id": 999, "title": "星际穿越", "original_title": "Interstellar B",
						"release_date": "1960-01-01", "poster_path": "/q.jpg",
						"popularity": 1, "vote_average": 5.0, "vote_count": 10,
					},
				},
			})

		case r.URL.Path == "/3/movie/157336":
			json.NewEncoder(w).Encode(map[string]any{
				"id": 157336, "imdb_id": "tt0816692",
				"title": "星际穿越", "original_title": "Interstellar",
				"overview": "近未来的地球黄沙遍野。", "tagline": "人类的终点，是另一个开始",
				"runtime": 169, "release_date": "2014-11-05",
				"vote_average": 8.437, "vote_count": 38000,
				"poster_path": "/p.jpg", "backdrop_path": "/b.jpg",
				"genres":               []map[string]any{{"name": "科幻"}, {"name": "冒险"}},
				"production_countries": []map[string]any{{"name": "美国"}},
				"credits": map[string]any{
					"cast": []map[string]any{{"name": "马修·麦康纳", "character": "Cooper", "order": 0}},
					"crew": []map[string]any{{"name": "克里斯托弗·诺兰", "job": "Director", "department": "Directing"}},
				},
			})

		default:
			http.NotFound(w, r)
		}
	}))
}

// TestScrapePipeline 是小规模的端到端验证：一个真实目录结构 + 假数据源。
func TestScrapePipeline(t *testing.T) {
	srv := fakeTMDB(t)
	defer srv.Close()

	root := t.TempDir()
	dir := filepath.Join(root, "电影", "星际穿越 (2014)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(dir, "星际穿越 (2014).mkv")
	if err := os.WriteFile(video, bytes.Repeat([]byte{0}, 2048), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.Library.Root = root
	cfg.Database.Path = filepath.Join(t.TempDir(), "index.db")
	cfg.Scrape.TMDBBase = srv.URL + "/3"
	cfg.Scrape.TMDBImageBase = srv.URL + "/t/p"
	cfg.Scrape.TMDBKey = "test-key"
	cfg.Scrape.IntervalMS = 1
	cfg.Scrape.Concurrency = 1

	db, err := OpenDB(cfg.Database.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	sc := NewScanner(cfg, db, false, false, false, "")
	if _, err := sc.Run(); err != nil {
		t.Fatalf("扫描失败：%v", err)
	}

	client := NewTMDBClient(cfg)
	runner := NewRunner(cfg, db, client, false, false, "", 0)
	stats, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("刮削失败：%v", err)
	}
	if stats.OK != 1 {
		t.Fatalf("应成功 1 条，实际 %+v", stats)
	}

	// 目录已按刮削到的英文名改名（original_title 是拉丁字母的 "Interstellar"）
	renamed := filepath.Join(root, "电影", "Interstellar (2014)")
	if _, err := os.Stat(renamed); err != nil {
		t.Fatalf("目录应改名为 Interstellar (2014)：%v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("旧目录应已不存在：%v", err)
	}

	// NFO 落盘且内容正确（在新目录里）
	nfoPath := filepath.Join(renamed, "movie.nfo")
	b, err := os.ReadFile(nfoPath)
	if err != nil {
		t.Fatalf("应生成 movie.nfo：%v", err)
	}
	nfo := string(b)
	for _, want := range []string{
		"<title>星际穿越</title>",
		`<uniqueid type="imdb" default="true">tt0816692</uniqueid>`,
		`<uniqueid type="tmdb">157336</uniqueid>`,
		"<poster>poster.jpg</poster>",
		"<runtime>169</runtime>",
	} {
		if !strings.Contains(nfo, want) {
			t.Errorf("NFO 缺少 %q\n%s", want, nfo)
		}
	}

	// 图片落盘（新目录里）
	for _, name := range []string{"poster.jpg", "fanart.jpg"} {
		fi, err := os.Stat(filepath.Join(renamed, name))
		if err != nil {
			t.Errorf("应下载 %s：%v", name, err)
			continue
		}
		if fi.Size() < 512 {
			t.Errorf("%s 体积异常：%d", name, fi.Size())
		}
	}

	// 索引回填，rel_path 与图片路径都指向新目录
	items, _, err := db.ListItems(ItemFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("索引应有 1 条，实际 %d", len(items))
	}
	it := items[0]
	if it.TmdbID != 157336 || it.Rating < 8.4 || it.PosterPath == "" || it.FanartPath == "" {
		t.Errorf("索引回填不完整：%+v", it)
	}
	if it.RelPath != filepath.ToSlash(filepath.Join("电影", "Interstellar (2014)", "星际穿越 (2014).mkv")) {
		t.Errorf("rel_path 应指向新目录：%q", it.RelPath)
	}
	if !strings.Contains(it.PosterPath, "Interstellar (2014)") {
		t.Errorf("poster_path 应在新目录里：%q", it.PosterPath)
	}
	// 改名后的条目按新 ID 存在（ItemID = sha1(root/rel)）
	if want := ItemID(root, filepath.ToSlash(it.RelPath)); it.ID != want {
		t.Errorf("ID 应与新路径一致：got %s want %s", it.ID, want)
	}
	if it.ScrapeStatus != "ok" || it.ScrapedAt == 0 {
		t.Errorf("状态应为 ok 且有刮削时间：%+v", it)
	}

	// 幂等：再跑一次不应有活干
	stats2, err := NewRunner(cfg, db, client, false, false, "", 0).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats2.Total != 0 {
		t.Errorf("第二次不应再处理任何条目，实际 %+v", stats2)
	}
}

// TestScrapeRespectsLockdata 手工锁定的 NFO 不能被覆盖。
func TestScrapeRespectsLockdata(t *testing.T) {
	srv := fakeTMDB(t)
	defer srv.Close()

	root := t.TempDir()
	dir := filepath.Join(root, "电影", "星际穿越 (2014)")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "星际穿越 (2014).mkv"), bytes.Repeat([]byte{0}, 1024), 0o644)
	locked := filepath.Join(dir, "movie.nfo")
	os.WriteFile(locked, []byte(`<?xml version="1.0" encoding="UTF-8"?>
<movie><title>我自己写的</title><lockdata>true</lockdata></movie>`), 0o644)

	cfg := DefaultConfig()
	cfg.Library.Root = root
	cfg.Database.Path = filepath.Join(t.TempDir(), "index.db")
	cfg.Scrape.TMDBBase = srv.URL + "/3"
	cfg.Scrape.TMDBImageBase = srv.URL + "/t/p"
	cfg.Scrape.TMDBKey = "k"
	cfg.Scrape.IntervalMS = 1

	db, _ := OpenDB(cfg.Database.Path)
	defer db.Close()
	if _, err := NewScanner(cfg, db, false, false, false, "").Run(); err != nil {
		t.Fatal(err)
	}
	stats, err := NewRunner(cfg, db, NewTMDBClient(cfg), false, false, "", 0).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Manual != 1 {
		t.Errorf("应记为已锁定，实际 %+v", stats)
	}
	b, _ := os.ReadFile(locked)
	if !strings.Contains(string(b), "我自己写的") {
		t.Error("锁定文件被覆盖了")
	}
}

// TestMatchCandidates 校验硬门槛：年份与相似度。
func TestMatchCandidates(t *testing.T) {
	q := Query{Title: "星际穿越", Year: 2014, Kind: KindMovie}
	cands := []Candidate{
		{Key: "1", Title: "星际穿越", OriginalTitle: "Interstellar", Year: 2014},
		{Key: "2", Title: "星际穿越", OriginalTitle: "Other", Year: 1960},
		{Key: "3", Title: "完全无关的名字", OriginalTitle: "Nothing", Year: 2014},
	}
	m := matchCandidates(cands, q, 0.75)
	if !m.OK || m.Cand.Key != "1" {
		t.Fatalf("应选中 Key=1，得到 %+v", m)
	}

	// 年份差 1 年仍可接受
	close := []Candidate{{Key: "4", Title: "星际穿越", Year: 2015}}
	if m := matchCandidates(close, q, 0.75); !m.OK {
		t.Error("年份相差 1 年应通过")
	}

	// 全部不合格
	if m := matchCandidates(cands[1:], q, 0.75); m.OK {
		t.Errorf("不应匹配成功，得到 %+v", m)
	}
}

// TestEmbeddedTmdbID 校验文件名里的 ID 标识符。
func TestEmbeddedTmdbID(t *testing.T) {
	cases := map[string]int{
		"星际穿越 (2014) {tmdb-157336}": 157336,
		"某片 [tmdbid=42]":            42,
		"某片 tmdb/123":               123,
		"普通文件名 (2014)":              0,
	}
	for name, want := range cases {
		if got := embeddedTmdbID(name); got != want {
			t.Errorf("embeddedTmdbID(%q) = %d，应为 %d", name, got, want)
		}
	}
}
