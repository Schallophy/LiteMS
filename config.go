package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config 对应 config.toml 的全部字段。字段名与文档第 11 章一一对应。
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Net      NetConfig      `toml:"net"`
	Library  LibraryConfig  `toml:"library"`
	Database DatabaseConfig `toml:"database"`
	Scrape   ScrapeConfig   `toml:"scrape"`
	UI       UIConfig       `toml:"ui"`
	Watch    WatchConfig    `toml:"watch"`
	Launch   LaunchConfig   `toml:"launch"`
}

type ServerConfig struct {
	Listen         string `toml:"listen"`
	AuthUser       string `toml:"auth_user"`
	AuthPass       string `toml:"auth_pass"`
	ReadHeaderSec  int    `toml:"read_header_timeout_sec"`
	IdleTimeoutSec int    `toml:"idle_timeout_sec"`
}

type NetConfig struct {
	// "auto" 探测网关；"" 用系统；"IP:53" 写死。见文档 13.2.5
	DNS string `toml:"dns"`
	// "socks5://127.0.0.1:7890" 或 "http://..."
	Proxy string `toml:"proxy"`
}

type LibraryConfig struct {
	Root       string   `toml:"root"`
	ExtraRoots []string `toml:"extra_roots"`
	VideoExt   []string `toml:"video_ext"`
	SubExt     []string `toml:"sub_ext"`
}

type DatabaseConfig struct {
	Path string `toml:"path"`
}

type ScrapeConfig struct {
	Provider      string   `toml:"provider"`
	OMDBKey       string   `toml:"omdb_key"`
	TMDBToken     string   `toml:"tmdb_token"`
	TMDBKey       string   `toml:"tmdb_key"`
	TMDBBase      string   `toml:"tmdb_base"`
	TMDBImageBase string   `toml:"tmdb_image_base"`
	DoubanEnrich  bool     `toml:"douban_enrich"`
	Language      string   `toml:"language"`
	FallbackLang  string   `toml:"fallback_language"`
	Concurrency   int      `toml:"concurrency"`
	IntervalMS    int      `toml:"interval_ms"`
	DailyLimit    int      `toml:"daily_limit"`
	WriteNFO      bool     `toml:"write_nfo"`
	WriteImages   bool     `toml:"write_images"`
	PosterSize    string   `toml:"poster_size"`
	FanartSize    string   `toml:"fanart_size"`
	TitleSimMin   float64  `toml:"title_sim_min"`
	SubLangs      []string `toml:"sub_langs"`
}

type UIConfig struct {
	Title    string `toml:"title"`
	Theme    string `toml:"theme"`
	PageSize int    `toml:"page_size"`
}

type WatchConfig struct {
	Enabled     bool `toml:"enabled"`
	IntervalMin int  `toml:"interval_min"`
}

type LaunchConfig struct {
	Listen  string         `toml:"listen"`
	Server  string         `toml:"server"`
	Token   string         `toml:"token"`
	Players []PlayerConfig `toml:"players"`
}

type PlayerConfig struct {
	Name    string   `toml:"name"`
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
}

// DefaultConfig 返回一份可运行的默认配置，其中的路径按当前系统推导。
func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		Server: ServerConfig{
			Listen:         "0.0.0.0:8080",
			ReadHeaderSec:  10,
			IdleTimeoutSec: 120,
		},
		Net: NetConfig{DNS: "auto"},
		Library: LibraryConfig{
			Root:     filepath.Join(home, "Media"),
			VideoExt: []string{".mkv", ".mp4", ".avi", ".ts", ".m4v", ".mov", ".wmv"},
			SubExt:   []string{".srt", ".ass", ".ssa", ".vtt"},
		},
		Database: DatabaseConfig{
			Path: filepath.Join(home, ".config", "litems", "index.db"),
		},
		Scrape: ScrapeConfig{
			Provider: "tmdb",
			// 实测 api.themoviedb.org 被 DNS 污染，而同一后端的 api.tmdb.org 直连可用（文档 13.2）
			TMDBBase:      "https://api.tmdb.org/3",
			TMDBImageBase: "https://image.tmdb.org/t/p",
			DoubanEnrich:  true,
			Language:      "zh-CN",
			FallbackLang:  "en-US",
			Concurrency:   2,
			IntervalMS:    200,
			DailyLimit:    1000,
			WriteNFO:      true,
			WriteImages:   true,
			PosterSize:    "w500",
			FanartSize:    "w1280",
			TitleSimMin:   0.75,
			SubLangs:      []string{"chs", "cht", "eng"},
		},
		UI: UIConfig{
			Title:    "我的媒体库",
			Theme:    "dark",
			PageSize: 60,
		},
		Watch: WatchConfig{Enabled: true, IntervalMin: 30},
		Launch: LaunchConfig{
			Listen: "127.0.0.1:9876",
		},
	}
}

// DefaultConfigPath 按文档第 11 章给出平台约定路径。
func DefaultConfigPath() string {
	// 兼容旧变量名，升级后已有脚本不至于失效
	for _, k := range []string{"LITEMS_CONFIG", "MEDIASRV_CONFIG"} {
		if p := os.Getenv(k); p != "" {
			return p
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(home, ".config", "litems", "config.toml")
}

// LoadConfig 读配置文件；文件不存在时返回默认配置（不报错，便于首次运行）。
func LoadConfig(path string) (*Config, bool, error) {
	cfg := DefaultConfig()
	if path == "" {
		path = DefaultConfigPath()
	}
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg.expandPaths()
			return cfg, false, nil
		}
		return nil, false, err
	}
	if fi.IsDir() {
		return nil, false, fmt.Errorf("%s 是目录，不是配置文件", path)
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return nil, false, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	cfg.applyDefaults()
	cfg.expandPaths()
	return cfg, true, nil
}

// applyDefaults 补齐 TOML 中缺省的零值字段。
func (c *Config) applyDefaults() {
	d := DefaultConfig()
	if c.Server.Listen == "" {
		c.Server.Listen = d.Server.Listen
	}
	if c.Server.ReadHeaderSec <= 0 {
		c.Server.ReadHeaderSec = d.Server.ReadHeaderSec
	}
	if c.Server.IdleTimeoutSec <= 0 {
		c.Server.IdleTimeoutSec = d.Server.IdleTimeoutSec
	}
	if c.Net.DNS == "" {
		c.Net.DNS = d.Net.DNS
	}
	if c.Library.Root == "" {
		c.Library.Root = d.Library.Root
	}
	if len(c.Library.VideoExt) == 0 {
		c.Library.VideoExt = d.Library.VideoExt
	}
	if len(c.Library.SubExt) == 0 {
		c.Library.SubExt = d.Library.SubExt
	}
	if c.Database.Path == "" {
		c.Database.Path = d.Database.Path
	}
	if c.Scrape.Provider == "" {
		c.Scrape.Provider = d.Scrape.Provider
	}
	if c.Scrape.TMDBBase == "" {
		c.Scrape.TMDBBase = d.Scrape.TMDBBase
	}
	if c.Scrape.TMDBImageBase == "" {
		c.Scrape.TMDBImageBase = d.Scrape.TMDBImageBase
	}
	if c.Scrape.IntervalMS <= 0 {
		c.Scrape.IntervalMS = d.Scrape.IntervalMS
	}
	if c.Scrape.Concurrency <= 0 {
		c.Scrape.Concurrency = d.Scrape.Concurrency
	}
	if c.Scrape.TitleSimMin <= 0 {
		c.Scrape.TitleSimMin = d.Scrape.TitleSimMin
	}
	if c.Scrape.Language == "" {
		c.Scrape.Language = d.Scrape.Language
	}
	if c.Scrape.FallbackLang == "" {
		c.Scrape.FallbackLang = d.Scrape.FallbackLang
	}
	if c.Scrape.PostersEmpty() {
		c.Scrape.PosterSize = d.Scrape.PosterSize
		c.Scrape.FanartSize = d.Scrape.FanartSize
	}
	if c.UI.PageSize <= 0 {
		c.UI.PageSize = d.UI.PageSize
	}
	if c.Watch.IntervalMin <= 0 {
		c.Watch.IntervalMin = d.Watch.IntervalMin
	}
	if c.Launch.Listen == "" {
		c.Launch.Listen = d.Launch.Listen
	}
	for i, e := range c.Library.VideoExt {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" && !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		c.Library.VideoExt[i] = e
	}
	for i, e := range c.Library.SubExt {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" && !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		c.Library.SubExt[i] = e
	}
}

// PostersEmpty 报告两个图片尺寸是否都未配置。
func (s ScrapeConfig) PostersEmpty() bool {
	return s.PosterSize == "" && s.FanartSize == ""
}

// expandPaths 展开 ~ 并转绝对路径。
func (c *Config) expandPaths() {
	c.Library.Root = ExpandPath(c.Library.Root)
	for i := range c.Library.ExtraRoots {
		c.Library.ExtraRoots[i] = ExpandPath(c.Library.ExtraRoots[i])
	}
	c.Database.Path = ExpandPath(c.Database.Path)
}

// ExpandPath 展开 ~（文档 11 章：仅支持 ~，不做环境变量插值）。
func ExpandPath(p string) string {
	if p == "" {
		return p
	}
	if p == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// AllRoots 返回 [主根] + extra_roots，去重保序。
func (c *Config) AllRoots() []string {
	out := []string{c.Library.Root}
	for _, r := range c.Library.ExtraRoots {
		if r == "" {
			continue
		}
		dup := false
		for _, e := range out {
			if e == r {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, r)
		}
	}
	return out
}

// isSharedStorage 判断路径是否落在 Android 共享存储（FUSE）上。
func isSharedStorage(p string) bool {
	p = filepath.ToSlash(p)
	for _, prefix := range []string{"/sdcard", "/storage/emulated", "/mnt/sdcard", "/storage/self"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// Validate 做启动前校验。返回的 error 里 warning 为警告，fatal 为必须终止。
func (c *Config) Validate(forLaunch bool) (warnings []string, fatals []string) {
	if isSharedStorage(c.Database.Path) {
		fatals = append(fatals,
			fmt.Sprintf("[database].path 位于共享存储（FUSE）：%s —— SQLite 在此会上锁失败甚至损坏，必须改到 $HOME 下", c.Database.Path))
	}
	if isSharedStorage(c.Library.Root) {
		warnings = append(warnings,
			fmt.Sprintf("[library].root 位于共享存储（FUSE）：%s —— 可用但 sendfile/inotify 退化为用户态，性能下降", c.Library.Root))
	}
	switch c.Scrape.Provider {
	case "omdb":
		if c.Scrape.OMDBKey == "" && c.Scrape.WriteNFO {
			warnings = append(warnings, "[scrape].provider=omdb 但 omdb_key 为空：scrape 会直接报错。")
		}
	case "tmdb":
		if c.Scrape.TMDBToken == "" && c.Scrape.TMDBKey == "" && c.Scrape.WriteNFO {
			warnings = append(warnings, "[scrape].provider=tmdb 但 tmdb_token 与 tmdb_key 都为空：scrape 会直接报错。申请见 https://www.themoviedb.org/settings/api")
		}
		if strings.Contains(c.Scrape.TMDBBase, "api.themoviedb.org") {
			warnings = append(warnings, "[scrape].tmdb_base 用的是 api.themoviedb.org：该域名在国内常被 DNS 污染，建议改成 https://api.tmdb.org/3（见文档 13.2）")
		}
	default:
		fatals = append(fatals, fmt.Sprintf("[scrape].provider=%q 不认识，可选 tmdb / omdb", c.Scrape.Provider))
	}
	if forLaunch && c.Launch.Token == "" {
		fatals = append(fatals, "[launch].token 为空：helper 拒绝启动（否则任意网页都能拉起播放器）")
	}
	if forLaunch && len(c.Launch.Players) == 0 {
		warnings = append(warnings, "[launch].players 为空：helper 起来也没有播放器可选")
	}
	if _, err := os.Stat(c.Library.Root); err != nil {
		warnings = append(warnings, fmt.Sprintf("[library].root 当前不可访问：%s（%v）", c.Library.Root, err))
	}
	return warnings, fatals
}
