package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TMDB 客户端。设计要点（文档 8.2）：
//   - 域名可配，默认 api.tmdb.org（实测在污染网络下仍可直连，见 13.2）
//   - 请求间强制间隔；并发由一个信号量兜住
//   - 429 读 Retry-After 并把间隔翻倍，5xx/网络错误退避重试，401/404 不重试
//   - 语言回落是字段级的：中文 overview 为空时补一次 fallback 语言
//
// 这里只用标准库，符合 ADR-004 的零依赖约束。

// ErrNotFound 表示目标资源不存在（404），调用方不应重试。
var ErrNotFound = errors.New("资源不存在")

// tmdbThrottle 同时限制并发与速率。
type tmdbThrottle struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
	sem      chan struct{}
}

func newTMDBThrottle(concurrency int, interval time.Duration) *tmdbThrottle {
	if concurrency < 1 {
		concurrency = 1
	}
	return &tmdbThrottle{
		interval: interval,
		sem:      make(chan struct{}, concurrency),
	}
}

// acquire 阻塞到可以发下一个请求；调用方必须配对 release。
func (t *tmdbThrottle) acquire(ctx context.Context) error {
	select {
	case t.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	t.mu.Lock()
	now := time.Now()
	if t.next.After(now) {
		wait := t.next.Sub(now)
		t.next = t.next.Add(t.interval)
		t.mu.Unlock()
		if err := sleepCtx(ctx, wait); err != nil {
			<-t.sem
			return err
		}
		return nil
	}
	t.next = now.Add(t.interval)
	t.mu.Unlock()
	return nil
}

func (t *tmdbThrottle) release() { <-t.sem }

// bumpInterval 在收到 429 后放宽间隔（上限 2s），避免继续撞墙。
func (t *tmdbThrottle) bumpInterval() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.interval *= 2
	if t.interval > 2*time.Second {
		t.interval = 2 * time.Second
	}
}

func (t *tmdbThrottle) current() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.interval
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TMDBClient 是 TMDB v3 API 的最小封装。
type TMDBClient struct {
	base       string // 如 https://api.tmdb.org/3
	imageBase  string // 如 https://image.tmdb.org/t/p
	apiKey     string
	apiToken   string
	language   string
	fallback   string
	hc         *http.Client
	th         *tmdbThrottle
	imgSizes   map[string]bool
	requestCnt int64
	mu         sync.Mutex
}

// NewTMDBClient 按配置构造客户端。
func NewTMDBClient(cfg *Config) *TMDBClient {
	base := strings.TrimRight(cfg.Scrape.TMDBBase, "/")
	if base == "" {
		base = "https://api.tmdb.org/3"
	}
	img := strings.TrimRight(cfg.Scrape.TMDBImageBase, "/")
	if img == "" {
		img = "https://image.tmdb.org/t/p"
	}
	return &TMDBClient{
		base:      base,
		imageBase: img,
		apiKey:    strings.TrimSpace(cfg.Scrape.TMDBKey),
		apiToken:  strings.TrimSpace(cfg.Scrape.TMDBToken),
		language:  cfg.Scrape.Language,
		fallback:  cfg.Scrape.FallbackLang,
		hc: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     60 * time.Second,
			},
		},
		th: newTMDBThrottle(cfg.Scrape.Concurrency, time.Duration(cfg.Scrape.IntervalMS)*time.Millisecond),
	}
}

// Name 实现 Scraper 接口。
func (c *TMDBClient) Name() string { return "tmdb" }

// NativeKey 只认 TMDB 数字 ID；只有 IMDb 号时由 Runner 走 find 接口反查。
func (c *TMDBClient) NativeKey(tmdbID int, imdbID string) string {
	if tmdbID > 0 {
		return strconv.Itoa(tmdbID)
	}
	return ""
}

// ImageURL 拼出指定尺寸的图片地址。path 是 TMDB 返回的 "/xxxx.jpg"。
func (c *TMDBClient) ImageURL(path, size string) string {
	if path == "" {
		return ""
	}
	if size == "" {
		size = "original"
	}
	return c.imageBase + "/" + size + path
}

func (c *TMDBClient) newRequest(ctx context.Context, path string, q url.Values) (*http.Request, error) {
	if q == nil {
		q = url.Values{}
	}
	if c.language != "" {
		q.Set("language", c.language)
	}
	if c.apiToken == "" {
		if c.apiKey == "" {
			return nil, errors.New("未配置 tmdb_token 或 tmdb_key，无法刮削")
		}
		q.Set("api_key", c.apiKey)
	}
	u := c.base + path
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiToken)
	}
	return req, nil
}

// get 是所有 API 调用的唯一出口，负责限速、重试与错误归类。
func (c *TMDBClient) get(ctx context.Context, path string, q url.Values, out any) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := c.th.acquire(ctx); err != nil {
			return err
		}
		req, err := c.newRequest(ctx, path, q)
		if err != nil {
			c.th.release()
			return err
		}
		c.mu.Lock()
		c.requestCnt++
		c.mu.Unlock()

		resp, err := c.hc.Do(req)
		if err != nil {
			c.th.release()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 网络错误里会带完整 URL（含 api_key），日志会落到文件，必须抹掉
			lastErr = fmt.Errorf("请求 TMDB 失败：%s", c.redact(err.Error()))
			if err := sleepCtx(ctx, backoffFor(attempt)); err != nil {
				return err
			}
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		c.th.release()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("解析 TMDB 响应失败（%s）：%w", path, err)
			}
			return nil
		case resp.StatusCode == http.StatusNotFound:
			return ErrNotFound
		case resp.StatusCode == http.StatusUnauthorized:
			return fmt.Errorf("TMDB 拒绝访问：API Key 无效（%s）", snippet(body))
		case resp.StatusCode == http.StatusTooManyRequests:
			wait := retryAfter(resp, 5*time.Second)
			c.th.bumpInterval()
			lastErr = fmt.Errorf("TMDB 限速（429），等待 %s", wait)
			warnf("触发 TMDB 限速，把请求间隔放宽到 %s", c.th.current())
			if err := sleepCtx(ctx, wait); err != nil {
				return err
			}
			continue
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("TMDB 服务端错误 %d：%s", resp.StatusCode, snippet(body))
			if err := sleepCtx(ctx, backoffFor(attempt)); err != nil {
				return err
			}
			continue
		default:
			return fmt.Errorf("TMDB 返回 %d：%s", resp.StatusCode, snippet(body))
		}
	}
	if lastErr == nil {
		lastErr = errors.New("TMDB 请求失败（未知原因）")
	}
	return lastErr
}

// backoffFor 给出 1s / 2s / 4s 的退避（文档 8.2）。
func backoffFor(attempt int) time.Duration {
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func retryAfter(resp *http.Response, def time.Duration) time.Duration {
	v := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 120 {
		return time.Duration(n) * time.Second
	}
	return def
}

// redact 抹掉错误信息里的 API Key（http.Client 的错误会带完整 URL）。
func (c *TMDBClient) redact(s string) string {
	if c.apiKey != "" {
		s = strings.ReplaceAll(s, c.apiKey, "***")
	}
	if c.apiToken != "" {
		s = strings.ReplaceAll(s, c.apiToken, "***")
	}
	return s
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ---------- 响应结构 ----------

type tmdbSearchResp struct {
	Page    int                `json:"page"`
	Results []tmdbSearchResult `json:"results"`
}

type tmdbSearchResult struct {
	ID            int     `json:"id"`
	Title         string  `json:"title"`          // movie
	Name          string  `json:"name"`           // tv
	OriginalTitle string  `json:"original_title"` // movie
	OriginalName  string  `json:"original_name"`  // tv
	ReleaseDate   string  `json:"release_date"`
	FirstAirDate  string  `json:"first_air_date"`
	Overview      string  `json:"overview"`
	PosterPath    string  `json:"poster_path"`
	Popularity    float64 `json:"popularity"`
	VoteAverage   float64 `json:"vote_average"`
	VoteCount     int     `json:"vote_count"`
}

type tmdbGenre struct {
	Name string `json:"name"`
}

type tmdbCountry struct {
	Code string `json:"iso_3166_1"`
	Name string `json:"name"`
}

type tmdbCast struct {
	Name      string `json:"name"`
	Character string `json:"character"`
	Order     int    `json:"order"`
}

type tmdbCrew struct {
	Name       string `json:"name"`
	Job        string `json:"job"`
	Department string `json:"department"`
}

type tmdbCredits struct {
	Cast []tmdbCast `json:"cast"`
	Crew []tmdbCrew `json:"crew"`
}

type tmdbAltTitles struct {
	Titles []struct {
		Title string `json:"title"`
	} `json:"titles"`
}

type tmdbMovie struct {
	ID                  int           `json:"id"`
	ImdbID              string        `json:"imdb_id"`
	Title               string        `json:"title"`
	OriginalTitle       string        `json:"original_title"`
	Overview            string        `json:"overview"`
	Tagline             string        `json:"tagline"`
	Runtime             int           `json:"runtime"`
	ReleaseDate         string        `json:"release_date"`
	VoteAverage         float64       `json:"vote_average"`
	VoteCount           int           `json:"vote_count"`
	PosterPath          string        `json:"poster_path"`
	BackdropPath        string        `json:"backdrop_path"`
	Genres              []tmdbGenre   `json:"genres"`
	ProductionCountries []tmdbCountry `json:"production_countries"`
	Credits             tmdbCredits   `json:"credits"`
	AlternativeTitles   tmdbAltTitles `json:"alternative_titles"`
}

type tmdbTV struct {
	ID            int           `json:"id"`
	Name          string        `json:"name"`
	OriginalName  string        `json:"original_name"`
	Overview      string        `json:"overview"`
	Tagline       string        `json:"tagline"`
	EpisodeRun    []int         `json:"episode_run_time"`
	FirstAirDate  string        `json:"first_air_date"`
	VoteAverage   float64       `json:"vote_average"`
	VoteCount     int           `json:"vote_count"`
	PosterPath    string        `json:"poster_path"`
	BackdropPath  string        `json:"backdrop_path"`
	Genres        []tmdbGenre   `json:"genres"`
	OriginCountry []string      `json:"origin_country"`
	Credits       tmdbCredits   `json:"credits"`
	AltTitles     tmdbAltTitles `json:"alternative_titles"`
	Seasons       []struct {
		SeasonNumber int `json:"season_number"`
		EpisodeCount int `json:"episode_count"`
	} `json:"seasons"`
	NumberSeasons  int `json:"number_of_seasons"`
	NumberEpisodes int `json:"number_of_episodes"`
}

type tmdbSeason struct {
	Episodes []struct {
		EpisodeNumber int     `json:"episode_number"`
		Name          string  `json:"name"`
		Overview      string  `json:"overview"`
		StillPath     string  `json:"still_path"`
		AirDate       string  `json:"air_date"`
		Runtime       int     `json:"runtime"`
		VoteAverage   float64 `json:"vote_average"`
		VoteCount     int     `json:"vote_count"`
	} `json:"episodes"`
}

// ---------- Scraper 实现 ----------

// Search 按标题（+年份）搜索候选。kind 决定走 movie 还是 tv 端点。
func (c *TMDBClient) Search(ctx context.Context, q Query) ([]Candidate, error) {
	vals := url.Values{}
	vals.Set("query", q.Title)
	vals.Set("include_adult", "false")

	path := "/search/movie"
	if q.Kind == KindEpisode {
		path = "/search/tv"
		if q.Year > 0 {
			vals.Set("first_air_date_year", strconv.Itoa(q.Year))
		}
	} else if q.Year > 0 {
		vals.Set("year", strconv.Itoa(q.Year))
	}

	var resp tmdbSearchResp
	if err := c.get(ctx, path, vals, &resp); err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(resp.Results))
	for _, r := range resp.Results {
		cand := Candidate{
			Provider:   "tmdb",
			Key:        strconv.Itoa(r.ID),
			ID:         r.ID,
			Kind:       q.Kind,
			PosterURL:  c.ImageURL(r.PosterPath, "w185"),
			Overview:   r.Overview,
			Popularity: r.Popularity,
			Rating:     r.VoteAverage,
		}
		if q.Kind == KindEpisode {
			cand.Title, cand.OriginalTitle, cand.Year = r.Name, r.OriginalName, yearOf(r.FirstAirDate)
		} else {
			cand.Title, cand.OriginalTitle, cand.Year = r.Title, r.OriginalTitle, yearOf(r.ReleaseDate)
		}
		out = append(out, cand)
	}
	return out, nil
}

// AltTitles 用于救回同名条目：只查一部片的别名，调用成本高于搜索本身，
// 因此只在正常匹配全部落空时才用（见 scrape.go 的 matchOrRescue）。
func (c *TMDBClient) AltTitles(ctx context.Context, key string, kind Kind) ([]string, error) {
	id, err := strconv.Atoi(key)
	if err != nil || id <= 0 {
		return nil, fmt.Errorf("不是合法的 TMDB ID：%q", key)
	}
	var out tmdbAltTitles
	path := fmt.Sprintf("/movie/%d/alternative_titles", id)
	if kind == KindEpisode {
		path = fmt.Sprintf("/tv/%d/alternative_titles", id)
	}
	if err := c.get(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Titles))
	for _, t := range out.Titles {
		if t.Title != "" {
			names = append(names, t.Title)
		}
	}
	return names, nil
}

// Detail 按 key（TMDB 数字 ID 的字符串形式）拉取详情，带字段级语言回落。
func (c *TMDBClient) Detail(ctx context.Context, key string, kind Kind) (*Meta, error) {
	id, err := strconv.Atoi(key)
	if err != nil || id <= 0 {
		return nil, fmt.Errorf("不是合法的 TMDB ID：%q", key)
	}
	if kind == KindEpisode {
		return c.tvDetail(ctx, id)
	}
	return c.movieDetail(ctx, id)
}

// movieDetail 拉取电影详情。
func (c *TMDBClient) movieDetail(ctx context.Context, id int) (*Meta, error) {
	var m tmdbMovie
	if err := c.get(ctx, fmt.Sprintf("/movie/%d", id), url.Values{
		"append_to_response": {"credits,alternative_titles"},
	}, &m); err != nil {
		return nil, err
	}
	// 中文简介缺失是常态（大量老片只有英文），只补空字段，不整包替换
	if strings.TrimSpace(m.Overview) == "" && c.fallback != "" && c.fallback != c.language {
		var fb tmdbMovie
		vals := url.Values{"language": {c.fallback}}
		if err := c.get(ctx, fmt.Sprintf("/movie/%d", id), vals, &fb); err == nil {
			if m.Overview == "" {
				m.Overview = fb.Overview
			}
			if m.Tagline == "" {
				m.Tagline = fb.Tagline
			}
		}
	}

	meta := &Meta{
		Provider:      "tmdb",
		TmdbID:        m.ID,
		ImdbID:        m.ImdbID,
		Title:         m.Title,
		OriginalTitle: m.OriginalTitle,
		Tagline:       m.Tagline,
		Overview:      m.Overview,
		Year:          yearOf(m.ReleaseDate),
		RuntimeMin:    m.Runtime,
		Rating:        m.VoteAverage,
		Votes:         m.VoteCount,
		PosterPath:    m.PosterPath,
		FanartPath:    m.BackdropPath,
	}
	for _, g := range m.Genres {
		if g.Name != "" {
			meta.Genres = append(meta.Genres, g.Name)
		}
	}
	for _, p := range m.ProductionCountries {
		if zh := zhCountry(p.Code, p.Name); zh != "" {
			meta.Countries = append(meta.Countries, zh)
		}
	}
	meta.Directors, meta.Writers, meta.Actors = splitCrew(m.Credits)
	return meta, nil
}

// tvDetail 拉取剧集详情。
func (c *TMDBClient) tvDetail(ctx context.Context, id int) (*Meta, error) {
	var t tmdbTV
	if err := c.get(ctx, fmt.Sprintf("/tv/%d", id), url.Values{
		"append_to_response": {"credits,alternative_titles"},
	}, &t); err != nil {
		return nil, err
	}
	if strings.TrimSpace(t.Overview) == "" && c.fallback != "" && c.fallback != c.language {
		var fb tmdbTV
		if err := c.get(ctx, fmt.Sprintf("/tv/%d", id), url.Values{"language": {c.fallback}}, &fb); err == nil {
			if t.Overview == "" {
				t.Overview = fb.Overview
			}
		}
	}
	meta := &Meta{
		Provider:      "tmdb",
		TmdbID:        t.ID,
		Title:         t.Name,
		OriginalTitle: t.OriginalName,
		Tagline:       t.Tagline,
		Overview:      t.Overview,
		Year:          yearOf(t.FirstAirDate),
		Rating:        t.VoteAverage,
		Votes:         t.VoteCount,
		PosterPath:    t.PosterPath,
		FanartPath:    t.BackdropPath,
		Seasons:       t.NumberSeasons,
		Episodes:      t.NumberEpisodes,
	}
	if len(t.EpisodeRun) > 0 {
		meta.RuntimeMin = t.EpisodeRun[0]
	}
	for _, g := range t.Genres {
		if g.Name != "" {
			meta.Genres = append(meta.Genres, g.Name)
		}
	}
	for _, c := range t.OriginCountry {
		if zh := zhCountry(c, ""); zh != "" {
			meta.Countries = append(meta.Countries, zh)
		}
	}
	meta.Directors, meta.Writers, meta.Actors = splitCrew(t.Credits)
	return meta, nil
}

// Season 拉取一季的分集列表，用于单集 NFO。
func (c *TMDBClient) Season(ctx context.Context, key string, season int) ([]EpisodeMeta, error) {
	id, err := strconv.Atoi(key)
	if err != nil || id <= 0 {
		return nil, fmt.Errorf("不是合法的 TMDB ID：%q", key)
	}
	var s tmdbSeason
	if err := c.get(ctx, fmt.Sprintf("/tv/%d/season/%d", id, season), nil, &s); err != nil {
		return nil, err
	}
	out := make([]EpisodeMeta, 0, len(s.Episodes))
	for _, e := range s.Episodes {
		out = append(out, EpisodeMeta{
			Season:    season,
			Episode:   e.EpisodeNumber,
			Title:     e.Name,
			Overview:  e.Overview,
			AirDate:   e.AirDate,
			Runtime:   e.Runtime,
			Rating:    e.VoteAverage,
			Votes:     e.VoteCount,
			StillPath: e.StillPath,
		})
	}
	return out, nil
}

// FindByImdb 用 IMDb ID 反查 TMDB ID（用于 override.json 只给了 tt 号的情况）。
func (c *TMDBClient) FindByImdb(ctx context.Context, imdbID string, kind Kind) (int, error) {
	var resp struct {
		MovieResults []struct {
			ID int `json:"id"`
		} `json:"movie_results"`
		TVResults []struct {
			ID int `json:"id"`
		} `json:"tv_results"`
	}
	if err := c.get(ctx, "/find/"+url.PathEscape(imdbID), url.Values{
		"external_source": {"imdb_id"},
	}, &resp); err != nil {
		return 0, err
	}
	if kind == KindEpisode {
		if len(resp.TVResults) > 0 {
			return resp.TVResults[0].ID, nil
		}
	} else if len(resp.MovieResults) > 0 {
		return resp.MovieResults[0].ID, nil
	}
	return 0, ErrNotFound
}

// splitCrew 从 credits 里分出导演、编剧与演员（文档 7.1 的字段映射）。
func splitCrew(cr tmdbCredits) (directors, writers []string, actors []Actor) {
	seenD := map[string]bool{}
	seenW := map[string]bool{}
	for _, m := range cr.Crew {
		switch m.Job {
		case "Director":
			if !seenD[m.Name] && m.Name != "" {
				seenD[m.Name] = true
				directors = append(directors, m.Name)
			}
		case "Writer", "Screenplay", "Story":
			if !seenW[m.Name] && m.Name != "" {
				seenW[m.Name] = true
				writers = append(writers, m.Name)
			}
		}
	}
	for _, a := range cr.Cast {
		if a.Name == "" {
			continue
		}
		actors = append(actors, Actor{Name: a.Name, Role: a.Character})
	}
	return directors, writers, actors
}

// yearOf 从 "2014-11-05" 这类日期里取年份，失败返回 0。
func yearOf(date string) int {
	date = strings.TrimSpace(date)
	if len(date) < 4 {
		return 0
	}
	n, err := strconv.Atoi(date[:4])
	if err != nil || n < 1800 || n > 2200 {
		return 0
	}
	return n
}

// Probe 做一次最小请求，用于 `scrape --probe` 自检连通性与凭据。
func (c *TMDBClient) Probe(ctx context.Context) error {
	var resp struct {
		Images struct {
			BaseURL string `json:"secure_base_url"`
		} `json:"images"`
	}
	return c.get(ctx, "/configuration", nil, &resp)
}
