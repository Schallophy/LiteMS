package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// OMDb 是兜底数据源（文档 8.2）。
//
// 它的定位很明确：**能直连、有免费额度，但没有本地化文本，也没有背景图和剧照**。
// 因此只在 TMDB 不可用时启用——至少能拿到标题、年份、简介（英文）、海报和 IMDb 评分，
// 不至于让整库刮削变成空白。
//
// 图片来源与 TMDB 不同：OMDb 直接给 Amazon 图床的绝对 URL，尺寸写在文件名里
// （..._SX300.jpg），所以 ImageURL 对绝对地址原样返回。
type OMDbClient struct {
	base   string
	apiKey string
	hc     *http.Client
	delay  time.Duration
	last   time.Time
}

// NewOMDbClient 构造兜底客户端。
func NewOMDbClient(cfg *Config) *OMDbClient {
	return &OMDbClient{
		base:   "https://www.omdbapi.com/",
		apiKey: strings.TrimSpace(cfg.Scrape.OMDBKey),
		hc:     &http.Client{Timeout: 20 * time.Second},
		delay:  time.Duration(cfg.Scrape.IntervalMS) * time.Millisecond,
	}
}

// Name 实现 Scraper 接口。
func (c *OMDbClient) Name() string { return "omdb" }

// ImageURL 对绝对地址原样返回（OMDb 的图片本身就是完整 URL）。
func (c *OMDbClient) ImageURL(path, size string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return ""
}

type omdbResponse struct {
	Response   string       `json:"Response"`
	Error      string       `json:"Error"`
	Title      string       `json:"Title"`
	Year       string       `json:"Year"`
	Rated      string       `json:"Rated"`
	Released   string       `json:"Released"`
	Runtime    string       `json:"Runtime"`
	Genre      string       `json:"Genre"`
	Director   string       `json:"Director"`
	Writer     string       `json:"Writer"`
	Actors     string       `json:"Actors"`
	Plot       string       `json:"Plot"`
	Language   string       `json:"Language"`
	Country    string       `json:"Country"`
	Poster     string       `json:"Poster"`
	ImdbRating string       `json:"imdbRating"`
	ImdbVotes  string       `json:"imdbVotes"`
	ImdbID     string       `json:"imdbID"`
	Type       string       `json:"Type"`
	TotalSeas  string       `json:"totalSeasons"`
	Search     []omdbSearch `json:"Search"`
	Season     string       `json:"Season"`
	Episodes   []omdbEp     `json:"Episodes"`
}

type omdbSearch struct {
	Title  string `json:"Title"`
	Year   string `json:"Year"`
	ImdbID string `json:"imdbID"`
	Type   string `json:"Type"`
	Poster string `json:"Poster"`
}

type omdbEp struct {
	Title    string `json:"Title"`
	Released string `json:"Released"`
	Episode  string `json:"Episode"`
	Season   string `json:"Season"`
	ImdbID   string `json:"imdbID"`
	Rating   string `json:"imdbRating"`
}

// call 发一次请求并解析。OMDb 用 HTTP 200 + Response=false 表示业务错误。
func (c *OMDbClient) call(ctx context.Context, q url.Values) (*omdbResponse, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("未配置 [scrape].omdb_key")
	}
	// 简单的节流：两次请求之间至少间隔 delay
	if d := time.Until(c.last.Add(c.delay)); d > 0 {
		if err := sleepCtx(ctx, d); err != nil {
			return nil, err
		}
	}
	c.last = time.Now()

	q.Set("apikey", c.apiKey)
	u := c.base + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("OMDb 拒绝访问：api_key 无效")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OMDb 返回 %d", resp.StatusCode)
	}
	var out omdbResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("解析 OMDb 响应失败: %w", err)
	}
	if strings.EqualFold(out.Response, "False") {
		if strings.Contains(strings.ToLower(out.Error), "not found") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("OMDb: %s", out.Error)
	}
	return &out, nil
}

// NativeKey 只认 IMDb 的 tt 号；只有 TMDB 数字 ID 时无法直接使用。
func (c *OMDbClient) NativeKey(tmdbID int, imdbID string) string {
	return strings.TrimSpace(imdbID)
}

// Search 按标题搜索。候选的 Key 就是 tt 号。
func (c *OMDbClient) Search(ctx context.Context, q Query) ([]Candidate, error) {
	vals := url.Values{"s": {q.Title}}
	if q.Kind == KindEpisode {
		vals.Set("type", "series")
	} else {
		vals.Set("type", "movie")
	}
	if q.Year > 0 {
		vals.Set("y", strconv.Itoa(q.Year))
	}
	resp, err := c.call(ctx, vals)
	if err != nil {
		if err == ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Candidate, 0, len(resp.Search))
	for _, s := range resp.Search {
		if s.ImdbID == "" {
			continue
		}
		out = append(out, Candidate{
			Provider:  "omdb",
			Key:       s.ImdbID,
			Title:     s.Title,
			Year:      yearOf(s.Year),
			Kind:      q.Kind,
			PosterURL: s.Poster,
		})
	}
	return out, nil
}

// Detail 按 tt 号取详情。kind 决定是否需要区分剧集——OMDb 返回里自带 type。
func (c *OMDbClient) Detail(ctx context.Context, key string, kind Kind) (*Meta, error) {
	resp, err := c.call(ctx, url.Values{"i": {key}, "plot": {"full"}})
	if err != nil {
		return nil, err
	}
	return c.metaOf(resp), nil
}

// Season 取一季分集。OMDb 的单集只有标题、播出日期与 IMDb 评分。
func (c *OMDbClient) Season(ctx context.Context, key string, season int) ([]EpisodeMeta, error) {
	resp, err := c.call(ctx, url.Values{
		"i":      {key},
		"Season": {strconv.Itoa(season)},
	})
	if err != nil {
		return nil, err
	}
	out := make([]EpisodeMeta, 0, len(resp.Episodes))
	for _, e := range resp.Episodes {
		n, _ := strconv.Atoi(e.Episode)
		r, _ := strconv.ParseFloat(e.Rating, 64)
		out = append(out, EpisodeMeta{
			Season:  season,
			Episode: n,
			Title:   e.Title,
			AirDate: e.Released,
			Rating:  r,
		})
	}
	return out, nil
}

// metaOf 把 OMDb 响应转成统一的 Meta。
func (c *OMDbClient) metaOf(r *omdbResponse) *Meta {
	m := &Meta{
		Provider:   "omdb",
		ImdbID:     r.ImdbID,
		Title:      r.Title,
		Overview:   cleanOMDbText(r.Plot),
		Year:       yearOf(r.Year),
		Votes:      atoiLoose(strings.ReplaceAll(r.ImdbVotes, ",", "")),
		Rating:     atofLoose(r.ImdbRating),
		PosterPath: r.Poster,
	}
	if d := parseRuntime(r.Runtime); d > 0 {
		m.RuntimeMin = d
	}
	m.Genres = splitComma(r.Genre)
	m.Countries = splitComma(r.Country)
	m.Directors = splitComma(r.Director)
	m.Writers = splitComma(r.Writer)
	for _, name := range splitComma(r.Actors) {
		m.Actors = append(m.Actors, Actor{Name: name})
	}
	if n, err := strconv.Atoi(r.TotalSeas); err == nil {
		m.Seasons = n
	}
	// OMDb 没有原始标题字段：Title 就是 IMDb 主标题
	m.OriginalTitle = ""
	return m
}

// cleanOMDbText 去掉 OMDb 常见的 "N/A" 占位。
func cleanOMDbText(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), "N/A") {
		return ""
	}
	return strings.TrimSpace(s)
}

func splitComma(s string) []string {
	s = cleanOMDbText(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseRuntime 解析 "169 min" / "1h 52min"。
func parseRuntime(s string) int {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "N/A") {
		return 0
	}
	var total int
	if idx := strings.Index(s, "h"); idx > 0 {
		if h, err := strconv.Atoi(strings.TrimSpace(s[:idx])); err == nil {
			total += h * 60
		}
		s = s[idx+1:]
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "min")
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil {
		total += n
	}
	return total
}

func atoiLoose(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atofLoose(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}
