package main

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 本文件负责 NFO 的读写。格式遵循 Kodi 的 schema（文档第 7 章）：
// 图片一律写相对文件名，`<art>` 用 Kodi 18+ 的写法，`<ratings>` 带 name 属性。
//
// 一个原则：写出的 NFO 必须能被 Kodi / Jellyfin / Emby 直接读懂，
// 因为这些文件是唯一真相，删掉 index.db 之后就是靠它们恢复元数据。

const nfoHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"

// nfoArt 是 `<art>` 块。doc 7.4 约定落盘文件名固定，因此这里只需要文件名。
type nfoArt struct {
	Poster string `xml:"poster,omitempty"`
	Fanart string `xml:"fanart,omitempty"`
	Thumb  string `xml:"thumb,omitempty"`
}

// nfoRating 是 `<ratings>` 里的单个来源。
type nfoRating struct {
	Name    string  `xml:"name,attr"`
	Max     string  `xml:"max,attr,omitempty"`
	Default bool    `xml:"default,attr,omitempty"`
	Value   float64 `xml:"value"`
	Votes   int     `xml:"votes,omitempty"`
}

type nfoRatings struct {
	Rating []nfoRating `xml:"rating"`
}

// nfoUniqueID 是 `<uniqueid>`。Kodi 只认 imdb / tmdb / tvdb 这几种。
type nfoUniqueID struct {
	Type    string `xml:"type,attr"`
	Default bool   `xml:"default,attr,omitempty"`
	Value   string `xml:",chardata"`
}

type nfoActor struct {
	Name  string `xml:"name"`
	Role  string `xml:"role,omitempty"`
	Order int    `xml:"order"`
	Thumb string `xml:"thumb,omitempty"`
}

// nfoMovie 对应 movie.nfo / <文件名>.nfo。
type nfoMovie struct {
	XMLName       xml.Name      `xml:"movie"`
	Title         string        `xml:"title"`
	OriginalTitle string        `xml:"originaltitle,omitempty"`
	SortTitle     string        `xml:"sorttitle,omitempty"`
	Year          int           `xml:"year,omitempty"`
	Plot          string        `xml:"plot,omitempty"`
	Tagline       string        `xml:"tagline,omitempty"`
	Runtime       int           `xml:"runtime,omitempty"`
	Ratings       *nfoRatings   `xml:"ratings,omitempty"`
	UniqueIDs     []nfoUniqueID `xml:"uniqueid"`
	Genres        []string      `xml:"genre"`
	Countries     []string      `xml:"country"`
	Directors     []string      `xml:"director"`
	Credits       []string      `xml:"credits"`
	Actors        []nfoActor    `xml:"actor"`
	Art           *nfoArt       `xml:"art,omitempty"`
	LockData      bool          `xml:"lockdata"`
}

// nfoTVShow 对应剧集根目录的 tvshow.nfo。
type nfoTVShow struct {
	XMLName       xml.Name      `xml:"tvshow"`
	Title         string        `xml:"title"`
	OriginalTitle string        `xml:"originaltitle,omitempty"`
	SortTitle     string        `xml:"sorttitle,omitempty"`
	Year          int           `xml:"year,omitempty"`
	Plot          string        `xml:"plot,omitempty"`
	Tagline       string        `xml:"tagline,omitempty"`
	Runtime       int           `xml:"runtime,omitempty"`
	Ratings       *nfoRatings   `xml:"ratings,omitempty"`
	UniqueIDs     []nfoUniqueID `xml:"uniqueid"`
	Genres        []string      `xml:"genre"`
	Countries     []string      `xml:"country"`
	Directors     []string      `xml:"director"`
	Credits       []string      `xml:"credits"`
	Actors        []nfoActor    `xml:"actor"`
	Season        int           `xml:"season"`
	Episode       int           `xml:"episode"`
	Art           *nfoArt       `xml:"art,omitempty"`
	LockData      bool          `xml:"lockdata"`
}

// nfoEpisode 对应单集 NFO。
type nfoEpisode struct {
	XMLName   xml.Name      `xml:"episodedetails"`
	Title     string        `xml:"title"`
	ShowTitle string        `xml:"showtitle,omitempty"`
	Season    int           `xml:"season"`
	Episode   int           `xml:"episode"`
	Plot      string        `xml:"plot,omitempty"`
	Runtime   int           `xml:"runtime,omitempty"`
	AirDate   string        `xml:"aired,omitempty"`
	Ratings   *nfoRatings   `xml:"ratings,omitempty"`
	UniqueIDs []nfoUniqueID `xml:"uniqueid"`
	Directors []string      `xml:"director"`
	Credits   []string      `xml:"credits"`
	Actors    []nfoActor    `xml:"actor"`
	Art       *nfoArt       `xml:"art,omitempty"`
	LockData  bool          `xml:"lockdata"`
}

// buildRatings 把评分整理成 Kodi 18+ 的 `<ratings>` 块。
// 没有评分时返回 nil，避免写出 `<ratings></ratings>` 这种空块。
func buildRatings(rating float64, votes int, source string) *nfoRatings {
	if rating <= 0 {
		return nil
	}
	if source == "" {
		source = "tmdb"
	}
	return &nfoRatings{Rating: []nfoRating{{
		Name:    source,
		Max:     "10",
		Default: true,
		Value:   round1(rating),
		Votes:   votes,
	}}}
}

// round1 保留一位小数：TMDB 给的是 8.437，NFO 里写 8.4 更干净。
func round1(v float64) float64 {
	n, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 1, 64), 64)
	return n
}

// buildUniqueIDs 按 imdb > tmdb 的顺序生成 uniqueid，imdb 优先设为 default。
// 文档 7.7：豆瓣 ID 写进去 Kodi 会忽略，所以永远不写。
func buildUniqueIDs(imdbID string, tmdbID int) []nfoUniqueID {
	var out []nfoUniqueID
	if imdbID != "" {
		out = append(out, nfoUniqueID{Type: "imdb", Default: true, Value: imdbID})
	}
	if tmdbID > 0 {
		out = append(out, nfoUniqueID{Type: "tmdb", Default: imdbID == "", Value: strconv.Itoa(tmdbID)})
	}
	return out
}

// buildActorList 截断演员表：Kodi 没有数量限制，但 NFO 会变得很大且没什么用。
// 取前 15 位，与 TMDB 详情页默认展示量级相当。
func buildActorList(actors []Actor) []nfoActor {
	const limit = 15
	out := make([]nfoActor, 0, limit)
	for i, a := range actors {
		if i >= limit {
			break
		}
		out = append(out, nfoActor{Name: a.Name, Role: a.Role, Order: i})
	}
	return out
}

// writeNFO 把结构体写成带 XML 声明的 UTF-8 文件（无 BOM）。
// 先写临时文件再 rename，避免中途崩溃留下半个 XML。
func writeNFO(path string, v any) error {
	body, err := xml.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 NFO 失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(nfoHeader), body...), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// nfoProbe 是只读探测用的宽松结构：movie / tvshow / episodedetails 都按这一份解。
// 目的只有两个：判断 lockdata，取出已有的外部 ID（用于跳过重复搜索）。
type nfoProbe struct {
	LockData  string        `xml:"lockdata"`
	Title     string        `xml:"title"`
	Year      string        `xml:"year"`
	UniqueIDs []nfoUniqueID `xml:"uniqueid"`
}

// nfoInfo 是从已有 NFO 里读出的关键信息。
type nfoInfo struct {
	Exists bool
	Locked bool
	TmdbID int
	ImdbID string
	Title  string
	Year   int
}

// readNFO 读取并解析 NFO；不存在时返回 Exists=false 而不是错误。
func readNFO(path string) nfoInfo {
	b, err := os.ReadFile(path)
	if err != nil {
		return nfoInfo{}
	}
	var p nfoProbe
	if err := xml.Unmarshal(b, &p); err != nil {
		// 手工改坏的 NFO 不该让扫描中断，当作"没有 NFO"处理
		warnf("NFO 解析失败，忽略：%s（%v）", path, err)
		return nfoInfo{}
	}
	info := nfoInfo{
		Exists: true,
		Locked: strings.EqualFold(strings.TrimSpace(p.LockData), "true"),
		Title:  strings.TrimSpace(p.Title),
	}
	if y, err := strconv.Atoi(strings.TrimSpace(p.Year)); err == nil {
		info.Year = y
	}
	for _, u := range p.UniqueIDs {
		v := strings.TrimSpace(u.Value)
		switch strings.ToLower(strings.TrimSpace(u.Type)) {
		case "tmdb":
			if n, err := strconv.Atoi(v); err == nil {
				info.TmdbID = n
			}
		case "imdb":
			info.ImdbID = v
		}
	}
	return info
}

// nfoPathFor 决定该条目 NFO 的落盘路径：
//
//	目录里只有一个视频 → movie.nfo（Kodi 官方推荐的"每片一目录"布局）
//	目录里有多个视频   → <文件名>.nfo，避免互相覆盖
func nfoPathFor(dir, stem string, videoCount int) string {
	if videoCount <= 1 {
		return filepath.Join(dir, "movie.nfo")
	}
	return filepath.Join(dir, stem+".nfo")
}

// imageBaseName 决定图片文件名：单片目录用 poster.jpg，多片目录加前缀。
func imageBaseName(dir, stem string, videoCount int) string {
	if videoCount <= 1 {
		return ""
	}
	return stem + "-"
}

// nfoDetailXML 是详情页展示用的宽松读取结构：movie / tvshow / episodedetails
// 三种根元素都按这一份解（没有 XMLName，字段按子元素名匹配）。
// 详情页的原则是「NFO 才是真相」：用户手工改过 NFO，页面立即跟着变。
type nfoDetailXML struct {
	Tagline   string        `xml:"tagline"`
	Genres    []string      `xml:"genre"`
	Countries []string      `xml:"country"`
	Directors []string      `xml:"director"`
	Credits   []string      `xml:"credits"`
	Actors    []nfoActor    `xml:"actor"`
	AirDate   string        `xml:"aired"`
	Ratings   *nfoRatings   `xml:"ratings"`
	UniqueIDs []nfoUniqueID `xml:"uniqueid"`
}

// readNFODetail 读取并解析 NFO 的展示字段；不存在或解析失败返回 nil。
func readNFODetail(path string) *nfoDetailXML {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var d nfoDetailXML
	if err := xml.Unmarshal(b, &d); err != nil {
		warnf("NFO 详情解析失败，忽略：%s（%v）", path, err)
		return nil
	}
	return &d
}
