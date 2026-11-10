package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ScanStats 是单次扫描的统计。
type ScanStats struct {
	Roots         int
	Found         int
	Need          int
	Skipped       int
	Updated       int
	Added         int
	Failed        int
	Deleted       int
	PendingScrape int
	Elapsed       time.Duration
}

// Scanner 负责遍历媒体库并维护索引。
type Scanner struct {
	cfg         *Config
	db          *DB
	dry         bool
	force       bool
	onlyMissing bool
	onlyID      string

	stats    ScanStats
	dirCache map[string]dirMeta
	seen     map[string]bool // "<rootIdx>:<relPath>"
}

// dirMeta 缓存目录级别的判定信息，避免同一目录反复 stat。
type dirMeta struct {
	season      int  // 目录名是季目录时给出季号
	hasTVNFO    bool // 含 tvshow.nfo
	hasSeason   bool // 含季子目录
	entries     []fs.DirEntry
	initialized bool
}

// NewScanner 构造扫描器。
func NewScanner(cfg *Config, db *DB, dry, force, onlyMissing bool, onlyID string) *Scanner {
	return &Scanner{
		cfg: cfg, db: db,
		dry: dry, force: force, onlyMissing: onlyMissing, onlyID: onlyID,
		dirCache: make(map[string]dirMeta),
		seen:     make(map[string]bool),
	}
}

// Run 执行扫描。
func (s *Scanner) Run() (ScanStats, error) {
	start := time.Now()
	roots := s.cfg.AllRoots()
	s.stats.Roots = len(roots)

	if err := s.db.SyncRoots(roots); err != nil {
		return s.stats, err
	}
	existing, err := s.db.Existing()
	if err != nil {
		return s.stats, err
	}

	for idx, root := range roots {
		root = filepath.Clean(root)
		fi, err := os.Stat(root)
		if err != nil {
			warnf("媒体根不可访问，跳过：%s（%v）", root, err)
			continue
		}
		if !fi.IsDir() {
			warnf("媒体根不是目录，跳过：%s", root)
			continue
		}
		fmt.Printf("扫描 %s\n", root)
		if err := s.walkRoot(idx, root, existing); err != nil {
			warnf("遍历 %s 出错：%v", root, err)
		}
		// 清理该根下已消失的条目
		if err := s.pruneRoot(idx, root); err != nil {
			warnf("清理 %s 失败：%v", root, err)
		}
	}

	s.stats.Elapsed = time.Since(start)
	_ = s.db.SetKV("last_scan", fmt.Sprint(time.Now().Unix()))
	return s.stats, nil
}

func (s *Scanner) walkRoot(idx int, root string, existing map[string]Snapshot) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			warnf("访问失败，跳过：%s（%v）", path, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != root && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if !s.cfg.Library.FollowFileSymlinks {
				return nil // 文档 6.5：默认不跟随符号链接
			}
			// 只跟随指向普通文件的链接。目录链接一律跳过：
			// 跟随目录可能成环，而库里的目录本来就该是实体目录。
			fi, statErr := os.Stat(path)
			if statErr != nil || fi.IsDir() || !fi.Mode().IsRegular() {
				return nil
			}
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !IsExtIn(ext, s.cfg.Library.VideoExt) {
			return nil
		}
		return s.handleVideo(idx, root, path, d, existing)
	})
}

func (s *Scanner) handleVideo(rootIdx int, root, path string, d fs.DirEntry, existing map[string]Snapshot) error {
	s.stats.Found++

	rel, err := filepath.Rel(root, path)
	if err != nil {
		warnf("无法计算相对路径：%s", path)
		return nil
	}
	rel = normalizeRel(rel)
	key := fmt.Sprintf("%d:%s", rootIdx, rel)
	s.seen[key] = true

	// 用 os.Stat 而不是 d.Info()：d.Info() 对符号链接返回链接自身的信息
	// （大小等于路径长度、mtime 是建链接的时间），增量判定会全部算错。
	info, err := os.Stat(path)
	if err != nil {
		warnf("stat 失败：%s（%v）", path, err)
		s.stats.Failed++
		return nil
	}

	meta := s.resolveKind(path, d.Name())
	nfoMtime := s.nfoMtime(filepath.Dir(path), d.Name(), meta.kind)
	subs := s.findSubtitles(filepath.Dir(path), d.Name())
	posterRel, fanartRel := s.findImages(root, path, meta.kind)

	it := &Item{
		ID:            ItemID(root, rel),
		RootIdx:       rootIdx,
		RelPath:       rel,
		Kind:          meta.kind,
		Title:         meta.title,
		OriginalTitle: meta.originalTitle,
		Year:          meta.year,
		Season:        meta.season,
		Episode:       meta.episode,
		SubPaths:      subs,
		PosterPath:    posterRel,
		FanartPath:    fanartRel,
		Container:     strings.TrimPrefix(strings.ToLower(filepath.Ext(d.Name())), "."),
		SizeBytes:     info.Size(),
		MtimeUnix:     info.ModTime().Unix(),
		NfoMtimeUnix:  nfoMtime,
		ScrapedAt:     0,
		ScrapeStatus:  "pending",
	}

	// 增量判定：索引层面只看文件属性；缺元数据是刮削阶段的事
	prev, exists := existing[key]
	if exists {
		it.ScrapedAt = prev.ScrapedAt
		it.ScrapeStatus = prev.ScrapeStatus
		if !s.force {
			if s.onlyMissing {
				if prev.ScrapedAt != 0 && prev.ScrapeStatus != "error" {
					s.stats.Skipped++
					return nil
				}
			} else if !NeedsReindex(prev, info.Size(), info.ModTime().Unix(), nfoMtime) {
				s.stats.Skipped++
				if NeedsScrape(prev, info.Size(), info.ModTime().Unix(), nfoMtime) {
					s.stats.PendingScrape++
				}
				return nil
			}
		}
	}

	if s.onlyID != "" && it.ID != s.onlyID {
		s.stats.Skipped++
		return nil
	}

	s.stats.Need++
	if s.dry {
		tag := "新增"
		if exists {
			tag = "更新"
		}
		fmt.Printf("  [%s] %-12s %s  标题=%q 年=%d S%dE%d 字幕=%d\n",
			tag, it.Kind, rel, it.Title, it.Year, it.Season, it.Episode, len(subs))
		if exists {
			s.stats.Updated++
		} else {
			s.stats.Added++
		}
		return nil
	}

	if err := s.db.UpsertItem(it); err != nil {
		warnf("写入失败：%s（%v）", rel, err)
		s.stats.Failed++
		return nil
	}
	if exists {
		s.stats.Updated++
	} else {
		s.stats.Added++
	}
	return nil
}

// kindMeta 是归属判定的结果。
type kindMeta struct {
	kind          Kind
	title         string
	originalTitle string
	year          int
	season        int
	episode       int
}

// resolveKind 按文档 6.3 的判定链决定 movie / episode，并回溯剧集目录。
func (s *Scanner) resolveKind(path, base string) kindMeta {
	p := ParseName(base)
	dir := filepath.Dir(path)

	m := kindMeta{
		title:   p.Title,
		year:    p.Year,
		season:  p.Season,
		episode: p.Episode,
		kind:    KindMovie,
	}

	// 1. 文件名里有 SxxExx
	if p.Episode >= 0 {
		m.kind = KindEpisode
		showDir := dir
		// 父目录是季目录 → 剧集目录在上一层
		if sn := ParseSeasonDir(filepath.Base(dir)); sn > 0 {
			if m.season == 0 {
				m.season = sn
			}
			showDir = filepath.Dir(dir)
		}
		if st, y, ok := ParseTitleYear(filepath.Base(showDir)); ok {
			m.title, m.year = st, y
		} else if q := ParseName(filepath.Base(showDir)); q.Title != "" {
			m.title = q.Title
			if m.year == 0 {
				m.year = q.Year
			}
		}
		if m.season == 0 {
			m.season = 1
		}
		return m
	}

	// 2. 父目录是季目录 → 按分集处理（集号缺失时置 0）
	if sn := ParseSeasonDir(filepath.Base(dir)); sn > 0 {
		m.kind = KindEpisode
		m.season = sn
		m.episode = 0
		showDir := filepath.Dir(dir)
		if st, y, ok := ParseTitleYear(filepath.Base(showDir)); ok {
			m.title, m.year = st, y
		} else if q := ParseName(filepath.Base(showDir)); q.Title != "" {
			m.title, m.year = q.Title, q.Year
		}
		return m
	}

	// 3/4. 父目录有 tvshow.nfo 或含季子目录 → 剧集
	dm := s.dirMeta(dir)
	if dm.hasTVNFO || dm.hasSeason {
		m.kind = KindEpisode
		if m.season == 0 {
			m.season = 1
		}
		if m.episode < 0 {
			m.episode = 0
		}
		if st, y, ok := ParseTitleYear(filepath.Base(dir)); ok {
			m.title, m.year = st, y
		} else if q := ParseName(filepath.Base(dir)); q.Title != "" {
			m.title, m.year = q.Title, q.Year
		}
		return m
	}

	// 5. 电影
	if p.Year == 0 {
		if st, y, ok := ParseTitleYear(filepath.Base(dir)); ok {
			if LevenshteinRatio(st, p.Title) > 0.4 || p.Title == "" {
				m.title, m.year = st, y
			}
		}
	}
	return m
}

// dirMeta 读取并缓存目录信息。
func (s *Scanner) dirMeta(dir string) dirMeta {
	if dm, ok := s.dirCache[dir]; ok {
		return dm
	}
	dm := dirMeta{season: ParseSeasonDir(filepath.Base(dir))}
	entries, err := os.ReadDir(dir)
	if err == nil {
		dm.entries = entries
		dm.initialized = true
		for _, e := range entries {
			name := strings.ToLower(e.Name())
			if e.IsDir() && ParseSeasonDir(e.Name()) > 0 {
				dm.hasSeason = true
			}
			if name == "tvshow.nfo" {
				dm.hasTVNFO = true
			}
		}
	}
	s.dirCache[dir] = dm
	return dm
}

// nfoMtime 返回与该条目相关的 NFO 的最新修改时间（0 表示没有）。
func (s *Scanner) nfoMtime(dir, videoName string, kind Kind) int64 {
	dm := s.dirMeta(dir)
	var latest int64
	mark := func(name string) {
		for _, e := range dm.entries {
			if strings.EqualFold(e.Name(), name) {
				if fi, err := e.Info(); err == nil && fi.ModTime().Unix() > latest {
					latest = fi.ModTime().Unix()
				}
				return
			}
		}
	}
	stem := strings.TrimSuffix(videoName, filepath.Ext(videoName))
	mark(stem + ".nfo")
	// 多视频共享目录里的 movie.nfo 是别的条目的（写入侧 nfoPathFor 在
	// 多视频时用 <stem>.nfo），认了就会把别人的 NFO 修改当成自己的刮削基准。
	if kind == KindMovie && s.videoCountIn(dir) <= 1 {
		mark("movie.nfo")
	} else if kind != KindMovie {
		mark("tvshow.nfo")
	}
	return latest
}

// videoCountIn 数一个目录里的视频文件数（与 Runner.videoCount 同一套扩展名）。
// 目录信息拿不到时按 1 处理，维持「每片一目录」的旧行为。
func (s *Scanner) videoCountIn(dir string) int {
	dm := s.dirMeta(dir)
	if !dm.initialized {
		return 1
	}
	n := 0
	for _, e := range dm.entries {
		if e.IsDir() {
			continue
		}
		if IsExtIn(strings.ToLower(filepath.Ext(e.Name())), s.cfg.Library.VideoExt) {
			n++
		}
	}
	return n
}

// findImages 找出磁盘上已有的海报/背景图（相对媒体根）。
// 刮削时会写这两个文件，但删库重建后必须能从文件本身重新发现，
// 否则「NFO 与图片是唯一真相」不成立——重建后海报会凭空消失。
// 剧集再往上找一层：图片挂在剧集目录，而不是 Season 目录。
//
// 命名约定与写入侧（imageBaseName / nfoPathFor）对称：
//
//	目录里只有一个视频 → poster.jpg / fanart.jpg（"每片一目录"布局）
//	目录里有多个视频   → <stem>-poster.jpg / <stem>-fanart.jpg
//
// 多视频共享目录（qbt 下载目录里堆一堆单文件）绝不能认领通用名的
// poster.jpg——那是别人的海报，认了就会出现「A 片用 B 片的海报」。
func (s *Scanner) findImages(root, videoPath string, kind Kind) (poster, fanart string) {
	dirs := []string{filepath.Dir(videoPath)}
	if kind == KindEpisode {
		dirs = append(dirs, filepath.Dir(dirs[0]))
	}
	stem := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))
	for _, d := range dirs {
		shared := s.videoCountIn(d) > 1
		if poster == "" {
			if !shared {
				if rel, ok := relIfExists(root, filepath.Join(d, "poster.jpg")); ok {
					poster = rel
				}
			}
			if poster == "" {
				if rel, ok := relIfExists(root, filepath.Join(d, stem+"-poster.jpg")); ok {
					poster = rel
				}
			}
		}
		if fanart == "" {
			if !shared {
				if rel, ok := relIfExists(root, filepath.Join(d, "fanart.jpg")); ok {
					fanart = rel
				}
			}
			if fanart == "" {
				if rel, ok := relIfExists(root, filepath.Join(d, stem+"-fanart.jpg")); ok {
					fanart = rel
				}
			}
		}
		if poster != "" && fanart != "" {
			break
		}
	}
	return poster, fanart
}

// relIfExists 在文件存在时返回它相对媒体根的路径。
func relIfExists(root, path string) (string, bool) {
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	return normalizeRel(rel), true
}

// findSubtitles 找出与视频同名的外挂字幕（同目录、前缀匹配）。
func (s *Scanner) findSubtitles(dir, videoName string) []SubEntry {
	dm := s.dirMeta(dir)
	if !dm.initialized {
		return nil
	}
	stem := strings.TrimSuffix(videoName, filepath.Ext(videoName))
	lowerStem := strings.ToLower(stem)
	out := make([]SubEntry, 0, 3)
	for _, e := range dm.entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if !IsExtIn(ext, s.cfg.Library.SubExt) {
			continue
		}
		subStem := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if !strings.HasPrefix(strings.ToLower(subStem), lowerStem) {
			continue
		}
		lang := NormalizeLang(SubtitleLang(e.Name()))
		out = append(out, SubEntry{Lang: lang, Path: normalizeRel(e.Name())})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return langRank(out[i].Lang, s.cfg.Scrape.SubLangs) < langRank(out[j].Lang, s.cfg.Scrape.SubLangs)
	})
	return out
}

// langRank 按 sub_langs 的顺序给出优先级，未列出的排最后。
func langRank(lang string, prefer []string) int {
	for i, p := range prefer {
		if NormalizeLang(p) == lang {
			return i
		}
	}
	if lang == "" {
		return len(prefer) + 1
	}
	return len(prefer)
}

// pruneRoot 删除该根下已不存在的条目。
func (s *Scanner) pruneRoot(idx int, root string) error {
	known, err := s.db.ListIDsByRoot(idx)
	if err != nil {
		return err
	}
	for rel, id := range known {
		if s.seen[fmt.Sprintf("%d:%s", idx, rel)] {
			continue
		}
		// 双保险：磁盘上真的没了才删
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
			continue
		}
		if s.dry {
			fmt.Printf("  [删除] %s\n", rel)
			s.stats.Deleted++
			continue
		}
		if err := s.db.DeleteItem(id); err != nil {
			warnf("删除失败：%s（%v）", rel, err)
			continue
		}
		s.stats.Deleted++
	}
	return nil
}

// PrintSummary 按文档 12.1 的格式输出摘要。
func (s *ScanStats) PrintSummary(dry bool) {
	suffix := ""
	if dry {
		suffix = "（dry-run，未写入）"
	}
	fmt.Println()
	fmt.Printf("  发现视频文件      %d\n", s.Found)
	fmt.Printf("  索引需要更新      %d\n", s.Need)
	fmt.Printf("  ├─ 新增          %d\n", s.Added)
	fmt.Printf("  ├─ 更新          %d\n", s.Updated)
	fmt.Printf("  ├─ 跳过          %d\n", s.Skipped)
	fmt.Printf("  └─ 失败          %d\n", s.Failed)
	if s.PendingScrape > 0 {
		fmt.Printf("  待刮削            %d（缺元数据，执行 scrape 阶段处理）\n", s.PendingScrape)
	}
	if s.Deleted > 0 {
		fmt.Printf("  清理已消失        %d\n", s.Deleted)
	}
	fmt.Printf("  耗时             %s%s\n", s.Elapsed.Round(time.Millisecond), suffix)
}
