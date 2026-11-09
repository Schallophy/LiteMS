package main

// 详情页的补充信息装配：从条目旁边的 NFO 直读完整元数据。
//
// 为什么不把这些字段塞进数据库：Item 表存的是索引缓存的公共部分，
// 类型/导演/演员这类「展示细节」只有详情页用，而 NFO 本来就是唯一真相
// （文档 7 章）——用户手工改过 NFO 后页面立即跟着变，不需要重扫。
// 一次详情页渲染只读 1-2 个几 KB 的 XML，代价可以忽略。

import (
	"path/filepath"
	"strconv"
	"strings"
)

// DetailInfo 是详情页模板用到的 NFO 补充字段。
type DetailInfo struct {
	Tagline   string
	Genres    []string
	Countries []string
	Directors []string
	Writers   []string
	Cast      []castMember
	AirDate   string // 剧集单集的播出日期
	RatingSrc string // 评分来源（tmdb），展示「TMDB 8.4 · 1610 人评过」用
	Votes     int
	TmdbID    int
	ImdbID    string
	HasTmdb   bool
	HasImdb   bool
}

// castMember 是主演列表的一项。
type castMember struct {
	Name string
	Role string
}

const castLimit = 12 // NFO 里最多存 15 位，页面展示前 12 个

// detailFor 装配条目的 NFO 补充信息。单集会合并剧集根目录 tvshow.nfo 里的
// 字段（类型/国家/演员在剧集层，单集层只有导演/编剧/播出日期）。
// 没有任何 NFO 时返回 nil，模板据此隐藏整个区块。
func detailFor(roots []string, it Item) *DetailInfo {
	if it.RootIdx < 0 || it.RootIdx >= len(roots) {
		return nil
	}
	root := roots[it.RootIdx]
	rel := filepath.FromSlash(it.RelPath)
	dir := filepath.Join(root, filepath.Dir(rel))
	stem := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))

	var d *nfoDetailXML
	// 条目自己的 NFO：单片目录是 movie.nfo，多片目录/单集是 <文件名>.nfo
	for _, name := range []string{"movie.nfo", stem + ".nfo"} {
		if x := readNFODetail(filepath.Join(dir, name)); x != nil {
			d = x
			break
		}
	}
	if d == nil {
		return nil
	}

	info := &DetailInfo{
		Tagline:   strings.TrimSpace(d.Tagline),
		Genres:    trimAll(d.Genres),
		Countries: trimAll(d.Countries),
		Directors: trimAll(d.Directors),
		Writers:   trimAll(d.Credits),
		AirDate:   strings.TrimSpace(d.AirDate),
	}

	// 单集：沿目录向上找 tvshow.nfo，剧集层的字段补进来
	if it.Kind == KindEpisode {
		for up := dir; ; up = filepath.Dir(up) {
			if show := readNFODetail(filepath.Join(up, "tvshow.nfo")); show != nil {
				info.Genres = firstNonEmptyList(info.Genres, trimAll(show.Genres))
				info.Countries = firstNonEmptyList(info.Countries, trimAll(show.Countries))
				if len(info.Directors) == 0 {
					info.Directors = trimAll(show.Directors)
				}
				if len(info.Writers) == 0 {
					info.Writers = trimAll(show.Credits)
				}
				if len(info.Cast) == 0 {
					info.Cast = castOf(show.Actors)
				}
				if info.Tagline == "" {
					info.Tagline = strings.TrimSpace(show.Tagline)
				}
				mergeIDs(info, show.UniqueIDs)
				break // 找到剧集根就停，避免一路撞到库根
			}
			if !strings.HasPrefix(up, root) || len(up) <= len(root) {
				break // 到库根了还没有，说明没有 tvshow.nfo
			}
		}
	}

	if len(info.Cast) == 0 {
		info.Cast = castOf(d.Actors)
	}
	mergeIDs(info, d.UniqueIDs)
	if d.Ratings != nil && len(d.Ratings.Rating) > 0 {
		rt := d.Ratings.Rating[0]
		info.RatingSrc = strings.ToUpper(rt.Name)
		info.Votes = rt.Votes
	}

	if len(info.Genres) == 0 && len(info.Countries) == 0 && len(info.Directors) == 0 &&
		len(info.Writers) == 0 && len(info.Cast) == 0 && info.TmdbID == 0 && info.ImdbID == "" &&
		info.Tagline == "" && info.AirDate == "" {
		return nil // 空的 NFO 不值得渲染一整个区块
	}
	return info
}

// castOf 截断演员表。
func castOf(actors []nfoActor) []castMember {
	out := make([]castMember, 0, castLimit)
	for i, a := range actors {
		if i >= castLimit {
			break
		}
		out = append(out, castMember{Name: strings.TrimSpace(a.Name), Role: strings.TrimSpace(a.Role)})
	}
	return out
}

// mergeIDs 从 uniqueid 列表取出 TMDB / IMDB 的 ID。
func mergeIDs(info *DetailInfo, ids []nfoUniqueID) {
	for _, u := range ids {
		switch strings.ToLower(strings.TrimSpace(u.Type)) {
		case "tmdb":
			info.TmdbID = parseIntSafe(u.Value)
			info.HasTmdb = info.TmdbID > 0
		case "imdb":
			info.ImdbID = strings.TrimSpace(u.Value)
			info.HasImdb = info.ImdbID != ""
		}
	}
}

func trimAll(list []string) []string {
	var out []string
	for _, v := range list {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out // 不排序：类型/国家的顺序来自 TMDB，重要的在前，保持原样
}

func firstNonEmptyList(a, b []string) []string {
	if len(a) > 0 {
		return a
	}
	return b
}

func parseIntSafe(v string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	return n
}
