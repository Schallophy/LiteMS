package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 刮削主流程。分层：
//
//	Scraper 接口  ← TMDB / OMDb 各自实现，业务逻辑不感知数据源
//	Matcher      ← 硬门槛（标题相似度、年份差）+ 排序分，不碰网络
//	Runner       ← 取待刮条目、按剧集分组、写 NFO 与图片、回填索引
//
// 关键约束（文档 8.5）：幂等。重复执行不覆盖 lockdata=true 的 NFO，
// 已存在的图片不重复下载；删掉 index.db 重扫，结果与之前一致。

// Query 是一次搜索请求。
type Query struct {
	Title string
	Year  int
	Kind  Kind
}

// Candidate 是搜索结果里的一条候选。
// Key 是数据源自己的标识：TMDB 是数字 ID，OMDb 是 tt 号，因此用字符串。
type Candidate struct {
	Provider      string
	Key           string
	ID            int
	Kind          Kind
	Title         string
	OriginalTitle string
	Year          int
	Overview      string
	PosterURL     string
	Popularity    float64
	Rating        float64
	AltTitles     []string
}

// Actor 是一条演职员记录。
type Actor struct {
	Name string
	Role string
}

// Meta 是一个条目的完整元数据。图片字段存的是数据源的相对路径，
// 具体落盘文件名由 Runner 决定。
type Meta struct {
	Provider      string
	TmdbID        int
	ImdbID        string
	Title         string
	OriginalTitle string
	Tagline       string
	Overview      string
	Year          int
	RuntimeMin    int
	Rating        float64
	Votes         int
	Genres        []string
	Countries     []string
	Directors     []string
	Writers       []string
	Actors        []Actor
	PosterPath    string
	FanartPath    string
	Seasons       int
	Episodes      int
}

// EpisodeMeta 是单集元数据。
type EpisodeMeta struct {
	Season    int
	Episode   int
	Title     string
	Overview  string
	AirDate   string
	Runtime   int
	Rating    float64
	Votes     int
	StillPath string
}

// Scraper 是数据源接口（文档 8.2）。新增数据源只需实现这四个方法。
// key 是数据源自己的标识，调用方只负责透传，不解释它的含义。
type Scraper interface {
	Name() string
	Search(ctx context.Context, q Query) ([]Candidate, error)
	Detail(ctx context.Context, key string, kind Kind) (*Meta, error)
	Season(ctx context.Context, key string, season int) ([]EpisodeMeta, error)
	// NativeKey 把本地已有的 ID（TMDB 数字 / IMDb tt 号）转成该数据源的 key。
	// 无法转换时返回空串，调用方会退回搜索。
	NativeKey(tmdbID int, imdbID string) string
}

// ---------- 匹配 ----------

// matchResult 是一次匹配的结论。
type matchResult struct {
	Cand  Candidate
	Score float64
	Sim   float64
	OK    bool
}

// matchCandidates 按文档 8.3 的规则挑选：先过硬门槛，再按分数排序。
//
// 硬门槛：
//   - 标题相似度 ≥ minSim（对标题/原名取最大值）
//   - 双方都有年份时，相差不超过 1 年
//
// 排序分：相似度为主，年份一致加分，popularity 只用来破同分。
func matchCandidates(cands []Candidate, q Query, minSim float64) matchResult {
	var best matchResult
	for _, c := range cands {
		sim := bestSimilarity(q.Title, c.Title, c.OriginalTitle)
		if sim < minSim {
			continue
		}
		if q.Year > 0 && c.Year > 0 {
			diff := q.Year - c.Year
			if diff < 0 {
				diff = -diff
			}
			if diff > 1 {
				continue
			}
		}
		score := sim * 100
		if q.Year > 0 && c.Year == q.Year {
			score += 20
		} else if q.Year > 0 && c.Year > 0 {
			score += 10
		}
		if c.Popularity > 0 {
			score += minF(c.Popularity/1000, 5)
		}
		if !best.OK || score > best.Score {
			best = matchResult{Cand: c, Score: score, Sim: sim, OK: true}
		}
	}
	return best
}

// bestSimilarity 在多个标题里取最高相似度，并用别名兜底。
func bestSimilarity(want string, titles ...string) float64 {
	best := 0.0
	for _, t := range titles {
		if t == "" {
			continue
		}
		if s := LevenshteinRatio(want, t); s > best {
			best = s
		}
	}
	return best
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// ---------- 统计 ----------

// ScrapeStats 是单次刮削的统计。
type ScrapeStats struct {
	Total   int
	OK      int
	Miss    int
	Manual  int
	Failed  int
	Skipped int
	Images  int
	Elapsed time.Duration
}

// PrintScrapeSummary 输出摘要。
func (s *ScrapeStats) PrintScrapeSummary(dry bool) {
	suffix := ""
	if dry {
		suffix = "（dry-run，未写文件、未调 API）"
	}
	fmt.Println()
	fmt.Printf("  待处理            %d\n", s.Total)
	fmt.Printf("  ├─ 成功          %d\n", s.OK)
	fmt.Printf("  ├─ 未匹配        %d\n", s.Miss)
	fmt.Printf("  ├─ 已锁定        %d（lockdata=true，未覆盖）\n", s.Manual)
	fmt.Printf("  ├─ 跳过          %d\n", s.Skipped)
	fmt.Printf("  └─ 失败          %d\n", s.Failed)
	if s.Images > 0 {
		fmt.Printf("  下载图片          %d 张\n", s.Images)
	}
	fmt.Printf("  耗时             %s%s\n", s.Elapsed.Round(time.Millisecond), suffix)
}

// ---------- Runner ----------

// Runner 执行一次刮削任务。
type Runner struct {
	cfg    *Config
	db     *DB
	sc     Scraper
	dry    bool
	force  bool
	onlyID string
	limit  int

	stats  ScrapeStats
	misses []missEntry
	shows  map[string]*showState
	videos map[string]int // 目录 → 视频文件数
	roots  []string
	imgHC  *http.Client
}

type missEntry struct {
	RootIdx int
	Rel     string
	Reason  string
}

// showState 缓存一部剧的刮削结果，避免同一部剧的每集都重新搜索。
type showState struct {
	key        string // 数据源标识
	meta       *Meta
	posterRel  string
	fanartRel  string
	eps        map[int]EpisodeMeta // key = epKey(season, episode)
	seasons    map[int]bool        // 已加载的分集表
	missReason string              // 非空表示"未匹配"，不是失败
	err        error
}

// epKey 把季号与集号压成一个键，避免多季的集号互相覆盖。
func epKey(season, episode int) int { return season*10000 + episode }

// NewRunner 构造刮削器。
func NewRunner(cfg *Config, db *DB, sc Scraper, dry, force bool, onlyID string, limit int) *Runner {
	return &Runner{
		cfg: cfg, db: db, sc: sc,
		dry: dry, force: force, onlyID: onlyID, limit: limit,
		shows:  make(map[string]*showState),
		videos: make(map[string]int),
		roots:  cfg.AllRoots(),
		imgHC: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        8,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     60 * time.Second,
			},
		},
	}
}

// Run 执行刮削。
func (r *Runner) Run(ctx context.Context) (ScrapeStats, error) {
	start := time.Now()
	items, err := r.db.ListForScrape(r.force, r.onlyID, r.limit)
	if err != nil {
		return r.stats, err
	}
	r.stats.Total = len(items)
	if len(items) == 0 {
		r.stats.Elapsed = time.Since(start)
		return r.stats, nil
	}
	if r.dry {
		logf("dry-run：以下 %d 条会被刮削（不调用 API、不写文件）", len(items))
	}

	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return r.stats, err
		}
		r.processItem(ctx, &it)
	}

	r.writeUnmatched()
	r.stats.Elapsed = time.Since(start)
	return r.stats, nil
}

// processItem 处理单条：先想尽办法确定目标 ID，再取详情、落盘、回填。
func (r *Runner) processItem(ctx context.Context, it *Item) {
	full := r.absPath(it)
	if full == "" {
		r.stats.Failed++
		warnf("找不到根目录对应的路径：%s", it.RelPath)
		return
	}
	dir := filepath.Dir(full)
	stem := strings.TrimSuffix(filepath.Base(full), filepath.Ext(full))

	// NFO 路径按类型取：剧集永远只认 <文件名>.nfo，
	// 避免把季目录里误留的 movie.nfo 当成自己的（那可能是上一层的电影）
	var nfoPath string
	if it.Kind == KindEpisode {
		nfoPath = filepath.Join(dir, stem+".nfo")
	} else {
		nfoPath = r.nfoPathFor(dir, stem)
	}
	prev := readNFO(nfoPath)

	// lockdata=true：用户手工接管，一律不碰（文档 8.5）
	if prev.Exists && prev.Locked {
		fmt.Printf("  [锁定] %s\n", it.RelPath)
		r.stats.Manual++
		r.setStatus(it.ID, "manual")
		return
	}

	if r.dry {
		target := ""
		if tmdbID, imdbID, src := r.declaredIDs(dir, stem, prev); tmdbID > 0 || imdbID != "" {
			target = fmt.Sprintf(" → 已指定 %d%s（来自 %s）", tmdbID, imdbID, src)
		}
		fmt.Printf("  [待刮] %-9s %s（%d）%s\n", it.Kind, it.Title, it.Year, target)
		r.stats.OK++
		return
	}

	if it.Kind == KindEpisode {
		r.processEpisode(ctx, it, full, dir, stem, prev)
		return
	}
	r.processMovie(ctx, it, full, dir, stem, prev)
}

// processMovie 处理电影。
func (r *Runner) processMovie(ctx context.Context, it *Item, full, dir, stem string, prev nfoInfo) {
	tmdbID, imdbID, _ := r.declaredIDs(dir, stem, prev)
	key := r.sc.NativeKey(tmdbID, imdbID)
	if key == "" && imdbID != "" {
		if v, err := r.resolveImdb(ctx, imdbID, KindMovie); err == nil {
			key = strconv.Itoa(v)
		} else {
			warnf("IMDb ID %s 反查失败：%v", imdbID, err)
		}
	}

	var meta *Meta
	var err error
	if key != "" {
		meta, err = r.sc.Detail(ctx, key, KindMovie)
	} else {
		meta, err = r.searchMovie(ctx, it)
	}
	if err != nil {
		r.fail(it, err)
		return
	}

	vcount := r.videoCount(dir)
	posterRel, fanartRel, images := r.saveMovieImages(ctx, it, dir, stem, vcount, meta)

	n := nfoPathFor(dir, stem, vcount)
	base := imageBaseName(dir, stem, vcount)
	nfo := &nfoMovie{
		Title:         firstNonEmpty(meta.Title, it.Title),
		OriginalTitle: meta.OriginalTitle,
		SortTitle:     sortTitle(meta.Title, meta.OriginalTitle),
		Year:          firstNonZero(meta.Year, it.Year),
		Plot:          meta.Overview,
		Tagline:       meta.Tagline,
		Runtime:       meta.RuntimeMin,
		Ratings:       buildRatings(meta.Rating, meta.Votes, meta.Provider),
		UniqueIDs:     buildUniqueIDs(meta.ImdbID, meta.TmdbID),
		Genres:        meta.Genres,
		Countries:     meta.Countries,
		Directors:     meta.Directors,
		Credits:       meta.Writers,
		Actors:        buildActorList(meta.Actors),
		Art:           artOf(base, posterRel != "", fanartRel != ""),
	}
	if err := writeNFO(n, nfo); err != nil {
		r.fail(it, err)
		return
	}

	it.Title = nfo.Title
	it.OriginalTitle = nfo.OriginalTitle
	it.Year = nfo.Year
	it.TmdbID = meta.TmdbID
	it.RuntimeMin = meta.RuntimeMin
	it.Rating = meta.Rating
	it.Votes = meta.Votes
	it.Overview = meta.Overview
	it.PosterPath = posterRel
	it.FanartPath = fanartRel
	it.NfoMtimeUnix = mtimeOf(n)
	if err := r.db.UpdateScraped(it); err != nil {
		r.fail(it, err)
		return
	}
	r.stats.OK++
	r.stats.Images += images
	fmt.Printf("  [成功] %-22s TMDB %-8d %s\n", truncate(nfo.Title, 22), meta.TmdbID, imageNote(images))
}

// processEpisode 处理单集：先确保剧集级别元数据，再补本集。
func (r *Runner) processEpisode(ctx context.Context, it *Item, full, dir, stem string, _ nfoInfo) {
	showDir := showDirOf(full)
	show := r.loadShow(ctx, it, showDir)
	if show.missReason != "" {
		r.miss(it, show.missReason)
		return
	}
	if show.err != nil {
		r.fail(it, show.err)
		return
	}

	season := it.Season
	if season <= 0 {
		season = 1
	}
	if err := r.ensureSeason(ctx, show, season); err != nil {
		r.fail(it, err)
		return
	}

	ep, ok := show.eps[epKey(season, it.Episode)]
	if !ok {
		// 集号可能解析错了（比如 0 或差一位），在**同一季**里找最近的一集
		ep, ok = fuzzyEpisode(show.eps, season, it.Episode)
	}
	if !ok {
		r.miss(it, fmt.Sprintf("第 %d 季第 %d 集在 TMDB 里不存在", season, it.Episode))
		return
	}

	// 单集截图
	thumbName := ""
	if ep.StillPath != "" {
		if url := r.stillURL(ep.StillPath); url != "" {
			name := stem + "-thumb.jpg"
			if err := r.download(ctx, url, filepath.Join(dir, name)); err == nil {
				thumbName = name
				r.stats.Images++
			} else {
				warnf("下载剧照失败：%s（%v）", it.RelPath, err)
			}
		}
	}

	n := filepath.Join(dir, stem+".nfo")
	nfo := &nfoEpisode{
		Title:     firstNonEmpty(ep.Title, fmt.Sprintf("第 %d 集", ep.Episode)),
		ShowTitle: show.meta.Title,
		Season:    ep.Season,
		Episode:   ep.Episode,
		Plot:      ep.Overview,
		Runtime:   ep.Runtime,
		AirDate:   ep.AirDate,
		Ratings:   buildRatings(ep.Rating, ep.Votes, "tmdb"),
		UniqueIDs: buildUniqueIDs("", 0), // 单集没有稳定的外部 ID，不写
		Art:       artOf("", false, false),
	}
	if thumbName != "" {
		nfo.Art = &nfoArt{Thumb: thumbName}
	}
	if err := writeNFO(n, nfo); err != nil {
		r.fail(it, err)
		return
	}

	it.Title = show.meta.Title
	it.OriginalTitle = show.meta.OriginalTitle
	it.Year = show.meta.Year
	it.TmdbID = show.meta.TmdbID
	it.RuntimeMin = ep.Runtime
	it.Rating = ep.Rating
	it.Votes = ep.Votes
	it.Overview = ep.Overview
	it.PosterPath = show.posterRel
	it.FanartPath = show.fanartRel
	it.NfoMtimeUnix = mtimeOf(n)
	if err := r.db.UpdateScraped(it); err != nil {
		r.fail(it, err)
		return
	}
	r.stats.OK++
	fmt.Printf("  [成功] %s S%02dE%02d  %s\n", truncate(show.meta.Title, 18), ep.Season, ep.Episode, truncate(nfo.Title, 20))
}

// loadShow 取（并缓存）一部剧的元数据，首次调用时落盘 tvshow.nfo 与海报。
func (r *Runner) loadShow(ctx context.Context, it *Item, showDir string) *showState {
	if st, ok := r.shows[showDir]; ok {
		return st
	}
	st := &showState{
		eps:     make(map[int]EpisodeMeta),
		seasons: make(map[int]bool),
	}

	// 已有 tvshow.nfo 且带外部 ID → 直接复用，省一次搜索
	tvNFO := filepath.Join(showDir, "tvshow.nfo")
	prev := readNFO(tvNFO)
	locked := prev.Exists && prev.Locked
	tmdbID, imdbID, _ := r.declaredIDs(showDir, filepath.Base(showDir), prev)
	key := r.sc.NativeKey(tmdbID, imdbID)
	if key == "" && imdbID != "" {
		if v, err := r.resolveImdb(ctx, imdbID, KindEpisode); err == nil {
			key = strconv.Itoa(v)
		}
	}
	if key == "" {
		cands, err := r.sc.Search(ctx, Query{Title: it.Title, Year: it.Year, Kind: KindEpisode})
		if err != nil {
			st.err = err
			r.shows[showDir] = st
			return st
		}
		m := matchCandidates(cands, Query{Title: it.Title, Year: it.Year, Kind: KindEpisode}, r.cfg.Scrape.TitleSimMin)
		if !m.OK {
			st.missReason = fmt.Sprintf("未找到匹配的剧集（标题 %q，年份 %d，候选 %d 个）", it.Title, it.Year, len(cands))
			r.shows[showDir] = st
			return st
		}
		key = m.Cand.Key
	}

	meta, err := r.sc.Detail(ctx, key, KindEpisode)
	if err != nil {
		st.err = err
		r.shows[showDir] = st
		return st
	}
	st.key = key
	st.meta = meta

	// 剧集级海报与背景图
	if meta.PosterPath != "" {
		url := r.posterURL(meta.PosterPath)
		if err := r.download(ctx, url, filepath.Join(showDir, "poster.jpg")); err == nil {
			st.posterRel = r.relTo(showDir, "poster.jpg")
			r.stats.Images++
		}
	}
	if meta.FanartPath != "" {
		url := r.fanartURL(meta.FanartPath)
		if err := r.download(ctx, url, filepath.Join(showDir, "fanart.jpg")); err == nil {
			st.fanartRel = r.relTo(showDir, "fanart.jpg")
			r.stats.Images++
		}
	}

	tv := &nfoTVShow{
		Title:         meta.Title,
		OriginalTitle: meta.OriginalTitle,
		SortTitle:     sortTitle(meta.Title, meta.OriginalTitle),
		Year:          meta.Year,
		Plot:          meta.Overview,
		Tagline:       meta.Tagline,
		Runtime:       meta.RuntimeMin,
		Ratings:       buildRatings(meta.Rating, meta.Votes, meta.Provider),
		UniqueIDs:     buildUniqueIDs(meta.ImdbID, meta.TmdbID),
		Genres:        meta.Genres,
		Countries:     meta.Countries,
		Directors:     meta.Directors,
		Credits:       meta.Writers,
		Actors:        buildActorList(meta.Actors),
		Season:        -1,
		Episode:       -1,
		Art:           artOf("", st.posterRel != "", st.fanartRel != ""),
	}
	// lockdata=true 的 tvshow.nfo 属于用户，不覆盖
	if !locked {
		if err := writeNFO(tvNFO, tv); err != nil {
			warnf("写 tvshow.nfo 失败：%s（%v）", showDir, err)
		}
	}

	r.shows[showDir] = st
	return st
}

// ensureSeason 懒加载某一季的分集表。多季的集号相同，因此必须按键区分。
func (r *Runner) ensureSeason(ctx context.Context, st *showState, season int) error {
	if st.seasons[season] {
		return nil
	}
	eps, err := r.sc.Season(ctx, st.key, season)
	if err != nil {
		return err
	}
	for _, e := range eps {
		st.eps[epKey(season, e.Episode)] = e
	}
	st.seasons[season] = true
	return nil
}

// searchMovie 搜索并匹配电影，必要时用别名救回。
func (r *Runner) searchMovie(ctx context.Context, it *Item) (*Meta, error) {
	q := Query{Title: it.Title, Year: it.Year, Kind: KindMovie}
	cands, err := r.sc.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	m := matchCandidates(cands, q, r.cfg.Scrape.TitleSimMin)
	if !m.OK {
		// 救回路径：对前三个候选查别名，港台译名常常只出现在这里
		for _, c := range topN(cands, 3) {
			alts, err := r.altTitles(ctx, c.Key)
			if err != nil || len(alts) == 0 {
				continue
			}
			sim := bestSimilarity(it.Title, alts...)
			if sim >= r.cfg.Scrape.TitleSimMin {
				logf("  别名命中：%q ≈ %q（%.2f）", it.Title, alts[0], sim)
				return r.sc.Detail(ctx, c.Key, KindMovie)
			}
		}
		r.miss(it, fmt.Sprintf("未找到匹配（候选 %d 个，最高相似度 %.2f）", len(cands), m.Sim))
		return nil, errMiss
	}
	return r.sc.Detail(ctx, m.Cand.Key, KindMovie)
}

// errMiss 用作内部信号：已经记过 miss，不必再记一次失败。
var errMiss = fmt.Errorf("未匹配")

// resolveImdb 用 IMDb ID 反查数据源自己的 ID。数据源不支持时返回 0。
func (r *Runner) resolveImdb(ctx context.Context, imdbID string, kind Kind) (int, error) {
	type imdbFinder interface {
		FindByImdb(ctx context.Context, imdbID string, kind Kind) (int, error)
	}
	if f, ok := r.sc.(imdbFinder); ok {
		return f.FindByImdb(ctx, imdbID, kind)
	}
	return 0, fmt.Errorf("当前数据源不支持按 IMDb ID 反查")
}

// altTitles 查别名，仅当数据源支持时可用。
func (r *Runner) altTitles(ctx context.Context, key string) ([]string, error) {
	type altProvider interface {
		AltTitles(ctx context.Context, key string, kind Kind) ([]string, error)
	}
	if p, ok := r.sc.(altProvider); ok {
		return p.AltTitles(ctx, key, KindMovie)
	}
	return nil, nil
}

// ---------- 落盘 ----------

// saveMovieImages 下载海报与背景图，返回相对媒体根的路径。
// 已存在且非 force 时跳过下载，但依然返回路径——文件在就说明能用。
func (r *Runner) saveMovieImages(ctx context.Context, it *Item, dir, stem string, vcount int, meta *Meta) (posterRel, fanartRel string, count int) {
	base := imageBaseName(dir, stem, vcount)

	if meta.PosterPath != "" {
		name := base + "poster.jpg"
		dest := filepath.Join(dir, name)
		if r.fileUsable(dest) {
			posterRel = r.relTo(dir, name)
		} else if err := r.download(ctx, r.posterURL(meta.PosterPath), dest); err == nil {
			posterRel = r.relTo(dir, name)
			count++
		} else {
			warnf("下载海报失败：%s（%v）", it.RelPath, err)
		}
	}
	if meta.FanartPath != "" {
		name := base + "fanart.jpg"
		dest := filepath.Join(dir, name)
		if r.fileUsable(dest) {
			fanartRel = r.relTo(dir, name)
		} else if err := r.download(ctx, r.fanartURL(meta.FanartPath), dest); err == nil {
			fanartRel = r.relTo(dir, name)
			count++
		} else {
			warnf("下载背景图失败：%s（%v）", it.RelPath, err)
		}
	}
	return posterRel, fanartRel, count
}

// download 下载到 dest。已存在且非 force 时直接返回；先写 .part 再 rename。
func (r *Runner) download(ctx context.Context, url, dest string) error {
	if url == "" {
		return fmt.Errorf("空 URL")
	}
	if r.fileUsable(dest) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "LiteMS/"+version)
	resp, err := r.imgHC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, 20<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if n < 512 {
		os.Remove(tmp)
		return fmt.Errorf("图片过小（%d 字节），可能是错误页", n)
	}
	return os.Rename(tmp, dest)
}

// fileUsable 判断文件是否已存在且看起来是张图。
func (r *Runner) fileUsable(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() || fi.Size() < 512 {
		return false
	}
	return !r.force
}

// ---------- 路径与 ID ----------

// absPath 把条目还原成绝对路径。
func (r *Runner) absPath(it *Item) string {
	if it.RootIdx < 0 || it.RootIdx >= len(r.roots) {
		return ""
	}
	return filepath.Join(r.roots[it.RootIdx], filepath.FromSlash(it.RelPath))
}

// relTo 计算相对媒体根的路径（入库用，文档 5.1）。
func (r *Runner) relTo(dir, name string) string {
	for _, root := range r.roots {
		if rel, err := filepath.Rel(root, filepath.Join(dir, name)); err == nil && !strings.HasPrefix(rel, "..") {
			return normalizeRel(rel)
		}
	}
	return ""
}

// nfoPathFor 找该条目已存在的 NFO：先看 <stem>.nfo，再看 movie.nfo。
func (r *Runner) nfoPathFor(dir, stem string) string {
	stemNFO := filepath.Join(dir, stem+".nfo")
	if _, err := os.Stat(stemNFO); err == nil {
		return stemNFO
	}
	return nfoPathFor(dir, stem, r.videoCount(dir))
}

// videoCount 数目录里有几个视频文件，用于决定 NFO 与图片命名。
func (r *Runner) videoCount(dir string) int {
	if n, ok := r.videos[dir]; ok {
		return n
	}
	entries, err := os.ReadDir(dir)
	n := 0
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			if IsExtIn(strings.ToLower(filepath.Ext(e.Name())), r.cfg.Library.VideoExt) {
				n++
			}
		}
	}
	r.videos[dir] = n
	return n
}

// declaredIDs 从三处找现成的目标 ID：override.json > 文件名内嵌 ID > NFO 里的 uniqueid。
// 顺序即优先级：越"人工"的越靠前。
func (r *Runner) declaredIDs(dir, stem string, prev nfoInfo) (tmdbID int, imdbID string, source string) {
	if ov := readOverride(dir); ov != nil {
		if ov.TmdbID > 0 {
			return ov.TmdbID, ov.ImdbID, "override.json"
		}
		if ov.ImdbID != "" {
			// 只有 tt 号时交给调用方用 find 接口反查；这里把 id 置零但带上 imdb 线索
			return 0, ov.ImdbID, "override.json"
		}
	}
	if id := embeddedTmdbID(stem); id > 0 {
		return id, "", "文件名"
	}
	if id := embeddedTmdbID(filepath.Base(dir)); id > 0 {
		return id, "", "目录名"
	}
	if prev.Exists && prev.TmdbID > 0 {
		return prev.TmdbID, prev.ImdbID, "已有 NFO"
	}
	if prev.Exists && prev.ImdbID != "" {
		return 0, prev.ImdbID, "已有 NFO"
	}
	return 0, "", ""
}

// overrideJSON 是同目录下的人工指定文件（文档 8.4）。
type overrideJSON struct {
	TmdbID int    `json:"tmdb_id"`
	ImdbID string `json:"imdb_id"`
	Kind   string `json:"kind"`
}

func readOverride(dir string) *overrideJSON {
	b, err := os.ReadFile(filepath.Join(dir, "override.json"))
	if err != nil {
		return nil
	}
	var ov overrideJSON
	if err := json.Unmarshal(b, &ov); err != nil {
		warnf("override.json 格式错误，忽略：%s", filepath.Join(dir, "override.json"))
		return nil
	}
	if ov.TmdbID == 0 && ov.ImdbID == "" {
		return nil
	}
	return &ov
}

// reEmbeddedTmdb 匹配 Kodi v21 的文件名标识符与常见的方括号写法：
//
//	{TMDB-123}  [tmdbid=123]  tmdb/123
var reEmbeddedTmdb = regexp.MustCompile(`(?i)(?:tmdb(?:id)?)[\s=_:\-/]?\[?\{?(\d{2,8})`)

func embeddedTmdbID(s string) int {
	m := reEmbeddedTmdb.FindStringSubmatch(s)
	if len(m) < 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// ---------- 收尾 ----------

// fail 记一次失败。errMiss 不重复计数（searchMovie 已记 miss）。
func (r *Runner) fail(it *Item, err error) {
	if err == errMiss {
		return
	}
	if err == ErrNotFound {
		r.miss(it, "数据源里没有这条记录（404）")
		return
	}
	r.stats.Failed++
	fmt.Printf("  [失败] %s：%v\n", it.RelPath, err)
	r.setStatus(it.ID, "error")
}

// miss 记一次未匹配，并写入 unmatched.txt。
func (r *Runner) miss(it *Item, reason string) {
	r.stats.Miss++
	fmt.Printf("  [未匹配] %s：%s\n", it.RelPath, reason)
	r.misses = append(r.misses, missEntry{RootIdx: it.RootIdx, Rel: it.RelPath, Reason: reason})
	r.setStatus(it.ID, "miss")
}

// setStatus 只改状态，不动 scraped_at（miss/manual 也算"处理过"，不再反复重试）。
func (r *Runner) setStatus(id, status string) {
	if r.dry {
		return
	}
	if err := r.db.SetScrapeStatus(id, status, nowUnix()); err != nil {
		warnf("更新状态失败：%s（%v）", id, err)
	}
}

// writeUnmatched 把本次未匹配清单落到 <根>/.litems/unmatched.txt。
// 每次覆盖写：这是"当前仍未匹配"的快照，比无限追加更有用。
func (r *Runner) writeUnmatched() {
	if r.dry {
		return
	}
	byRoot := map[int][]missEntry{}
	for _, m := range r.misses {
		byRoot[m.RootIdx] = append(byRoot[m.RootIdx], m)
	}
	for idx, list := range byRoot {
		if idx < 0 || idx >= len(r.roots) {
			continue
		}
		dir := filepath.Join(r.roots[idx], ".litems")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			warnf("创建 %s 失败：%v", dir, err)
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Rel < list[j].Rel })
		var b strings.Builder
		b.WriteString("# litems 未匹配清单（每次扫描覆盖写）\n")
		b.WriteString("# 修正方式：在同目录放 override.json，内容 {\"tmdb_id\": 12345}\n\n")
		for _, m := range list {
			fmt.Fprintf(&b, "%-70s  %s\n", m.Rel, m.Reason)
		}
		if err := os.WriteFile(filepath.Join(dir, "unmatched.txt"), []byte(b.String()), 0o644); err != nil {
			warnf("写 unmatched.txt 失败：%v", err)
		}
	}
}

// ---------- 小工具 ----------

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// sortTitle 只在原名与译名不同时写，避免 NFO 里塞无用字段。
func sortTitle(title, original string) string {
	if original != "" && !strings.EqualFold(strings.TrimSpace(title), strings.TrimSpace(original)) {
		return title
	}
	return ""
}

// artOf 按实际存在的文件生成 `<art>`，缺失的不写空标签。
func artOf(base string, hasPoster, hasFanart bool) *nfoArt {
	if !hasPoster && !hasFanart {
		return nil
	}
	a := &nfoArt{}
	if hasPoster {
		a.Poster = base + "poster.jpg"
	}
	if hasFanart {
		a.Fanart = base + "fanart.jpg"
	}
	return a
}

func imageNote(n int) string {
	if n == 0 {
		return "（图片已存在）"
	}
	return fmt.Sprintf("+%d 图", n)
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}

func mtimeOf(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime().Unix()
	}
	return 0
}

// showDirOf 找出剧集根目录：若父目录是季目录，则再上一层。
func showDirOf(videoPath string) string {
	dir := filepath.Dir(videoPath)
	if ParseSeasonDir(filepath.Base(dir)) > 0 {
		return filepath.Dir(dir)
	}
	return dir
}

// fuzzyEpisode 在集号对不上时，在同一季内找最近的一集（文件名解析偶有偏差）。
func fuzzyEpisode(eps map[int]EpisodeMeta, season, want int) (EpisodeMeta, bool) {
	best := -1
	var found EpisodeMeta
	for k, e := range eps {
		if e.Season != season || k/10000 != season {
			continue
		}
		d := e.Episode - want
		if d < 0 {
			d = -d
		}
		if best < 0 || d < best {
			best, found = d, e
		}
	}
	if best >= 0 && best <= 1 {
		return found, true
	}
	return EpisodeMeta{}, false
}

// topN 取前 n 个候选，顺序保持数据源给的排序（通常按热度）。
func topN(cands []Candidate, n int) []Candidate {
	if len(cands) <= n {
		return cands
	}
	return cands[:n]
}

// ---------- 图片 URL ----------

type imageURLProvider interface {
	ImageURL(path, size string) string
}

func (r *Runner) posterURL(path string) string {
	if p, ok := r.sc.(imageURLProvider); ok {
		return p.ImageURL(path, r.cfg.Scrape.PosterSize)
	}
	return ""
}

func (r *Runner) fanartURL(path string) string {
	if p, ok := r.sc.(imageURLProvider); ok {
		return p.ImageURL(path, r.cfg.Scrape.FanartSize)
	}
	return ""
}

func (r *Runner) stillURL(path string) string {
	if p, ok := r.sc.(imageURLProvider); ok {
		return p.ImageURL(path, "w300")
	}
	return ""
}
