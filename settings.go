package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// scrapeSettingKeys 是设置页允许写入 [scrape] 的字段白名单。
// 白名单之外一律不碰——配置文件里可能有注释、顺序和手工调过的其它键。
var scrapeSettingKeys = []string{"provider", "language", "tmdb_token", "tmdb_key", "omdb_key"}

// librarySettingKeys 是设置页允许写入 [library] 的字段白名单。
var librarySettingKeys = []string{"root", "extra_roots", "follow_file_symlinks", "download_dir", "download_mode"}

// uiSettingKeys 是设置页允许写入 [ui] 的字段白名单（主题只写这一项，标题不动）。
var uiSettingKeys = []string{"theme"}

// tomlString 把 Go 字符串序列化成 TOML 字符串字面量。
// 写回函数现在写的是「原始 TOML 右值」，字符串必须由调用方自行加引号。
func tomlString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

// tomlArray 把路径列表序列化成单行 TOML 数组。
func tomlArray(items []string) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, tomlString(it))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// parsePathLines 把多行文本拆成路径列表：去空行、去重、保序。
func parsePathLines(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// newSettingsToken 生成进程级令牌，用来挡跨站表单提交。
// 启用 Basic Auth 时浏览器会自动带上凭据，所以「同源」本身挡不住 CSRF；
// 这个令牌只出现在页面里，跨站攻击者读不到。
func newSettingsToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "settings-token-fallback"
	}
	return hex.EncodeToString(b)
}

// scrapeCreds 是设置页可改的几个字段，单独存一份快照。
type scrapeCreds struct {
	Provider  string
	Language  string
	TMDBToken string
	TMDBKey   string
	OMDBKey   string
}

func credsFromConfig(cfg *Config) scrapeCreds {
	return scrapeCreds{
		Provider:  cfg.Scrape.Provider,
		Language:  cfg.Scrape.Language,
		TMDBToken: cfg.Scrape.TMDBToken,
		TMDBKey:   cfg.Scrape.TMDBKey,
		OMDBKey:   cfg.Scrape.OMDBKey,
	}
}

func (s *Server) credsSnapshot() scrapeCreds {
	s.credsMu.RLock()
	defer s.credsMu.RUnlock()
	return s.creds
}

func (s *Server) setCreds(c scrapeCreds) {
	s.credsMu.Lock()
	s.creds = c
	s.credsMu.Unlock()
}

// pageTheme 返回当前生效的主题（auto / light / dark）。设置页改完立刻生效，
// 所以先看运行时的值，没有才回落到配置文件里的初值。
func (s *Server) pageTheme() string {
	s.themeMu.RLock()
	t := s.theme
	s.themeMu.RUnlock()
	if t != "" {
		return t
	}
	return NormalizeTheme(s.cfg.UI.Theme)
}

func (s *Server) setTheme(t string) {
	s.themeMu.Lock()
	s.theme = NormalizeTheme(t)
	s.themeMu.Unlock()
}

// settingsView 是设置页的渲染数据。
type settingsView struct {
	Title        string
	Theme        string
	Token        string
	Provider     string
	Language     string
	TMDBMasked   string
	TMDBKeyMask  string
	OMDBMasked   string
	ConfigPath   string
	ConfigExists bool
	AuthOn       bool
	LibraryRoot  string
	LibraryExtra string
	LibraryLinks bool
	DownloadDir  string
	DownloadMode string
	Message      string
	Error        string
	Probe        string
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	cfgPath := s.cfg.Path
	if cfgPath == "" {
		cfgPath = DefaultConfigPath()
	}
	cur := s.credsSnapshot()
	v := settingsView{
		Title:        s.cfg.UI.Title,
		Theme:        s.pageTheme(),
		Token:        s.settingsToken,
		Provider:     cur.Provider,
		Language:     cur.Language,
		TMDBMasked:   maskSecret(cur.TMDBToken),
		TMDBKeyMask:  maskSecret(cur.TMDBKey),
		OMDBMasked:   maskSecret(cur.OMDBKey),
		ConfigPath:   cfgPath,
		AuthOn:       s.cfg.Server.AuthUser != "",
		LibraryRoot:  s.cfg.Library.Root,
		LibraryExtra: strings.Join(s.cfg.Library.ExtraRoots, "\n"),
		LibraryLinks: s.cfg.Library.FollowFileSymlinks,
		DownloadDir:  s.cfg.Library.DownloadDir,
		DownloadMode: s.cfg.Library.DownloadMode,
	}
	if _, err := os.Stat(cfgPath); err == nil {
		v.ConfigExists = true
	}

	status := http.StatusOK
	if r.Method == http.MethodPost {
		if !s.applySettings(w, r, &v, cfgPath) {
			status = http.StatusForbidden
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := s.tpl.ExecuteTemplate(w, "settings.html", v); err != nil {
		warnf("渲染设置页失败：%v", err)
	}
}

// applySettings 处理表单提交。返回 false 表示请求被拒绝（令牌不匹配）。
func (s *Server) applySettings(w http.ResponseWriter, r *http.Request, v *settingsView, cfgPath string) bool {
	if err := r.ParseForm(); err != nil {
		v.Error = "表单解析失败"
		return true
	}
	if r.PostFormValue("token") != s.settingsToken {
		v.Error = "页面已过期，请刷新后重试"
		return false
	}

	// 空字段表示「不修改」，所以从快照打底
	cur := s.credsSnapshot()
	next := *s.cfg
	next.Scrape.Provider = cur.Provider
	next.Scrape.Language = cur.Language
	next.Scrape.TMDBToken = cur.TMDBToken
	next.Scrape.TMDBKey = cur.TMDBKey
	next.Scrape.OMDBKey = cur.OMDBKey
	if p := strings.TrimSpace(r.PostFormValue("provider")); p != "" {
		next.Scrape.Provider = p
	}
	if l := strings.TrimSpace(r.PostFormValue("language")); l != "" {
		next.Scrape.Language = l
	}
	setIfFilled(r.PostFormValue("tmdb_token"), &next.Scrape.TMDBToken)
	setIfFilled(r.PostFormValue("tmdb_key"), &next.Scrape.TMDBKey)
	setIfFilled(r.PostFormValue("omdb_key"), &next.Scrape.OMDBKey)

	if next.Scrape.Provider != "tmdb" && next.Scrape.Provider != "omdb" {
		v.Error = fmt.Sprintf("数据源只能是 tmdb 或 omdb，收到 %q", next.Scrape.Provider)
		return true
	}

	// ---- 外观 ----
	// 表单里没有这个字段（旧页面缓存）就保持原值，别把用户明确的 light/dark 冲成 auto
	if vals, ok := r.PostForm["page_theme"]; ok && len(vals) > 0 {
		theme := NormalizeTheme(vals[0])
		next.UI.Theme = theme
		v.Theme = theme
	}

	// ---- 媒体库 ----
	libRoot := strings.TrimSpace(r.PostFormValue("lib_root"))
	if libRoot == "" {
		v.Error = "媒体根目录不能为空"
		return true
	}
	if !strings.HasPrefix(libRoot, "/") && !strings.HasPrefix(libRoot, "~") {
		v.Error = "媒体根必须是绝对路径（/…）或以 ~ 开头，收到 " + libRoot
		return true
	}
	if strings.Contains(libRoot, "..") {
		v.Error = "媒体根里不允许出现 .."
		return true
	}
	libExtra := parsePathLines(r.PostFormValue("lib_extra"))
	libSymlinks := r.PostFormValue("lib_symlinks") == "on"
	dlDir := strings.TrimSpace(r.PostFormValue("dl_dir"))
	if dlDir != "" {
		if !strings.HasPrefix(dlDir, "/") && !strings.HasPrefix(dlDir, "~") {
			v.Error = "下载目录必须是绝对路径（/…）或以 ~ 开头，收到 " + dlDir
			return true
		}
		if strings.Contains(dlDir, "..") {
			v.Error = "下载目录里不允许出现 .."
			return true
		}
		// 下载目录放进库根 = 绕开镜像机制，直接往做种目录写侧车文件
		if underPath(libRoot, dlDir) || underPath(dlDir, libRoot) {
			v.Error = "下载目录不能与媒体根互相包含（否则 scrape 会往做种目录写文件）"
			return true
		}
	}
	dlMode := r.PostFormValue("dl_mode")
	if dlMode == "" {
		dlMode = "symlink"
	}
	if dlMode != "symlink" && dlMode != "copy" {
		v.Error = "镜像方式只能是 symlink 或 copy"
		return true
	}
	next.Library.Root = libRoot
	next.Library.ExtraRoots = libExtra
	next.Library.FollowFileSymlinks = libSymlinks
	next.Library.DownloadDir = dlDir
	next.Library.DownloadMode = dlMode

	v.LibraryRoot = libRoot
	v.LibraryExtra = strings.Join(libExtra, "\n")
	v.LibraryLinks = libSymlinks
	v.DownloadDir = dlDir
	v.DownloadMode = dlMode

	// 无论测试还是保存，回显都用提交后的值
	v.Provider = next.Scrape.Provider
	v.Language = next.Scrape.Language
	v.TMDBMasked = maskSecret(next.Scrape.TMDBToken)
	v.TMDBKeyMask = maskSecret(next.Scrape.TMDBKey)
	v.OMDBMasked = maskSecret(next.Scrape.OMDBKey)

	if r.PostFormValue("action") == "test" {
		s.probeSettings(r, v, &next)
		return true
	}

	scrapeVals := map[string]string{
		"provider":   tomlString(next.Scrape.Provider),
		"language":   tomlString(next.Scrape.Language),
		"tmdb_token": tomlString(next.Scrape.TMDBToken),
		"tmdb_key":   tomlString(next.Scrape.TMDBKey),
		"omdb_key":   tomlString(next.Scrape.OMDBKey),
	}
	if err := writeSectionKeys(cfgPath, "scrape", scrapeSettingKeys, scrapeVals); err != nil {
		v.Error = "写入配置文件失败：" + err.Error()
		return true
	}

	libVals := map[string]string{
		"root":                 tomlString(next.Library.Root),
		"extra_roots":          tomlArray(next.Library.ExtraRoots),
		"follow_file_symlinks": fmt.Sprintf("%t", next.Library.FollowFileSymlinks),
		"download_dir":         tomlString(next.Library.DownloadDir),
		"download_mode":        tomlString(next.Library.DownloadMode),
	}
	if err := writeSectionKeys(cfgPath, "library", librarySettingKeys, libVals); err != nil {
		v.Error = "写入配置文件失败：" + err.Error()
		return true
	}

	// 外观：主题改完立刻生效（页面每次渲染都读运行时值），不用重启
	if err := writeSectionKeys(cfgPath, "ui", uiSettingKeys, map[string]string{
		"theme": tomlString(next.UI.Theme),
	}); err != nil {
		v.Error = "写入配置文件失败：" + err.Error()
		return true
	}
	s.setTheme(next.UI.Theme)

	s.setCreds(credsFromConfig(&next))
	v.ConfigExists = true
	v.Message = "已保存到 " + cfgPath + "。下一次 litems scrape 会使用新凭据。" +
		"媒体根与下载目录的修改在重启服务后生效（重启时会自动镜像、重新扫描并补刮）。"
	return true
}

// probeSettings 用「提交中的值」跑一次连通性与凭据自检，不落盘。
func (s *Server) probeSettings(r *http.Request, v *settingsView, cfg *Config) {
	sc, err := buildScraper(cfg)
	if err != nil {
		v.Error = err.Error()
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	var buf strings.Builder
	logTo := func(format string, args ...any) {
		fmt.Fprintf(&buf, format+"\n", args...)
	}
	if err := probeScraper(ctx, sc, cfg, logTo); err != nil {
		v.Probe = buf.String()
		v.Error = "测试失败：" + err.Error()
		return
	}
	v.Probe = buf.String()
	v.Message = "连接正常。点「保存」写入配置文件后生效。"
}

// writeScrapeSettings 是兼容封装：值为纯字符串，由这里统一加引号。
// 这样手工写的注释、键顺序和不在白名单里的配置都不会被覆盖。
func writeScrapeSettings(path string, kv map[string]string) error {
	vals := make(map[string]string, len(kv))
	for k, v := range kv {
		vals[k] = tomlString(v)
	}
	return writeSectionKeys(path, "scrape", scrapeSettingKeys, vals)
}

// writeSectionKeys 按「行编辑」更新指定段的若干键。
// kv 的值是原始 TOML 右值（字符串由调用方用 tomlString 加引号，数组用 tomlArray）。
// 段内已存在的键原位替换，缺失的键按 keys 顺序补在段尾；段不存在就追加到文件末尾。
// 其余行——注释、顺序、其它段——一律不动。
func writeSectionKeys(path, section string, keys []string, kv map[string]string) error {
	if path == "" {
		return fmt.Errorf("配置路径为空")
	}
	if section == "" {
		return fmt.Errorf("段名为空")
	}
	data, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	var lines []string
	if exists {
		lines = strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		for len(lines) > 0 && strings.TrimRight(lines[len(lines)-1], " \t") == "" {
			lines = lines[:len(lines)-1]
		}
	} else {
		lines = []string{"# LiteMS 配置（由设置页生成，可手工编辑）"}
	}

	sectionLine := "[" + section + "]"
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == sectionLine {
			start = i
			break
		}
	}
	if start < 0 {
		lines = append(lines, "", sectionLine)
		start = len(lines) - 1
	}

	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			end = i
			break
		}
	}

	done := make(map[string]bool, len(kv))
	for i := start + 1; i < end; i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		eq := strings.Index(t, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(t[:eq])
		val, ok := kv[key]
		if !ok || done[key] {
			continue
		}
		lines[i] = fmt.Sprintf("%s = %s", key, val)
		done[key] = true
	}

	var missing []string
	for _, k := range keys {
		if !done[k] {
			missing = append(missing, fmt.Sprintf("%s = %s", k, kv[k]))
		}
	}
	if len(missing) > 0 {
		tail := append([]string(nil), lines[end:]...)
		lines = append(lines[:end], append(missing, tail...)...)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// setIfFilled 仅在表单字段非空时覆盖，空值代表「保持原样」。
func setIfFilled(v string, dst *string) {
	if t := strings.TrimSpace(v); t != "" {
		*dst = t
	}
}

// maskSecret 只显示首尾各四位，避免整串密钥出现在页面上。
func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 12 {
		return strings.Repeat("•", len(s))
	}
	return s[:4] + strings.Repeat("•", 6) + s[len(s)-4:]
}
