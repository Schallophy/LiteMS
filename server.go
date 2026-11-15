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
	"net/url"
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
	mux.HandleFunc("/api/transcodes", s.handleTranscodesJSON)
	mux.HandleFunc("/api/transcodes/", s.handleTranscodesAdmin)
	mux.HandleFunc("/api/live/", s.handleLive)
	mux.HandleFunc("/api/sub/", s.handleSubUpload)
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
	mux.HandleFunc("/transcodes", s.handleTranscodesPage)
	mux.HandleFunc("/transcodes/", s.handleTranscodesPage)
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

// rootPath 返回条目所属的媒体根。配置里删掉/重排根之后，库里可能残留
// 旧 root_idx 的条目，所有取路径的地方都必须先经过这里，不能直接下标。
func (s *Server) rootPath(it Item) (string, bool) {
	if it.RootIdx < 0 || it.RootIdx >= len(s.roots) {
		return "", false
	}
	return s.roots[it.RootIdx], true
}

// requireRoot 对根越界的条目直接回 404，避免 handler 后续 panic。
func (s *Server) requireRoot(w http.ResponseWriter, it Item) bool {
	if _, ok := s.rootPath(it); !ok {
		writeErr(w, http.StatusNotFound, "条目所属的媒体根已不在配置中")
		return false
	}
	return true
}

// artworkExists 校验图片文件真的在盘上（>512 字节，与刮削时的判定一致）。
func (s *Server) artworkExists(it Item, rel string) bool {
	root, ok := s.rootPath(it)
	if !ok {
		return false
	}
	fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
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
	if !s.requireRoot(w, *it) {
		return
	}
	full := s.itemPath(*it)
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
	it, err := s.db.GetItem(id)
	if err != nil || it == nil {
		writeErr(w, http.StatusNotFound, "条目不存在")
		return
	}
	if !s.requireRoot(w, *it) {
		return
	}
	track := trackParam(r)
	src := s.itemPath(*it)
	if !s.tc.Usable(id, track, src) {
		writeErr(w, http.StatusNotFound, "还没有这条音轨的缓存")
		return
	}
	w.Header().Set("Content-Type", "audio/mp4")
	http.ServeFile(w, r, s.tc.CachePath(id, track))
}

// trackParam 取 ?track=N（ffmpeg 音轨序号，0 起）；缺省或非法按 0。
func trackParam(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("track"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// handleLive 开/复用实时转码会话，返回 HLS 播放地址。
//
//	/api/live/{id}?start=秒&track=N
//
// 同一时间点、同一条音轨重复请求复用现有会话；换轨或拖动进度会重启 ffmpeg。
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
	if !s.requireRoot(w, *it) {
		return
	}
	start, _ := strconv.ParseFloat(r.URL.Query().Get("start"), 64)
	track := trackParam(r)
	if track > 0 {
		if tracks, _ := s.audioTracks(*it); track >= len(tracks) {
			writeErr(w, http.StatusBadRequest, "没有这条音轨")
			return
		}
	}
	src := s.itemPath(*it)

	// ?ctl=pause|resume|ping|stop：同步 ffmpeg 的暂停/继续，暂停期间靠 ping
	// 续命（只 touch、不 SIGCONT），切回直连时 stop 立刻收掉 ffmpeg。
	if ctl := r.URL.Query().Get("ctl"); ctl != "" {
		tok := r.URL.Query().Get("token")
		var ok bool
		switch ctl {
		case "ping":
			ok = s.hls.Ping(id, tok)
		case "pause", "resume":
			ok = s.hls.Pause(id, tok, ctl == "pause")
		case "stop":
			ok = s.hls.Stop(id, tok)
		default:
			writeErr(w, http.StatusBadRequest, "ctl 只支持 pause/resume/ping/stop")
			return
		}
		if !ok {
			writeErr(w, http.StatusNotFound, "转码会话不存在或已过期")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	sess, err := s.hls.Start(id, src, start, track)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	// 等第一段出来再返回，避免播放器拿着空播放列表反复重试；
	// 等不到说明 ffmpeg 起不来（源坏了、编码不支持……），别给一个死链接。
	if !waitFile(filepath.Join(sess.Dir, "video", "index.m3u8"), 8*time.Second) {
		s.hls.Stop(id, sess.Token)
		writeErr(w, http.StatusServiceUnavailable, "实时转码没有产出分段，检查 ffmpeg 是否正常")
		return
	}
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
	// 已滑出窗口/ffmpeg 已退出时不会再出现，别每一次都空等 8 秒
	if sess.segmentGone(name) {
		writeErr(w, http.StatusNotFound, "分段已不存在")
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
	if !s.requireRoot(w, *it) {
		return
	}
	track := trackParam(r)
	tracks, _ := s.audioTracks(*it)
	if track >= len(tracks) {
		writeErr(w, http.StatusBadRequest, "没有这条音轨")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.tc.Status(id, track))
	case http.MethodPost:
		src := s.itemPath(*it)
		force := r.URL.Query().Get("force") == "1"
		job, err := s.tc.Start(id, track, src, force)
		if err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, job)
	case http.MethodDelete:
		if !s.tc.Cancel(id, track) {
			writeErr(w, http.StatusConflict, "没有正在进行的转码任务")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "只支持 GET/POST/DELETE")
	}
}

// ---------- 音轨转码管理 ----------

// tcRow 是管理页的一行：运行中带进度，已缓存带体积与完成时间。
type tcRow struct {
	ID          string `json:"id"`
	Track       int    `json:"track"`
	Title       string `json:"title"`
	Link        bool   `json:"-"`
	TrackLabel  string `json:"track_label"`
	State       string `json:"state,omitempty"`
	ProgressPct int    `json:"progress_pct"`
	Size        string `json:"size,omitempty"`
	When        string `json:"when,omitempty"`
}

// tcRowFor 填片名与音轨描述。条目或音轨探测不到时退回序号，不挡管理操作。
func (s *Server) tcRowFor(id string, track int) tcRow {
	row := tcRow{ID: id, Track: track, Title: "未知条目", TrackLabel: fmt.Sprintf("音轨 %d", track+1)}
	if it, err := s.db.GetItem(id); err == nil && it != nil {
		row.Title, row.Link = it.Title, true
		tracks, _ := s.audioTracks(*it)
		if track >= 0 && track < len(tracks) {
			row.TrackLabel = audioTrackLabel(tracks[track])
		}
	}
	return row
}

func (s *Server) transcodeRunningRows() []tcRow {
	jobs := s.tc.Running()
	rows := make([]tcRow, 0, len(jobs))
	for _, j := range jobs {
		row := s.tcRowFor(j.ID, j.Track)
		row.State = j.State
		row.ProgressPct = int(j.Progress*100 + 0.5)
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) transcodeCachedRows() ([]tcRow, int64) {
	files := s.tc.Cached()
	rows := make([]tcRow, 0, len(files))
	var total int64
	for _, f := range files {
		row := s.tcRowFor(f.ID, f.Track)
		row.Size = humanSize(f.Size)
		row.When = f.Mod.Format("2006-01-02 15:04")
		total += f.Size
		rows = append(rows, row)
	}
	return rows, total
}

func humanSize(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := [...]string{"KB", "MB", "GB", "TB"}
	f := float64(n) / 1024
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if f >= 100 {
		return fmt.Sprintf("%.0f %s", f, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// handleTranscodesJSON 给管理页轮询用（运行中任务的进度会变）。
func (s *Server) handleTranscodesJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 GET")
		return
	}
	cached, total := s.transcodeCachedRows()
	writeJSON(w, http.StatusOK, map[string]any{
		"running":   s.transcodeRunningRows(),
		"cached":    cached,
		"total":     humanSize(total),
		"cache_dir": s.tc.Dir(),
	})
}

// handleTranscodesAdmin 管理页的打断 / 删除：
//
//	DELETE /api/transcodes/{id}?track=N            打断运行中的任务
//	DELETE /api/transcodes/{id}?track=N&purge=1    打断（如有）并删除缓存文件
func (s *Server) handleTranscodesAdmin(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(r.URL.Path, "/api/transcodes/")
	if !ok {
		writeErr(w, http.StatusBadRequest, "id 必须是 16 位十六进制")
		return
	}
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 DELETE")
		return
	}
	track := trackParam(r)
	if r.URL.Query().Get("purge") == "1" {
		cancelled, removed := s.tc.Remove(id, track)
		if !cancelled && !removed {
			writeErr(w, http.StatusNotFound, "这条音轨没有转码任务或缓存")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cancelled": cancelled, "removed": removed})
		return
	}
	if !s.tc.Cancel(id, track) {
		writeErr(w, http.StatusConflict, "没有正在进行的转码任务")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTranscodesPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/transcodes" && r.URL.Path != "/transcodes/" {
		http.NotFound(w, r)
		return
	}
	cached, total := s.transcodeCachedRows()
	data := pageData{
		Title: s.cfg.UI.Title, Theme: s.pageTheme(),
		TranscodeRunning: s.transcodeRunningRows(),
		TranscodeCached:  cached,
		TranscodeTotal:   humanSize(total),
		TranscodeDir:     s.tc.Dir(),
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, "transcodes.html", data); err != nil {
		warnf("渲染转码管理页失败：%v", err)
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
	if !s.requireRoot(w, *it) {
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
		if !s.requireRoot(w, *it) {
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
		root, _ := s.rootPath(*it)
		full := filepath.Join(root, filepath.FromSlash(rel))
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
	// AudioHint 详情页/播放页用：音轨可能放不出声的提示等级（bad/maybe/""）
	AudioHint string
	// AudioCodec 默认音轨的编码显示名（实测；探测失败为空）
	AudioCodec string
	// AudioMulti 容器里有多条音轨（详情页据此提示可以换轨）
	AudioMulti bool
	// AudioOK 默认音轨能被浏览器直接解码（ffprobe 实测）
	AudioOK bool
	// AudioTracks 容器里的全部音轨；AudioDefault 是默认轨的序号
	AudioTracks  []AudioTrackDTO
	AudioDefault int
	// AudioReady 默认音轨的预转码缓存已就绪（/audio/{id} 可用）
	AudioReady bool
	// LiveReady 实时转码可用（ffmpeg 已安装）
	LiveReady bool
	// Subs 是合并后的字幕清单（外挂/导入/内嵌），详情页与播放页共用
	Subs []SubDTO
	// 转码管理页：运行中任务、已缓存音轨与汇总
	TranscodeRunning []tcRow
	TranscodeCached  []tcRow
	TranscodeTotal   string
	TranscodeDir     string
}

// AudioTrackDTO 是播放页「声音」面板里的一条音轨。
type AudioTrackDTO struct {
	N        int
	Label    string
	Lang     string
	Codec    string
	Channels int
	OK       bool // 浏览器能直接解码
	Ready    bool // 预转码 m4a 已就绪
	Default  bool // 容器标记的默认轨
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
	if _, ok := s.rootPath(*it); !ok {
		http.NotFound(w, r)
		return
	}
	d := s.toDTO(*it)
	// 提示以容器里的实际音轨为准；探测失败才退回文件名标签
	hint := audioHint(it.RelPath, it.Container)
	codecLabel := ""
	tracks, probed := s.audioTracks(*it)
	if probed {
		if dt, ok := defaultAudioTrack(tracks); ok && !browserAudioOK(dt.Codec) {
			codecLabel = audioCodecLabel(dt.Codec)
			if lvl := audioHint(strings.ToLower(dt.Codec)); lvl != "" {
				hint = lvl
			} else if hint == "" {
				hint = "maybe"
			}
		} else {
			hint = ""
		}
	}
	data := pageData{
		Title: s.cfg.UI.Title, Theme: s.pageTheme(), Item: &d,
		Detail:      detailFor(s.roots, *it),
		HelperToken: s.cfg.Launch.Token, HelperPort: helperPort(s.cfg.Launch.Listen),
		AudioHint: hint, AudioCodec: codecLabel, AudioMulti: len(tracks) > 1,
		Subs: s.subsFor(*it),
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
	if _, ok := s.rootPath(*it); !ok {
		http.NotFound(w, r)
		return
	}
	d := s.toDTO(*it)
	// 真实音轨能直接播时，文件名标签的提示就不作数了。
	audioOK := s.nativeAudioOK(*it)
	hint := audioHint(it.RelPath, it.Container)
	if audioOK {
		hint = ""
	}
	tracks, _ := s.audioTracks(*it)
	audioDefault := 0
	if dt, ok := defaultAudioTrack(tracks); ok {
		audioDefault = dt.N
	}
	// 缓存比源文件旧说明源被替换过，旧音轨不能再用（否则新画面配旧声音）
	src := s.itemPath(*it)
	atDTO := make([]AudioTrackDTO, 0, len(tracks))
	for _, t := range tracks {
		atDTO = append(atDTO, AudioTrackDTO{
			N: t.N, Label: audioTrackLabel(t), Lang: t.Lang, Codec: t.Codec,
			Channels: t.Channels, OK: browserAudioOK(t.Codec),
			Ready: s.tc.Usable(id, t.N, src), Default: t.Default,
		})
	}
	data := pageData{
		Title:        s.cfg.UI.Title,
		Theme:        s.pageTheme(),
		Item:         &d,
		AudioHint:    hint,
		AudioOK:      audioOK,
		AudioTracks:  atDTO,
		AudioDefault: audioDefault,
		AudioReady:   s.tc.Usable(id, audioDefault, src),
		LiveReady:    ffmpegReady(),
		Subs:         s.subsFor(*it),
	}
	// 注意：转码缓存就绪不代表原生音轨能解——/stream/ 永远给原文件，不能因为
	// 缓存存在就把提示抹掉；这里只抹掉「实测能直放」的情况。
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
	if !s.requireRoot(w, *it) {
		return
	}

	host := r.Host
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// 凭据要按 URL 组件转义：密码里的 @ / : 直接拼接会把地址截断
	u := &url.URL{Scheme: scheme, Host: host}
	if s.cfg.Server.AuthUser != "" {
		u.User = url.UserPassword(s.cfg.Server.AuthUser, s.cfg.Server.AuthPass)
	}
	base := u.String()
	auth := u.User != nil

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
		"has_auth":    auth,
		"auth_in_url": "凭据内嵌在 URL 中，注意命令会出现在 shell 历史里",
	})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
