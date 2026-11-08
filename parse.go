package main

import (
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Kind 是条目类型。
type Kind string

const (
	KindMovie   Kind = "movie"
	KindEpisode Kind = "episode"
)

// Parsed 是单次文件名解析的结果。
type Parsed struct {
	Title   string
	Year    int
	Season  int // 0 = 未知
	Episode int // -1 = 不是分集
	Kind    Kind
}

var (
	// S01E03 / S01.E03 / S01EP03
	reSeasonEp = regexp.MustCompile(`(?i)[\s._\-\[\(]?[Ss](\d{1,2})[\s._\-]?(?:[Ee][Pp]?)(\d{1,3})`)
	// 番剧：清洗后形如 "番名 - 01"
	reEpDash = regexp.MustCompile(`^(.+?)\s*[-–—]\s*(\d{1,3})(?:v\d)?\s*$`)
	// 季目录： Season 01 / S01 / 第1季
	reSeasonDir = regexp.MustCompile(`(?i)^\s*(?:season[\s._-]*(\d{1,2})|s(\d{1,2})|第\s*(\d{1,2})\s*季)\s*$`)
	// 任意位置的四位年份，允许被括号包住
	reYear = regexp.MustCompile(`[(（\[]?\s*((?:19|20)\d{2})\s*[)）\]]?`)
	// 严格形态：标题 (年份)
	reTitleYear = regexp.MustCompile(`^(.+?)\s*[(（\[]\s*((?:19|20)\d{2})\s*[)）\]]\s*$`)
	// 分隔符
	reSep = regexp.MustCompile(`[\s._\-–—]+`)
	// 连续空白
	reSpace = regexp.MustCompile(`\s+`)
)

// junkTokens 是要丢弃的发布标记。
var junkTokens = []string{
	"1080p", "2160p", "720p", "480p", "4k", "8k", "uhd",
	"bluray", "webdl", "webrip", "hdrip", "bdrip", "dvdrip", "hdtv", "hdtc",
	"x264", "x265", "h264", "h265", "hevc", "avc", "10bit", "8bit",
	"aac", "ac3", "eac3", "dts", "ddp", "dd", "flac", "truehd", "atmos",
	"hdr", "hdr10", "dv", "dolbyvision",
	"remux", "proper", "repack", "extended", "imax", "complete",
	"internal", "limited", "uncut", "chd", "wiki", "cmct", "frds",
}

// reJunkWord 匹配独立成词的发布标记，保留两侧的分隔符。
// Go 的 RE2 不支持 lookahead，所以用捕获组把分隔符放回去。
var reJunkWord *regexp.Regexp

// junkNormPairs 先把带分隔符的复合标记压成单词，且不改动其他部分的大小写。
var junkNormPairs = []struct {
	re *regexp.Regexp
	to string
}{
	{regexp.MustCompile(`(?i)web[\s._-]?rip`), "webrip"},
	{regexp.MustCompile(`(?i)web[\s._-]?dl`), "webdl"},
	{regexp.MustCompile(`(?i)blu[\s._-]?ray`), "bluray"},
	{regexp.MustCompile(`(?i)bd[\s._-]?rip`), "bdrip"},
	{regexp.MustCompile(`(?i)dvd[\s._-]?rip`), "dvdrip"},
	{regexp.MustCompile(`(?i)hd[\s._-]?rip`), "hdrip"},
	{regexp.MustCompile(`(?i)dolby[\s._-]?vision`), "dolbyvision"},
	{regexp.MustCompile(`(?i)true[\s._-]?hd`), "truehd"},
	{regexp.MustCompile(`(?i)dolby[\s._-]?atmos`), "atmos"},
	{regexp.MustCompile(`(?i)(?:ddp?|ac3|dts)[\s._-]?5\.1`), "dd"},
	{regexp.MustCompile(`(?i)\b[57]\.1\b`), " "},
}

func init() {
	// 长的先匹配，避免 "hdr10" 被 "hdr" 截断
	sorted := append([]string(nil), junkTokens...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for i := range sorted {
		sorted[i] = regexp.QuoteMeta(sorted[i])
	}
	reJunkWord = regexp.MustCompile(`(?i)(^|[\s._\-])` + "(?:" + strings.Join(sorted, "|") + `)([\s._\-]|$)`)
}

// ParseName 解析文件名或目录名。顺序见文档 6.3：
// 先抽年份/季集，再清洗标题——反过来会把年份当噪声删掉。
func ParseName(name string) Parsed {
	base := filepath.Base(strings.TrimRight(name, "/\\"))
	stem := base
	if ext := filepath.Ext(base); ext != "" {
		stem = strings.TrimSuffix(base, ext)
	}

	p := Parsed{Episode: -1}

	// ① 年份（必须在此刻消费掉）
	p.Year, stem = extractYear(stem)

	// ② SxxExx
	if loc := reSeasonEp.FindStringSubmatchIndex(stem); loc != nil {
		sub := reSeasonEp.FindStringSubmatch(stem)
		p.Season, _ = strconv.Atoi(sub[1])
		p.Episode, _ = strconv.Atoi(sub[2])
		stem = stem[:loc[0]] + " " + stem[loc[1]:]
	}

	// ③ 去括号组（不含 CJK 的）
	stem = stripJunkBrackets(stem)

	// ④ 去发布标记，保留分隔符——这样 "番名 - 01" 的连字符还在
	stem = stripJunkWords(stem)

	// ⑤ 番剧形态： "标题 - 编号"（此时还在清洗前的形态里判断）
	if p.Episode < 0 {
		if m := reEpDash.FindStringSubmatch(strings.TrimSpace(stem)); m != nil {
			if n, err := strconv.Atoi(m[2]); err == nil {
				p.Title = normalizeTitle(m[1])
				p.Episode = n
				p.Season = 1
				return finishKind(p)
			}
		}
	}

	p.Title = normalizeTitle(stem)
	return finishKind(p)
}

func finishKind(p Parsed) Parsed {
	if p.Episode >= 0 {
		p.Kind = KindEpisode
		if p.Season == 0 {
			p.Season = 1
		}
	} else {
		p.Kind = KindMovie
	}
	return p
}

// extractYear 抽出第一个落在 1930–2035 的四位年份并从串中移除。
// 前后紧邻数字的不算（避免从像素值里截年份）。
func extractYear(s string) (int, string) {
	for _, loc := range reYear.FindAllStringSubmatchIndex(s, -1) {
		y, err := strconv.Atoi(s[loc[2]:loc[3]])
		if err != nil || y < 1930 || y > 2035 {
			continue
		}
		if loc[2] > 0 && isASCIIDigit(s[loc[2]-1]) {
			continue
		}
		if loc[3] < len(s) && isASCIIDigit(s[loc[3]]) {
			continue
		}
		return y, s[:loc[0]] + " " + s[loc[1]:]
	}
	return 0, s
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// ParseSeasonDir 解析季目录名，返回季号（0 = 不是季目录）。
func ParseSeasonDir(name string) int {
	name = strings.TrimSpace(name)
	if m := reSeasonDir.FindStringSubmatch(name); m != nil {
		for _, g := range m[1:] {
			if g != "" {
				if n, err := strconv.Atoi(g); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

// ParseTitleYear 严格解析 "标题 (年份)"，供目录归属判定使用。
func ParseTitleYear(name string) (title string, year int, ok bool) {
	if m := reTitleYear.FindStringSubmatch(strings.TrimSpace(name)); m != nil {
		y, _ := strconv.Atoi(m[2])
		return normalizeTitle(stripJunkWords(m[1])), y, true
	}
	return "", 0, false
}

// normalizeJunk 把带分隔符的复合标记压成单词，不改动其他地方的大小写。
func normalizeJunk(s string) string {
	for _, p := range junkNormPairs {
		s = p.re.ReplaceAllString(s, p.to)
	}
	return s
}

// stripJunkWords 去掉发布标记，保留原有分隔符（保住 "标题 - 01" 的结构）。
func stripJunkWords(s string) string {
	s = normalizeJunk(s)
	// 跑两遍，处理相邻的两个标记
	for i := 0; i < 2; i++ {
		s = reJunkWord.ReplaceAllString(s, "${1} ${2}")
	}
	return s
}

// stripJunkBrackets 删除不含中日韩字符的括号组（含括号本身）。
// 含 CJK 的保留，那通常是片名的一部分，如「琅琊榜（TV版）」。
func stripJunkBrackets(s string) string {
	rs := []rune(s)
	out := make([]rune, 0, len(rs))
	i := 0
	for i < len(rs) {
		var closer rune
		switch rs[i] {
		case '[':
			closer = ']'
		case '(':
			closer = ')'
		case '（':
			closer = '）'
		case '【':
			closer = '】'
		default:
			out = append(out, rs[i])
			i++
			continue
		}
		j := i + 1
		for j < len(rs) && rs[j] != closer {
			j++
		}
		if j >= len(rs) { // 没闭合，原样保留剩余部分
			out = append(out, rs[i:]...)
			break
		}
		if hasCJK(rs[i+1 : j]) {
			out = append(out, rs[i:j+1]...)
		} else {
			out = append(out, ' ')
		}
		i = j + 1
	}
	return string(out)
}

// hasCJK 判断是否含中日韩字符。
func hasCJK(rs []rune) bool {
	for _, r := range rs {
		if unicode.Is(unicode.Han, r) ||
			unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) ||
			unicode.Is(unicode.Hangul, r) {
			return true
		}
	}
	return false
}

// normalizeTitle 折叠分隔符与空白，去掉首尾杂符。
// 注意：这里不能把括号纳入 Trim 集合——括号组的取舍已经由 stripJunkBrackets
// 决定过了，再 Trim 会把「琅琊榜（TV版）」的右括号吃掉。
func normalizeTitle(s string) string {
	s = reSep.ReplaceAllString(s, " ")
	s = reSpace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = strings.Trim(s, " -–—_")
	return strings.TrimSpace(s)
}

// subLangs 是能识别的字幕语言标记。
var subLangs = map[string]bool{
	"chs": true, "cht": true, "chi": true, "zho": true, "zh": true,
	"zh-cn": true, "zh-tw": true, "zh-hk": true, "zh-hans": true, "zh-hant": true,
	"sc": true, "tc": true, "gb": true, "big5": true,
	"eng": true, "en": true, "jpn": true, "ja": true, "kor": true, "ko": true,
}

// SubtitleLang 从字幕文件名提取语言标记，识别不了返回空串。
func SubtitleLang(name string) string {
	base := filepath.Base(name)
	stem := base
	if ext := filepath.Ext(base); ext != "" {
		stem = strings.TrimSuffix(base, ext)
	}
	// 形如 film.chs.srt / film.chs.forced.srt
	parts := strings.Split(stem, ".")
	for i := len(parts) - 1; i >= 1 && i >= len(parts)-3; i-- {
		if c := strings.ToLower(parts[i]); subLangs[c] {
			return c
		}
	}
	return ""
}

// NormalizeLang 把常见写法收敛成三字母码，便于按 sub_langs 排序。
func NormalizeLang(l string) string {
	switch strings.ToLower(l) {
	case "sc", "gb", "zh", "zh-cn", "zh-hans", "chi", "zho", "chs":
		return "chs"
	case "tc", "big5", "zh-tw", "zh-hk", "zh-hant", "cht":
		return "cht"
	case "en", "eng":
		return "eng"
	case "ja", "jpn":
		return "jpn"
	case "ko", "kor":
		return "kor"
	}
	return strings.ToLower(l)
}

// IsExtIn 判断扩展名（含点）是否在允许列表内。
func IsExtIn(ext string, allowed []string) bool {
	ext = strings.ToLower(ext)
	for _, e := range allowed {
		if strings.EqualFold(e, ext) {
			return true
		}
	}
	return false
}

// skipDir 返回 true 表示该目录应被跳过。
func skipDir(name string) bool {
	switch name {
	case ".litems", ".mediasrv", "@eaDir", "$RECYCLE.BIN", "System Volume Information", ".thumbnails", "#recycle":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// LevenshteinRatio 返回相似度比（1 - dist/maxlen），已折叠大小写与标点。
func LevenshteinRatio(a, b string) float64 {
	a = foldForCompare(a)
	b = foldForCompare(b)
	if a == "" && b == "" {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la > lb {
		ra, rb = rb, ra
		la, lb = lb, la
	}
	prev := make([]int, la+1)
	cur := make([]int, la+1)
	for i := 0; i <= la; i++ {
		prev[i] = i
	}
	for j := 1; j <= lb; j++ {
		cur[0] = j
		for i := 1; i <= la; i++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			m := prev[i-1] + cost
			if v := prev[i] + 1; v < m {
				m = v
			}
			if v := cur[i-1] + 1; v < m {
				m = v
			}
			cur[i] = m
		}
		prev, cur = cur, prev
	}
	return 1 - float64(prev[la])/float64(lb)
}

// foldForCompare 去掉标点、空白并转小写。
func foldForCompare(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsPunct(r) || unicode.IsSpace(r) || unicode.IsSymbol(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
