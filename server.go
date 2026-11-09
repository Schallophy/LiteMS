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

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/items", s.handleItems)
	mux.HandleFunc("/api/items/", s.handleItemJSON)
	mux.HandleFunc("/api/continue", s.handleContinue)
	mux.HandleFunc("/api/recent", s.handleRecent)
	mux.HandleFunc("/api/progress", s.handleProgress)
	mux.HandleFunc("/api/progress/", s.handleProgressGet)
	mux.HandleFunc("/api/cmd/", s.handleCmd)
	// 第三方前端库：build 时打进二进制，局域网离线可用
	vendor, err := fs.Sub(webFS, "web/vendor")
	if err != nil {
		return err // 该函数只返回 error（RunServer 内部装配 mux）
	}
	mux.Handle("/vendor/", http.StripPrefix("/vendor/", http.FileServer(http.FS(vendor))))
	mux.HandleFunc("/stream/", s.handleStream)
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
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	OriginalTitle string     `json:"original_title,omitempty"`
	Year          int        `json:"year,omitempty"`
	Kind          string     `json:"kind"`
	Season        int        `json:"season,omitempty"`
	Episode       int        `json:"episode,omitempty"`
	RuntimeMin    int        `json:"runtime_min,omitempty"`
	Rating        float64    `json:"rating,omitempty"`
	Overview      string     `json:"overview,omitempty"`
	Poster        string     `json:"poster,omitempty"`
	Fanart        string     `json:"fanart,omitempty"`
	Subs          []SubEntry `json:"subs,omitempty"`
	Finished      bool       `json:"finished"`
	PositionMS    int64      `json:"position_ms,omitempty"`
	DurationMS    int64      `json:"duration_ms,omitempty"`
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
		Subs: it.SubPaths,
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
	writeJSON(w, http.StatusOK, s.toDTO(*it))
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

// handleSub 返回外挂字幕，支持 ?lang=chs。
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
	if len(it.SubPaths) == 0 {
		writeErr(w, http.StatusNotFound, "该条目没有外挂字幕")
		return
	}
	want := NormalizeLang(r.URL.Query().Get("lang"))
	pick := it.SubPaths[0]
	if want != "" {
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
	dir := filepath.Dir(filepath.Join(s.roots[it.RootIdx], filepath.FromSlash(it.RelPath)))
	full := filepath.Join(dir, filepath.FromSlash(pick.Path))
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
		vtt, err := toVTT(raw, ext, pick.Lang)
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
		Title: s.cfg.UI.Title, Theme: s.cfg.UI.Theme,
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
		Title: s.cfg.UI.Title, Theme: s.cfg.UI.Theme, Item: &d,
		Detail:      detailFor(s.roots, *it),
		HelperToken: s.cfg.Launch.Token, HelperPort: helperPort(s.cfg.Launch.Listen),
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
		Title:     s.cfg.UI.Title,
		Theme:     s.cfg.UI.Theme,
		Item:      &d,
		AudioHint: audioHint(it.RelPath, it.Container),
	}
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
	if len(it.SubPaths) > 0 {
		cmd += fmt.Sprintf(` --sub-file="%s/sub/%s?lang=%s"`, base, id, it.SubPaths[0].Lang)
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
