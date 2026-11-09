package main

import (
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SchemaVersion 变了就整库重建（文档 5.5：缓存层不写迁移脚本）。
const SchemaVersion = 2

const schemaSQL = `
CREATE TABLE IF NOT EXISTS roots (
    idx  INTEGER PRIMARY KEY,
    path TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS items (
    id             TEXT PRIMARY KEY,
    root_idx       INTEGER NOT NULL DEFAULT 0,
    rel_path       TEXT NOT NULL,
    kind           TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    original_title TEXT NOT NULL DEFAULT '',
    year           INTEGER,
    tmdb_id        INTEGER,
    season         INTEGER,
    episode        INTEGER,
    runtime_min    INTEGER,
    rating         REAL,
    votes          INTEGER,
    overview       TEXT NOT NULL DEFAULT '',
    poster_path    TEXT,
    fanart_path    TEXT,
    logo_path      TEXT,
    sub_paths      TEXT NOT NULL DEFAULT '[]',
    container      TEXT,
    size_bytes     INTEGER NOT NULL,
    mtime_unix     INTEGER NOT NULL,
    nfo_mtime_unix INTEGER NOT NULL DEFAULT 0,
    scraped_at     INTEGER NOT NULL DEFAULT 0,
    scrape_status  TEXT NOT NULL DEFAULT 'pending',
    UNIQUE (root_idx, rel_path),
    FOREIGN KEY (root_idx) REFERENCES roots(idx) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_items_status ON items(scrape_status);
CREATE INDEX IF NOT EXISTS idx_items_year   ON items(year);
CREATE INDEX IF NOT EXISTS idx_items_kind   ON items(kind);
CREATE INDEX IF NOT EXISTS idx_items_scraped ON items(scraped_at);
CREATE INDEX IF NOT EXISTS idx_items_mtime  ON items(mtime_unix DESC);

CREATE TABLE IF NOT EXISTS progress (
    item_id     TEXT PRIMARY KEY,
    position_ms INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    finished    INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER NOT NULL,
    FOREIGN KEY (item_id) REFERENCES items(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_progress_recent ON progress(updated_at DESC);

CREATE TABLE IF NOT EXISTS kv (
    k TEXT PRIMARY KEY,
    v TEXT NOT NULL
);
`

// SubEntry 是一条外挂字幕。
type SubEntry struct {
	Lang string `json:"lang"`
	Path string `json:"path"`
}

// Item 对应 items 表的一行。
type Item struct {
	ID            string
	RootIdx       int
	RelPath       string
	Kind          Kind
	Title         string
	OriginalTitle string
	Year          int
	TmdbID        int
	Season        int
	Episode       int
	RuntimeMin    int
	Rating        float64
	Votes         int
	Overview      string
	PosterPath    string
	FanartPath    string
	LogoPath      string
	SubPaths      []SubEntry
	Container     string
	SizeBytes     int64
	MtimeUnix     int64
	NfoMtimeUnix  int64
	ScrapedAt     int64
	ScrapeStatus  string
}

// DB 包装 SQLite 连接。
type DB struct {
	sql  *sql.DB
	path string
}

// ItemID = sha1(rootPath + "/" + relPath) 前 16 位十六进制（文档 5.2 / ADR-006）。
func ItemID(rootPath, relPath string) string {
	h := sha1.Sum([]byte(filepath.ToSlash(filepath.Join(rootPath, relPath))))
	return hex.EncodeToString(h[:])[:16]
}

// OpenDB 打开数据库。schema 版本不符时直接删库重建（索引是缓存，NFO 才是真相）。
func OpenDB(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据库目录失败: %w", err)
		}
	}

	if ver, ok := peekSchemaVersion(path); ok && ver != SchemaVersion {
		logf("索引版本 %d 与当前 %d 不一致，删除重建（需重新 scan）", ver, SchemaVersion)
		for _, s := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(path + s)
		}
	}

	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"
	h, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc 走单连接串行，避免写冲突（文档 4.2）
	h.SetMaxOpenConns(1)
	h.SetMaxIdleConns(1)
	h.SetConnMaxLifetime(0)

	if _, err := h.Exec(schemaSQL); err != nil {
		h.Close()
		return nil, fmt.Errorf("建表失败: %w", err)
	}
	if _, err := h.Exec(`INSERT INTO kv(k,v) VALUES('schema_version',?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`, fmt.Sprint(SchemaVersion)); err != nil {
		h.Close()
		return nil, err
	}
	return &DB{sql: h, path: path}, nil
}

// peekSchemaVersion 只读版本号；库不存在或无该表时返回 false。
func peekSchemaVersion(path string) (int, bool) {
	if _, err := os.Stat(path); err != nil {
		return 0, false
	}
	h, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return 0, false
	}
	defer h.Close()
	var v string
	if err := h.QueryRow(`SELECT v FROM kv WHERE k='schema_version'`).Scan(&v); err != nil {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

func (d *DB) Close() error { return d.sql.Close() }

// SyncRoots 把配置里的根目录同步进 roots 表。
func (d *DB) SyncRoots(roots []string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, r := range roots {
		if _, err := tx.Exec(`INSERT INTO roots(idx,path) VALUES(?,?)
			ON CONFLICT(idx) DO UPDATE SET path=excluded.path`, i, filepath.ToSlash(r)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpsertItem 插入或更新一条记录。已刮削的元数据不会被扫描覆盖。
func (d *DB) UpsertItem(it *Item) error {
	subs, _ := json.Marshal(it.SubPaths)
	if len(subs) == 0 {
		subs = []byte("[]")
	}
	_, err := d.sql.Exec(`
INSERT INTO items(id,root_idx,rel_path,kind,title,original_title,year,tmdb_id,season,episode,
  runtime_min,rating,votes,overview,poster_path,fanart_path,logo_path,sub_paths,container,
  size_bytes,mtime_unix,nfo_mtime_unix,scraped_at,scrape_status)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  kind=excluded.kind,
  container=excluded.container,
  size_bytes=excluded.size_bytes,
  mtime_unix=excluded.mtime_unix,
  nfo_mtime_unix=excluded.nfo_mtime_unix,
  sub_paths=excluded.sub_paths,
  -- 已刮削的条目不被扫描结果清空：扫描只在确实发现文件时才覆盖
  poster_path=CASE WHEN items.scraped_at=0 THEN excluded.poster_path
                   ELSE COALESCE(NULLIF(excluded.poster_path,''), items.poster_path) END,
  fanart_path=CASE WHEN items.scraped_at=0 THEN excluded.fanart_path
                   ELSE COALESCE(NULLIF(excluded.fanart_path,''), items.fanart_path) END,
  title=CASE WHEN items.scraped_at=0 THEN excluded.title ELSE items.title END,
  year=CASE WHEN items.scraped_at=0 THEN excluded.year ELSE items.year END,
  season=excluded.season,
  episode=excluded.episode`,
		it.ID, it.RootIdx, filepath.ToSlash(it.RelPath), string(it.Kind), it.Title, it.OriginalTitle,
		nullableInt(it.Year), nullableInt(it.TmdbID), nullableInt(it.Season), nullableInt(it.Episode),
		nullableInt(it.RuntimeMin), nullableFloat(it.Rating), nullableInt(it.Votes), it.Overview,
		it.PosterPath, it.FanartPath, it.LogoPath, string(subs), it.Container,
		it.SizeBytes, it.MtimeUnix, it.NfoMtimeUnix, it.ScrapedAt, it.ScrapeStatus)
	return err
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullableFloat(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// CountItems 返回条目总数。
func (d *DB) CountItems() (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&n)
	return n, err
}

// CountByStatus 返回某状态的条目数。
func (d *DB) CountByStatus(status string) (int, error) {
	var n int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM items WHERE scrape_status=?`, status).Scan(&n)
	return n, err
}

// Snapshot 是扫描时用于增量判定的最小信息。
type Snapshot struct {
	RootIdx      int
	RelPath      string
	SizeBytes    int64
	MtimeUnix    int64
	NfoMtimeUnix int64
	ScrapedAt    int64
	ScrapeStatus string
}

// Existing 返回库中已有的路径快照，key 为 "<rootIdx>:<relPath>"。
func (d *DB) Existing() (map[string]Snapshot, error) {
	rows, err := d.sql.Query(`SELECT root_idx,rel_path,size_bytes,mtime_unix,nfo_mtime_unix,scraped_at,scrape_status FROM items`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]Snapshot, 1024)
	for rows.Next() {
		var s Snapshot
		if err := rows.Scan(&s.RootIdx, &s.RelPath, &s.SizeBytes, &s.MtimeUnix, &s.NfoMtimeUnix, &s.ScrapedAt, &s.ScrapeStatus); err != nil {
			return nil, err
		}
		out[fmt.Sprintf("%d:%s", s.RootIdx, s.RelPath)] = s
	}
	return out, rows.Err()
}

// DeleteItem 删除一条记录（progress 会随外键级联删除）。
func (d *DB) DeleteItem(id string) error {
	_, err := d.sql.Exec(`DELETE FROM items WHERE id=?`, id)
	return err
}

// ListIDsByRoot 返回某个根下的所有条目 ID，用于清理已消失的文件。
func (d *DB) ListIDsByRoot(rootIdx int) (map[string]string, error) {
	rows, err := d.sql.Query(`SELECT id,rel_path FROM items WHERE root_idx=?`, rootIdx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var id, rel string
		if err := rows.Scan(&id, &rel); err != nil {
			return nil, err
		}
		out[rel] = id
	}
	return out, rows.Err()
}

// Progress 是一条播放进度。
type Progress struct {
	ItemID     string
	PositionMS int64
	DurationMS int64
	Finished   bool
	UpdatedAt  int64
}

// SetProgress 上报进度。完成判定见文档 9.7。
func (d *DB) SetProgress(itemID string, posMS, durMS int64) error {
	finished := false
	if durMS > 0 && float64(posMS)/float64(durMS) > 0.95 {
		finished = true
		posMS = 0
	}
	fin := 0
	if finished {
		fin = 1
	}
	_, err := d.sql.Exec(`INSERT INTO progress(item_id,position_ms,duration_ms,finished,updated_at)
		VALUES(?,?,?,?,?)
		ON CONFLICT(item_id) DO UPDATE SET
		  position_ms=excluded.position_ms,
		  duration_ms=excluded.duration_ms,
		  finished=excluded.finished,
		  updated_at=excluded.updated_at`,
		itemID, posMS, durMS, fin, time.Now().Unix())
	return err
}

// GetProgress 读取进度；不存在返回 (nil, nil)。
func (d *DB) GetProgress(itemID string) (*Progress, error) {
	var p Progress
	var fin int
	err := d.sql.QueryRow(`SELECT item_id,position_ms,duration_ms,finished,updated_at
		FROM progress WHERE item_id=?`, itemID).
		Scan(&p.ItemID, &p.PositionMS, &p.DurationMS, &fin, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Finished = fin != 0
	return &p, nil
}

// SetKV 写一个键值。
func (d *DB) SetKV(k, v string) error {
	_, err := d.sql.Exec(`INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// GetKV 读一个键值；不存在返回空串。
func (d *DB) GetKV(k string) (string, error) {
	var v string
	err := d.sql.QueryRow(`SELECT v FROM kv WHERE k=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// NeedsReindex 判断文件属性是否变化到需要重写索引行。
// 注意这里**不看 scraped_at**：未刮削不等于索引要重写，两者是不同的事。
// 未刮削的条目由扫描器单独计入 PendingScrape，交给刮削阶段处理。
func NeedsReindex(s Snapshot, size, mtime, nfoMtime int64) bool {
	return s.SizeBytes != size || s.MtimeUnix != mtime || s.NfoMtimeUnix != nfoMtime
}

// NeedsScrape 判断是否需要（重新）刮削（文档 5.3）。
func NeedsScrape(s Snapshot, size, mtime, nfoMtime int64) bool {
	if s.ScrapedAt == 0 || s.ScrapeStatus == "error" {
		return true
	}
	return NeedsReindex(s, size, mtime, nfoMtime)
}

// normalizeRel 统一相对路径的分隔符与首尾斜杠。
func normalizeRel(p string) string {
	p = filepath.ToSlash(p)
	p = strings.TrimPrefix(p, "./")
	return strings.TrimPrefix(p, "/")
}

// ItemFilter 是列表查询条件（文档 9.3）。
type ItemFilter struct {
	Q     string
	Sort  string
	Page  int
	Limit int
	Kind  string
	Year  int
}

const itemCols = `id,root_idx,rel_path,kind,title,original_title,year,tmdb_id,season,episode,
runtime_min,rating,votes,overview,poster_path,fanart_path,logo_path,sub_paths,container,
size_bytes,mtime_unix,nfo_mtime_unix,scraped_at,scrape_status`

// scanItem 从一行结果构造 Item。
func scanItem(sc interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var kind, subs, container, poster, fanart, logo sql.NullString
	var year, tmdb, season, ep, runtime, votes sql.NullInt64
	var rating sql.NullFloat64
	err := sc.Scan(&it.ID, &it.RootIdx, &it.RelPath, &kind, &it.Title, &it.OriginalTitle,
		&year, &tmdb, &season, &ep, &runtime, &rating, &votes, &it.Overview,
		&poster, &fanart, &logo, &subs, &container,
		&it.SizeBytes, &it.MtimeUnix, &it.NfoMtimeUnix, &it.ScrapedAt, &it.ScrapeStatus)
	if err != nil {
		return it, err
	}
	it.Kind = Kind(kind.String)
	it.Year = int(year.Int64)
	it.TmdbID = int(tmdb.Int64)
	it.Season = int(season.Int64)
	it.Episode = int(ep.Int64)
	it.RuntimeMin = int(runtime.Int64)
	it.Rating = rating.Float64
	it.Votes = int(votes.Int64)
	it.PosterPath = poster.String
	it.FanartPath = fanart.String
	it.LogoPath = logo.String
	it.Container = container.String
	if subs.String != "" {
		_ = json.Unmarshal([]byte(subs.String), &it.SubPaths)
	}
	return it, nil
}

// ListItems 按条件分页查询，同时返回总数。
func (d *DB) ListItems(f ItemFilter) ([]Item, int, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Limit < 1 {
		f.Limit = 60
	}
	if f.Limit > 200 {
		f.Limit = 200
	}

	var where []string
	var args []any
	if f.Q != "" {
		where = append(where, `(title LIKE ? OR original_title LIKE ?)`)
		like := "%" + f.Q + "%"
		args = append(args, like, like)
	}
	if f.Kind != "" {
		where = append(where, `kind = ?`)
		args = append(args, f.Kind)
	}
	if f.Year > 0 {
		where = append(where, `year = ?`)
		args = append(args, f.Year)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM items`+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order := "mtime_unix DESC"
	switch f.Sort {
	case "title":
		order = "title COLLATE NOCASE ASC"
	case "year":
		order = "year DESC, title ASC"
	case "rating":
		order = "rating IS NULL, rating DESC, title ASC"
	}

	q := `SELECT ` + itemCols + ` FROM items` + clause +
		` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	qargs := append(append([]any{}, args...), f.Limit, (f.Page-1)*f.Limit)

	rows, err := d.sql.Query(q, qargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]Item, 0, f.Limit)
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, it)
	}
	return out, total, rows.Err()
}

// GetItem 按 ID 取单条，不存在返回 (nil, nil)。
func (d *DB) GetItem(id string) (*Item, error) {
	row := d.sql.QueryRow(`SELECT `+itemCols+` FROM items WHERE id=?`, id)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// ListContinue 返回未看完且有进度的条目。
func (d *DB) ListContinue(limit int) ([]Item, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.sql.Query(`SELECT `+itemCols+` FROM items
		WHERE id IN (SELECT item_id FROM progress WHERE finished=0 AND position_ms > 30000)
		ORDER BY mtime_unix DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// RelPathOf 取条目的绝对路径（基于其所属根）。
func (d *DB) RelPathOf(it *Item, roots []string) string {
	if it.RootIdx >= 0 && it.RootIdx < len(roots) {
		return filepath.Join(roots[it.RootIdx], filepath.FromSlash(it.RelPath))
	}
	return ""
}

// ResetScrapeStatus 把指定条目打回待刮削。
// 用于启动补刮：把「状态是成功、但海报文件实际不在」的条目捡回来重试。
func (d *DB) ResetScrapeStatus(ids []string) error {
	for _, id := range ids {
		if _, err := d.sql.Exec(
			`UPDATE items SET scraped_at=0, scrape_status='pending' WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// ListForScrape 返回本次要刮削的条目。
//
//	onlyID 非空 → 只处理这一条（调试用）
//	force     → 全部重来
//	默认       → 未刮削过的，以及上次失败的
//
// 注意 miss / manual 不在此列：它们算"处理过"，反复重试只会白烧配额（文档 8.6）。
func (d *DB) ListForScrape(force bool, onlyID string, limit int) ([]Item, error) {
	var where string
	var args []any
	switch {
	case onlyID != "":
		where = " WHERE id=?"
		args = append(args, onlyID)
	case force:
		where = ""
	default:
		where = " WHERE scraped_at=0 OR scrape_status='error'"
	}
	q := `SELECT ` + itemCols + ` FROM items` + where + ` ORDER BY kind DESC, rel_path`
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// UpdateScraped 写入刮削结果。只动元数据字段，不碰文件属性。
func (d *DB) UpdateScraped(it *Item) error {
	_, err := d.sql.Exec(`
UPDATE items SET
  title=?, original_title=?, year=?, tmdb_id=?, runtime_min=?, rating=?, votes=?,
  overview=?, poster_path=?, fanart_path=?, nfo_mtime_unix=?, scraped_at=?,
  scrape_status='ok'
WHERE id=?`,
		it.Title, it.OriginalTitle, nullableInt(it.Year), nullableInt(it.TmdbID),
		nullableInt(it.RuntimeMin), nullableFloat(it.Rating), nullableInt(it.Votes),
		it.Overview, nullString(it.PosterPath), nullString(it.FanartPath),
		it.NfoMtimeUnix, time.Now().Unix(), it.ID)
	return err
}

// SetScrapeStatus 只更新状态与时间戳（miss / manual / error 用）。
func (d *DB) SetScrapeStatus(id, status string, ts int64) error {
	_, err := d.sql.Exec(`UPDATE items SET scrape_status=?, scraped_at=? WHERE id=?`, status, ts, id)
	return err
}

// StatKV 是健康检查用的简短统计。
func (d *DB) StatKV() (items int, lastScan string, err error) {
	if items, err = d.CountItems(); err != nil {
		return
	}
	lastScan, _ = d.GetKV("last_scan")
	return
}
