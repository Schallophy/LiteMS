package main

// 电影刮削成功后，把条目目录名换成刮削到的名称（优先英文名）。
//
// 目录名一变，ItemID（root+rel 的 sha1）跟着变，牵一发动全身：
//   - progress.item_id 是外键，要搬行
//   - search_key 含文件路径，要重建
//   - 海报/背景图路径含目录名，要改写
//   - 两处按 ID 存的缓存要搬：转码音轨 transcode/<id>.m4a、导入字幕 .litems/subs/<id>
//
// 全部成功才算完成；文件系统先改、数据库其次、缓存最后，缓存失败不影响主流程。

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// renameToScrapedName 把条目目录改成刮削名，并迁移 ID 相关的一切。
// 只对「目录里只有一个视频」的电影生效——共享目录改名会殃及邻居。
func (r *Runner) renameToScrapedName(ctx context.Context, it *Item, dir string, meta *Meta) {
	if r.dry || it.Kind != KindMovie || r.videoCount(dir) != 1 {
		return
	}
	// 条目直接躺在媒体根（rel 无目录段）时没有目录可改名
	if filepath.Dir(it.RelPath) == "." {
		return
	}
	want := scrapedDirName(ctx, meta, r.sc)
	if want == "" || want == filepath.Base(dir) {
		return
	}
	target := filepath.Join(filepath.Dir(dir), want)
	if _, err := os.Lstat(target); err == nil {
		warnf("目录重命名跳过：%s 已存在", want)
		return
	}

	oldID, oldBase := it.ID, filepath.Base(dir)
	if err := os.Rename(dir, target); err != nil {
		warnf("目录重命名失败 %s → %s：%v", oldBase, want, err)
		return
	}

	root := r.roots[it.RootIdx]
	// RelPath 指向视频文件，它的 Dir 是条目目录（根相对）；改名是替换目录段，
	// 不是在目录里再嵌一层——嵌了就跟文件系统的真实位置对不上
	dirRel := filepath.Dir(filepath.ToSlash(it.RelPath))
	newRel := filepath.ToSlash(filepath.Join(filepath.Dir(dirRel), want, filepath.Base(it.RelPath)))
	newID := ItemID(root, newRel)
	posterRel := rewriteDirOf(it.PosterPath, want)
	fanartRel := rewriteDirOf(it.FanartPath, want)

	if err := r.db.RekeyItem(it.ID, newID, newRel, posterRel, fanartRel, it.Title, it.OriginalTitle); err != nil {
		// 文件系统已改、数据库没跟上：下次全量扫描会按新路径重新入库，
		// 但播放进度丢了。所以这条必须大声报出来。
		warnf("目录已改名但数据库迁移失败（进度可能丢失）：%v", err)
		return
	}
	it.ID, it.RelPath, it.PosterPath, it.FanartPath = newID, newRel, posterRel, fanartRel

	// 按旧 ID 存的缓存搬到新 ID（不存在就静默跳过）
	moveCache(filepath.Join(filepath.Dir(r.cfg.Database.Path), "transcode", oldID+".m4a"),
		filepath.Join(filepath.Dir(r.cfg.Database.Path), "transcode", newID+".m4a"))
	moveCache(filepath.Join(root, ".litems", "subs", oldID),
		filepath.Join(root, ".litems", "subs", newID))

	fmt.Printf("  [改名] %s → %s\n", truncate(oldBase, 40), truncate(want, 40))
}

// scrapedDirName 目录名：优先拉丁字母标题（英文名），中文原题就查 TMDB
// 的 en-US 标题，再不行退回刮削到的 Title（可能是中文，但总比发布组名好）。
// 尾部带年份，避免同名片子撞目录。
func scrapedDirName(ctx context.Context, meta *Meta, sc Scraper) string {
	name := meta.OriginalTitle
	if !mostlyLatin(name) {
		if tc, ok := sc.(*TMDBClient); ok && meta.TmdbID > 0 {
			name = tc.latinTitle(ctx, meta.TmdbID)
		}
	}
	if !mostlyLatin(name) {
		name = meta.Title
	}
	name = sanitizeDirName(name)
	if name == "" {
		return ""
	}
	if meta.Year > 0 {
		name += fmt.Sprintf(" (%d)", meta.Year)
	}
	return name
}

// latinTitle 取 TMDB 的 en-US 电影标题，失败返回空串。
// 中文片的 OriginalTitle 是中文，英文名只能从 en-US 端点拿。
func (c *TMDBClient) latinTitle(ctx context.Context, id int) string {
	var resp struct {
		Title string `json:"title"`
	}
	if err := c.get(ctx, "/movie/"+strconv.Itoa(id),
		url.Values{"language": {"en-US"}}, &resp); err != nil {
		return ""
	}
	return resp.Title
}

// mostlyLatin 判断标题是否以拉丁字母为主：拉丁字母数过半才算。
// 「The Odyssey」是，「流浪地球2」不是。
func mostlyLatin(s string) bool {
	letters, latin := 0, 0
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			latin++
		}
	}
	return letters > 0 && latin*2 >= letters
}

// sanitizeDirName 去掉文件系统不容的字符，压掉多余空白与首尾的点空格。
func sanitizeDirName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return ' '
		}
		return r
	}, s)
	return strings.Trim(strings.Join(strings.Fields(s), " "), " .")
}

// rewriteDirOf 把根相对路径里条目目录那段（倒数第二段）换成新目录名
// （poster/fanart 用）：Downloads/<dir>/poster.jpg → Downloads/<new>/poster.jpg。
// 路径为空或层级不够就原样返回。结果统一正斜杠（与库里 rel_path 的存法一致）。
func rewriteDirOf(rel, dir string) string {
	dirRel := filepath.Dir(rel)
	if rel == "" || dirRel == "." || filepath.Dir(dirRel) == "." {
		return rel
	}
	newDir := filepath.Join(filepath.Dir(dirRel), dir)
	return filepath.ToSlash(filepath.Join(newDir, filepath.Base(rel)))
}

// moveCache 搬一个按旧 ID 存的缓存文件/目录，源不存在或目标已存在就跳过。
func moveCache(src, dst string) {
	if _, err := os.Lstat(src); err != nil {
		return
	}
	if _, err := os.Lstat(dst); err == nil {
		return
	}
	os.Rename(src, dst)
}
