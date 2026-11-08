package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// version 由构建脚本通过 -ldflags 注入。
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "scan":
		cmdScan(args[1:])
	case "scrape":
		cmdScrape(args[1:])
	case "serve":
		cmdServe(args[1:])
	case "launch":
		cmdLaunch(args[1:])
	case "version", "-v", "--version":
		fmt.Printf("litems %s  %s/%s  %s\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`litems —— 家庭媒体库索引与流服务

用法：
  litems <command> [flags]

命令：
  scan      扫描媒体库，写入索引（不联网）
  scrape    按索引刮削元数据，写 NFO 与图片
  serve     启动 HTTP 服务
  launch    启动本地播放器 helper（PC 侧）
  version   打印版本
  help      显示本帮助

全局参数（各子命令均支持）：
  --config PATH    配置文件路径，默认 ~/.config/litems/config.toml
  --verbose        输出调试日志

示例：
  litems scan --dry-run
  litems scrape --probe
  litems scrape
  litems serve --listen 0.0.0.0:7788
`)
}

// logf 是普通日志。
func logf(format string, a ...any) {
	fmt.Printf(format+"\n", a...)
}

// warnf 是警告日志，走 stderr。
func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "警告: "+format+"\n", a...)
}

// errf 是错误日志，走 stderr。
func errf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "错误: "+format+"\n", a...)
}

// loadAndCheck 是三个子命令共用的配置加载 + 校验。
func loadAndCheck(path string, forLaunch bool) *Config {
	cfg, found, err := LoadConfig(path)
	if err != nil {
		errf("%v", err)
		os.Exit(1)
	}
	if found {
		logf("配置 %s", pathOrDefault(path))
	} else {
		logf("未找到配置文件，使用默认值（媒体根 %s）", cfg.Library.Root)
	}
	warnings, fatals := cfg.Validate(forLaunch)
	for _, w := range warnings {
		warnf("%s", w)
	}
	if len(fatals) > 0 {
		for _, f := range fatals {
			errf("%s", f)
		}
		os.Exit(1)
	}
	return cfg
}

func pathOrDefault(p string) string {
	if p == "" {
		return DefaultConfigPath()
	}
	return p
}

func cmdScan(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	dry := fs.Bool("dry-run", false, "只打印将执行的操作，不写文件不写库")
	force := fs.Bool("force", false, "忽略增量判定，全部重扫（仍尊重 lockdata）")
	onlyMissing := fs.Bool("only-missing", false, "仅处理尚未刮削与上次失败的条目")
	onlyID := fs.String("id", "", "只处理指定 ID")
	reindex := fs.Bool("reindex", false, "丢弃现有索引，从文件系统全量重建")
	_ = fs.Bool("verbose", false, "调试日志")
	fs.Usage = func() {
		fmt.Print(`litems scan [flags]

  --dry-run        只打印，不写库
  --force          忽略增量判定
  --only-missing   仅处理未刮削/失败的条目
  --id ID          只处理指定 ID
  --reindex        删库重建后再扫描
  --config PATH    配置文件路径
`)
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cfg := loadAndCheck(*cfgPath, false)

	if *reindex {
		for _, s := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(cfg.Database.Path + s); err == nil {
				logf("已删除 %s", cfg.Database.Path+s)
			}
		}
	}

	db, err := OpenDB(cfg.Database.Path)
	if err != nil {
		errf("打开数据库失败：%v", err)
		os.Exit(1)
	}
	defer db.Close()

	sc := NewScanner(cfg, db, *dry, *force, *onlyMissing, *onlyID)
	stats, err := sc.Run()
	if err != nil {
		errf("扫描失败：%v", err)
		os.Exit(1)
	}
	stats.PrintSummary(*dry)

	total, _ := db.CountItems()
	fmt.Printf("  索引条目          %d\n", total)
	fmt.Println()
	if !*dry && stats.Deleted > 0 {
		logf("提示：已有 %d 条记录从索引移除，NFO 与媒体文件未被改动", stats.Deleted)
	}
}

// buildScraper 按配置构造数据源。凭据缺失时直接报错，而不是静默降级成"只索引"。
func buildScraper(cfg *Config) (Scraper, error) {
	switch cfg.Scrape.Provider {
	case "tmdb", "":
		if cfg.Scrape.TMDBToken == "" && cfg.Scrape.TMDBKey == "" {
			return nil, fmt.Errorf("缺少凭据：请在配置里填 [scrape].tmdb_token（v4）或 tmdb_key（v3）")
		}
		return NewTMDBClient(cfg), nil
	case "omdb":
		if cfg.Scrape.OMDBKey == "" {
			return nil, fmt.Errorf("缺少凭据：请在配置里填 [scrape].omdb_key")
		}
		return NewOMDbClient(cfg), nil
	default:
		return nil, fmt.Errorf("不认识的 provider %q，可选 tmdb / omdb", cfg.Scrape.Provider)
	}
}

func cmdScrape(args []string) {
	fs := flag.NewFlagSet("scrape", flag.ExitOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	dry := fs.Bool("dry-run", false, "只列出待刮削条目，不调 API、不写文件")
	force := fs.Bool("force", false, "忽略增量判定，全部重刮（仍尊重 lockdata）")
	onlyID := fs.String("id", "", "只处理指定 ID")
	limit := fs.Int("limit", 0, "最多处理 N 条（试跑用）")
	probe := fs.Bool("probe", false, "只做一次连通性与凭据自检")
	provider := fs.String("provider", "", "覆盖 [scrape].provider（tmdb / omdb）")
	_ = fs.Bool("verbose", false, "调试日志")
	fs.Usage = func() {
		fmt.Print(`litems scrape [flags]

  --probe          只做连通性/凭据自检
  --dry-run        只列出待刮削条目，不调 API
  --force          忽略增量判定，全部重刮
  --id ID          只处理指定 ID
  --limit N        最多处理 N 条
  --provider NAME  覆盖数据源（tmdb / omdb）
  --config PATH    配置文件路径
`)
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cfg := loadAndCheck(*cfgPath, false)
	if *provider != "" {
		cfg.Scrape.Provider = *provider
	}
	if dnsDesc := SetupNet(cfg); dnsDesc != "" {
		logf("DNS %s", dnsDesc)
	}

	sc, err := buildScraper(cfg)
	if err != nil {
		// dry-run 不联网，缺凭据也允许先把待办清单打出来
		if !*dry {
			errf("%v", err)
			os.Exit(1)
		}
		warnf("%v —— dry-run 不需要凭据，继续", err)
	}
	if sc != nil {
		logf("数据源 %s  接口 %s  语言 %s", sc.Name(), cfg.Scrape.TMDBBase, cfg.Scrape.Language)
	}

	ctx := context.Background()
	if *probe {
		if sc == nil {
			errf("自检需要凭据，请先配置 [scrape] 段")
			os.Exit(1)
		}
		start := time.Now()
		if err := probeScraper(ctx, sc, cfg, logf); err != nil {
			errf("自检失败：%v", err)
			os.Exit(1)
		}
		logf("自检通过，耗时 %s", time.Since(start).Round(time.Millisecond))
		return
	}

	db, err := OpenDB(cfg.Database.Path)
	if err != nil {
		errf("打开数据库失败：%v", err)
		os.Exit(1)
	}
	defer db.Close()

	if n, _ := db.CountItems(); n == 0 {
		errf("索引为空，请先执行 `litems scan`")
		os.Exit(1)
	}

	r := NewRunner(cfg, db, sc, *dry, *force, *onlyID, *limit)
	stats, err := r.Run(ctx)
	if err != nil {
		errf("刮削中断：%v", err)
		os.Exit(1)
	}
	stats.PrintScrapeSummary(*dry)
	if !*dry && stats.Miss > 0 {
		logf("未匹配条目已写入各媒体根下的 .litems/unmatched.txt")
	}
	if !*dry && stats.OK > 0 {
		logf("提示：NFO 与图片已落盘，重启 serve 或等待下次扫描即可刷新索引")
	}
}

// probeScraper 发一次最小请求，验证凭据与网络。
// log 由调用方提供：CLI 写 stderr，设置页收集成文本显示。
func probeScraper(ctx context.Context, sc Scraper, cfg *Config, log func(string, ...any)) error {
	if c, ok := sc.(*TMDBClient); ok {
		if err := c.Probe(ctx); err != nil {
			return err
		}
		cands, err := c.Search(ctx, Query{Title: "Interstellar", Year: 2014, Kind: KindMovie})
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			return fmt.Errorf("接口通了，但搜索没有返回结果")
		}
		log("搜索 \"Interstellar\"：%d 个候选，首个是 %q（%d）", len(cands), cands[0].Title, cands[0].Year)
		meta, err := c.Detail(ctx, cands[0].Key, KindMovie)
		if err != nil {
			return err
		}
		log("详情：%q %d 年 %d 分钟 评分 %.1f，海报 %v，背景 %v",
			meta.Title, meta.Year, meta.RuntimeMin, meta.Rating, meta.PosterPath != "", meta.FanartPath != "")
		return nil
	}
	cands, err := sc.Search(ctx, Query{Title: "Interstellar", Kind: KindMovie})
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		return fmt.Errorf("接口通了，但搜索没有返回结果")
	}
	log("搜索 \"Interstellar\"：%d 个候选，首个 %q（%d）", len(cands), cands[0].Title, cands[0].Year)
	return nil
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	listen := fs.String("listen", "", "监听地址，覆盖配置")
	noWatch := fs.Bool("no-watch", false, "关闭后台定时扫描")
	_ = fs.Bool("verbose", false, "调试日志")
	fs.Usage = func() {
		fmt.Print(`litems serve [flags]

  --listen ADDR    监听地址，如 0.0.0.0:7788
  --no-watch       关闭后台定时扫描
  --config PATH    配置文件路径
`)
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cfg := loadAndCheck(*cfgPath, false)
	if *listen != "" {
		cfg.Server.Listen = *listen
	}
	if *noWatch {
		cfg.Watch.Enabled = false
	}

	db, err := OpenDB(cfg.Database.Path)
	if err != nil {
		errf("打开数据库失败：%v", err)
		os.Exit(1)
	}
	defer db.Close()

	if n, _ := db.CountItems(); n == 0 {
		warnf("索引为空，请先执行 `litems scan`")
	}

	if err := RunServer(cfg, db); err != nil {
		if isAddrInUse(err) {
			errf("端口 %s 已被占用。换个端口：serve --listen 0.0.0.0:<端口>，或改配置里的 [server].listen", cfg.Server.Listen)
		}
		errf("服务退出：%v", err)
		os.Exit(1)
	}
}

func cmdLaunch(args []string) {
	fs := flag.NewFlagSet("launch", flag.ExitOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	player := fs.String("player", "", "播放器名，默认取配置第一个")
	port := fs.String("port", "", "覆盖监听端口")
	_ = fs.Bool("verbose", false, "调试日志")
	fs.Usage = func() {
		fmt.Print(`litems launch [flags]

  --player NAME    播放器名，默认配置里第一个
  --port PORT      覆盖监听端口
  --config PATH    配置文件路径
`)
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cfg := loadAndCheck(*cfgPath, true)
	if *port != "" {
		cfg.Launch.Listen = "127.0.0.1:" + *port
	}
	if err := RunLaunch(cfg, *player); err != nil {
		errf("helper 退出：%v", err)
		os.Exit(1)
	}
}

// 供 RunServer / RunLaunch 使用的小工具

func nowUnix() int64 { return time.Now().Unix() }

func mustAbs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
