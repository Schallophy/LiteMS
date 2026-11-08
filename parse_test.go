package main

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestParseName 覆盖文档 6.3 / 6.4 的关键场景。
// 最重要的一条：年份必须在清洗之前抽掉，否则 "（2014）" 会被当括号组删掉。
func TestParseName(t *testing.T) {
	cases := []struct {
		in     string
		title  string
		year   int
		kind   Kind
		season int
		ep     int
		why    string
	}{
		{"星际穿越 (2014).mkv", "星际穿越", 2014, KindMovie, 0, -1, "标准电影，年份在括号里"},
		{"切尔诺贝利（2019）.mkv", "切尔诺贝利", 2019, KindMovie, 0, -1, "全角括号"},
		{"SomeMovie.mkv", "SomeMovie", 0, KindMovie, 0, -1, "无年份"},
		{"Interstellar.2014.1080p.BluRay.x264-SPARKS.mkv", "Interstellar SPARKS", 2014, KindMovie, 0, -1, "发布名：组名会残留，靠 M2 的降级查询兜"},
		{"Show.S01E03.2160p.WEB-DL.mkv", "Show", 0, KindEpisode, 1, 3, "SxxExx 优先于年份"},
		{"[SubGroup] 番名 - 01 [1080p].mkv", "番名", 0, KindEpisode, 1, 1, "番剧：清洗必须保住连字符"},
		{"琅琊榜（TV版）(2015).mkv", "琅琊榜（TV版）", 2015, KindMovie, 0, -1, "含 CJK 的括号要保留"},
		{"The.Matrix.1999.mkv", "The Matrix", 1999, KindMovie, 0, -1, "点分隔"},
		{"1917.2019.1080p.mkv", "1917", 2019, KindMovie, 0, -1, "片名本身是数字"},
		{"Blade.Runner.2049.2017.mkv", "Blade Runner 2049", 2017, KindMovie, 0, -1, "2049 超出年份区间，被当作片名的一部分"},
	}
	for _, c := range cases {
		got := ParseName(c.in)
		if got.Title != c.title {
			t.Errorf("%s\n  title = %q, want %q  (%s)", c.in, got.Title, c.title, c.why)
		}
		if got.Year != c.year {
			t.Errorf("%s\n  year = %d, want %d  (%s)", c.in, got.Year, c.year, c.why)
		}
		if got.Kind != c.kind {
			t.Errorf("%s\n  kind = %q, want %q  (%s)", c.in, got.Kind, c.kind, c.why)
		}
		if c.kind == KindEpisode {
			if got.Season != c.season || got.Episode != c.ep {
				t.Errorf("%s\n  S%dE%d, want S%dE%d  (%s)", c.in, got.Season, got.Episode, c.season, c.ep, c.why)
			}
		}
	}
}

func TestParseSeasonDir(t *testing.T) {
	cases := map[string]int{
		"Season 01": 1,
		"season 2":  2,
		"S03":       3,
		"第4季":       4,
		"Season 05": 5,
		"特别篇":       0,
		"Extras":    0,
	}
	for in, want := range cases {
		if got := ParseSeasonDir(in); got != want {
			t.Errorf("ParseSeasonDir(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseTitleYear(t *testing.T) {
	if title, year, ok := ParseTitleYear("星际穿越 (2014)"); !ok || title != "星际穿越" || year != 2014 {
		t.Errorf("got %q %d %v", title, year, ok)
	}
	if _, _, ok := ParseTitleYear("Season 01"); ok {
		t.Error("Season 01 不应被当成 标题(年份)")
	}
	if _, _, ok := ParseTitleYear("没有年份的目录"); ok {
		t.Error("无年份不应命中")
	}
}

func TestSubtitleLang(t *testing.T) {
	cases := map[string]string{
		"星际穿越.chs.srt":          "chs",
		"星际穿越.chs.forced.srt":   "chs",
		"Show.S01E01.zh-CN.ass": "zh-cn",
		"Show.S01E01.eng.vtt":   "eng",
		"some.other.file.srt":   "",
		"movie.srt":             "",
	}
	for in, want := range cases {
		if got := SubtitleLang(in); got != want {
			t.Errorf("SubtitleLang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeLang(t *testing.T) {
	cases := map[string]string{
		"zh-CN": "chs", "sc": "chs", "chs": "chs",
		"zh-TW": "cht", "big5": "cht",
		"en": "eng", "eng": "eng",
	}
	for in, want := range cases {
		if got := NormalizeLang(in); got != want {
			t.Errorf("NormalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLevenshteinRatio 验证相似度计算。
// 注意 "星际穿越"/"星际效应" 只有 0.5——两处替换。这说明跨译名匹配
// 光靠编辑距离不够（0.5 < 0.75 门槛），必须依赖别名表，见文档 8.3。
func TestLevenshteinRatio(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"Interstellar", "Interstellar", 1},
		{"Interstellar", "interstellar", 1},
		{"星际穿越", "星际效应", 0.5},
		{"Interstellar (2014)", "Interstellar", 0.75},
		{"", "", 1},
		{"abc", "", 0},
	}
	for _, c := range cases {
		if got := LevenshteinRatio(c.a, c.b); !near(got, c.want) {
			t.Errorf("LevenshteinRatio(%q,%q) = %.4f, want %.4f", c.a, c.b, got, c.want)
		}
	}
}

func TestStripJunkBrackets(t *testing.T) {
	cases := map[string]string{
		"[SubGroup] 片名":    "片名",
		"片名 [1080p]":       "片名",
		"片名（TV版）":          "片名（TV版）",
		"片名 (2014) [x265]": "片名",
	}
	for in, want := range cases {
		if got := normalizeTitle(stripJunkBrackets(in)); got != want {
			t.Errorf("stripJunkBrackets(%q) → %q, want %q", in, got, want)
		}
	}
}

func TestSkipDir(t *testing.T) {
	for _, d := range []string{".litems", ".mediasrv", "@eaDir", "$RECYCLE.BIN", ".hidden", "#recycle"} {
		if !skipDir(d) {
			t.Errorf("%q 应被跳过", d)
		}
	}
	for _, d := range []string{"电影", "剧集", "Movies", "Media"} {
		if skipDir(d) {
			t.Errorf("%q 不应被跳过", d)
		}
	}
}
