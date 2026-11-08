package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteMovieNFOGolden 校验写出的 XML 结构能被 Kodi 读懂：
// 声明头、`<art>` 块、uniqueid 顺序、空字段不写。
func TestWriteMovieNFOGolden(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "movie.nfo")
	nfo := &nfoMovie{
		Title:         "星际穿越",
		OriginalTitle: "Interstellar",
		SortTitle:     "星际穿越",
		Year:          2014,
		Plot:          "地球黄沙遍野。",
		Runtime:       169,
		Ratings:       buildRatings(8.437, 38000, "tmdb"),
		UniqueIDs:     buildUniqueIDs("tt0816692", 157336),
		Genres:        []string{"科幻", "冒险"},
		Directors:     []string{"克里斯托弗·诺兰"},
		Actors:        buildActorList([]Actor{{Name: "马修·麦康纳", Role: "Cooper"}}),
		Art:           artOf("", true, true),
	}
	if err := writeNFO(path, nfo); err != nil {
		t.Fatalf("写 NFO 失败：%v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)

	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`,
		`<title>星际穿越</title>`,
		`<originaltitle>Interstellar</originaltitle>`,
		`<year>2014</year>`,
		`<uniqueid type="imdb" default="true">tt0816692</uniqueid>`,
		`<uniqueid type="tmdb">157336</uniqueid>`,
		`<poster>poster.jpg</poster>`,
		`<fanart>fanart.jpg</fanart>`,
		`<name>马修·麦康纳</name>`,
		`<lockdata>false</lockdata>`,
		`<rating name="tmdb" max="10" default="true">`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("NFO 缺少 %q\n---\n%s", want, s)
		}
	}

	// 评分保留一位小数，不要写出 8.437
	if !strings.Contains(s, "8.4</value>") {
		t.Errorf("评分未按一位小数写出：%s", s)
	}
	// 空字段不应产生空标签
	if strings.Contains(s, "<tagline></tagline>") {
		t.Error("空 tagline 不应写出标签")
	}
}

// TestReadNFOLockdata 校验 lockdata 与 uniqueid 的读取。
func TestReadNFOLockdata(t *testing.T) {
	dir := t.TempDir()

	locked := filepath.Join(dir, "locked.nfo")
	os.WriteFile(locked, []byte(`<?xml version="1.0" encoding="UTF-8"?>
<movie><title>手工</title><year>1999</year><lockdata>true</lockdata>
<uniqueid type="tmdb" default="true">42</uniqueid></movie>`), 0o644)

	info := readNFO(locked)
	if !info.Exists || !info.Locked {
		t.Fatalf("应识别为已锁定：%+v", info)
	}
	if info.TmdbID != 42 || info.Year != 1999 || info.Title != "手工" {
		t.Errorf("字段读取错误：%+v", info)
	}

	// 不存在时不应报错，只返回 Exists=false
	if got := readNFO(filepath.Join(dir, "nope.nfo")); got.Exists {
		t.Errorf("不存在的文件应返回 Exists=false，得到 %+v", got)
	}
}

// TestBuildUniqueIDs 校验 imdb 优先成为 default。
func TestBuildUniqueIDs(t *testing.T) {
	if got := buildUniqueIDs("", 157336); len(got) != 1 || !got[0].Default {
		t.Errorf("只有 tmdb 时它应为 default：%+v", got)
	}
	got := buildUniqueIDs("tt0816692", 157336)
	if len(got) != 2 {
		t.Fatalf("应写出两个 uniqueid，得到 %+v", got)
	}
	if got[0].Type != "imdb" || !got[0].Default {
		t.Errorf("imdb 应排第一且为 default：%+v", got)
	}
	if got[1].Default {
		t.Errorf("tmdb 不应再是 default：%+v", got)
	}
}

// TestNFOFileName 校验单片目录与多片目录的命名差异。
func TestNFOFileName(t *testing.T) {
	dir := "/media/电影/某片 (2020)"
	if got := nfoPathFor(dir, "某片 (2020)", 1); filepath.Base(got) != "movie.nfo" {
		t.Errorf("单片目录应写 movie.nfo，得到 %s", filepath.Base(got))
	}
	if got := nfoPathFor(dir, "某片 (2020)", 3); filepath.Base(got) != "某片 (2020).nfo" {
		t.Errorf("多片目录应写 <文件名>.nfo，得到 %s", filepath.Base(got))
	}
	if got := imageBaseName(dir, "某片 (2020)", 1); got != "" {
		t.Errorf("单片目录不应加前缀，得到 %q", got)
	}
	if got := imageBaseName(dir, "某片 (2020)", 2); got != "某片 (2020)-" {
		t.Errorf("多片目录应加前缀，得到 %q", got)
	}
}
