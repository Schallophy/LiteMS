package main

// 下载目录镜像：[library].download_dir 的自动入库机制。
//
// 设计动机：此前的方案依赖一个写死路径的 bash 钩子脚本 + 在 qbt 里手工配
// AutoRun，只适合本机定制，没法分发，换目录、换方案都要手改脚本。
// 现在下载目录是 LiteMS 自己的设置项：serve 每分钟看一眼下载目录，
// 出现新内容就镜像进库，然后触发增量扫描与刮削。下载工具侧零配置。
//
// 布局：
//
//	<download_dir>/<种子>/...            下载工具独占，做种用，一个字节不写
//	<库根>/<下载目录名>/<种子>/...        镜像落点：符号链接（或 copy 模式的副本）
//	<库根>/<下载目录名>/<种子>/*.nfo     scrape 的侧车文件写在这边
//
// 镜像落点取「库根 + 下载目录的目录名」：~/Downloads 对应 <库根>/Downloads。
// 下载目录本身绝不能在库根里面——那会绕开整个镜像机制，直接把侧车文件
// 写进做种目录，startUpSync 会拒绝启动并明确报错。

import (
	"context"
	"io"
	"io/fs"
	"os"
	"sort"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// underPath 判断 child 是否位于 parent 目录内（含相等）。
// 下载目录与媒体根互相包含时，镜像机制会被绕开、侧车文件会写进做种目录，
// 所以 ingest 前和设置页校验都用它拦截。
func underPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// downloadSyncEvery 是 serve 检查下载目录的间隔。没有新内容时开销只有
// 一次 ReadDir 加若干 Lstat，一分钟一次对手机毫无压力。
const downloadSyncEvery = time.Minute

// tempSuffixes 是「还没下完」的文件特征。qbt 的未完成文件后缀是 .!qB，
// 其它下载工具常见 .part。镜像时跳过，下完的那一轮自然会被收进来。
var tempSuffixes = []string{".!qb", ".part"}

// mirrorMu 让 sync 触发的扫描与 watch 循环、启动扫描串行，
// 避免同一时刻两轮全量遍历抢同一把 SQLite 写锁。
var mirrorMu sync.Mutex

// downloadLinkDir 计算镜像落点：库根 + 下载目录的目录名。
func downloadLinkDir(cfg *Config) string {
	return filepath.Join(cfg.Library.Root, filepath.Base(cfg.Library.DownloadDir))
}

// isTempFile 判断是否为未完成的下载文件。
func isTempFile(name string) bool {
	l := strings.ToLower(name)
	for _, s := range tempSuffixes {
		if strings.HasSuffix(l, s) {
			return true
		}
	}
	return false
}

// planMirror 计算一个下载条目（种子目录或单文件）应镜像到库里的文件清单。
//
// 目录：按原结构归档到 <落点>/<目录名>/ 下，用内容目录名而不是相对下载根
// 的路径——自定义保存位置不会把外层层级带进库。
// 单文件：文件名与条目名一致时平铺，不一致就补一层条目名目录，
// 避免两个种子的同名文件互相覆盖。
func planMirror(entryPath, entryName, linkDir string) ([][2]string, error) {
	fi, err := os.Stat(entryPath)
	if err != nil {
		return nil, err
	}

	var items [][2]string // [源, 落点]
	if !fi.IsDir() {
		base := filepath.Base(entryPath)
		dst := filepath.Join(linkDir, entryName)
		if base != entryName {
			dst = filepath.Join(linkDir, entryName, base)
		}
		return [][2]string{{entryPath, dst}}, nil
	}

	base := filepath.Base(entryPath)
	err = filepath.WalkDir(entryPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if isTempFile(d.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(entryPath, path)
		if relErr != nil {
			return relErr
		}
		items = append(items, [2]string{path, filepath.Join(linkDir, base, rel)})
		return nil
	})
	return items, err
}

// applyMirror 落盘一条镜像。幂等：目标已是指向同一源头的链接（或同大小
// 同 mtime 的副本）时跳过；落点被别的目录占用时停下报告，绝不递归删除。
func applyMirror(src, dst, mode string) (created, skipped bool, err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return false, false, err
	}
	if mode == "copy" {
		return applyCopy(src, dst)
	}
	return applySymlink(src, dst)
}

func applySymlink(src, dst string) (created, skipped bool, err error) {
	if fi, err := os.Lstat(dst); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			if target, lerr := os.Readlink(dst); lerr == nil && target == src {
				return false, true, nil // 已是同目标链接
			}
			if err := os.Remove(dst); err != nil {
				return false, false, err
			}
		} else if fi.IsDir() {
			return false, false, errDstIsDir(dst)
		} else {
			if err := os.Remove(dst); err != nil {
				return false, false, err
			}
		}
	} else if !os.IsNotExist(err) {
		return false, false, err
	}
	if err := os.Symlink(src, dst); err != nil {
		return false, false, err
	}
	return true, false, nil
}

type errDstIsDir string

func (e errDstIsDir) Error() string {
	return "镜像落点已是目录：" + string(e) + "，请手工处理"
}

func applyCopy(src, dst string) (created, skipped bool, err error) {
	si, err := os.Stat(src)
	if err != nil {
		return false, false, err
	}
	if di, err := os.Stat(dst); err == nil &&
		di.Mode().IsRegular() && di.Size() == si.Size() &&
		di.ModTime().Sub(si.ModTime()).Abs() < time.Second {
		return false, true, nil // 同大小同 mtime，视为已复制
	}
	in, err := os.Open(src)
	if err != nil {
		return false, false, err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".mirror-*")
	if err != nil {
		return false, false, err
	}
	tmp := out.Name()
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return false, false, err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return false, false, err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return false, false, err
	}
	// 保留源文件 mtime，增量扫描按 mtime 判定，副本必须和源一致
	if err := os.Chtimes(tmp, si.ModTime(), si.ModTime()); err != nil {
		os.Remove(tmp)
		return false, false, err
	}
	if _, err := os.Lstat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			os.Remove(tmp)
			return false, false, err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return false, false, err
	}
	return true, false, nil
}

// cleanDangling 清理镜像目录里指向已消失文件的链接（在 qbt 里删了种子、
// 手工删了文件）。种子目录是镜像时 MkdirAll 出来的实体目录，链接在它里面，
// 所以必须递归整棵镜像树，不能只看顶层。只删符号链接本身，旁边的
// .nfo、海报一律不动；全部链接悬空的目录顺手移除。
// copy 模式的副本无法与用户自放文件区分，不清理。
func cleanDangling(linkDir string) (removed int) {
	var dirs []string
	_ = filepath.WalkDir(linkDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不到的条目不碰
		}
		switch {
		case d.IsDir():
			if path != linkDir {
				dirs = append(dirs, path)
			}
		case d.Type()&fs.ModeSymlink != 0:
			if _, err := os.Stat(path); err != nil && os.IsNotExist(err) {
				if os.Remove(path) == nil {
					removed++
				}
			}
		}
		return nil
	})
	// 从最深的目录开始清理孤儿壳：种子删除后目录里只剩 scrape 写的
	// 侧车文件（nfo / 海报），没有任何链接与真实媒体，整目录移除。
	// 有活链接或非侧车实体文件（用户放的）的目录一律保留。
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		keep := false
		for _, e := range entries {
			switch {
			case e.Type()&fs.ModeSymlink != 0:
				keep = true // 还有活链接（悬空的上一轮已删）
			case e.Type().IsRegular() && !sidecarExts[strings.ToLower(filepath.Ext(e.Name()))]:
				keep = true // 非侧车的实体文件不是我们管的
			}
			if keep {
				break
			}
		}
		if !keep {
			os.RemoveAll(d)
		}
	}
	return removed
}

// sidecarExts 是 scrape 写在视频旁边的元数据/图片，不算「真实内容」。
var sidecarExts = map[string]bool{
	".nfo": true, ".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
}

// syncDownloads 检查下载目录并把新内容镜像进库。返回新建条目数。
func syncDownloads(cfg *Config) (int, error) {
	if cfg.Library.DownloadDir == "" {
		return 0, nil
	}
	linkDir := downloadLinkDir(cfg)

	entries, err := os.ReadDir(cfg.Library.DownloadDir)
	if err != nil {
		return 0, err
	}

	created := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || isTempFile(name) {
			continue
		}
		entryPath := filepath.Join(cfg.Library.DownloadDir, name)
		items, err := planMirror(entryPath, name, linkDir)
		if err != nil {
			warnf("下载镜像：跳过 %s（%v）", name, err)
			continue
		}
		for _, it := range items {
			c, _, err := applyMirror(it[0], it[1], cfg.Library.DownloadMode)
			if err != nil {
				warnf("下载镜像：%v", err)
				continue
			}
			if c {
				created++
			}
		}
	}
	if cfg.Library.DownloadMode == "symlink" {
		cleanDangling(linkDir)
	}
	return created, nil
}

// syncAndIndex 镜像 + （有新内容时）增量扫描 + 刮削。
// serve 的同步循环与启动流程共用这一段。
func syncAndIndex(cfg *Config, db *DB) {
	mirrorMu.Lock()
	defer mirrorMu.Unlock()

	created, err := syncDownloads(cfg)
	if err != nil {
		if !os.IsNotExist(err) {
			warnf("下载镜像失败：%v", err)
		}
		return
	}
	if created == 0 {
		return
	}
	if !cfg.Library.FollowFileSymlinks && cfg.Library.DownloadMode == "symlink" {
		warnf("下载镜像：已建 %d 条链接，但 [library].follow_file_symlinks 未开启，扫描不会索引它们", created)
		return
	}

	st, err := NewScanner(cfg, db, false, false, false, "").Run()
	if err != nil {
		warnf("下载镜像后扫描失败：%v", err)
		return
	}
	logf("下载镜像：新增 %d 个文件，扫描完成（新增 %d，清理 %d）", created, st.Added, st.Deleted)

	sc, err := buildScraper(cfg)
	if err != nil || sc == nil {
		return // 没配凭据是正常状态，安静跳过；启动补刮也会兜底
	}
	r := NewRunner(cfg, db, sc, false, false, "", 20)
	stats, err := r.Run(context.Background())
	if err != nil {
		warnf("下载镜像后刮削失败：%v", err)
		return
	}
	logf("下载镜像刮削：成功 %d，未匹配 %d，图片 %d 张", stats.OK, stats.Miss, stats.Images)
}

// startDownloadSyncLoop 启动 serve 的下载目录同步循环。
// 启动时的第一次镜像由 cmdServe 的启动流程完成（镜像→扫描→补刮串行一次），
// 这里只负责之后的定时检查。
func startDownloadSyncLoop(ctx context.Context, cfg *Config, db *DB) {
	ticker := time.NewTicker(downloadSyncEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncAndIndex(cfg, db)
		}
	}
}
