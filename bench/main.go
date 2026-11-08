// litems 组件基准
//
//	go run . -mode=sqlite -db ~/bench.db -n 10000
//	go run . -mode=serve  -root ~/big.bin -addr 0.0.0.0:8080
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE roots (idx INTEGER PRIMARY KEY, path TEXT NOT NULL UNIQUE);
CREATE TABLE items (
    id TEXT PRIMARY KEY,
    root_idx INTEGER NOT NULL DEFAULT 0,
    rel_path TEXT NOT NULL,
    kind TEXT NOT NULL,
    title TEXT NOT NULL DEFAULT '',
    original_title TEXT NOT NULL DEFAULT '',
    year INTEGER,
    tmdb_id INTEGER,
    season INTEGER,
    episode INTEGER,
    runtime_min INTEGER,
    rating REAL,
    overview TEXT NOT NULL DEFAULT '',
    poster_path TEXT,
    size_bytes INTEGER NOT NULL,
    mtime_unix INTEGER NOT NULL,
    nfo_mtime_unix INTEGER NOT NULL DEFAULT 0,
    scraped_at INTEGER NOT NULL DEFAULT 0,
    scrape_status TEXT NOT NULL DEFAULT 'pending',
    UNIQUE (root_idx, rel_path)
);
CREATE INDEX idx_items_status ON items(scrape_status);
CREATE INDEX idx_items_year ON items(year);
CREATE INDEX idx_items_kind ON items(kind);
CREATE TABLE progress (
    item_id TEXT PRIMARY KEY,
    position_ms INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    finished INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);
`

func main() {
	mode := flag.String("mode", "sqlite", "sqlite | serve")
	dbPath := flag.String("db", "bench.db", "数据库路径")
	n := flag.Int("n", 10000, "条目数")
	root := flag.String("root", "", "serve 模式下要发送的文件")
	addr := flag.String("addr", "0.0.0.0:8080", "监听地址")
	flag.Parse()

	switch *mode {
	case "sqlite":
		benchSQLite(*dbPath, *n)
	case "serve":
		serve(*root, *addr)
	default:
		log.Fatalf("unknown mode %q", *mode)
	}
}

func benchSQLite(path string, n int) {
	for _, s := range []string{"", "-wal", "-shm"} {
		os.Remove(path + s)
	}

	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	t0 := time.Now()
	if _, err := db.Exec(schema); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("建表            %v\n", ms(time.Since(t0)))

	// 1) 单条自动提交，1000 条，用来量化"不开事务"的代价
	const singleN = 1000
	t0 = time.Now()
	for i := 0; i < singleN; i++ {
		if _, err := db.Exec(`INSERT INTO items(id,rel_path,kind,title,year,size_bytes,mtime_unix)
			VALUES(?,?,?,?,?,?,?)`, fmt.Sprintf("s%015d", i), fmt.Sprintf("dir/s%d.mkv", i),
			"movie", fmt.Sprintf("单片 %d", i), 2000+i%25, 1<<30, time.Now().Unix()); err != nil {
			log.Fatal(err)
		}
	}
	d1 := time.Since(t0)
	fmt.Printf("插入 %d 条（逐条提交） %v   → %.0f 条/秒\n", singleN, ms(d1), float64(singleN)/d1.Seconds())

	// 2) 事务批量，n 条
	titles := []string{"星际穿越", "Interstellar", "切尔诺贝利", "Chernobyl", "让子弹飞", "盗梦空间", "Inception"}
	t0 = time.Now()
	const batch = 100
	for b := 0; b < n/batch; b++ {
		tx, err := db.Begin()
		if err != nil {
			log.Fatal(err)
		}
		st, err := tx.Prepare(`INSERT INTO items(id,root_idx,rel_path,kind,title,original_title,year,
			season,episode,runtime_min,rating,overview,size_bytes,mtime_unix,nfo_mtime_unix,scraped_at,scrape_status)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			log.Fatal(err)
		}
		for i := 0; i < batch; i++ {
			idx := b*batch + i
			kind := "movie"
			var season, ep any
			if idx%3 == 0 {
				kind = "episode"
				season, ep = 1, idx%24+1
			}
			_, err := st.Exec(fmt.Sprintf("b%015d", idx), 0,
				fmt.Sprintf("剧集/片名 (%d)/S01/e%d.mkv", 1990+idx%35, idx), kind,
				titles[idx%len(titles)], "Original Title", 1990+idx%35,
				season, ep, 90+idx%60, 5.0+float64(idx%50)/10,
				strings.Repeat("简介文本。", 20), int64(idx)*1024*1024,
				time.Now().Unix()-int64(idx), 0, 0, "pending")
			if err != nil {
				log.Fatal(err)
			}
		}
		st.Close()
		if err := tx.Commit(); err != nil {
			log.Fatal(err)
		}
	}
	d2 := time.Since(t0)
	fmt.Printf("插入 %d 条（事务 %d/批） %v   → %.0f 条/秒\n", n, batch, ms(d2), float64(n)/d2.Seconds())

	total := singleN + n

	// 3) 查询：分页
	fmt.Println()
	t0 = time.Now()
	rows, err := db.Query(`SELECT id,title,year,rating FROM items WHERE kind=? ORDER BY mtime_unix DESC LIMIT 60 OFFSET 0`, "movie")
	if err != nil {
		log.Fatal(err)
	}
	cnt := 0
	for rows.Next() {
		var id, title string
		var year, rating sql.NullFloat64
		rows.Scan(&id, &title, &year, &rating)
		cnt++
	}
	rows.Close()
	fmt.Printf("分页查询(60)     %v   %d 行\n", ms(time.Since(t0)), cnt)

	// 4) 查询：COUNT
	t0 = time.Now()
	var c int
	db.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&c)
	fmt.Printf("COUNT(*)        %v   %d 行\n", ms(time.Since(t0)), c)

	// 5) 查询：LIKE 全表扫
	t0 = time.Now()
	rows, _ = db.Query(`SELECT id FROM items WHERE title LIKE ? OR original_title LIKE ? LIMIT 60`, "%穿越%", "%穿越%")
	cnt = 0
	for rows.Next() {
		var id string
		rows.Scan(&id)
		cnt++
	}
	rows.Close()
	fmt.Printf("LIKE %%关键词%%    %v   %d 行（全表扫，无索引可用）\n", ms(time.Since(t0)), cnt)

	// 6) 索引命中的过滤
	t0 = time.Now()
	rows, _ = db.Query(`SELECT id FROM items WHERE scrape_status=? LIMIT 60`, "pending")
	cnt = 0
	for rows.Next() {
		var id string
		rows.Scan(&id)
		cnt++
	}
	rows.Close()
	fmt.Printf("状态过滤         %v   %d 行（走 idx_items_status）\n", ms(time.Since(t0)), cnt)

	// 7) 进度 upsert
	t0 = time.Now()
	for i := 0; i < 100; i++ {
		db.Exec(`INSERT INTO progress(item_id,position_ms,duration_ms,updated_at) VALUES(?,?,?,?)
			ON CONFLICT(item_id) DO UPDATE SET position_ms=excluded.position_ms, updated_at=excluded.updated_at`,
			fmt.Sprintf("b%015d", i), i*1000, 6000000, time.Now().Unix())
	}
	fmt.Printf("进度 upsert ×100 %v\n", ms(time.Since(t0)))

	// 8) WAL 并发：持续写入的同时查询
	fmt.Println()
	var stop int32
	var wg sync.WaitGroup
	var writeErr atomic.Value
	wg.Add(1)
	go func() {
		defer wg.Done()
		for atomic.LoadInt32(&stop) == 0 {
			if _, err := db.Exec(`UPDATE items SET scraped_at=? WHERE id=?`,
				time.Now().Unix(), fmt.Sprintf("b%015d", time.Now().UnixNano()%int64(n))); err != nil {
				writeErr.Store(err)
				return
			}
		}
	}()
	t0 = time.Now()
	reads := 0
	for i := 0; i < 50; i++ {
		var c int
		if err := db.QueryRow(`SELECT COUNT(*) FROM items WHERE scrape_status='pending'`).Scan(&c); err != nil {
			log.Fatal(err)
		}
		reads++
	}
	d3 := time.Since(t0)
	atomic.StoreInt32(&stop, 1)
	wg.Wait()
	if e := writeErr.Load(); e != nil {
		fmt.Printf("WAL 并发        FAIL  %v\n", e)
	} else {
		fmt.Printf("WAL 并发        写的同时 %d 次读 %v（单连接串行，无 locked）\n", reads, ms(d3))
	}

	// 9) 体积
	fmt.Println()
	for _, s := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(path + s); err == nil {
			fmt.Printf("%-24s %8.2f MB\n", path+s, float64(fi.Size())/1024/1024)
		}
	}
	fmt.Printf("条目总数 %d\n", total)
}

func serve(root, addr string) {
	if root == "" {
		log.Fatal("-root 必填")
	}
	fi, err := os.Stat(root)
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "pong")
	})
	mux.HandleFunc("/file", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, root)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fl, _ := w.(http.Flusher)
		for i := 0; i < 40; i++ {
			fmt.Fprintf(w, "tick %d %s\n", i, time.Now().Format("15:04:05"))
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(500 * time.Millisecond)
		}
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		WriteTimeout:      0,
	}
	log.Printf("listening %s  file=%s (%d 字节)  WriteTimeout=0", addr, root, fi.Size())
	log.Fatal(srv.ListenAndServe())
}

func ms(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
}
