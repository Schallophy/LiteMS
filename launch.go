package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

var reHexID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Launcher 是 PC 侧的播放器 helper（文档 10）。
type Launcher struct {
	cfg    *Config
	player PlayerConfig
	server string
	token  string

	mu   sync.Mutex
	proc *exec.Cmd
}

// RunLaunch 启动 helper。只允许监听回环地址（文档 10.6）。
func RunLaunch(cfg *Config, playerName string) error {
	host, _, err := net.SplitHostPort(cfg.Launch.Listen)
	if err != nil {
		return fmt.Errorf("[launch].listen 格式不对：%q（%v）", cfg.Launch.Listen, err)
	}
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		return fmt.Errorf("[launch].listen 必须是回环地址，当前 %q —— helper 不允许暴露到局域网", host)
	}
	if cfg.Launch.Token == "" {
		return fmt.Errorf("[launch].token 为空：任意网页都能拉起播放器，拒绝启动")
	}
	if len(cfg.Launch.Players) == 0 {
		return fmt.Errorf("[launch].players 为空，没有可用播放器")
	}

	pick := cfg.Launch.Players[0]
	if playerName != "" {
		found := false
		for _, p := range cfg.Launch.Players {
			if p.Name == playerName {
				pick, found = p, true
				break
			}
		}
		if !found {
			return fmt.Errorf("配置里没有名为 %q 的播放器", playerName)
		}
	}
	if _, err := exec.LookPath(pick.Command); err != nil {
		warnf("找不到播放器可执行文件 %q，点击播放会失败（仍可用「复制命令」）", pick.Command)
	}

	l := &Launcher{
		cfg:    cfg,
		player: pick,
		server: strings.TrimRight(cfg.Launch.Server, "/"),
		token:  cfg.Launch.Token,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/play", l.handlePlay)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              cfg.Launch.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	logf("helper 已启动  http://%s", cfg.Launch.Listen)
	logf("服务端         %s", l.server)
	logf("播放器         %s (%s)", pick.Name, pick.Command)
	logf("在浏览器中点击播放即可调用本地播放器；若无效，请用页面上的「复制命令」。")
	if l.server == "" {
		warnf("[launch].server 为空，播放命令里的地址会拼不出来")
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		return err
	case <-sig:
		l.mu.Lock()
		if l.proc != nil && l.proc.Process != nil {
			_ = l.proc.Process.Signal(syscall.SIGTERM)
		}
		l.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return srv.Shutdown(ctx)
	}
}

type playRequest struct {
	ID    string `json:"id"`
	Start int    `json:"start"`
	Token string `json:"token"`
	Lang  string `json:"lang"`
}

// handlePlay 是唯一有副作用的端点，必须三道校验（文档 10.2）。
func (l *Launcher) handlePlay(w http.ResponseWriter, r *http.Request) {
	// 预检：浏览器从私网页面访问 localhost 必须先过这一步（文档 10.3）
	if r.Method == http.MethodOptions {
		origin := r.Header.Get("Origin")
		if origin != "" && !l.originAllowed(origin) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "content-type")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}

	origin := r.Header.Get("Origin")
	if origin != "" {
		if !l.originAllowed(origin) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}

	var body playRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
		http.Error(w, "请求体不是合法 JSON", http.StatusBadRequest)
		return
	}
	if body.Token != l.token {
		http.Error(w, "token 不匹配", http.StatusForbidden)
		return
	}
	if !reHexID.MatchString(body.ID) {
		http.Error(w, "id 必须是 16 位十六进制", http.StatusBadRequest)
		return
	}

	url, sub := l.buildURLs(body.ID, body.Lang)
	args, err := renderArgs(l.player.Args, map[string]string{
		"url":   url,
		"sub":   sub,
		"start": fmt.Sprint(maxInt(body.Start, 0)),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// 已有一个在跑就先收掉，避免连点开出多个窗口
	if l.proc != nil && l.proc.Process != nil {
		_ = l.proc.Process.Signal(syscall.SIGTERM)
	}
	cmd := exec.Command(l.player.Command, args...)
	cmd.Env = safeEnv()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		http.Error(w, "启动播放器失败："+err.Error(), http.StatusInternalServerError)
		return
	}
	l.proc = cmd
	go func(c *exec.Cmd) { _ = c.Wait() }(cmd)

	w.WriteHeader(http.StatusNoContent)
}

// originAllowed 只允许来自配置中 server 的同源请求。
func (l *Launcher) originAllowed(origin string) bool {
	if l.server == "" {
		return false
	}
	s := l.server
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s = strings.TrimRight(s, "/")
	return strings.EqualFold(origin, "http://"+s) || strings.EqualFold(origin, "https://"+s)
}

// buildURLs 拼出视频与字幕地址；配置了认证时把凭据内嵌进 URL。
func (l *Launcher) buildURLs(id, lang string) (video, sub string) {
	base := l.server
	prefix := ""
	if l.cfg.Server.AuthUser != "" {
		if i := strings.Index(base, "://"); i >= 0 {
			prefix = l.cfg.Server.AuthUser + ":" + l.cfg.Server.AuthPass + "@"
			base = base[:i+3] + prefix + base[i+3:]
		}
	}
	video = base + "/stream/" + id
	sub = base + "/sub/" + id
	if lang != "" {
		sub += "?lang=" + lang
	}
	return video, sub
}

// renderArgs 替换模板占位符。空值的参数项整条剔除（文档 10.5）。
func renderArgs(tpl []string, vars map[string]string) ([]string, error) {
	out := make([]string, 0, len(tpl))
	for _, raw := range tpl {
		hasEmpty := false
		res := raw
		for k, v := range vars {
			ph := "{" + k + "}"
			if !strings.Contains(res, ph) {
				continue
			}
			if v == "" || (k == "start" && v == "0") {
				hasEmpty = true
				break
			}
			res = strings.ReplaceAll(res, ph, v)
		}
		if hasEmpty {
			continue
		}
		out = append(out, res)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("模板渲染后参数为空，检查 [launch.players].args")
	}
	return out, nil
}

// safeEnv 只传出播放器必需的少量环境变量（文档 10.6）。
func safeEnv() []string {
	keep := []string{"PATH", "HOME", "LANG", "LC_ALL", "DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "TMPDIR"}
	allow := make(map[string]bool, len(keep))
	for _, k := range keep {
		allow[k] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		if i := strings.Index(kv, "="); i > 0 && allow[kv[:i]] {
			out = append(out, kv)
		}
	}
	return out
}
