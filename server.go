package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

var reItemID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Server 持有 HTTP 服务所需的全部状态。
type Server struct {
	cfg           *Config
	db            *DB
	tpl           *template.Template
	roots         []string
	settingsToken string // 设置页 CSRF 令牌，进程启动时生成

	// 设置页可改的凭据快照。单独存一份并加锁，避免直接写 cfg 与其它请求竞争。
	credsMu sync.RWMutex
	creds   scrapeCreds

	// 主题同理：设置页改完立刻生效，不必重启；空值表示沿用 cfg 里的初值。
	themeMu sync.RWMutex
	theme   string

	tc  *Transcoder
	hls *HLSServer

	// 字幕默认偏移缓存：key = 条目ID|字幕URL，value = 秒（float64）
	subOff sync.Map
}

// tplFuncs 是模板里用到的格式化助手。
// initial 按 rune 取首字，避免中文被按字节截断；pct/duration 供进度条与续播文案使用。
var tplFuncs = template.FuncMap{
	"initial": func(s string) string {
		for _, r := range s {
			return string(r)
		}
		return "?"
	},
	"pct": func(pos, dur int64) string {
		if dur <= 0 || pos <= 0 {
			return "0%"
		}
		p := pos * 100 / dur
		if p > 100 {
			p = 100
		}
		return strconv.FormatInt(p, 10) + "%"
	},
	"duration": func(ms int64) string {
		if ms <= 0 {
			return "0:00"
		}
		s := ms / 1000
		h, m, sec := s/3600, (s%3600)/60, s%60
		if h > 0 {
			return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
		}
		return fmt.Sprintf("%d:%02d", m, sec)
	},
}

// isAddrInUse 判断错误是否为端口被占用。
// 端口冲突的解决办法只有一个：换端口。值得单独提示，而不是把底层错误原样丢给用户。
func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "Only one usage of each socket address")
}

// RunServer 启动 HTTP 服务，阻塞直到收到信号。
func RunServer(cfg *Config, db *DB) error {
	tpl, err := template.New("").Funcs(tplFuncs).ParseFS(webFS, "web/*.html")
	if err != nil {
		return fmt.Errorf("解析模板失败: %w", err)
	}
	s := &Server{
		cfg: cfg, db: db, tpl: tpl, roots: cfg.AllRoots(),
		settingsToken: newSettingsToken(),
		creds:         credsFromConfig(cfg),
	}
	// 转码缓存放数据库同目录：$HOME 私有存储，不进媒体库、不会被扫描捡走
	tc, err := NewTranscoder(filepath.Join(filepath.Dir(cfg.Database.Path), "transcode"))
	if err != nil {
		return err
	}
	s.tc = tc
	// 实时转码的分段放同一个父目录下，进程退出即清空
	hls, err := NewHLSServer(filepath.Join(filepath.Dir(cfg.Database.Path), "hls"))
	if err != nil {
		return err
	}
	s.hls = hls

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/items", s.handleItems)
	mux.HandleFunc("/api/items/", s.handleItemJSON)
	mux.HandleFunc("/api/continue", s.handleContinue)
	mux.HandleFunc("/api/recent", s.handleRecent)
	mux.HandleFunc("/api/progress", s.handleProgress)
	mux.HandleFunc("/api/progress/", s.handleProgressGet)
	mux.HandleFunc("/api/cmd/", s.handleCmd)
	mux.HandleFunc("/api/transcode/", s.handleTranscode)
	mux.HandleFunc("/api/live/", s.handleLive)
	mux.HandleFunc("/api/sub/", s.handleSubUpload)
	mux.HandleFunc("/api/suboffset/", s.handleSubOffset)
	// 第三方前端库：build 时打进二进制，局域网离线可用
	vendor, err := fs.Sub(webFS, "web/vendor")
	if err != nil {
		return err // 该函数只返回 error（RunServer 内部装配 mux）
	}
	mux.Handle("/vendor/", http.StripPrefix("/vendor/", http.FileServer(http.FS(vendor))))
	mux.HandleFunc("/stream/", s.handleStream)
	mux.HandleFunc("/audio/", s.handleAudio)
	mux.HandleFunc("/hls/", s.handleHLS)
	mux.HandleFunc("/sub/", s.handleSub)
	mux.HandleFunc("/poster/", s.handleImage(true))
	mux.HandleFunc("/fanart/", s.handleImage(false))
	mux.HandleFunc("/item/", s.handleItemPage)
	mux.HandleFunc("/play/", s.handlePlayPage)
	mux.HandleFunc("/settings", s.handleSettings)
	mux.HandleFunc("/", s.handleIndex)

	var h http.Handler = mux
	if cfg.Server.AuthUser != "" {
		h = basicAuth(cfg.Server.AuthUser, cfg.Server.AuthPass, mux)
	}

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           h,
		ReadHeaderTimeout: time.Duration(cfg.Server.ReadHeaderSec) * time.Second,
		IdleTimeout:       time.Duration(cfg.Server.IdleTimeoutSec) * time.Second,
		WriteTimeout:      0, // 必须为 0，否则大文件推流会被切断（文档 4.3）
	}

	// 后台定时增量扫描（文档 12.2）
	scanStop := make(chan struct{})
	if cfg.Watch.Enabled && cfg.Watch.IntervalMin > 0 {
		go s.watchLoop(scanStop)
	}

	errCh := make(chan error, 1)
	go func() {
		logf("服务已启动  http://%s  （媒体根 %d 个）", cfg.Server.Listen, len(s.roots))
		errCh <- srv.ListenAndServe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		close(scanStop)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-sig:
		logf("收到退出信号，正在关闭…")
		close(scanStop)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}

// watchLoop 每 interval_min 触发一次增量扫描。
func (s *Server) watchLoop(stop chan struct{}) {
	t := time.NewTicker(time.Duration(s.cfg.Watch.IntervalMin) * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			sc := NewScanner(s.cfg, s.db, false, false, false, "")
			st, err := sc.Run()
			if err != nil {
				warnf("后台扫描失败：%v", err)
				continue
			}
			if st.Found > 0 {
				logf("后台扫描：发现 %d，处理 %d，新增 %d，清理 %d", st.Found, st.Need, st.Added, st.Deleted)
			}
		}
	}
}

// ---------- 中间件 ----------

func basicAuth(user, pass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="litems"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// parseID 从路径尾部取出 16 位十六进制 ID。
func parseID(path, prefix string) (string, bool) {
	id := strings.TrimPrefix(path, prefix)
	id = strings.TrimSuffix(id, "/")
	if !reItemID.MatchString(id) {
		return "", false
	}
	return id, true
}

// ---------- 处理器 ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	n, last, err := s.db.StatKV()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"db": "error", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"db":        "ok",
		"items":     n,
		"last_scan": last,
	})
}

// ItemDTO 是给前端的条目视图（文档 9.3）。
type ItemDTO struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	OriginalTitle string   `json:"original_title,omitempty"`
	Year          int      `json:"year,omitempty"`
	Kind          string   `json:"kind"`
	Season        int      `json:"season,omitempty"`
	Episode       int      `json:"episode,omitempty"`
	RuntimeMin    int      `json:"runtime_min,omitempty"`
	Rating        float64  `json:"rating,omitempty"`
	Overview      string   `json:"overview,omitempty"`
	Poster        string   `json:"poster,omitempty"`
	Fanart        string   `json:"fanart,omitempty"`
	Subs          []SubDTO `json:"subs,omitempty"`
	Finished      bool     `json:"finished"`
	PositionMS    int64    `json:"position_ms,omitempty"`
	DurationMS    int64    `json:"duration_ms,omitempty"`
}

// artworkExists 校验图片文件真的在盘上（>512 字节，与刮削时的判定一致）。
func (s *Server) artworkExists(it Item, rel string) bool {
	if it.RootIdx < 0 || it.RootIdx >= len(s.roots) {
		return false
	}
	fi, err := os.Stat(filepath.Join(s.roots[it.RootIdx], filepath.FromSlash(rel)))
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 512
}

func (s *Server) toDTO(it Item) ItemDTO {
	d := ItemDTO{
		ID: it.ID, Title: it.Title, OriginalTitle: it.OriginalTitle,
		Year: it.Year, Kind: string(it.Kind), Season: it.Season, Episode: it.Episode,
		RuntimeMin: it.RuntimeMin, Rating: it.Rating, Overview: it.Overview,
	}
	// 图片 URL 带版本号：/poster 的缓存头是 max-age=604800，URL 不变的话
	// 重刮之后浏览器还会拿旧图（哪怕服务端已经换了文件），刷新和重启都救不回来。
	// 用 ScrapedAt 当版本号，重刮一次 URL 就变，缓存自然失效。
	// 图片要「路径在 DB 且文件真在盘上」才输出 URL。
	// 刮削失败或图片被删后 DB 里的路径还在，直接渲染只会得到裂图；
	// 占位符只在「确实没有图」时出现，这是它的唯一含义。
	ver := ""
	if it.ScrapedAt > 0 {
		ver = fmt.Sprintf("?v=%d", it.ScrapedAt)
	}
	if it.PosterPath != "" && s.artworkExists(it, it.PosterPath) {
		d.Poster = "/poster/" + it.ID + ver
	}
	if it.FanartPath != "" && s.artworkExists(it, it.FanartPath) {
		d.Fanart = "/fanart/" + it.ID + ver
	}
	if p, err := s.db.GetProgress(it.ID); err == nil && p != nil {
		d.Finished = p.Finished
		d.PositionMS = p.PositionMS
		d.DurationMS = p.DurationMS
	}
	return d
}

func (s *Server) handleItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	year, _ := strconv.Atoi(q.Get("year"))
	f := ItemFilter{
		Q:     strings.TrimSpace(q.Get("q")),
		Sort:  q.Get("sort"),
		Page:  page,
		Limit: limit,
		Kind:  q.Get("kind"),
		Year:  year,
	}
	if f.Limit == 0 {
		f.Limit = s.cfg.UI.PageSize
	}
	items, total, err := s.db.ListItems(f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]ItemDTO, 0, len(items))
	for _, it := range items {
		out = append(out, s.toDTO(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": total,
		"page":  maxInt(f.Page, 1),
		"limit": f.Limit,
		"items": out,
	})
}

func (s *Server) handleItemJSON(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/api/items/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "id 必须是 16 位十六进制")
		return
	}
	it, err := s.db.GetItem(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if it == nil {
		writeErr(w, http.StatusNotFound, "条目不存在")
		return
	}
	d := s.toDTO(*it)
	d.Subs = s.subsFor(*it)
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleContinue(w http.ResponseWriter, r *http.Request) {
	items, err := s.db.ListContinue(20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]ItemDTO, 0, len(items))
	for _, it := range items {
		out = append(out, s.toDTO(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleRecent(w http.ResponseWriter, r *http.Request) {
	items, _, err := s.db.ListItems(ItemFilter{Sort: "recent", Page: 1, Limit: 20})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]ItemDTO, 0, len(items))
	for _, it := range items {
		out = append(out, s.toDTO(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	var body struct {
		ItemID     string `json:"item_id"`
		PositionMS int64  `json:"position_ms"`
		DurationMS int64  `json:"duration_ms"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if !reItemID.MatchString(body.ItemID) {
		writeErr(w, http.StatusBadRequest, "item_id 必须是 16 位十六进制")
		return
	}
	if body.PositionMS < 0 || body.DurationMS < 0 {
		writeErr(w, http.StatusBadRequest, "时间不能为负")
		return
	}
	if err := s.db.SetProgress(body.ItemID, body.PositionMS, body.DurationMS); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleProgressGet(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/api/progress/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "id 必须是 16 位十六进制")
		return
	}
	p, err := s.db.GetProgress(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == nil {
		writeJSON(w, http.StatusOK, map[string]any{"item_id": id, "position_ms": 0, "duration_ms": 0, "finished": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"item_id": p.ItemID, "position_ms": p.PositionMS,
		"duration_ms": p.DurationMS, "finished": p.Finished, "updated_at": p.UpdatedAt,
	})
}

// handleStream 直接 ServeFile：Range / ETag / Content-Type 全部由标准库处理。
// 始终是媒体库里的原文件——音轨转码只产出独立的 .m4a（见 /audio/{id}），
// 不再复制整片视频。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/stream/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "id 必须是 16 位十六进制")
		return
	}
	it, err := s.db.GetItem(id)
	if err != nil || it == nil {
		writeErr(w, http.StatusNotFound, "条目不存在")
		return
	}
	full := filepath.Join(s.roots[it.RootIdx], filepath.FromSlash(it.RelPath))
	if _, err := os.Stat(full); err != nil {
		writeErr(w, http.StatusNotFound, "文件已不存在")
		return
	}
	http.ServeFile(w, r, full)
}

// handleAudio 提供预转码出来的 AAC 音轨（.m4a）。网页播放时由 <audio> 加载，
// 与原视频（静音）对齐播放；Range 支持由 ServeFile 负责。
func (s *Server) handleAudio(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/audio/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "id 必须是 16 位十六进制")
		return
	}
	if !s.tc.Has(id) {
		writeErr(w, http.StatusNotFound, "还没有音轨缓存")
		return
	}
	w.Header().Set("Content-Type", "audio/mp4")
	http.ServeFile(w, r, s.tc.CachePath(id))
}

// handleLive 开/复用实时转码会话，返回 HLS 播放地址。
//
//	/api/live/{id}?start=秒
//
// 同一时间点重复请求复用现有会话；拖动进度会因 start 变化而重启 ffmpeg。
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/api/live/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "id 必须是 16 位十六进制")
		return
	}
	it, err := s.db.GetItem(id)
	if err != nil || it == nil {
		writeErr(w, http.StatusNotFound, "条目不存在")
		return
	}
	start, _ := strconv.ParseFloat(r.URL.Query().Get("start"), 64)
	src := filepath.Join(s.roots[it.RootIdx], filepath.FromSlash(it.RelPath))

	// ?ctl=pause|resume：观众暂停/继续时同步掐停 ffmpeg，别让实时窗口滑过播放位置
	if ctl := r.URL.Query().Get("ctl"); ctl != "" {
		if tok := r.URL.Query().Get("token"); tok != "" {
			s.hls.Pause(id, tok, ctl == "pause")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	sess, err := s.hls.Start(id, src, start)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	// 等第一段出来再返回，避免播放器拿着空播放列表反复重试
	waitFile(filepath.Join(sess.Dir, "video", "index.m3u8"), 8*time.Second)
	writeJSON(w, http.StatusOK, map[string]any{
		"token": sess.Token,
		"start": sess.Start,
		"url":   "/hls/" + id + "/" + sess.Token + "/master.m3u8",
		// 全片时长（秒）。HLS 播放列表是滑动窗口，video.duration 只有已转出的
		// 部分，前端进度条必须按这个值折算成全片比例，跳转才有意义。
		"total": mediaDurationSec(src),
	})
}

// handleHLS 提供实时转码的播放列表与分段：/hls/{id}/{token}/{file}
// 每送出一段就顺手删掉很早以前的分段，磁盘占用保持在几十 MB。
func (s *Server) handleHLS(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/hls/"), "/", 3)
	if len(parts) != 3 {
		writeErr(w, http.StatusBadRequest, "路径应为 /hls/{id}/{token}/{file}")
		return
	}
	itemID, token, name := parts[0], parts[1], parts[2]
	sess := s.hls.Lookup(itemID, token)
	if sess == nil {
		writeErr(w, http.StatusNotFound, "转码会话不存在或已过期")
		return
	}
	// master 是现场生成的（带 CODECS），不是磁盘上的文件
	if name == "master.m3u8" {
		waitFile(filepath.Join(sess.Dir, "video", "index.m3u8"), 8*time.Second)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(sess.Master()))
		return
	}
	full, ok := safeHLSPath(sess.Dir, name)
	if !ok {
		writeErr(w, http.StatusBadRequest, "非法文件名")
		return
	}
	if !waitFile(full, 8*time.Second) {
		writeErr(w, http.StatusNotFound, "分段尚未生成")
		return
	}

	switch {
	case strings.HasSuffix(name, ".m3u8"):
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
	case strings.HasSuffix(name, ".mp4"):
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "no-store")
	default:
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "no-store")
	}
	http.ServeFile(w, r, full)
}

// handleTranscode 查询（GET）/ 启动（POST）音轨转码任务。
func (s *Server) handleTranscode(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/api/transcode/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "id 必须是 16 位十六进制")
		return
	}
	it, err := s.db.GetItem(id)
	if err != nil || it == nil {
		writeErr(w, http.StatusNotFound, "条目不存在")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.tc.Status(id))
	case http.MethodPost:
		src := filepath.Join(s.roots[it.RootIdx], filepath.FromSlash(it.RelPath))
		force := r.URL.Query().Get("force") == "1"
		job, err := s.tc.Start(id, src, force)
		if err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, job)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "只支持 GET/POST")
	}
}

// handleSub 统一字幕出口：
//
//	/sub/{id}?sub=N      外挂字幕（按扫描顺序，N 从 0 起）
//	/sub/{id}?lang=chs   外挂字幕（按语言，兼容旧链接）
//	/sub/{id}?imp=NAME   用户导入的字幕（.litems/subs/<id>/）
//	/sub/{id}?embed=N    容器内嵌字幕轨（ffmpeg 抽成 VTT 并缓存）
//
// 带 fmt=vtt 时把 srt/ass 转成 WebVTT 给网页播放器；不带上原始文件给 mpv。
func (s *Server) handleSub(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/sub/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "id 必须是 16 位十六进制")
		return
	}
	it, err := s.db.GetItem(id)
	if err != nil || it == nil {
		writeErr(w, http.StatusNotFound, "条目不存在")
		return
	}
	q := r.URL.Query()

	// 导入字幕
	if imp := q.Get("imp"); imp != "" {
		name := filepath.Base(imp)
		if strings.HasPrefix(name, "embed-") || !subExts[strings.ToLower(filepath.Ext(name))] {
			writeErr(w, http.StatusNotFound, "字幕不存在")
			return
		}
		full := filepath.Join(s.subsDir(*it), name)
		if _, err := os.Stat(full); err != nil {
			writeErr(w, http.StatusNotFound, "字幕不存在")
			return
		}
		s.serveSubFile(w, r, full, SubtitleLang(name))
		return
	}

	// 内嵌字幕轨
	if emb := q.Get("embed"); emb != "" {
		idx, err := strconv.Atoi(emb)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "embed 必须是数字")
			return
		}
		path, err := s.extractEmbedSub(*it, idx)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		http.ServeFile(w, r, path)
		return
	}

	// 外挂字幕：?sub=N 优先，其次 ?lang=
	if len(it.SubPaths) == 0 {
		writeErr(w, http.StatusNotFound, "该条目没有外挂字幕")
		return
	}
	pick := it.SubPaths[0]
	if n := q.Get("sub"); n != "" {
		i, err := strconv.Atoi(n)
		if err != nil || i < 0 || i >= len(it.SubPaths) {
			writeErr(w, http.StatusNotFound, "字幕序号越界")
			return
		}
		pick = it.SubPaths[i]
	} else if want := NormalizeLang(q.Get("lang")); want != "" {
		found := false
		for _, sp := range it.SubPaths {
			if NormalizeLang(sp.Lang) == want {
				pick, found = sp, true
				break
			}
		}
		if !found {
			writeErr(w, http.StatusNotFound, "没有该语言的字幕")
			return
		}
	}
	dir := filepath.Dir(s.itemPath(*it))
	full := filepath.Join(dir, filepath.FromSlash(pick.Path))
	s.serveSubFile(w, r, full, pick.Lang)
}

// serveSubFile 输出单个字幕文件：fmt=vtt 转 WebVTT，否则原样（mpv 用）。
func (s *Server) serveSubFile(w http.ResponseWriter, r *http.Request, full, lang string) {
	ext := strings.ToLower(filepath.Ext(full))
	ct := "text/plain; charset=utf-8"
	switch ext {
	case ".srt":
		ct = "application/x-subrip; charset=utf-8"
	case ".ass", ".ssa":
		ct = "text/x-ssa; charset=utf-8"
	case ".vtt":
		ct = "text/vtt; charset=utf-8"
	}
	if mt := mime.TypeByExtension(ext); mt != "" && ct == "text/plain; charset=utf-8" {
		ct = mt
	}
	// fmt=vtt：给网页播放器用。HTML5 <video> 只认 WebVTT，而字幕文件的
	// 格式（srt/ass）与编码（GBK/Big5）都五花八门，统一在这里转换。
	// 不带 fmt 保持原始文件——mpv 的 --sub-file 走这条路，它自己会猜编码。
	if r.URL.Query().Get("fmt") == "vtt" {
		raw, err := os.ReadFile(full)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取字幕失败："+err.Error())
			return
		}
		vtt, err := toVTT(raw, ext, NormalizeLang(lang))
		if err != nil {
			writeErr(w, http.StatusUnsupportedMediaType, err.Error())
			return
		}
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(vtt)
		return
	}
	w.Header().Set("Content-Type", ct)
	http.ServeFile(w, r, full)
}

// handleImage 输出海报或背景图。
func (s *Server) handleImage(poster bool) http.HandlerFunc {
	prefix := "/fanart/"
	if poster {
		prefix = "/poster/"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(r.URL.Path, prefix)
		if !ok {
			writeErr(w, http.StatusBadRequest, "id 必须是 16 位十六进制")
			return
		}
		it, err := s.db.GetItem(id)
		if err != nil || it == nil {
			writeErr(w, http.StatusNotFound, "条目不存在")
			return
		}
		rel := it.FanartPath
		if poster {
			rel = it.PosterPath
		}
		if rel == "" {
			writeErr(w, http.StatusNotFound, "没有图片")
			return
		}
		full := filepath.Join(s.roots[it.RootIdx], filepath.FromSlash(rel))
		w.Header().Set("Cache-Control", "public, max-age=604800")
		http.ServeFile(w, r, full)
	}
}

// ---------- 页面 ----------

type pageData struct {
	Title       string
	Theme       string
	Query       string
	Kind        string
	Sort        string
	Page        int
	PrevPage    int
	NextPage    int
	Total       int
	Items       []ItemDTO
	Item        *ItemDTO
	Detail      *DetailInfo
	RootPath    string
	HelperToken string
	HelperPort  string
	// AudioHint 播放页用：从文件名识别的音频标签提示等级（bad/maybe/""）
	AudioHint string
	// AudioReady 预转码音轨已就绪（/audio/{id} 可用）
	AudioReady bool
	// LiveReady 实时转码可用（ffmpeg 已安装）
	LiveReady bool
	// Subs 是合并后的字幕清单（外挂/导入/内嵌），详情页与播放页共用
	Subs []SubDTO
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	f := ItemFilter{
		Q:     strings.TrimSpace(q.Get("q")),
		Kind:  q.Get("kind"),
		Sort:  q.Get("sort"),
		Page:  page,
		Limit: s.cfg.UI.PageSize,
	}
	items, total, err := s.db.ListItems(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]ItemDTO, 0, len(items))
	for _, it := range items {
		out = append(out, s.toDTO(it))
	}
	next := 0
	if page*f.Limit < total {
		next = page + 1
	}
	data := pageData{
		Title: s.cfg.UI.Title, Theme: s.pageTheme(),
		Query: f.Q, Kind: f.Kind, Sort: f.Sort, Page: page,
		PrevPage: page - 1, NextPage: next, Total: total, Items: out,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "index.html", data); err != nil {
		warnf("渲染首页失败：%v", err)
	}
}

func (s *Server) handleItemPage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/item/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	it, err := s.db.GetItem(id)
	if err != nil || it == nil {
		http.NotFound(w, r)
		return
	}
	d := s.toDTO(*it)
	data := pageData{
		Title: s.cfg.UI.Title, Theme: s.pageTheme(), Item: &d,
		Detail:      detailFor(s.roots, *it),
		HelperToken: s.cfg.Launch.Token, HelperPort: helperPort(s.cfg.Launch.Listen),
		AudioHint: audioHint(it.RelPath, it.Container),
		Subs:      s.subsFor(*it),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "item.html", data); err != nil {
		warnf("渲染详情页失败：%v", err)
	}
}

// handlePlayPage 沉浸式网页播放页（/play/<id>）：详情页只留信息，
// 网页播放跳到专门页面，看完/中途退出再回来。
func (s *Server) handlePlayPage(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/play/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	it, err := s.db.GetItem(id)
	if err != nil || it == nil {
		http.NotFound(w, r)
		return
	}
	d := s.toDTO(*it)
	data := pageData{
		Title:      s.cfg.UI.Title,
		Theme:      s.pageTheme(),
		Item:       &d,
		AudioHint:  audioHint(it.RelPath, it.Container),
		AudioReady: s.tc.Has(id),
		LiveReady:  ffmpegReady(),
		Subs:       s.subsFor(*it),
	}
	// 注意：这里**不能**因为转码缓存就绪就把 AudioHint 抹掉。播放页要拿它判断
	// 「能不能直接播」——抹掉后就会误判成原生直连，而 /stream/ 永远是原文件，
	// 结果画面正常、音轨依旧无声。缓存就绪时页面会自己走预转码，那条「没声音」
	// 的提示在非原生模式下是隐藏的，不需要在这里替它消音。
	// 页面没有 ETag/Last-Modified，浏览器会启发式缓存，导致播放页更新后仍是旧版
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "play.html", data); err != nil {
		warnf("渲染播放页失败：%v", err)
	}
}

// helperPort 从 [launch].listen 里取出端口，供页面拼 helper 地址。
func helperPort(listen string) string {
	if _, p, err := net.SplitHostPort(listen); err == nil {
		return p
	}
	return "9876"
}

// handleCmd 返回一行可直接粘贴的播放命令（文档 10.4，helper 的降级通道）。
func (s *Server) handleCmd(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/api/cmd/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "id 必须是 16 位十六进制")
		return
	}
	it, err := s.db.GetItem(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if it == nil {
		writeErr(w, http.StatusNotFound, "条目不存在")
		return
	}

	host := r.Host
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	auth := ""
	if s.cfg.Server.AuthUser != "" {
		auth = s.cfg.Server.AuthUser + ":" + s.cfg.Server.AuthPass + "@"
	}
	base := fmt.Sprintf("%s://%s%s", scheme, auth, host)

	player := "mpv"
	if len(s.cfg.Launch.Players) > 0 && s.cfg.Launch.Players[0].Command != "" {
		player = s.cfg.Launch.Players[0].Command
	}
	cmd := fmt.Sprintf(`%s "%s/stream/%s"`, player, base, id)
	// 挑第一条网页/外部播放器能用的字幕（内嵌轨 mpv 自己会加载，不必显式指定）
	for _, sd := range s.subsFor(*it) {
		if sd.URL != "" && sd.Src != "embed" {
			cmd += fmt.Sprintf(` --sub-file="%s%s"`, base, sd.URL)
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"command":     cmd,
		"has_auth":    auth != "",
		"auth_in_url": "凭据内嵌在 URL 中，注意命令会出现在 shell 历史里",
	})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
