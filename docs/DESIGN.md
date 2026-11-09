---
title: DESIGN
---

# LiteMS 设计文档

> 一个跑在 Termux 上的轻量媒体库服务器：负责刮削、索引、文件服务；
> 播放交给 PC 上的本地播放器。
>
> 文档版本：v2.8　　日期：2026-11-09　　状态：M1–M5 代码全部完成；M2 待真实 API key 验收

---

## 目录

1. [项目目标](#1-项目目标)
2. [范围界定](#2-范围界定)
3. [架构](#3-架构)
4. [技术选型与约束](#4-技术选型与约束)
5. [数据模型](#5-数据模型)
6. [媒体库目录规范](#6-媒体库目录规范)
7. [NFO 规范](#7-nfo-规范)
8. [刮削流程](#8-刮削流程)
9. [HTTP 接口](#9-http-接口)
10. [播放器集成](#10-播放器集成)
11. [配置文件](#11-配置文件)
12. [CLI 设计](#12-cli-设计)
13. [Termux 部署](#13-termux-部署)
14. [跨平台构建](#14-跨平台构建)
15. [已知限制](#15-已知限制)
16. [里程碑](#16-里程碑)
17. [风险登记](#17-风险登记)
18. [决策记录](#18-决策记录)
19. [变更记录](#19-变更记录)

---

## 1. 项目目标

把一台闲置 Android 手机（Redmi K20 Pro / raphael / Snapdragon 855 / 8GB RAM）变成家庭媒体库服务器，满足：

| 目标        | 说明                                            |
| --------- | --------------------------------------------- |
| **元数据自主** | 刮削结果以 NFO + 图片形式落盘，不锁死在数据库里                   |
| **零转码**   | 服务器只发文件字节，解码全部交给客户端播放器                        |
| **PC 优先** | 浏览器负责浏览和挑选，播放交给本地播放器（mpv/VLC）                 |
| **跨平台**   | 同一份代码可编译到 Linux / macOS / Windows / ARM / x86 |
| **长跑稳定**  | 7×24 运行，进程崩溃能自愈，开机自启                          |

核心判断：**手机不适合做转码，但非常适合做文件服务。** 把职责边界划清晰，这个小工具就能长期稳定工作。

---

## 2. 范围界定

### 2.1 做

- 递归扫描媒体库，识别电影/剧集
- 调用 OMDb（或 TMDB）刮削，写 `movie.nfo` / `tvshow.nfo` / 单集 nfo
- 下载海报、背景图到媒体目录
- SQLite 索引（可删除重建）
- HTTP 服务：列表 API、图片、视频流（Range）、字幕
- 轻量 Web UI（海报墙 / 详情页 / 搜索）
- 本地 helper：把播放请求转成本地播放器进程；另有「复制播放命令」的降级通道
- 播放进度记录与续播

### 2.2 不做

| 不做                     | 原因                                         |
| ---------------------- | ------------------------------------------ |
| 视频转码 / remux           | 手机 CPU 无法支撑，且客户端播放器解码能力更强                  |
| 浏览器内播放                 | 见 [15-已知限制](#15-已知限制)                      |
| Jellyfin / Emby API 兼容 | 当前仅单人 + PC 场景，收益不足以抵消 1500 行成本             |
| DLNA / UPnP            | Android 非 root 无法获取 MulticastLock，多播包被系统过滤 |
| SMB / NFS              | 端口 <1024 需要 root 权限                        |
| 多用户体系                  | 单人为主，提供单账号 Basic Auth 即可                   |
| 移动端 App                | 用现成播放器（Infuse / nPlayer / VLC）挂 WebDAV 即可  |
| 网盘 / 文件同步              | **不自研**。由同机的第三方服务提供，见 [13.9](#139-与其他服务共存网盘--同步) |

### 2.3 写入边界（对「只读」的精确表述）

媒体库中**永远不会被 LiteMS 修改、移动、删除**的只有视频文件本身。

会被写入的：

| 路径                                | 内容                      |
| --------------------------------- | ----------------------- |
| `<影片目录>/movie.nfo`               | 电影元数据                   |
| `<剧集目录>/tvshow.nfo`              | 剧集元数据                   |
| `<单集同名>.nfo`                     | 分集元数据                   |
| `<影片目录>/poster.jpg` 等           | 图片                      |
| `<媒体根>/.litems/`               | `unmatched.txt`、扫描摘要、状态文件 |

`.litems/` 是隐藏目录，扫描自身时会被跳过。

### 2.4 明确的非目标

不做「Jellyfin 替代品」。这个项目的定位是**一个把 NFO 刮好、把文件发出去的单二进制工具**，功能刻意保持窄。

---

## 3. 架构

### 3.1 组件关系

```
┌──────────────────────┐
│  PC 浏览器            │
│  http://phone:7788   │
│  ┌────────────────┐  │
│  │ 海报墙 / 详情页 │  │
│  └───────┬────────┘  │
└──────────┼───────────┘
           │ ① GET /api/items, /poster, /stream
           │
           │ ② 点击播放 → POST 127.0.0.1:9876/play
           ↓
┌──────────────────────┐        ┌─────────────────────────┐
│  PC 本地 helper       │        │  Termux 服务端           │
│  litems launch     │───────>│  litems serve          │
│  127.0.0.1:9876      │ ③ exec │  0.0.0.0:7788            │
└──────────┬───────────┘        │  ┌───────────────────┐  │
           │                    │  │ SQLite 索引        │  │
           ↓ ④                   │  │ NFO / 图片         │  │
┌──────────────────────┐        │  │ 媒体文件           │  │
│  mpv / VLC           │───────>│  └───────────────────┘  │
│  http://phone/stream │ ⑤ HTTP │                         │
└──────────────────────┘        └─────────────────────────┘
```

调用序列：

1. 浏览器从服务端拉列表与图片
2. 点击播放，页面 `POST` 到**本机** helper（不回服务端），带一次性 token
3. helper 校验 Origin 与 token，以子进程方式启动播放器，参数里带视频 URL 和字幕 URL
4. 播放器自己发起 HTTP 请求拉流（支持 Range，可拖动）
5. 可选：helper 通过 mpv IPC 读取播放位置，回报服务端

### 3.2 为什么 helper 独立于服务端

服务端在手机上，helper 在 PC 上。它们是**同一个二进制文件的两个子命令**，但运行在不同机器。

- 浏览器无法直接启动本地程序（沙箱限制），必须有个本地 HTTP 端点被调用
- helper 只监听 `127.0.0.1`，不暴露到局域网
- 用同一份代码便于分发：用户只需要下载一个二进制

### 3.3 helper 的浏览器兼容性（必须先验证）

从 `http://192.168.1.10:7788` 这样的**私网页面**去请求 `http://127.0.0.1:9876`，Chrome/Edge 的 Private Network Access（新版叫 Local Network Access）会要求预检，且**`no-cors` 模式满足不了**——请求会被浏览器直接阻断，前端拿不到任何响应，表现为「点了没反应」。

因此协议必须按下面设计，且 M0 阶段（见 [16-里程碑](#16-里程碑)）就要在真浏览器上验证一次：

```
# 预检
OPTIONS /play
Access-Control-Allow-Origin: <配置的 server origin>
Access-Control-Allow-Private-Network: true
Access-Control-Allow-Methods: POST, OPTIONS
Access-Control-Allow-Headers: content-type

# 实际请求
POST /play
Content-Type: application/json
{"id": "a3f9c1d2e4b5f607", "start": 2535, "token": "..."}
```

若验证失败（某些浏览器版本策略更严），走降级通道 `/api/cmd/{id}`：服务端返回一行可直接复制的 mpv 命令。这是 20 行代码的保险，不依赖任何浏览器特性。

### 3.4 数据流：谁是真相

```
媒体文件 + NFO + 图片      ← 唯一真相（人类可读、可迁移、可版本控制）
        ↓ 扫描
    SQLite 索引            ← 纯缓存，删除后可从文件重建
        ↓
    HTTP 响应
```

**任何情况下删除 `index.db` 都不应丢失数据。** 这是硬约束。

---

## 4. 技术选型与约束

### 4.1 语言与依赖

| 项      | 选择                           | 理由                                         |
| ------ | ---------------------------- | ------------------------------------------ |
| 语言     | **Go 1.26+**                 | 单静态二进制、交叉编译简单、标准库够用。下限由 `modernc.org/sqlite v1.62` 的 `go 1.26.0` 决定，不是随意选的  |
| CGO    | **全项目禁用**（`CGO_ENABLED=0`）   | Termux 的 libc 是 bionic，CGO 产物无法运行          |
| SQLite | `modernc.org/sqlite`         | 纯 Go 实现，无 C 工具链依赖；代价是体积 +15MB、速度约为 C 版 1/3 |
| HTTP   | `net/http`                   | `ServeFile` 自带 Range / ETag / Content-Type |
| 模板     | `html/template` + `embed.FS` | 无前端构建步骤，二进制自包含                             |
| 配置     | `BurntSushi/toml`            | 语义清晰，无缩进歧义                                 |
| 日志     | `log/slog`                   | 标准库，支持 JSON handler                        |

**依赖白名单原则**：`go.mod` 中任何引入 CGO 的模块都视为违规。CI 中执行 `CGO_ENABLED=0 go build ./...` 作为守门检查。

### 4.2 SQLite 连接串（硬性要求）

`modernc.org/sqlite` 的三个 pragma 缺一不可，否则会在两个地方出问题：外键级联删除失效、后台扫描与前台推流并发时 `database is locked`。

```go
dsn := "file:" + path +
    "?_pragma=journal_mode(WAL)" +
    "&_pragma=busy_timeout(5000)" +
    "&_pragma=foreign_keys(1)" +
    "&_pragma=synchronous(NORMAL)"
db, _ := sql.Open("sqlite", dsn)
db.SetMaxOpenConns(1)   // 单写者，避免 modernc 的写冲突
```

- `foreign_keys` 默认是 OFF，`ON DELETE CASCADE` 不加这行等于没写
- `WAL` 让后台扫描的写不阻塞前台读；WAL 需要 `$HOME` 所在 ext4，与 [5.4](#54-存储位置全部落在-ext4) 一致
- modernc 版本建议**单连接**串行访问，读写都排队；媒体库规模下吞吐足够
- 批量写（扫描入库）用显式事务，每 100 条提交一次。逐条提交会慢 3.6 倍（实测 1964 vs 7143 条/秒，见附录 C）

### 4.3 HTTP 服务参数

```go
srv := &http.Server{
    Addr:              cfg.Server.Listen,
    ReadHeaderTimeout: 10 * time.Second,
    IdleTimeout:       120 * time.Second,
    WriteTimeout:      0,   // 必须为 0，否则大文件推流会被中途切断
}
```

同时处理 `SIGTERM`：`sv down` 时先停后台扫描、再 `srv.Shutdown(ctx)`，超时 5 秒强制退出。

### 4.4 前端技术

- 原生 JavaScript，无框架、无 npm、无打包
- 深色主题，CSS 变量定义配色
- 资源通过 `//go:embed web/*` 打入二进制

**视觉基线**（v1.9 重做）：底色 `#08090c`，表面 `#12141a`，分隔线用 `rgba(255,255,255,.07)` 而不是实色；强调色是暖金 `#e3b774`（评分、主按钮、进度），不引入第二种强调色。字号收敛为 4 档（11 / 12.5 / 13.5 / 26–40），标题字重 600，正文行高 1.6–1.9。海报统一 2:3、圆角 14px、内描边 `inset 0 1px 0 rgba(255,255,255,.05)`。

**缺图不留白**：没有海报时用「细网格 + 标题首字」的占位块（首字由 `initial` 模板函数按 rune 取，避免中文被按字节截断）；详情页没有 `fanart.jpg` 时，退回用海报本身做 52px 高斯虚化铺底。这条比直接显示灰底或空 `<img>` 更能保住观感，也不依赖任何图片处理库。

**降级要求**：所有动效走 `transform` / `opacity`，并在 `prefers-reduced-motion: reduce` 下关闭；布局必须经 390 / 834 / 1024 / 1440 四档实测无横向溢出（`scrollWidth == innerWidth`）。

### 4.5 外部依赖

| 依赖                    | 是否必需       | 缺失时的行为                            |
| --------------------- | ---------- | --------------------------------- |
| 刮削后端（OMDb**或**TMDB）   | 刮削必需（二选一）  | 跳过刮削，仅索引文件                        |
| 豆瓣（非官方接口）             | 可选         | 只有英文标题与简介，中文缺失——不影响 NFO 可用性       |
| `mpv` / `vlc`         | 播放必需（PC 侧） | helper 启动失败，提示未找到播放器，并回退到「复制命令」模式  |
| `ffprobe`             | 可选         | 无法读取时长/分辨率时留空，不影响播放               |

**网络前提**（实测见 [13.2](#132-网络可达性已实测)）：`api.themoviedb.org` 被 DNS 污染，但同一后端的 `api.tmdb.org` 与图片 CDN `image.tmdb.org` 直连可用。因此**默认后端是 TMDB 且不需要代理**；`[scrape].provider` 可切到 OMDb 作兜底。

---

## 5. 数据模型

### 5.1 表结构

```sql
-- 媒体根。idx=0 是 library.root，1..n 是 extra_roots，顺序即配置顺序
CREATE TABLE roots (
    idx  INTEGER PRIMARY KEY,
    path TEXT NOT NULL UNIQUE        -- 规范化后的绝对路径
);

CREATE TABLE items (
    id             TEXT PRIMARY KEY,   -- sha1(root_path + "/" + rel_path)[:16]
    root_idx       INTEGER NOT NULL DEFAULT 0,
    rel_path       TEXT NOT NULL,      -- 相对所属 root 的路径
    kind           TEXT NOT NULL,      -- 'movie' | 'episode'
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
    poster_path    TEXT,               -- 相对媒体根的路径，如 "电影/xxx (2014)/poster.jpg"
    fanart_path    TEXT,
    logo_path      TEXT,
    sub_paths      TEXT NOT NULL DEFAULT '[]',  -- JSON: [{"lang":"chs","path":"a.chs.srt"}, ...]
    container      TEXT,               -- mkv / mp4 / avi
    size_bytes     INTEGER NOT NULL,
    mtime_unix     INTEGER NOT NULL,
    nfo_mtime_unix INTEGER NOT NULL DEFAULT 0,  -- 同级 NFO 的 mtime，0 表示无 NFO
    scraped_at     INTEGER NOT NULL DEFAULT 0,  -- 0 表示未刮削
    scrape_status  TEXT NOT NULL DEFAULT 'pending', -- pending|ok|miss|error|manual
    UNIQUE (root_idx, rel_path),
    FOREIGN KEY (root_idx) REFERENCES roots(idx) ON DELETE CASCADE
);

CREATE INDEX idx_items_status ON items(scrape_status);
CREATE INDEX idx_items_year   ON items(year);
CREATE INDEX idx_items_kind   ON items(kind);
CREATE INDEX idx_items_scraped ON items(scraped_at);
-- 列表默认 sort=recent 按 mtime_unix 排序，没有这条索引就会全表扫 + 排序。
-- 实测 1.1 万条时 33.6ms，加上索引后降到毫秒级（见附录 C）。
CREATE INDEX idx_items_mtime  ON items(mtime_unix DESC);

CREATE TABLE progress (
    item_id      TEXT PRIMARY KEY,
    position_ms  INTEGER NOT NULL DEFAULT 0,
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    finished     INTEGER NOT NULL DEFAULT 0,   -- 0/1，看完标记
    updated_at   INTEGER NOT NULL,
    FOREIGN KEY (item_id) REFERENCES items(id) ON DELETE CASCADE
);

CREATE INDEX idx_progress_recent ON progress(updated_at DESC);

CREATE TABLE kv (
    k TEXT PRIMARY KEY,
    v TEXT NOT NULL
);
-- schema_version = 2
-- last_full_scan = <unix>
```

**关于 `q` 搜索**：`title LIKE '%关键词%'` 用不上任何 B-tree 索引，必然全表扫。1 万条以内（本项目目标量级）实测可忽略，因此**不建 `idx_items_title`**——留着会误导后人以为它在起作用。若未来条目数超 5 万，再引入 FTS5 外部内容表，届时同步 `items_fts` 的 trigger。

**关于 `sub_paths`**：中文场景常见 `chs / cht / eng` 三份外挂字幕，单字段存不下。存 JSON 数组，读取时解析；接口按 `?lang=` 挑选。

### 5.2 主键设计

`id = sha1(root_path + "/" + rel_path)` 取前 16 位十六进制。

- 稳定：同一路径永远同一 ID，重启/重扫不变
- **唯一**：多根目录下，`电影/xxx.mkv` 在根 A 和根 B 里会得到不同 ID（这是 v1.0 的漏洞）
- 缺点：根路径变更或文件重命名后 ID 变化，进度丢失（可接受，提供 `scan --reindex` 重建索引）

### 5.3 增量判定

扫描时对每个文件计算四元组 `(size_bytes, mtime_unix, nfo_mtime_unix, scraped_at)`：

```
scraped_at == 0                        → 需要刮削
size / mtime 变化                       → 需要重刮
nfo_mtime 变化（用户手改过 NFO）          → 需要重读 NFO，重算元数据
文件已不存在                            → 删除记录（级联删除进度）
```

**为什么要看 `nfo_mtime`**：用户手工修正了一份 NFO（`lockdata=false`）后，视频本身没变。只看 size+mtime 的话这份修改永远不会被读取，元数据永远落后于文件。这是最容易漏的一条。

**不做内容哈希**：大文件哈希在 UFS 上是纯浪费，mtime+size 足够。

### 5.4 存储位置：全部落在 ext4

`index.db` 与媒体库默认都放在 `$HOME`（`/data/data/com.termux/files/home`），该分区是 ext4。

**为什么不能用 `/storage/emulated/0`**：它是 FUSE 实现（`sdcardfs` 的继任者），`fcntl` 文件锁语义不完整，SQLite 会间歇性报 `database is locked`，严重时导致数据库损坏。这是本项目最容易踩的坑之一。

**为什么媒体库也放这里**：见 [6.5](#65-为什么媒体库放-home-而不是共享目录)。同一分区还额外带来 `sendfile` 可用、inotify 可用、符号链接可用三项好处。

若坚持把媒体库放在共享目录，功能可用（扫描、刮削、推流都正常），但要接受：

- 数据库仍必须留在 `$HOME`（启动时校验，落在共享目录直接拒绝）
- `sendfile` 退化为用户态 copy，吞吐下降
- 扫描期间的随机读会加剧推流抖动

### 5.5 Schema 迁移策略

不做在线迁移。启动时读 `kv.schema_version`：

```
版本一致            → 正常启动
版本更低            → 直接删除 index.db，重建空库，日志提示「索引已重建，请执行 scan」
版本更高            → 拒绝启动并报错（二进制降级场景）
```

理由：索引是纯缓存，删了能重建；为缓存层写迁移脚本是负价值。

---

## 6. 媒体库目录规范

### 6.1 电影

```
媒体根/
└── 电影/
    └── 星际穿越 (2014)/
        ├── 星际穿越 (2014).mkv
        ├── movie.nfo
        ├── poster.jpg
        ├── fanart.jpg
        └── 星际穿越 (2014).chs.srt
```

### 6.2 剧集

```
媒体根/
└── 剧集/
    └── 切尔诺贝利 (2019)/
        ├── tvshow.nfo
        ├── poster.jpg
        ├── fanart.jpg
        └── Season 01/
            ├── 切尔诺贝利 S01E01.mkv
            ├── 切尔诺贝利 S01E01.nfo
            └── 切尔诺贝利 S01E01.srt
```

### 6.3 解析流程（顺序不能换）

**核心原则：先抽信息，再清洗标题。** 反过来做会把年份、季集号当作噪声删掉——`(2014)` 括号里没有中日韩字符，会被清洗规则当成组名整段丢弃。

对每个视频文件执行：

```
① 取 stem = 去掉扩展名的文件名
② 抽年份   ← 必须在此刻完成，且此后不允许再动
③ 抽季集   ← SxxExx / 第N季 / - NN
④ 抽字幕语言 ← .chs.srt / .zh-CN.srt / .eng.ass
⑤ 清洗干扰标记（见 6.4，年份与季集已消费，不会被误删）
⑥ 折叠空白，得到 title 候选
```

②的抽取规则：从 stem 中找第一个落在 `1930–2035` 区间的四位数字，且它前面不是数字（避免 `1080` 被截成 `1080`）。抽到后**在 stem 中标记为已消费**，不参与后续 title。

③的规则：

| 优先级 | 正则 / 条件                              | 例子                                | 结果                     |
| --- | ------------------------------------ | --------------------------------- | ---------------------- |
| 1   | `[Ss](\d{1,2})[Ee](\d{1,3})`         | `Show.S01E03.2160p`               | season=1, episode=3    |
| 2   | `第\s*(\d{1,2})\s*[季集]` 或 `Season\s*(\d{1,2})`（在父目录名里） | `Season 01/` | season=1               |
| 3   | `^(.+?)\s*[-–]\s*(\d{1,3})(v\d)?$`   | `[SubGroup] 番名 - 01`             | episode=1, season=1    |

**归属判定（kind）**，按顺序，命中即停：

```
1. stem 里有 SxxExx               → episode
2. 父目录名匹配 Season \d+ / S\d+ / 第N季 → episode，剧集目录 = 祖父目录
3. 父目录能被「标题 (年份)」解析，且存在 tvshow.nfo → episode
4. 父目录能被「标题 (年份)」解析，且存在 Season 子目录 → episode
5. 其余                            → movie
```

**剧集目录回溯**：episode 的 `title` 取**剧集目录**（不是文件名）解析出的片名，`season`/`episode` 取文件名。番剧这类「没有季目录」的情况，剧集目录 = 父目录，`season` 固定 1。

> v1.0 只写了「`SxxExx` 命中就当集」，没写剧名从哪来、`tvshow.nfo` 写在哪一层。这条必须在 M1 定死，因为它决定 `kind`、文件落位和接口语义。

### 6.4 清洗规则

解析前剔除以下标记（大小写不敏感）：

```
1080p 2160p 720p 480p 4K UHD BluRay Blu-ray WEB-DL WEBRip HDRip
x264 x265 H264 H265 HEVC AVC AAC AC3 DTS DDP DD5.1 FLAC 10bit
HDR HDR10 DV DolbyVision REMUX PROPER REPACK EXTENDED IMAX
```

括号组（`[...]`、`(...)`）在清洗阶段整段丢弃，但**两种情况保留**：

1. 括号内含中日韩字符（`星际穿越（国语）` 里的括号是片名的一部分）
2. 括号内是已抽取的年份（`(2014)`）——虽然已在②步被消费，清洗时仍按位置跳过而非删除，避免破坏后续可能的重解析

**边界修正**：v1.0 的 `^(.+?)[\.\s](\d{4})[\.\s]` 要求年份后面必须跟分隔符，而 `Interstellar.2014`（清洗后末尾点被折叠）没有尾部分隔符，会失配。改成：

```
^(.+?)[\.\s](\d{4})(?=[\.\s]|$)
```

### 6.5 为什么媒体库放 `$HOME` 而不是共享目录

默认位置：`/data/data/com.termux/files/home/Media`（ext4）。

这台手机**只作为服务器，没有人会亮屏使用**，因此「把文件放在共享目录让手机 App 能看见」这个理由不成立。而共享目录 `/storage/emulated/0` 是 FUSE 实现，会引入五个本可以避免的问题：

| 问题              | ext4（`$HOME`）  | FUSE（`/storage/emulated/0`） |
| --------------- | -------------- | -------------------------- |
| `sendfile` 零拷贝 | 可用             | 退化为用户态 copy，吞吐下降           |
| `fcntl` 文件锁     | 完整             | 语义不完整，SQLite 可能损坏          |
| inotify         | 可用             | 不可用，只能轮询                   |
| 符号链接 / 硬链接      | 可用             | 不支持                        |
| 访问权限            | 天然属于自己         | 需 `MANAGE_EXTERNAL_STORAGE` |

代价只有一个：手机上的相册、系统播放器看不到这些文件。但本项目的播放全部走 HTTP（PC 上的 mpv 拉 `/stream`），不需要任何 App 直接访问媒体文件。

**怎么把片子放进去**：`scp` / `sftp`，或通过 [13.9](#139-与其他服务共存网盘--同步) 的 Filebrowser 上传，也可以先放到共享目录再 `mv` 过来。

`WalkDir` 默认不对符号链接做跟随（避免环路），这一点与分区无关。但
`[library].follow_file_symlinks = true` 时允许**跟随指向普通文件的链接**：目录链接依旧不进，
文件链接不可能成环。这条开关是为「下载目录留在原处做种、库这边只放链接」准备的（见 13.10）。

**补充（2026-11-09 实测）**：上表里「硬链接可用」这一条在本机不成立。`ln` 在同一个目录内也返回
`Permission denied`（`$PREFIX/tmp` 同样），是 Android 的 SELinux 策略拦掉了 `link()`，
与文件系统无关。所以「下载目录 + 库」这种场景只能用符号链接，不能用硬链接省掉第二份名字。

### 6.6 多根目录

`extra_roots` 中的每个根独立扫描，`rel_path` 相对各自根。ID 里带根路径，因此两个根下的同名文件不会冲突（见 [5.2](#52-主键设计)）。根的顺序变化只影响 `root_idx`，不影响 `id`。

---

## 7. NFO 规范

### 7.1 电影 `movie.nfo`

```xml
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<movie>
  <title>星际穿越</title>
  <originaltitle>Interstellar</originaltitle>
  <sorttitle>星际穿越</sorttitle>
  <year>2014</year>
  <plot>近未来的地球黄沙遍野，基础农作物相继因枯萎病灭绝……</plot>
  <runtime>169</runtime>
  <ratings>
    <rating name="tmdb" max="10" default="true">
      <value>8.4</value>
      <votes>38000</votes>
    </rating>
  </ratings>
  <mpaa>PG-13</mpaa>
  <uniqueid type="imdb" default="true">tt0816692</uniqueid>
  <uniqueid type="tmdb">157336</uniqueid>
  <genre>科幻</genre>
  <genre>冒险</genre>
  <genre>剧情</genre>
  <country>美国</country>
  <tagline>人类的终点，是另一个开始</tagline>
  <director>克里斯托弗·诺兰</director>
  <credits>乔纳森·诺兰</credits>
  <actor><name>马修·麦康纳</name><role>Cooper</role><order>0</order></actor>
  <actor><name>安妮·海瑟薇</name><role>Brand</role><order>1</order></actor>
  <art>
    <poster>poster.jpg</poster>
    <fanart>fanart.jpg</fanart>
  </art>
  <lockdata>false</lockdata>
</movie>
```

**关键字段说明：**

- `<uniqueid type="...">` — 客户端据此识别影片，**必须有至少一条标准类型**（`imdb` / `tmdb` / `tvdb`），优先级规则见 [7.7](#77-uniqueid-的优先级)
- `<art><poster>` — **只写相对文件名**，不写绝对路径，保证整目录迁移后仍有效
- `<ratings><rating name="...">` — Kodi 18+ 的写法，`name` 取元数据实际来源（`imdb` 或 `tmdb`）；旧版 `<rating>` + `<votes>` 平铺字段也一并写出以兼容老客户端
- `<lockdata>true</lockdata>` — 存在时刮削器跳过该文件，保护手工修改

> v1.0 用的是 legacy 形式 `<fanart><thumb>fanart.jpg</thumb></fanart>` 与 `<thumb aspect="poster">`。Kodi 仍能解析，但新写没有理由不用 `<art>` 块。

### 7.2 剧集 `tvshow.nfo`

```xml
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<tvshow>
  <title>切尔诺贝利</title>
  <originaltitle>Chernobyl</originaltitle>
  <year>2019</year>
  <plot>1986 年 4 月，切尔诺贝利核电站发生爆炸……</plot>
  <ratings>
    <rating name="tmdb" max="10" default="true"><value>9.4</value><votes>6800</votes></rating>
  </ratings>
  <uniqueid type="tmdb" default="true">87108</uniqueid>
  <genre>剧情</genre>
  <genre>历史</genre>
  <season>-1</season>
  <episode>-1</episode>
  <art>
    <poster>poster.jpg</poster>
    <fanart>fanart.jpg</fanart>
  </art>
</tvshow>
```

### 7.3 单集 `S01E01.nfo`

```xml
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<episodedetails>
  <title>1:23:45</title>
  <season>1</season>
  <episode>1</episode>
  <plot>爆炸发生当晚的经过……</plot>
  <runtime>65</runtime>
  <ratings>
    <rating name="tmdb" max="10" default="true"><value>9.1</value></rating>
  </ratings>
  <uniqueid type="tmdb" default="true">1569757</uniqueid>
  <art>
    <thumb>thumb.jpg</thumb>
  </art>
</episodedetails>
```

单集 NFO 必须与视频**同名同目录**（`切尔诺贝利 S01E01.mkv` → `切尔诺贝利 S01E01.nfo`），否则 Kodi 不会关联。`tvshow.nfo` 落在剧集目录（季目录的上一层）。

### 7.4 图片命名约定

| 文件名             | 用途    | 来源                                  | 目标尺寸        |
| --------------- | ----- | ----------------------------------- | ----------- |
| `poster.jpg`    | 竖版海报  | OMDb `Poster` / TMDB `poster_path`   | 长边 ≥ 500px  |
| `fanart.jpg`    | 横版背景  | TMDB `backdrop_path` / 豆瓣封面（次选） | 宽 1280px    |
| `clearlogo.png` | 透明标题图 | TMDB `images.logos`                 | original（可选） |
| `thumb.jpg`     | 单集截图  | TMDB `still_path`                    | w300        |

**OMDb 没有 fanart**——它只返回一张竖版海报。所以纯 OMDb 刮削时 `fanart.jpg` 会缺失，详情页背景留空。想让详情页好看，必须靠 TMDB（需代理）或豆瓣封面，二者都不可用时接受留空。

**不做本地缩放**：直接向源站请求目标尺寸，避免引入图像处理库（会破坏零 CGO 约束）。各源的尺寸参数不同，见 [9.4](#94-图片-get-posterid--fanartid)。

### 7.5 编码与字符

- NFO 一律 UTF-8 无 BOM，首行声明 `encoding="UTF-8"`
- 文件名中的 `: / \ ? * " < > |` 替换为全角对应字符或空格
- NFO 内部文本保留原始字符，不做任何替换
- 中文片名不做拼音转换

### 7.6 语言回落的字段级语义

`language = "zh-CN"` 的主要问题是**中文简介的空值率很高**（TMDB 大量老片/冷门片只有英文 overview；OMDb 则只有英文）。如果只在「整条没搜到」时才回落，会得到一墙没有简介的 NFO。

因此 `fallback_language` 是**字段级**的：

```
对 plot / tagline / overview 这类长文本：
    先取 language 版本 → 若为空、长度 < 10 或与原文相同，取 fallback_language 版本
对 title：
    先取 language 版本 → 若与 original_title 相同且 zh 版本存在，仍用 zh
    （避免把 English title 当中文标题写进 <title>）
对 genre / country / 人名：
    不做回落，TMDB 已按 language 返回本地化文本
```

### 7.7 `uniqueid` 的优先级

Kodi 只认标准类型：`imdb` / `tmdb` / `tvdb`。**豆瓣 ID 没有任何位置可以放**——写进 `<uniqueid type="douban">` 等于注释，Kodi 会忽略。

```
1. 有 ImdbID        → <uniqueid type="imdb" default="true">tt0816692</uniqueid>
2. 同时有 TmdbID    → 再加一条 <uniqueid type="tmdb">157336</uniqueid>（default 仍在 imdb）
3. 只有 TmdbID      → <uniqueid type="tmdb" default="true">...</uniqueid>
4. 只有豆瓣 ID      → 不写 uniqueid，降级状态（见下）
```

**为什么 imdb 优先**：首选源 OMDb 只提供 IMDb ID；且 IMDb ID 是跨平台最通用的标识。TMDB ID 作为第二标识一并写入，将来接入 TMDB 时无需重刮。

**第 4 种情况的后果必须说清楚**：若某条目只有豆瓣数据（典型是冷门国产片、老港片），NFO 里没有标准 `uniqueid`，Kodi 每次媒体库更新都会重新识别，可能认错或反复提示。这类条目在 `unmatched.txt` 里单独标注 `no-standard-id`，提示用户手工补一个 IMDb ID：

```
电影/某冷门片 (1998)/某冷门片 (1998).mkv   → 无标准 ID，请手工补 imdb_id
```

`override.json` 因此支持直接给 ID：

```json
{ "imdb_id": "tt0123456" }
```

---

## 8. 刮削流程

### 8.1 主流程图

```
开始
 ├─ 遍历每个 root（filepath.WalkDir）
 │   ├─ 跳过隐藏目录、$RECYCLE.BIN、@eaDir、.LiteMS
 │   ├─ 视频扩展名匹配 → 生成 item
 │   └─ 读同目录 NFO，存在则解析为已有元数据
 │
 ├─ 对每个 item：
 │   ├─ 命中增量规则 → 跳过
 │   ├─ NFO 中有 <lockdata>true</lockdata> → 跳过，标记 manual
 │   ├─ 查询刮削后端（OMDb 默认 / TMDB 可选）
 │   │   ├─ 命中 → 写 NFO + 下载图片 → status=ok
 │   │   ├─ 未命中 → 写 unmatched.txt → status=miss
 │   │   └─ 网络错误 → 重试 3 次（指数退避）→ status=error
 │   └─ 更新 SQLite
 │
 └─ 输出摘要：新增 N / 更新 N / 失败 N
```

### 8.2 刮削后端（可插拔）

数据源不写死。同一接口，配置里切换，NFO 生成逻辑完全不受影响。

```go
type Query struct{ Title string; Year int; Kind Kind }

type Candidate struct {
    Provider      string
    Key           string // 数据源自己的标识：TMDB 是数字 ID，OMDb 是 tt 号
    Title         string
    OriginalTitle string
    Year          int
    PosterURL     string
    Popularity    float64
}

type Scraper interface {
    Name() string
    Search(ctx context.Context, q Query) ([]Candidate, error)
    Detail(ctx context.Context, key string, kind Kind) (*Meta, error)
    Season(ctx context.Context, key string, season int) ([]EpisodeMeta, error)
    NativeKey(tmdbID int, imdbID string) string // 本地已有 ID → 数据源 key，转不了返回空串
}
```

`Key` 用 `string` 而不是 `int`，是被 OMDb 逼出来的：它只有 `tt0816692` 这种标识。统一成「数据源自己的标识」之后，调用方只负责透传，不解释它的含义——这是让两个数据源能共用一套流程的关键。

#### 优先级：TMDB 主，OMDb 兜底（v1.5 的判断已推翻）

v1.5 曾把 OMDb 定为首选，理由是「本网络直连可达」。实测之后这个理由站不住：**TMDB 换一个域名就能直连，而 OMDb 的能力缺口无法弥补。**

| 项 | **TMDB（主）** | **OMDb（兜底）** |
| --- | --- | --- |
| 性质 | 官方 API | 第三方，数据来自 IMDb |
| 本网络实测（2026-11-09） | `api.tmdb.org` 返回标准 401，**直连可用**；`api.themoviedb.org` 被污染为 `0.0.0.0` | 401 @2.2s，直连可用 |
| 鉴权 | v4 Bearer（推荐）/ v3 key | `apikey` 查询参数 |
| 中文标题/简介 | 有（`language=zh-CN`） | **无**（没有 language 参数） |
| 背景图 fanart | 有 | **无**（只有一张 Poster） |
| 分集 | 剧照 + 剧情 + 评分 + 播出日期 | 仅标题 / 日期 / 评分 |
| Kodi `uniqueid` | `tmdb` ✓ | `imdb` ✓ |
| 额度 | 约 40 次/10 秒 | 1000 次/天（免费 key） |

**OMDb 不能当主源的三个硬原因**（均来自官方文档核对）：

1. **没有 `language` 参数** —— 只有 IMDb 主标题，中文标题与简介一律拿不到
2. **只有 `Poster` 一个图像字段** —— 没有背景图，详情页 hero 区永远空白
3. **分集几乎为空** —— `?i=<seriesID>&Season=n` 能列集，但每集只有 Title / Released / imdbRating / imdbID，没有剧情也没有剧照

它的正确位置是**兜底**：TMDB 不可用时至少还能拿到标题、年份、英文简介、海报与 IMDb 评分，不至于让整库刮削变成空白。切换方式是 `[scrape].provider = "omdb"`。

**一个横向对照**：真实项目里也没有人把 OMDb 当主源——Kodi 官方刮削器是「TMDB 元数据 + fanart.tv 图片 + IMDb/Trakt 评分」，MoviePilot 的模块优先级是 TMDB → 豆瓣 → Bangumi → TVDB → IMDb → AniList，MetaShark 是豆瓣 → TMDB。OMDb 在它们里面只出现在「取 Rotten Tomatoes 评分」的位置。

#### 端点

**TMDB**

```
GET /search/movie?query=&year=&language=
GET /movie/{id}?append_to_response=credits,alternative_titles
GET /search/tv?query=&first_air_date_year=
GET /tv/{id}?append_to_response=credits,alternative_titles
GET /tv/{id}/season/{n}
GET /find/{imdb_id}?external_source=imdb_id      # 只有 tt 号时反查
```

鉴权优先 `tmdb_token`（v4 Bearer），缺失时用 `tmdb_key`（v3 query 参数）。**注意**：v3 的 key 会出现在 URL 里，网络错误的字符串会带出完整 URL，因此写日志前必须抹掉（`redact()`）。

**OMDb**

```
GET https://www.omdbapi.com/?apikey=<k>&s=<title>&type=movie|series   # 列表搜索
GET https://www.omdbapi.com/?apikey=<k>&i=<imdbID>&plot=full          # 详情
GET https://www.omdbapi.com/?apikey=<k>&i=<seriesID>&Season=<n>       # 分集
```

`Response` 字段为 `"False"` 时表示未命中，错误信息在 `Error` 字段。**这是 HTTP 200 而不是 404**，只看状态码会把它当成功。

**豆瓣：本期不接入。** 官方 API 2018 年关闭，网页详情页 2026 年起有 PoW 挑战，唯一稳定路径是 App 的 Frodo 接口（用从 APK 提取的共享凭据做 HMAC-SHA1 签名）。技术上可行，但它属于灰色依赖，随时可能失效，不该出现在主链路上。中文数据先全部依赖 TMDB 的 `zh-CN`。

#### 限速与重试

| 后端 | 默认并发 | 间隔 | 额度处理 |
| --- | --- | --- | --- |
| TMDB | 2 | 100ms 起 | 429 读 `Retry-After`，同时把间隔翻倍（上限 2s） |
| OMDb | 1 | 200ms | 按日计数，接近 1000 时停止并提示 |

```
收到 429      → 读 Retry-After（秒），全局暂停该时长，并把 interval 翻倍
收到 5xx / 网络错误 → 重试 3 次，间隔 1s / 2s / 4s
收到 401      → 立即失败，报「API Key 无效」
收到 404      → 不重试，记为 miss
OMDb Response=False → 视为 miss，不重试
```

不要把速率写死成常量：配额会随账号等级变化，自适应比算数靠谱。

#### 图片下载

| 源 | 域名 | 本网络实测 | 注意 |
| --- | --- | --- | --- |
| TMDB | `image.tmdb.org` | **200，105KB @3.2s** | 直连可用，不需要代理 |
| OMDb | `m.media-amazon.com` | 200 @1.4s | 尺寸写在文件名里（`_SX300`） |
| 豆瓣 | `img*.doubanio.com` | 未接入 | 有防盗链，需 `Referer` |

图片与 API 是**不同域名**，必须分别验证——这正是「API 通了但图全挂」的常见成因。下载与 API 分开限速：图片并发 4，不占 API 配额。

落盘约定（`write_images` 生效时）：单片目录写 `poster.jpg` / `fanart.jpg`，多片目录加 `<文件名>-` 前缀；剧集的 `tvshow.nfo`、海报、背景图写在**剧集根目录**，单集只写 `<文件名>-thumb.jpg`。图片已存在且非 `--force` 时不重复下载。

### 8.3 匹配决策

匹配分两步走：**先精确查，失败再搜索打分**。

```
第一步：精确查询（仅 OMDb 支持）
    GET ?t=<解析出的标题>&y=<年份>&type=movie
    ├─ 返回单条 → 仍要复核 sim 门槛
    │             （OMDb 会做模糊匹配，拼错也可能返回别的片，不能盲信）
    └─ Response=False → 进第二步

第二步：搜索 + 打分
    OMDb  ?s=<标题>&type=      → 简化列表（Title / Year / imdbID / Poster）
    TMDB  /search/movie        → 完整列表（含 popularity）
    豆瓣  subject_suggest      → 别名列表（仅用于取原名，不参与打分）
```

**跨源回查**（这是多源真正的价值）：

```
中文标题在 OMDb 查不到（OMDb 只认英文原名）
  → 豆瓣 subject_suggest 拿到原名 "Interstellar" 与年份
  → 用该原名回查 OMDb
  → 命中，写入 imdbID，再由豆瓣补中文标题
```

**门槛与排序**

```
硬门槛（任一不满足即淘汰该候选）：
  · kind 必须与查询类型一致
  · 标题相似度 ≥ 0.75  或  命中别名/alternative_titles
  · 若双方都有年份：|Δyear| ≤ 1

排序分（在合格候选里挑最优）：
  sim × 100                                  # 主因子
  + 年份一致 20 / 相差 1 年 10                 # 次因子
  + popularity 归一化 × 5                     # 仅 TMDB 有，OMDb 无此项
```

`sim` = 去掉标点与空格、统一大小写后的 **Levenshtein 相似度比**（`1 - dist/maxlen`），不是二值的「完全一致/包含」。

为什么必须这样：中文片名有大量港台译名差异，《星际效应》对《星际穿越》在「完全/包含」体系下拿 0 分，实际是同一条目。编辑距离比 + 别名表才能救回来。

**OMDb 的破同分手段更少**：`?s=` 不返回评分和票数，所以同年份、同相似度的候选只能按标题长度接近度排序。真需要甄别时，对前 2 个候选各调一次 `Detail` 比票数——这会多消耗配额，只在 `sim` 差距 < 0.05 时触发。

所有候选都不合格 → `miss`，不写 NFO。

### 8.4 人工介入通道

`miss` 的记录追加到 `<媒体根>/.litems/unmatched.txt`：

```
电影/Unknown File (2019)/Unknown File (2019).mkv    → 未找到匹配
```

用户可在同目录放置 `override.json`：

```json
{ "tmdb_id": 157336, "kind": "movie" }
```

下次扫描时优先读取该文件，直接按指定 ID 刮削，`scrape_status` 记 `manual`。

### 8.5 幂等性与并发

- 重复执行 `scan` 不产生副作用（除新增文件）
- 已有 NFO 且 `lockdata=true` 时绝不覆盖
- 图片仅在缺失时下载，不重复请求
- 删除 `index.db` 后重扫，结果与之前一致（NFO 作为输入被读取）
- **图片引用由扫描重建**：`scan` 会检查目录里是否已有 `poster.jpg` / `fanart.jpg`（剧集再往上一层找），命中就写回索引。否则删库重建后海报会凭空消失，与「NFO 与图片是唯一真相」的约定矛盾
- **扫描不清空已刮削条目的图片**：`UpsertItem` 的 `ON CONFLICT` 分支对 `poster_path` / `fanart_path` 用 `COALESCE(NULLIF(excluded.x,''), items.x)`，只在扫描确实发现文件时才覆盖
- **扫描互斥**：`serve` 的后台定时扫描与手动 `scan` 共用一把文件锁（`$HOME/.config/litems/scan.lock`），后者发现前者在跑则等待或跳过，绝不并发跑两份
- **扫描期间限速**：`concurrency=1` 遍历、每处理 50 个文件 `time.Sleep(50ms)`，避免大量随机读抢占磁盘队列、影响正在进行的推流

### 8.6 状态语义

| `scrape_status` | 含义                        | 会被 `--only-missing` 选中 |
| --------------- | ------------------------- | --------------------- |
| `pending`       | 尚未尝试                      | 是（`scraped_at=0`）      |
| `ok`            | 成功                        | 否                     |
| `miss`          | 搜过，无合格候选                  | 否（避免每轮重试烧配额）          |
| `error`         | 网络/服务端错误                  | **是**（临时故障，值得重试）      |
| `manual`        | `lockdata=true` 或 `override.json` | 否                     |

---

## 9. HTTP 接口

### 9.1 通用约定

- 基础路径 `/`，无版本前缀
- 请求/响应均为 `application/json; charset=utf-8`
- 时间戳统一 Unix 秒（进度用毫秒）
- 错误响应：`{"error": "message"}` + 对应状态码
- 所有 `id` 必须匹配 `^[0-9a-f]{16}$`，否则 400（不进入数据库查询）

### 9.2 端点清单

| 方法     | 路径                   | 说明              |
| ------ | -------------------- | --------------- |
| `GET`  | `/`                  | Web UI 首页（HTML） |
| `GET`  | `/item/{id}`         | 详情页（HTML）       |
| `GET`  | `/play/{id}`         | 网页播放页（HTML，沉浸式）  |
| `GET`  | `/api/items`         | 列表查询            |
| `GET`  | `/api/items/{id}`    | 单条详情            |
| `GET`  | `/api/continue`      | 继续观看            |
| `GET`  | `/api/recent`        | 最近添加            |
| `GET`  | `/api/cmd/{id}`      | 返回一行可复制的播放命令（helper 降级通道） |
| `GET`  | `/api/transcode/{id}` | 音轨转码状态（none/running/done/error） |
| `POST` | `/api/transcode/{id}` | 启动音轨转码任务           |
| `GET`  | `/api/progress/{id}` | 读取进度            |
| `POST` | `/api/progress`      | 上报进度            |
| `GET`  | `/poster/{id}`       | 海报图片（支持 `?w=`）  |
| `GET`  | `/fanart/{id}`       | 背景图片            |
| `GET`  | `/stream/{id}`       | 视频流（支持 Range）   |
| `GET`  | `/sub/{id}`          | 外挂字幕（支持 `?lang=`） |
| `GET`  | `/settings`          | 设置页（HTML，写刮削凭据） |
| `POST` | `/settings`          | 保存 / 测试凭据（需 CSRF 令牌） |
| `GET`  | `/healthz`           | 健康检查            |

`/healthz` 的语义要明确：`200` 仅在「数据库可查询」时返回，响应体带 `{"db":"ok","items":1284,"last_scan":1731000000}`。不要只 `return 200`，那样探活探不出东西。

### 9.3 列表查询参数

```
GET /api/items?q=星际&sort=recent&page=1&limit=60&kind=movie&year=2014
```

| 参数      | 取值                                     | 默认       |
| ------- | -------------------------------------- | -------- |
| `q`     | 关键词（匹配 title / original_title，忽略空格、标点与大小写）         | 空        |
| `sort`  | `recent` / `title` / `year` / `rating` | `recent` |
| `page`  | ≥1                                     | 1        |
| `limit` | 1–200                                  | 60       |
| `kind`  | `movie` / `episode`                    | 全部       |
| `year`  | 四位年份                                   | 不限       |

`sort=recent` 按 `mtime_unix DESC`（发现时间），非 `scraped_at`。`sort=rating` 时 `rating IS NULL` 的条目排在最后。

**折叠搜索（v2.10）**：`items.search_key` 保存「标题|原名|文件路径」去掉空格/标点、统一小写
后的键（`SearchKey`，复用 `foldForCompare`），查询时对 key 与 q 双向折叠，所以
`lala` / `LALALAND` / `La La Land` / `la-la land` 都能命中同一部片；原样 LIKE 仍保留兜底。
**文件名必须进键**：国产片的英文国际名（如《捕风追影》= The Shadow's Edge）不在 TMDB
`original_title` 里（那里也是中文），只存在于发布文件名，不带上就搜不到。
老库通过 `migrateSearchKey` 做 `ALTER TABLE ADD COLUMN` 并按 `search_key_ver` 全量重算——
**不升 SchemaVersion**（升版本会整库删除，观看进度一起丢）；键格式变更时靠版本号而非
「空值」触发重算。首页搜索框为实时搜索：180ms 防抖请求本接口，客户端重建卡片，
回车仍走整页 GET 保留分页。

响应：

```json
{
  "total": 128,
  "page": 1,
  "limit": 60,
  "items": [
    {
      "id": "a3f9c1d2e4b5f607",
      "title": "星际穿越",
      "original_title": "Interstellar",
      "year": 2014,
      "kind": "movie",
      "runtime_min": 169,
      "rating": 8.4,
      "poster": "/poster/a3f9c1d2e4b5f607",
      "fanart": "/fanart/a3f9c1d2e4b5f607",
      "subs": [{"lang": "chs", "url": "/sub/a3f9c1d2e4b5f607?lang=chs"}],
      "finished": false,
      "position_ms": 2535000,
      "duration_ms": 10140000
    }
  ]
}
```

### 9.4 图片 `GET /poster/{id}` / `/fanart/{id}`

- 支持 `?w=185|342|500`（poster）、`?w=300|780|1280`（fanart）。实现是**改写源站 URL 的尺寸段重新下载一次并缓存到 `.litems/thumbs/`**，不做本地缩放，不破坏零 CGO
- 未带 `w` 时返回落盘的原图，行为与 v1.0 一致
- 响应带 `Cache-Control: public, max-age=604800` 与 `ETag`（用文件 size+mtime 生成）。海报墙一次 60 张图，没有缓存头每次翻页都要重传 3MB
- 落盘原图缺失时返回 404，前端显示占位色块

**尺寸改写规则按源不同**（这是换源时最容易漏的一处）：

| 源 | URL 形态 | 改法 |
| --- | --- | --- |
| TMDB | `image.tmdb.org/t/p/w500/abc.jpg` | 替换路径段 `w500` → `w185` |
| OMDb（Amazon） | `m.media-amazon.com/images/M/MV5B...@._V1_SX300.jpg` | 替换文件名里的 `SX300` → `SX500` / `SX1000` |
| 豆瓣 | `img9.doubanio.com/.../view/photo/s_ratio_poster/public/xxx.jpg` | `s_ratio_poster` → `l_ratio_poster`（大图）；需带 Referer |

改写失败（URL 形态不认识）时**直接返回原图，不要报错**——宁可用小图，不能没有图。

### 9.5 播放流 `GET /stream/{id}`

- 直接 `http.ServeFile`，自动处理 `Range` / `If-Modified-Since` / `Content-Type`
- 必须支持 `206 Partial Content`，否则播放器无法拖动进度条
- 不加额外缓存头、不做限速（与图片不同，视频不该被浏览器缓存）

**性能（实测，1 GB 单文件）**

| 场景 | 实测 | 结论 |
| --- | --- | --- |
| 单流全量下载 | **31.7 MB/s**（32.3s / 1GB） | 约 254 Mbps |
| 双流并发总吞吐 | 34 MB/s（2GB / 59s） | 仅比单流高 2.3 → **链路已饱和** |
| 存储顺序写（`dd conv=fsync`） | **205 MB/s** | 存储远快于网络，不是瓶颈 |
| Range 分段 1MB | `206`，103ms | 拖动进度条的前提成立 |
| 20 秒慢响应 | 40 次 tick 全部送达 | `WriteTimeout=0` 生效，长连接不被切断 |

瓶颈确认在 Wi-Fi（Windows 移动热点约 272 Mbps 上限），不在存储、不在 Go。4K 高码率片源（80–100 Mbps）有充足余量；换成正经路由器只会更快。

注意这是**热点**环境下的数字，手机直连路由器后应重测一次并回填。

### 9.6 字幕 `GET /sub/{id}`

```
GET /sub/{id}?lang=chs      # 省略 lang 时返回 sub_paths 中的第一个
```

- 原样返回文件字节（`srt` / `ass` / `vtt`），不转 WebVTT
- `Content-Type` 按扩展名映射：`.srt → application/x-subrip`，`.ass → text/x-ssa`，`.vtt → text/vtt`
- 无该语言字幕 → 404，响应体 `{"error":"no such subtitle"}`

### 9.7 进度上报

```http
POST /api/progress
Content-Type: application/json

{ "item_id": "a3f9c1d2e4b5f607", "position_ms": 2535000, "duration_ms": 10140000 }
```

服务端以 `item_id` 为键 upsert。完成判定：

```
duration_ms > 0 且 position_ms / duration_ms > 0.95
    → position_ms = 0, finished = 1
否则
    → finished = 0（重新开始看一部已完成的片子时自动取消完成态）
```

`/api/continue` 的查询条件因此是：

```sql
SELECT ... FROM progress p JOIN items i ON i.id = p.item_id
WHERE p.finished = 0 AND p.position_ms > 30000
ORDER BY p.updated_at DESC LIMIT 20
```

> v1.0 只有 `position_ms`，没有 `finished`，把位置归零后无法区分「刚看完」和「从未播放」，「继续观看」必然混入已看完的条目。这是 v1.1 修掉的结构性缺陷。

### 9.8 设置页 `GET/POST /settings`

只做一件事：把刮削凭据写进配置文件。**不接管配置文件**——只在 `[scrape]` 段里改白名单内的五个键
（`provider` / `language` / `tmdb_token` / `tmdb_key` / `omdb_key`），其余段落、注释、键顺序原样保留。
理由：配置文件是这个项目唯一可手工编辑的配置来源，整份重写会把用户写在里面的注释和注释掉的行一起冲掉。

写入行为：

- 文件不存在则创建，权限 `0600`；内容先写临时文件再 `rename`，不会出现半截配置
- 表单项留空 = 保持原值，避免「只改语言却把 token 清空」
- 「测试连接」用**提交中的值**跑一次 `probeScraper`，不落盘；验证通过再保存

**安全边界**（这是全项目唯一能写文件的外部入口，必须写清楚）：

| 风险 | 处理 |
| --- | --- |
| 跨站表单提交（CSRF） | 表单带进程级随机令牌 `settingsToken`，提交时比对。**只靠 Basic Auth 挡不住**——浏览器会自动为同源请求带上凭据，这正是 CSRF 的前提 |
| 明文密钥出现在页面 | 只回显掩码（首尾各 4 位），字段类型 `password`，`Cache-Control: no-store` |
| 未启用认证时暴露 | 页面显式提示「局域网内任何人都能打开本页」，不做静默处理 |
| 密钥被日志记录 | 任何路径都不打印 token；`probeScraper` 的错误信息在 `tmdb.go` 里做过 API Key 脱敏 |

**生效时机**：保存只影响之后的 `litems scrape`。`serve` 进程内的凭据快照会同步更新用于回显，但浏览与推流本就不依赖它，因此**不需要重启服务**。

### 9.9 认证

可选 Basic Auth。启用后所有端点（含 `/stream`、`/poster`）均需认证。

```toml
[server]
auth_user = "media"
auth_pass = "xxxxxxxx"
```

由于播放器请求 `/stream` 时也要带认证，helper 需要把凭据拼进 URL：

```
http://media:pass@192.168.1.10:7788/stream/a3f9...
```

mpv 与 VLC 均支持 URL 内嵌凭据。两点必须写进 README：

1. **密码会出现在 `ps` 的进程参数里**（同机其他用户可见），也会出现在 helper 的日志里（日志需对含 `@` 的 URL 做脱敏）
2. **Basic Auth 走明文 HTTP，等同把密码广播到局域网**。若在意这一点，关闭认证、仅依赖局域网隔离 + 路由器 MAC 绑定，比「以为有认证但实际明文传输」更诚实。本期不做 TLS；需要外网访问时另设隧道（Cloudflare Tunnel + HTTPS），那时认证才真正有意义

---

## 10. 播放器集成

### 10.1 helper 子命令

```bash
litems launch \
  --listen 127.0.0.1:9876 \
  --server http://192.168.1.10:7788 \
  --token  <与服务端 [launch].token 相同>
```

| 端点                          | 方法       | 说明              |
| --------------------------- | -------- | --------------- |
| `/play`                     | `POST`   | 播放指定条目          |
| `/play`                     | `OPTIONS` | PNA/CORS 预检     |
| `/healthz`                  | `GET`    | 存活检查            |

请求体：

```json
{ "id": "a3f9c1d2e4b5f607", "start": 2535, "token": "..." }
```

`start` 单位为秒，缺省则从头播。

> v1.0 用 `GET /play?id=`。**有副作用的 GET 必须改掉**：任何你访问的网页都可以用 `<img src="http://127.0.0.1:9876/play?id=xxx">` 盲打这个端点——`no-cors` 拿不到响应，但请求照发，播放器照样被拉起。改 POST 并不会自动解决（表单也能 POST），所以真正的防线是下一节的 token 与 Origin 校验。

### 10.2 三道校验

| 校验      | 实现                                                          |
| ------- | ----------------------------------------------------------- |
| 只监听回环   | `--listen` 不是 `127.0.0.1` / `::1` / `localhost` 时拒绝启动           |
| Origin  | 请求头 `Origin` 必须等于配置的 `server` 的 origin，否则 403；无 Origin 头（curl 等本地工具）时放行 |
| Token   | 请求体 `token` 必须与服务端 `[launch].token` 一致，否则 403；未配置 token 时拒绝启动 |

`token` 由服务端在渲染页面时嵌入（`<meta name="helper-token" content="...">`），helper 与服务端共用配置文件里的同一个值。这样即使别的网页盲打了 `/play`，也过不了 token 这一关。

### 10.3 CORS / PNA 预检（浏览器会卡在这里）

```http
OPTIONS /play HTTP/1.1
Origin: http://192.168.1.10:7788

HTTP/1.1 204 No Content
Access-Control-Allow-Origin: http://192.168.1.10:7788
Access-Control-Allow-Methods: POST, OPTIONS
Access-Control-Allow-Headers: content-type
Access-Control-Allow-Private-Network: true
Access-Control-Max-Age: 600
```

前端用标准 `fetch(..., {method:'POST', mode:'cors'})`，**不要用 `no-cors`**——`no-cors` 无法通过 PNA 预检，请求会被浏览器静默阻断。

`Access-Control-Allow-Private-Network: true` 是 Chrome 从私网页面访问 localhost 的必需响应头。不同浏览器版本策略有差异，**必须在 M0 阶段真机验证**（见 [16-里程碑](#16-里程碑)），失败则启用降级通道。

### 10.4 降级通道 `/api/cmd/{id}`

服务端返回一行可直接粘贴到终端的命令：

```json
{
  "command": "mpv \"http://192.168.1.10:7788/stream/a3f9...\" --sub-file=\"http://192.168.1.10:7788/sub/a3f9...?lang=chs\" --start=2535",
  "server_auth_hint": "含认证时命令里会内嵌 user:pass"
}
```

Web UI 的播放按钮旁边放一个「复制命令」的小图标。成本约 20 行，收益是 helper 彻底失效（PNA 拦截、播放器未装、防火墙）时用户仍然能看片。
> v2.9 起 UI 已移除「复制命令」与「直接打开」按钮（网页播放成为默认播放），本端点保留为程序化降级通道；mpv 播放退居详情页的「mpv 播放」次按钮。

### 10.5 播放器命令模板

配置中定义模板，`{url}` 与 `{sub}`、`{start}` 为占位符：

```toml
[[launch.players]]
name    = "mpv"
command = "mpv"
args    = ["{url}", "--sub-file={sub}", "--start={start}", "--force-window=yes"]

[[launch.players]]
name    = "vlc"
command = "vlc"
args    = ["{url}", "--sub-file={sub}", "--start-time={start}"]
```

占位符替换规则：

- `{sub}` 为空（该条目无外挂字幕）→ **整个参数项被剔除**，而不是替换成空串（`--sub-file=` 会让 mpv 报错）
- `{start}` 为 0 → 同样剔除整个参数项
- 多个字幕语言 → 生成多个 `--sub-file` 参数（mpv 支持），按 `sub_langs` 顺序

### 10.6 安全约束

| 约束     | 实现                                         |
| ------ | ------------------------------------------ |
| 只监听回环  | 硬编码校验，`--listen` 非回环地址时拒绝启动                |
| 不经过 shell | `exec.Command(name, args...)`，禁止字符串拼 shell |
| 播放器白名单 | 只允许配置文件中定义的播放器名，URL 由服务端构造，不接受客户端传入任意 URL |
| ID 校验  | `id` 必须匹配 `^[0-9a-f]{16}$`                |
| 环境隔离   | `cmd.Env` 只传白名单变量（`PATH`、`HOME`、`DISPLAY`/`WAYLAND_DISPLAY`），不继承全部环境 |
| 子进程回收  | 保存 `*exec.Cmd`，helper 退出时转发信号；已有一个播放进程在跑时不重复启动 |

### 10.7 进度回传（M4 实现）

mpv 启动时附加：

```
--input-ipc-server=/tmp/litems-mpv.sock     (Linux/macOS)
--input-ipc-server=\\.\pipe\litems-mpv      (Windows)
```

helper 连接该 socket，每 10 秒发送：

```json
{"command": ["get_property", "time-pos"]}
```

读到位置后 POST 回服务端 `/api/progress`。播放器退出时再上报一次。VLC 无对应的简单 IPC，此功能仅对 mpv 生效。

### 10.8 网页播放页 `/play/{id}`

详情页只承担信息浏览，网页播放跳到专门的沉浸式页面（v2.7 起，不再内嵌在详情页里）。
页面职责：ArtPlayer 播放 `/stream/{id}`、外挂字幕（`/sub/{id}?fmt=vtt`，设置面板多语言切换）、
续播 >30s 跳转、进度上报（节流 10s + pause/ended + `sendBeacon`）。

**无声提示（audio.go）**：浏览器内核自带解码器只有 AAC/MP3/FLAC/Opus/Vorbis，
DTS/TrueHD/Atmos/PCM（bad 级）与 AC3/EAC3/DDP（maybe 级）解不了时的表现是
**画面正常播放、零报错、没声音**——`video:error` 不触发，前端无从感知。
LiteMS 不做容器级嗅探，改从文件名/容器名识别发布组惯标（`audioHint`，正则边界注意
`DDP5.1` 尾随的是数字），在播放页顶部分级提示。aac/flac 等通用编码不提示。

**静音兜底**：从详情页点过来是一次导航，新页面的有声自动播放可能被浏览器策略拦截
（表现为停在暂停）。起播 1.2s 后仍 paused 则静音重试一次，并提示用户点音量图标恢复。

### 10.9 音轨转码（v2.8）

浏览器内核只带 AAC/MP3/FLAC/Opus 解码器，EAC3/DTS/TrueHD 一律解不了——画面正常、
无声、零报错（实测 EAC3 在 MKV 里连 Edge 的 Dolby 解码器都不生效，MKV 走 Chromium
内置 demuxer，音轨在这一层就被丢了）。**客户端转码不可行**（WebCodecs 的解码器集合
与 `<video>` 相同；ffmpeg.wasm 体积与性能都不现实），要出声只能服务端转。

- 依赖：`pkg install ffmpeg`（Termux）；未安装时启动接口报错引导
- 命令：`ffmpeg -i src -map 0:v:0 -map 0:a:0? -map 0:s? -c:v copy -c:a aac -b:a 320k
  -c:s copy -progress pipe:1 -f matroska <cache>/<id>.mkv.part`
  视频原样复制（画质无损、CPU 几乎为零），实际耗时全部在音频 AAC 编码，
  2 小时片约 15 分钟（手机端实测）
- 缓存：`<database 目录>/transcode/<id>.mkv`——$HOME 私有存储，不进媒体库、
  不会被扫描捡走。完成时 rename 原子落位；启动时清残留 .part；缓存 mtime 对齐源文件
- 播放：`/stream/{id}?web=1` 有缓存优先返回转码版（完整文件，Range/拖进度正常），
  播放页固定走它；转码完成后播放页的无声提示自动消失
- 状态：只存内存 + 以缓存文件存在为准（服务重启后 done 依然成立）；
  同一时刻只跑一个任务；源文件更新过（mtime 更新）则重转
- UI：**详情页**「转换音轨」按钮（audioHint 命中 bad/maybe 时显示）+ 进度条
  （`/api/transcode/{id}` 轮询，2s 间隔）；播放页不放按钮和进度
- 诊断工具：`mkvprobe/` 纯 Go EBML 解析，打印容器内真实音轨 CodecID/声道/语言
  （文件名标签可能缺失或不准，如本例文件名无 EAC3 但目录名有）

---

## 11. 配置文件

默认路径：`$HOME/.config/litems/config.toml`（Linux/macOS）、`%APPDATA%\litems\config.toml`（Windows）。可用 `--config` 覆盖。

```toml
[server]
listen    = "0.0.0.0:7788"
# auth_user = "media"
# auth_pass = "changeme"
read_header_timeout_sec = 10
idle_timeout_sec        = 120

[net]
# 实测见 13.2：本网络外网 UDP 53 全阻断，只有网关 DNS 可达。
# "auto"  = 探测本机出口地址推断网关并用作 DNS（推荐，换网络无需改配置）
# ""      = 用系统默认。GOOS=linux 产物读 /etc/resolv.conf，Termux 上该文件不存在 → 必然失败
# "IP:53" = 写死，如 "192.168.137.1:53"
dns = "auto"
# HTTP / SOCKS 代理。TMDB 域名被污染，必须走代理；OMDb 不需要。
# 支持 "socks5://127.0.0.1:7890" 与 "http://127.0.0.1:7890"
proxy = ""

[library]
root = "/data/data/com.termux/files/home/Media"   # ext4，勿改到共享目录，理由见 6.5
# 可追加多个根目录
# extra_roots = ["/storage/1A2B-3C4D/Movies"]
video_ext = [".mkv", ".mp4", ".avi", ".ts", ".m4v", ".mov", ".wmv"]
sub_ext   = [".srt", ".ass", ".ssa", ".vtt"]

[database]
path = "/data/data/com.termux/files/home/.config/litems/index.db"

[scrape]
provider          = "tmdb"        # tmdb（默认，元数据最全）| omdb（兜底，只有英文）
omdb_key          = ""            # https://www.omdbapi.com/apikey.aspx 免费 1000 次/天
tmdb_token        = ""            # v4 Read Access Token，推荐
tmdb_key          = ""            # v3 兼容，token 为空时才用
tmdb_base         = "https://api.tmdb.org/3"          # 注意不是 api.themoviedb.org（已污染，见 13.2.5）
tmdb_image_base   = "https://image.tmdb.org/t/p"
language          = "zh-CN"
fallback_language = "en-US"       # 字段级回落：中文 overview 为空时补一次
concurrency       = 2             # 起步值，遇 429 自动降级
interval_ms       = 100
daily_limit       = 1000          # OMDb 免费额度，达到后停止并在摘要中提示
write_nfo         = true
write_images      = true
poster_size       = "w500"
fanart_size       = "w1280"
title_sim_min     = 0.75
sub_langs         = ["chs", "cht", "eng"]

[ui]
title     = "我的媒体库"
theme     = "dark"
page_size = 60

[watch]
interval_min = 30                 # 后台增量扫描间隔，0 = 关闭
enabled      = true

[launch]
listen  = "127.0.0.1:9876"
server  = "http://192.168.1.10:7788"
token   = "change-me-to-a-random-string"

[[launch.players]]
name    = "mpv"
command = "mpv"
args    = ["{url}", "--sub-file={sub}", "--start={start}"]
```

**路径规则**：支持 `~` 展开（展开为用户主目录），不支持环境变量插值。

**配置校验**：启动时校验，任一失败即退出并打印具体键名——

- `[scrape].provider` 对应的 key 为空，且 `write_nfo = true` → 警告（可以只索引不刮削）
  - `provider = "omdb"` 但 `omdb_key` 为空
  - `provider = "tmdb"` 但 `tmdb_token` 与 `tmdb_key` 都为空
- `[scrape].tmdb_base` 指向 `api.themoviedb.org` → **警告**，提示改成 `api.tmdb.org`（前者被污染，见 [13.2.5](#1325-换一个域名就绕开了污染)）
- `[net].dns` 为 `"auto"`：探测出口地址推导网关（13.2.5），探测不到 → **拒绝启动**并打印排查清单
- `[net].dns` 为具体地址但探测不可达 → **拒绝启动**（配了却用不了比不配更危险）
- `[scrape]` 的凭据为空 → `scrape` 直接报错退出（`scan`/`serve` 不受影响，它们不联网）
- `[net].proxy` 非空但连不通 → **警告**，按无代理运行
- `[watch]` 段缺失 → 视为 `enabled=false`（v1.0 在 12.2 承诺「可配置关闭」但配置里没有这个键）
- `[launch].token` 为空 → `launch` 子命令拒绝启动
- `[database].path` 落在 `/sdcard` 或 `/storage/emulated` 下 → **直接拒绝启动**，见 [5.4](#54-存储位置全部落在-ext4)
- `[library].root` 落在共享目录 → **警告但不阻止**，功能可用但性能下降，见 [6.5](#65-为什么媒体库放-home-而不是共享目录)

---

## 12. CLI 设计

```
litems <command> [flags]

命令：
  scan      扫描媒体库，写入索引（不联网）
  scrape    按索引刮削元数据，写 NFO 与图片
  serve     启动 HTTP 服务
  launch    启动本地播放器 helper（PC 侧）
  version   打印版本

全局：
  --config PATH    配置文件路径
  --verbose        调试日志
```

**scan 与 scrape 分离**（v1.8 调整）：v1.0 让 `scan` 一并刮削，实际用下来两个问题——断网时连索引都建不了；刮削失败会污染「扫描成功」的语义。拆开之后，`scan` 只碰文件系统（毫秒级、可反复跑），`scrape` 才联网（分钟级、受配额限制），互不牵连。

### 12.1 scan

```
litems scan [--dry-run] [--force] [--only-missing] [--id ID] [--reindex]
```

| 参数 | 说明 |
| --- | --- |
| `--dry-run` | 只打印将执行的操作，不写库 |
| `--force` | 忽略增量判定，重写所有索引行 |
| `--only-missing` | 仅处理尚未刮削与上次失败的条目（快速补齐用） |
| `--id` | 只处理指定 ID，用于调试单条 |
| `--reindex` | 删除 `index.db` 后全量重建（NFO 作为输入被重读） |

> `--unlock`（临时忽略 `lockdata`）在 v1.1 提到过，实现推迟到 M5：它会覆盖用户手工维护的 NFO，需要先做备份再动手，不宜和主流程一起上线。

输出：

```
扫描 /data/data/com.termux/files/home/Media
  发现视频文件      1284
  索引需要更新      37
  ├─ 新增          34
  ├─ 更新           3
  ├─ 跳过         1247
  └─ 失败           0
  待刮削            37（缺元数据，执行 scrape 阶段处理）
  耗时             1.2s
  索引条目          1284
```

### 12.2 scrape

```
litems scrape [--probe] [--dry-run] [--force] [--id ID] [--limit N] [--provider NAME]
```

| 参数 | 说明 |
| --- | --- |
| `--probe` | 只发一次真实请求，验证凭据与网络，然后退出 |
| `--dry-run` | 列出待刮削条目，不调 API、不写文件（缺凭据也允许跑） |
| `--force` | 忽略 `scraped_at`，全部重刮（仍尊重 `lockdata`） |
| `--id` | 只处理指定 ID |
| `--limit` | 最多处理 N 条，先试几条看效果 |
| `--provider` | 临时覆盖 `[scrape].provider`（`tmdb` / `omdb`） |

选择逻辑：`scraped_at = 0` 或 `scrape_status = 'error'` 的条目会被处理。`miss` 与 `manual` 不在此列——它们算「处理过」，反复重试只会白烧配额；要重试就 `--force` 或改文件名（mtime 变化会自动重新纳入）。

**serve 启动补刮（2026-11-09 起）**：serve 启动 5 秒后在后台自动跑一轮上面的逻辑，另外把
「状态是成功、但海报文件实际不在（下载中断 / 被误删）」的条目也捡回来重试（`ResetScrapeStatus`
打回 pending）。用户不需要手动 `scrape --force`。渲染侧则要求「DB 有路径且文件在盘上」才输出
图片 URL——否则显示占位符。占位符因此只代表一种状态：**没有真海报**。

输出：

```
数据源 tmdb  接口 https://api.tmdb.org/3  语言 zh-CN
DNS 网关 192.168.137.1（由本机 192.168.137.193 推断）
  [成功] 星际穿越                TMDB 157336   +2 图
  [成功] 切尔诺贝利 S01E01  1:23:45
  [未匹配] 番剧/某番名/[SubGroup] 某番名 - 01 [1080p].mkv：未找到匹配（候选 3 个，最高相似度 0.31）

  待处理            3
  ├─ 成功          2
  ├─ 未匹配        1
  ├─ 已锁定        0（lockdata=true，未覆盖）
  ├─ 跳过          0
  └─ 失败          0
  下载图片          5 张
  耗时             4.2s
```

幂等性：重复执行不覆盖 `lockdata=true` 的 NFO，已存在的图片不重复下载，已成功的条目不再进入队列。

### 12.3 serve

```
litems serve [--listen 0.0.0.0:7788] [--no-watch]
```

启动时若索引为空，提示先执行 `scan`。`[watch].enabled=true` 时后台每 `interval_min` 分钟触发一次增量**扫描**（`--no-watch` 可临时关闭）。注意后台定时任务只做索引，不做刮削——刮削是显式命令，避免在你不注意的时候烧配额。

### 12.4 launch

```
litems launch [--player mpv] [--port 9876]
```

启动后在终端打印：

```
helper 已启动  http://127.0.0.1:9876
服务端         http://192.168.1.10:7788
播放器         mpv
如果浏览器点击无反应，请在网页上使用「复制播放命令」。
```

---

## 13. Termux 部署

### 13.1 前置条件

| 项          | 要求                                                                |
| ---------- | ----------------------------------------------------------------- |
| Termux     | 从 F-Droid 或 GitHub Releases 安装（**不要用 Google Play 版本**，已停止维护且签名不同） |
| 存储权限       | **LiteMS 自身不需要**。媒体库在 `$HOME`，属 Termux 自有空间。只有在部署 Filebrowser / Syncthing 访问共享目录时，才需 `termux-setup-storage` + 「所有文件访问权限」 |
| 架构         | arm64（K20 Pro 为 `aarch64`）                                        |
| Android 版本 | 9 及以上                                                             |

### 13.2 网络可达性（已实测）

以下结论来自 2026-11-08 首测与 2026-11-09 补测，不是推断。换网络环境后跑一次 `litems scrape --probe` 即可覆盖全部检查（见 13.2.8）。

#### 13.2.1 外网 UDP 53 被阻断

| DNS 服务器 | UDP 53 探测 | 结果 |
| --- | --- | --- |
| `8.8.8.8` / `8.8.4.4` | `dial udp 8.8.4.4:53: i/o timeout` | 不可达 |
| `223.5.5.5`（阿里） | `dial udp 223.5.5.5:53: i/o timeout` | 不可达 |
| `192.168.137.1`（网关） | 8ms 返回 `183.2.172.177` | **可用** |

ICMP 是通的（`ping 223.5.5.5` 14ms 无丢包），只有 UDP 53 出站被拦。这意味着**任何公共 DNS 都救不了场，只能用本地网关或路由器下发的 DNS**。

#### 13.2.2 Termux 的 Go 读 `$PREFIX/etc/resolv.conf`，不是 `/etc/resolv.conf`

```
/etc                  → /system/etc（符号链接，里面没有 resolv.conf）
/etc/resolv.conf      → 不存在
$PREFIX/etc/resolv.conf → 存在，默认内容 "nameserver 8.8.8.8 / 8.8.4.4"
```

Go 源码硬编码读 `/etc/resolv.conf`，但 **Termux 的 golang 包打了路径补丁**，改读 `$PREFIX/etc/resolv.conf`。证据：`CGO_ENABLED=0` 编译的二进制报错为 `lookup ... on 8.8.4.4:53`，正是该文件里的第二个 nameserver；把文件改写成网关地址后立即恢复。

⚠️ 该文件默认指向 **8.8.8.8**，在本网络下不可达 —— 这就是「纯静态二进制完全没有网络」的直接原因。

#### 13.2.3 三个组合的行为差异（实测）

`GOOS` 与 `CGO_ENABLED` 是两个独立开关，组合出的三种形态 DNS 行为各不相同——这是本项目最容易误判的一处。

| 组合 | ELF 形态 | 体积 | DNS 配置来源 | 默认配置下 |
| --- | --- | --- | --- | --- |
| `GOOS=android` `CGO=0`<br>（在 Termux 里 `go build` 的默认） | `DYN`(PIE)，带 INTERP → `/system/bin/linker64` | 5.9M | `$PREFIX/etc/resolv.conf`<br>**Termux 的 golang 包打了路径补丁** | 不可用（该文件默认指 8.8.8.8） |
| `GOOS=android` `CGO=1`<br>（Termux 的 Go 默认） | `DYN` + INTERP，链接 `libc.so` / `libdl.so` | 8.3M | bionic `getaddrinfo` → netd → 系统下发 | **开箱可用** |
| `GOOS=linux` `CGO=0`<br>（文档 14.2 采用） | `EXEC`，**无 INTERP，真静态** | 5.3M | `/etc/resolv.conf`（**不存在**，无补丁）→ 回退 `[::1]:53` | 不可用，`connection refused` |

注意第一行：`CGO_ENABLED=0` 在 Termux 里产出的**不是真静态二进制**，它仍是 PIE 且由 Android 的 linker 加载。只有 `GOOS=linux` 才是 `EXEC` + 无 INTERP 的真静态。

**对照实验 A（证明 CGO=1 不看 resolv.conf）**

把 `resolv.conf` 指向不可达的 `8.8.8.8`，同一时刻跑两个产物：

```
CGO=0 (android)  DNS-go FAIL  dial udp 8.8.8.8:53: i/o timeout  (10.0s)
                 HTTPS    FAIL  context deadline exceeded
CGO=1 (android)  DNS-go ok    10ms
                 HTTPS    ok    501ms  → status=200
```

CGO=1 无视那份坏配置，因为它走 netd。

**对照实验 B（证明 linux 目标可以靠程序内 DNS 自救）**

```
USE_ALT=1 DNS_ALT=192.168.137.1 ./probe-linux www.baidu.com
  runtime  linux/arm64  go1.27.3
  DNS-go   ok    5ms   → [183.2.172.177 ...]
  HTTPS    ok    442ms → status=200
  write    ok
```

**结论**

1. 用 `GOOS=android` 时：改 `$PREFIX/etc/resolv.conf` 即可修复
2. 用 `GOOS=linux` 时：**改 `resolv.conf` 无效，必须依赖 `[net].dns`**
3. 两者都能工作，但 linux 目标更省心——不依赖系统文件补丁，产物可扔到任何 arm64 Linux

**本项目选 linux 目标，且 `[net].dns` 为必填项。**

#### 13.2.4 各域名的实际可达性

2026-11-08 首测，2026-11-09 补测 `api.tmdb.org`、`image.tmdb.org`、`api.bgm.tv`、`frodo.douban.com`。下表为合并后的结论：

| 域名 | 结果 | 说明 |
| --- | --- | --- |
| `api.themoviedb.org` | 解析为 `0.0.0.0`，连接被拒 | **污染** |
| `api.tmdb.org` | `18.155.173.85`，`401` + 标准 TMDB JSON | **可用**，与上面是同一后端 |
| `image.tmdb.org` | `200`，105KB @3.2s | **可用**（首测误记为「与 API 同域，同样取不到」，实为独立 CDN） |
| `api.bgm.tv` | 解析为 `127.0.0.1` | 污染 |
| `frodo.douban.com` | `400`（缺签名参数，域名本身可达） | 可达 |
| `www.omdbapi.com` | `401` @2.2s | 可达（401 = 未带 key） |
| `www.imdb.com` | `403` @3.5s | 可达但被 Cloudflare 反爬拦截，不要指望爬页面 |
| `m.media-amazon.com` | `200` @1.4s | IMDb 图片 CDN，完全可用 |
| `datasets.imdbws.com` | `200` @4.3s | IMDb 官方数据集可下载 |
| `www.baidu.com` | `200` @0.66s | 基准参照 |

#### 13.2.5 换一个域名就绕开了污染

**污染是逐个域名做的，不是整站封禁。** TMDB 的 API 有两个域名指向同一后端，其中 `api.tmdb.org` 未被污染：

| 域名 | 结果 | 判断 |
| --- | --- | --- |
| `api.themoviedb.org` | 解析为 `0.0.0.0`，连接被拒 | **污染** |
| `api.tmdb.org` | `18.155.173.85`，返回 `401` + 标准 TMDB 错误 JSON | **可用**，与上面是同一后端 |

决定性证据是 401 的响应体：

```
$ curl -s https://api.tmdb.org/3/movie/157336
{"status_code":7,"status_message":"Invalid API key: You must be granted a valid key.","success":false}
```

假 key 也返回同样的 JSON，说明这不是山寨镜像，就是 TMDB 自己在应答。

**结论：本项目不需要代理。** `[scrape].tmdb_base` 默认指向 `https://api.tmdb.org/3`，图片 CDN 也直连。这直接推翻了 v1.5 的判断——当时误以为「TMDB 全部域名被污染」，于是把 OMDb 当成首选源，用中文简介、背景图和分集剧照去换「不用代理」，代价过大。

风险仍在：污染名单是可变的，`api.tmdb.org` 也可能某天被加入。所以域名必须做成配置项，并且每次 `scrape` 启动都做一次解析自检，不靠「上次能通」的假设。

#### 13.2.5.1 DNS 自检的三级回退

`net.go` 里的 `setupDNS` 在每次 `scrape` 启动时执行：

```
1. 配置指定 [net].dns = "IP"     → 用它，但必须先验证能解析（配了却用不了比不配更危险）
2. 系统默认解析器                 → 桌面平台通常这一步就成
3. 由本机出口地址推断网关          → UDP connect 拿本机 IP，取同网段 .1
4. 公共 DNS（223.5.5.5 / 1.1.1.1 / 8.8.8.8）
全部失败 → 打印排查清单，但不退出（索引与播放不受影响）
```

实测输出：

```
DNS 网关 192.168.137.1（由本机 192.168.137.193 推断）
```

#### 13.2.6 最小可用配置

本项目用 `GOOS=linux` 构建（[14.2](#142-termux-目标)），`$PREFIX/etc/resolv.conf` 对它**无效**，必须靠 `[net].dns`：

```toml
[net]
dns = "auto"          # 自动探测；也可写死 "192.168.137.1:53"
```

`auto` 的探测依赖 UDP `connect`，不需要任何权限：

```go
// UDP 没有握手，connect 不实际发包，但内核会据此选路并填入本端地址
c, _ := net.Dial("udp4", "8.8.8.8:80")
ip := c.LocalAddr().(*net.UDPAddr).IP        // 实测：192.168.137.193
// 同网段的 .1 即网关
```

**为什么不用 `ip route` 或 `/proc/net/route`**：

```bash
$ ip route                      → 空输出（无权限）
$ cat /proc/net/route           → Permission denied（Android 10+ 侧信道防护）
```

所以只能靠 UDP `connect` 这个内核行为反推。这是 `auto` 值得实现 30 行的原因——它把「换网络就要改配置」彻底消掉了。

**改 `resolv.conf` 对本项目无效**：那只对 `GOOS=android` 的产物起作用（[13.2.3](#1323-三个组合的行为差异实测)）。留着不用管，Termux 的 `curl` 走 bionic 也不看它。

#### 13.2.7 代理

现在不需要代理。仅在 `api.tmdb.org` 也被污染时才用得上——已验证 Go 的 `http.ProxyFromEnvironment` 支持 `socks5://`，且走代理时域名由代理解析，本地 DNS 完全不参与。

```toml
[net]
proxy = "socks5://192.168.1.100:1080"
```

注意一个前提：若代理运行在 PC 上，手机需能访问其监听端口——只绑 `127.0.0.1` 的客户端（多数代理软件默认如此）手机连不到，必须开启「允许局域网连接」，或走 SSH 反向隧道。代理本身的部署不属于本项目（见 [13.9](#139-与其他服务共存网盘--同步)）。

#### 13.2.8 换网络后怎么自检

一条命令足够：

```bash
$HOME/bin/litems scrape --probe
```

它会依次做：DNS 自检 → 一次 `/configuration` → 一次搜索 → 一次详情，全部打印出来。手工核对时看这几处：

```bash
ping -c1 api.tmdb.org                 # 若显示 127.0.0.1/0.0.0.0，说明新环境把 TMDB 也污染了
curl -m 8 -o /dev/null -w "%{http_code}\n" https://api.tmdb.org/3/movie/157336   # 401 = 正常
curl -m 8 -o /dev/null -w "%{http_code}\n" https://image.tmdb.org/t/p/w500/gEU2QniE6E77NI6lCU6MxlNBvIx.jpg  # 200 = 图片可用
```

**注意**：`curl` 通不代表 LiteMS 通——curl 走 bionic 的系统 DNS，而 `GOOS=linux` 的静态二进制走 `[net].dns`。两者是不同路径，必须用 `--probe` 验。

### 13.3 安装步骤

```bash
# 1. 基础环境
pkg update && pkg upgrade -y
pkg install -y termux-services

# 2. 放置二进制
mkdir -p $HOME/bin
cp litems-linux-arm64 $HOME/bin/litems
chmod +x $HOME/bin/litems

# 3. 配置
mkdir -p $HOME/.config/litems
$EDITOR $HOME/.config/litems/config.toml

# 4. 首次刮削
$HOME/bin/litems scan --dry-run
$HOME/bin/litems scan

# 5. 试运行
$HOME/bin/litems serve
```

### 13.4 常驻运行

创建 runit 服务。**run 脚本内必须写绝对路径**：

`runsv` 由 Termux:Boot 拉起的 `runsvdir` 派生，环境变量继承链不保证有 `HOME` / `PREFIX`。用 `$HOME` 展开在「手动 sv up」时能跑、重启后却全 down —— 这种只在重启后出现的故障极难排查。

```bash
mkdir -p $PREFIX/var/service/litems/log

cat > $PREFIX/var/service/litems/run <<'EOF'
#!/data/data/com.termux/files/usr/bin/sh
exec /data/data/com.termux/files/home/bin/litems serve
EOF

chmod +x $PREFIX/var/service/litems/run
ln -sf $PREFIX/share/termux-services/svlogger $PREFIX/var/service/litems/log/run

sv-enable litems
sv up litems
sv status litems
```

### 13.5 开机自启

```bash
# 安装 Termux:Boot（F-Droid，签名须与主程序一致）
mkdir -p $HOME/.termux/boot
cat > $HOME/.termux/boot/start-litems.sh <<'EOF'
#!/data/data/com.termux/files/usr/bin/sh
termux-wake-lock
source /data/data/com.termux/files/usr/etc/profile.d/start-services.sh
EOF
chmod +x $HOME/.termux/boot/start-litems.sh
```

**易踩的坑**：`source start-services.sh` 不能省。Termux:Boot 执行脚本时是非交互 shell，不会加载 profile，缺这一行 runit 不会启动，所有服务保持 down 状态直到手动打开 App。

### 13.6 系统侧设置（MIUI）

| 设置项  | 路径                      | 值                            |
| ---- | ----------------------- | ---------------------------- |
| 省电策略 | 设置 → 应用 → Termux → 省电策略 | 无限制                          |
| 自启动  | 设置 → 应用 → 自启动           | **Termux 和 Termux:Boot 都要开** |
| 电池优化 | 设置 → 电池 → 应用耗电管理        | 允许后台活动                       |
| 任务锁定 | 多任务界面下拉 Termux 卡片       | 加锁                           |

### 13.7 网络

- 在路由器上为手机 MAC 绑定固定 IP，避免 DHCP 变更导致所有客户端失效
- 建议关闭 Wi-Fi 省电模式（部分 ROM 在息屏后降低 Wi-Fi 功率，导致推流卡顿）
- 端口 7788 无需转发到公网；如需外网访问，另设隧道方案（本期不实现）

### 13.8 电源与散热

- 限制充电至 70%（`echo 70 > /sys/class/power_supply/battery/charge_control_end_threshold`，需内核支持，MIUI 官方内核通常不提供）
- 使用 5V/2A 慢充头，避免长期快充
- 保持机身通风，勿覆盖
- **注意**：`/sys/class/power_supply/battery/charge_full` 这类节点在不同 ROM 上权限不一，多数非 root 不可读。健康度检查放到 adb 或 root shell 里做，不要写成 Termux 定时任务

### 13.9 与其他服务共存（网盘 / 同步）

手机作为家庭服务器，除了媒体库还要兼做网盘。**这部分不自研**——Go 生态已经有成熟的单二进制方案，重新写一遍是浪费。LiteMS 的职责只有媒体刮削与流服务，文件管理交给第三方，两者共用一台手机、互不干扰。

#### 端口规划

**必须提前分配，不能都留在默认值上。** Filebrowser 的默认端口恰好也是 `7788`，与 LiteMS 完全相同，后启动的那个会静默失败。

| 服务              | 端口                | 用途                          |
| --------------- | ----------------- | --------------------------- |
| litems        | `7788`            | 媒体库 UI / API / 视频流         |
| Filebrowser     | `8081`            | 文件管理 Web UI（上传/下载/重命名/删除）  |
| rclone serve    | `8082`            | WebDAV（给 Infuse / nPlayer 挂载） |
| Syncthing       | `8384` / `22000`  | 同步管理界面 / 同步协议               |

```bash
# Filebrowser 必须显式改端口和数据库位置
filebrowser -a 0.0.0.0:8081 -p $HOME/.config/filebrowser/filebrowser.db -r /storage/emulated/0
```

#### 硬性约束

| 约束                                   | 原因                                                  |
| ------------------------------------ | --------------------------------------------------- |
| 所有服务的 `.db` / 配置目录必须在 `$HOME` 下 | `/sdcard` 是 FUSE，SQLite 会报 `database is locked`，与 LiteMS 同一条理由 |
| 媒体目录不做为任何服务的「配置/数据库」目录               | 索引类文件混进媒体库，迁移时全带过去，且会被扫描器读到                         |
| 各服务用独立 runit service，互不 exec 依赖      | 一个崩了不该拖垮另一个                                         |

#### 写冲突：第三方会改动媒体目录

Filebrowser 允许上传、重命名、删除文件，Syncthing 会同步进来新文件。这些改动 LiteMS 都不知道，但**不需要特殊处理**：

```
新增文件      → 下次扫描时被发现（size+mtime 为 0，视为新条目）
文件被替换    → size 或 mtime 变化 → 重刮
文件被删除    → 条目残留到下次扫描，随后清除
```

前提是**后台扫描不能关**：`[watch].enabled = true` 且 `interval_min` 不要设为 0。把扫描关掉再让 Filebrowser 写文件，索引就会永远落后于磁盘——这是最容易犯的配置错误。

#### 并发读同一目录

媒体库在 ext4 上，三个服务同时读（LiteMS 的 `WalkDir`、Syncthing 的 inotify、Filebrowser 的浏览请求）代价远小于 FUSE 时代——inotify 是内核通知，不占 IO；`WalkDir` 读的是 dentry 缓存。

仍建议 `[watch]` 触发的扫描把并发压到 1（见 8.5）：大量随机读会抢占磁盘队列，与正在进行的 4K 推流争带宽。

若网盘服务指向 `/storage/emulated/0`，它和媒体库分属不同分区，IO 互不影响——这是一个附带的好处。

#### Syncthing 与存储分区

媒体库在 `$HOME` 时，Syncthing 可以正常工作——它依赖的 inotify、`utime`、`chmod`、原子 rename 在 ext4 上全部可用。这也是 6.5 把媒体库放在 ext4 的又一个理由：**换成共享目录，Syncthing 直接不可用**。

`/storage/emulated/0` 的 FUSE 层缺这三样：

```
inotify 不可用        → 只能轮询，延迟大、耗电
chmod / utime 失败    → 权限与时间戳无法保持
原子 rename 受限      → 冲突处理可能产生临时文件残留
```

**所以不要把 Syncthing 的同步目录设成 `/storage/emulated/0` 下的任何路径**（包括相册目录）。要同步手机照片，先把 Filebrowser 或 Termux 脚本把它们移进 `$HOME`，再让 Syncthing 从那里往外同步。

#### 认证各自独立

四个服务四套账号，不做统一 SSO（收益不足以抵消复杂度）。原则：

- 全部只在局域网监听，不做公网暴露
- 至少给 Filebrowser 和 Syncthing 设强密码——它们有写权限，比只读的 LiteMS 危险得多
- LiteMS 的 Basic Auth 是否启用见 9.9，明文传输的局限要清楚

#### 内存与进程数

每个 Go 服务常驻 20–50MB，7.5GB 内存不是约束，**真正的约束是 MIUI 的后台清理策略**：进程越多、总占用越高，被整组杀掉的风险越大。建议只常驻真正在用的服务，别把四个都开着。

---

## 14. 跨平台构建

### 14.1 构建矩阵

```bash
#!/bin/sh
set -e
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS="-s -w -X main.version=$VERSION"

for t in \
  linux/amd64 linux/arm64 linux/arm linux/386 \
  darwin/amd64 darwin/arm64 \
  windows/amd64 windows/arm64 \
  freebsd/amd64
do
  GOOS=${t%/*}
  GOARCH=${t#*/}
  OUT=dist/litems-${GOOS}-${GOARCH}
  [ "$GOOS" = "windows" ] && OUT=${OUT}.exe
  echo "building $t"
  CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH \
    go build -trimpath -ldflags="$LDFLAGS" -o "$OUT" .
done
```

### 14.2 Termux 目标

**使用 `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`。**

实测依据见 [13.2.3](#1323-三个组合的行为差异实测)：

| | `GOOS=android`（Termux 默认） | `GOOS=linux`（选定） |
| --- | --- | --- |
| ELF 类型 | `DYN`(PIE) + INTERP → `/system/bin/linker64` | `EXEC`，无 INTERP，**真静态** |
| 体积 | 5.9M | 5.3M |
| 可移植 | 只能跑在 Android 上（需系统 linker） | 任意 arm64 Linux，含 Android |
| DNS 配置 | 依赖 Termux 打过补丁的 `resolv.conf` | 依赖 `[net].dns`（程序内） |
| 交叉编译 | 从 Windows/macOS 编 android 目标要配 NDK | 一条环境变量，零额外依赖 |

> v1.0 说「`GOOS=android` 会引入 NDK 链接要求」——在 `CGO_ENABLED=0` 下**这句话不准确**：实测 `GOOS=android CGO=0` 编译成功且能跑，不需要 NDK。选 linux 目标的真实理由是**真静态、可移植、DNS 可控**，不是「android 目标编不出来」。

**硬性要求**：linux 目标产物在 Termux 上**必须配 `[net].dns`（填 `auto` 即可）**。否则它读不到 `/etc/resolv.conf`（不存在），会回退到 `[::1]:53`，所有解析直接拒绝——表现为「编译成功、运行无网络」。

启动时若 `[net].dns` 为空，做一次 3 秒的解析探测：
- 探测成功 → 正常启动
- 探测失败 → 打印 [13.2.8](#1328-换网络后怎么自检) 的排查清单（不退出，索引与播放不受影响）

### 14.3 体积（实测）

| 组成 | 大小 | 来源 |
| --- | --- | --- |
| 纯 Go 程序（含 `net/http`） | 5.3 MB | 实测：`probe`，linux/arm64，`-s -w` |
| + `database/sql` + modernc sqlite | 9.5 MB | 实测：`bench` |
| **+ `html/template` + `embed` + 业务代码（完整程序）** | **13 MB** | 实测：`litems` linux/arm64 |
| 其他平台 | 13–14 MB | 实测九平台矩阵 |

> v1.0 估的是 22 MB，其中「modernc SQLite +15 MB」。**实测 SQLite 增量只有 4.2 MB**，但 `html/template` 这套模板引擎又带回约 3.5 MB——两端偏差方向相反，最终落在 13 MB，比原估仍低 40%。
>
> 体积曾影响「要不要改用 bbolt、要不要放弃模板引擎手拼 HTML」的决策。现在可以确认：都不值得，13 MB 在 129 G 可用的手机上不是问题。

九平台实测产物（`./build.sh`，2026-11-08）：

```
linux/amd64    14M    darwin/amd64   14M    windows/amd64   14M
linux/arm64    13M    darwin/arm64   13M    windows/arm64   13M
linux/arm      14M    freebsd/amd64  14M
linux/386      13M
```


### 14.4 构建守门

CI 中固定执行：

```bash
CGO_ENABLED=0 go build ./...      # 必须成功
go vet ./...
go test ./...
```

任何 PR 引入 CGO 依赖即失败。

### 14.5 发布前冒烟清单

每次打 tag 前手工跑一遍（自动化价值不高，但漏掉代价很大）：

1. `litems scan --dry-run` 在真实媒体根上条目数与文件数一致
2. `litems serve` 后浏览器能出海报墙，`/stream` 能拖动
3. Windows/macOS 二进制能启动 `launch` 并拉起播放器
4. Termux 上 DNS 解析正常（见 [13.2](#132-网络可达性已实测)）

---

## 15. 已知限制

只列**已接受、不打算解决**的限制。待验证的不确定性统一放在 [17-风险登记](#17-风险登记)，两章不重复。

| 限制                | 原因          | 影响                    | 缓解                              |
| ----------------- | ----------- | --------------------- | ------------------------------- |
| 浏览器不能启动本地程序       | 沙箱安全模型      | 必须有 helper 或复制命令      | `launch` 子命令 + `/api/cmd` 降级通道  |
| 无转码               | CPU 能力      | 客户端必须能解片源             | 用 mpv/VLC，解码能力远超浏览器              |
| 内嵌字幕无法单独导出        | 需 demux      | 播放器自己处理内嵌字幕，外挂字幕单独提供  | 接受                              |
| 单用户               | 简化设计        | 家人共享同一账号              | 后续可扩展                           |
| 无外网访问             | 未实现隧道       | 只能局域网                 | 后续接 Cloudflare Tunnel           |
| 文件名解析非 100%       | 命名千奇百怪      | 部分需手工 override        | `override.json` + `unmatched.txt` |
| 根路径变更后 ID 失效      | ID 含根路径     | 进度丢失                  | `scan --reindex`，属可接受代价          |
| 搜索走全表 LIKE         | 未上 FTS5     | 5 万条以上变慢             | 当前量级无感，超阈值再引入                   |

---

## 16. 里程碑

**v1.0 把剧集推到 M5 是错的**：剧集层级影响建表（`season`/`episode` 非空语义）、解析（季目录回溯）、NFO 落位、接口语义。M1 定表时不考虑，M5 就是重构。

调整后：

| 阶段              | 交付物                                             | 验收标准                                          | 预估  | 状态（2026-11-09） |
| --------------- | ----------------------------------------------- | --------------------------------------------- | --- | --- |
| **M0 验证**（新增）  | 一个 60 行的 helper demo + 一个静态页                   | **真浏览器点按钮能拉起 mpv**；PNA 预检通过，或确认不可用并启用降级通道 | 1 天 | ⚠️ 网络部分已验证（13.2），浏览器 PNA 未测 |
| **M1 索引**       | `scan` 可用，SQLite 建库，文件名解析，**电影/剧集两套目录树都跑通**     | 真实库条目数 = 文件数；`--dry-run` 输出正确；剧集层级与 `tvshow.nfo` 落位正确 | 4 天 | ✅ 完成 |
| **M2 刮削**       | `Scraper` 接口 + TMDB 后端（OMDb 兜底）、匹配门槛、NFO 写入、图片下载、DNS 自检 | 假数据源端到端通过；真实样本匹配率 > 85%；NFO 可被 Kodi 正确识别（含剧集与单集） | 5 天 | 🟡 代码完成，待真实 key 验证匹配率 |
| **M3 浏览**       | `serve` + Web UI（海报墙/详情/搜索）+ `/poster` 缓存头       | 浏览器可浏览、搜索、详情页正确；翻页不重复下载海报                     | 3 天 | ✅ 完成 |
| **M4 播放**       | `/stream` + `/sub` + helper 完整校验 + `/api/cmd`   | mpv 用 URL 可播放且能拖动；点按钮 1 秒内拉起；复制命令可用            | 3 天 | ✅ 代码完成，helper 未与真实 mpv 联调 |
| **M5 打磨**       | 进度记录（含 `finished`）、字幕多语言、日志、构建脚本                | 续播正确、已看完不混入继续观看；九个平台构建通过                     | 3 天 | ✅ 除日志分级外基本完成 |

**实际推进与计划的偏差**：M3/M4/M5 的代码在 M1 阶段一并写完了。原因是这些模块共用同一套 DB 与路由，分两次写反而要重新建立上下文。代价是 M1 的验收只覆盖了扫描，HTTP 层虽然通过接口测试，但**还没有在真实浏览器里点过**。

下一步是 M2（刮削），前置条件是 OMDb 的 key。在此之前建议补两件事：浏览器里实际打开一次界面、在 PC 上跑一次 `launch` 联调 mpv。

总计约 19 个工作日。相比 v1.0 的 16 天，多出的 3 天分别给了 M0 的浏览器验证、M1 的剧集支持、M2 的多源抽象——这三处省下来，后面要花更多时间返工。

---

## 17. 风险登记

只列**尚未验证、可能推翻设计**的不确定性。已实测并已定论的（DNS、TMDB 污染、CGO 行为）见 [13.2](#132-网络可达性已实测)，不再列为风险。已接受的永久限制见 [15](#15-已知限制)。

| 风险                          | 概率    | 影响           | 应对                                                       |
| --------------------------- | ----- | ------------ | -------------------------------------------------------- |
| **手机经 PC 热点上网，PC 关机即断网**     | 高     | 整个服务器不可用     | 当前网关是 `192.168.137.1`（Windows ICS 网段）。要 7×24 必须让手机直连路由器，见下条 |
| **DNS 配置随网络环境漂移**           | 低（有 `auto`） | 全部外网请求失败     | `[net].dns = "auto"` 自动探测网关；仅当网关不是 `.1`/`.254` 时需手填；换网络后跑 13.2.8 自检 |
| **浏览器 PNA 拦截 helper**       | 中高    | 播放按钮失效       | M0 前置验证；回 `Access-Control-Allow-Private-Network`；备用 `/api/cmd` |
| Termux 后台被 MIUI 杀            | 高     | 服务不可用        | 三重保活 + 系统白名单；run 脚本用绝对路径                                  |
| OMDb 免费额度（1000/天）不够         | 中     | 大批量首次刮削中断    | 分批 `scan --only-missing`；`daily_limit` 达上限即停并提示；付费档 $1/月    |
| **代理链路不稳定**                 | 中     | TMDB 不可用     | 默认 OMDb，不依赖代理；TMDB 仅作可选后端                                 |
| 豆瓣非官方接口失效                   | 中     | 中文标题/简介缺失    | 装饰器模式，失败静默跳过；NFO 仍可用（英文）                                 |
| 文件名解析覆盖不足                   | 高     | 漏刮/错刮        | 先抽年份后清洗；分阶段扩充规则；`unmatched.txt` 清单                       |
| 存储空间不足                      | 中     | 图片写入失败       | 图片总量：1000 部约 0.6–1.5 GB（视尺寸参数）                           |
| 数据库损坏                       | 低     | 需重建          | 索引可从文件重建，NFO 是真相；启动时校验 schema 版本                         |
| 手机电池老化                      | 高（长期） | 停机           | 限充 70%（需内核支持）；定期 adb 检查容量                                |

**关于第一条**：`192.168.137.x` 是 Windows「移动热点 / Internet 连接共享」的默认网段。也就是说手机现在是借 PC 上网的——PC 睡眠或关机，媒体服务器连带断网。这不影响开发验证，但**不能作为最终形态**。正式部署时让手机连家里的路由器（同一个 SSID），那时网关会变成路由器地址，`auto` 会自动跟上，不需要改配置。

---

## 18. 决策记录

### ADR-001 不做浏览器内播放

**背景**：浏览器 `<video>` 不支持 MKV 容器，无法切换音轨，ASS 字幕需 WASM 渲染，HDR 会掉色。

**决策**：Web UI 只负责浏览与选择，播放交给本地播放器。

**后果**：删除 remux 管道、ffmpeg 依赖、约 300 行播放器代码。代价是用户需要安装 mpv/VLC。

### ADR-002 不做 Jellyfin API 兼容

**背景**：Jellyfin API 可带来现成客户端生态与跨端续播。

**决策**：本期不做。单人 + PC 场景下，收益不足以抵消 1500 行实现与字段兼容风险。

**后果**：手机端需用播放器挂 WebDAV；续播仅在 helper 支持的播放器内生效。数据层预留 `tmdb_id`，未来可扩展。

### ADR-003 NFO 作为唯一真相

**背景**：元数据可存数据库，也可存文件。

**决策**：NFO + 图片落盘，数据库仅作缓存。

**后果**：删库可重建；数据可迁移、可版本控制；代价是写文件比写库慢（可忽略）。

### ADR-004 禁用 CGO

**背景**：Termux 使用 bionic libc，CGO 产物无法运行。

**决策**：全项目 `CGO_ENABLED=0`，SQLite 使用纯 Go 实现。

**后果**：体积 +15MB、数据库性能约为 C 版 1/3；换取九个平台一条命令构建。副作用是必须自己处理 DNS 解析差异（见 [13.2](#132-网络可达性已实测)），因为失去了 `netdns=cgo` 这个逃生门。

### ADR-005 helper 与 service 同二进制

**背景**：PC 需要本地端点来启动播放器。

**决策**：`launch` 作为子命令，与 `serve` 共用二进制。

**后果**：分发简单，用户只下载一个文件；代价是 PC 侧也要有完整二进制（22MB）。

### ADR-006 路径即 ID

**背景**：需要稳定的条目标识。

**决策**：`id = sha1(root_path + "/" + rel_path)[:16]`。

**后果**：实现简单、可预测、多根目录不冲突；根路径变更或文件重命名导致 ID 变化、进度丢失（提供 `scan --reindex`）。

> v1.0 是 `sha1(rel_path)`，在 `extra_roots` 场景下两个根的同名文件会撞主键。已修正。

### ADR-007 不做 DLNA

**背景**：DLNA 可让电视直接发现服务器。

**决策**：不做。

**后果**：Android 非 root 无法持有 `MulticastLock`，SSDP 多播包被系统过滤，发现机制不可靠，投入产出比过低。

### ADR-008 为什么是 helper + 本地 HTTP，而不是别的

**背景**（v1.0 缺这一条）：浏览器不能启动本地程序，有至少三种绕法——

| 方案                     | 成本          | 问题                                     |
| ---------------------- | ----------- | -------------------------------------- |
| 复制命令粘贴到终端              | ~20 行       | 每次播放都要切窗口粘贴，体验差但零维护                    |
| 自定义协议 `litems://play?id=` | ~150 行     | 需在 PC 注册协议处理器；每次弹「是否允许打开」；跨平台注册方式各异    |
| 本地 HTTP helper（选定）      | ~200 行 + 配置 | 需处理 Origin/token/PNA；受浏览器策略变动影响（见 3.3） |

**决策**：选 helper，但**同时保留「复制命令」作为降级通道**——两套加起来 220 行，比只做 helper 多 20 行，换来的是浏览器策略变动时不会整个播放链路瘫痪。

**后果**：用户需要多启一个进程；换来一键播放。

### ADR-009 schema 迁移不做增量

**背景**：索引结构会随版本演进。

**决策**：版本不匹配直接删库重建，不写迁移脚本。

**后果**：升级后需重扫一次（几分钟）；省下的是每个版本的迁移代码与测试成本。前提是「NFO 是真相」这条 ADR 真的被遵守——所以这条决策依赖 ADR-003。

### ADR-010 刮削后端可插拔（默认源由 OMDb 改为 TMDB）

**背景**：原设计只接 TMDB。v1.5 时实测发现 `api.themoviedb.org` 被污染为 `0.0.0.0`，而 OMDb 直连可达，于是把 OMDb 设为首选。v1.8 补测推翻了那个前提：同一后端的 `api.tmdb.org` 未被污染，图片 CDN 也可用（[13.2.5](#1325-换一个域名就绕开了污染)）。

**决策**：保留 `Scraper` 抽象，但**默认改为 `provider = "tmdb"`**，OMDb 降为兜底。理由不是「哪个能连上」，而是能力差距——OMDb 没有 `language` 参数、没有 fanart、分集只有标题（[8.2](#82-刮削后端可插拔)）。豆瓣本期不接入，只预留位置。

**后果**：

- 元数据质量回到 TMDB 水平：中文标题与简介、背景图、分集剧照
- 接口的 `Key` 从 `int` 改成 `string`——OMDb 只有 `tt` 号，用数字装不下；这个改动让两个源共用一套流程
- 多约 60 行接口代码，换取以后换源不动业务逻辑
- 仍然保留「配置一行切到 OMDb」的能力，应对 `api.tmdb.org` 哪天也被污染

### ADR-011 禁 CGO 的真实理由（v1.4 修正）

**背景**：ADR-004 写的理由是「Termux 的 libc 是 bionic，CGO 产物无法运行」。**这句话不准确**——Termux 官方 Go 包默认 `CGO_ENABLED=1`，用 clang + bionic 编出来的东西在 Termux 上跑得很正常。真正的问题是：从 Windows 交叉编译带 CGO 到 android 目标极其麻烦。

**决策**：仍然禁 CGO，但理由改为「九个平台一条命令构建」，而不是「CGO 产物跑不了」。

**实测代价**（[13.2.3](#1323-cgo-开与关的行为完全不同)）：

| | CGO=1 | CGO=0 |
| --- | --- | --- |
| DNS 来源 | bionic → netd → 系统下发 | 纯 Go → `$PREFIX/etc/resolv.conf` |
| 默认配置下 | 开箱可用 | **连百度都解析不了** |
| 改对 resolv.conf 后 | 可用 | 可用 |

**后果**：`CGO_ENABLED=0` 的二进制「编译成功、运行无网络」是这张卡上最容易踩的坑。部署步骤（13.2.5）与启动自检必须检查这一项，不能只写在 ADR 里让人自己发现。

---

## 19. 变更记录

### v2.6（2026-11-09）—— 内嵌网页播放器（ArtPlayer）

详情页新增「网页播放」：ArtPlayer v5.4（单文件 134KB，MIT）打进二进制的 /vendor/ 路由，
离线可用。进度复用 /api/progress（>95% 自动标已看完），播放中节流 10s 上报、暂停/播完
立即存、离页 sendBeacon。续播 >30s 自动跳转。

字幕适配（fmt=vtt）是本次的核心工程量：

- **格式**：srt→VTT（时间戳逗号换点）；ass/ssa→VTT（抽对白，样式丢弃；libass wasm 插件留作二期）
- **编码**：BOM → UTF-8 校验 → 按语言提示解 GB18030/Big5（golang.org/x/text）
- **多语言**：播放器设置面板列出 SubPaths 各语言，切换即换 URL
- mpv 路径（不带 fmt）保持原始文件——它自己会猜编码，不受影响
- 仅外挂字幕文件；mkv 内嵌轨（PGS/内嵌 ASS）浏览器拿不到，仍走 mpv

实装中抓到的两个 bug（都由单测钉死）：

1. ASS 正则分组错位：`Dialogue:(层)(起始)(结束)` 的起止时间是 m[2]/m[3]，
   误用 m[1] 拿到层号 "0"，时间解析失败导致全部行被跳过、输出为空
2. WEBVTT 头是死代码：`out` 构建器写了头但 srt/ass 分支都没返回它

### v2.5（2026-11-09）—— 详情页展示完整 NFO 元数据

详情页此前只有标题/评分/简介，NFO 里的类型、导演、编剧、演员、国家、播出日期、
外部 ID 全部闲置。新规则延续「NFO 才是真相」：详情页直接读条目旁的 NFO（movie.nfo /
<stem>.nfo，单集再向上合并 tvshow.nfo），不做入库——用户手工改 NFO，页面立即跟着变。

- hero：tagline、类型标签、评分人数
- 详细信息：导演 / 编剧 / 国家地区 / 播出日期 / TMDB+IMDb 外链
- 主演：胶囊列出姓名与角色（前 12 位）
- 剧照：横屏 fanart 以 16:9 清晰展示（hero 背景那层被遮罩压暗，这里放亮的）
- 类型/国家保留 TMDB 的语义顺序不排序；空 NFO 不渲染区块
- 国家/地区映射为中文：TMDB 不做国家本地化（ISO 码 + 英文名），刮削时经
  countries.go 的映射表写成中文进 NFO（HK→中国香港、TW→中国台湾、MO→中国澳门等，
  覆盖 70+ 常用国家；查不到的原样保留）。Kodi/Jellyfin 读同一份 NFO 同样受益。
  考虑过用豆瓣兜底中文数据：实测其官方 API 已关闭、条目页对非浏览器请求跳
  sec.douban.com 反爬验证，每下载必刮的频率特征极易触发封禁，放弃；角色名的
  罗马拼音是 TMDB 无中文翻译，保留原样（用户确认：隐藏不如保留）

### v2.4（2026-11-09）—— 下载目录成为一等设置项

取代外部钩子脚本（写死路径、要在 qbt 里手工配 AutoRun、换设备/换目录都要手改）：

- `[library].download_dir` + `download_mode`（symlink / copy），设置页可改
- serve 每分钟镜像一次下载目录：新内容 → 媒体根 `<下载目录名>/` 下建链接 → 增量扫描 → 刮削（limit 20）
- 未完成文件（`.!qB` / `.part` / 隐藏目录）跳过；镜像幂等，重复检查零开销
- 删除清理（symlink 模式）：悬空链接递归移除；只剩 nfo/海报的孤儿壳目录一并移除；
  copy 模式的副本无法与用户文件区分，不自动清理
- 镜像、watch 扫描、启动扫描共用一把互斥锁，避免同时刻两轮全量遍历抢 SQLite 写锁
- 设置页校验下载目录不能与媒体根互相包含

### v2.3（2026-11-09）—— 设置页支持媒体库路径

「媒体库」卡片新增三个字段：媒体根目录、额外目录（一行一个）、跟随文件符号链接。
写回机制从 `writeScrapeSettings` 泛化为 `writeSectionKeys(path, section, keys, kv)`，
值改为原始 TOML 右值（字符串由 `tomlString` 加引号、数组由 `tomlArray` 序列化），
任意段都能用同一套行编辑逻辑，注释与其它段照旧不动。

配套两件事：

- **serve 启动即做一次增量扫描**，然后才补刮——改了媒体根、手动加了文件，都不用等
  watch 循环的第一个 30 分钟周期。刮削排在扫描之后，因为它消费扫描写入的条目。
- 媒体根的修改**需要重启服务**：进程内的 roots 是启动时快照，流媒体与图片路径都取自它；
  换根会让所有条目 ID 变化（ID = sha1(根+相对路径)），进度记录无法迁移，这是换根的固有代价。

### v2.2（2026-11-09）—— DNS 修复、图片缓存失效、启动补刮

三个真实使用中暴露的问题：

**serve 没接管 DNS**：`SetupNet` 只挂在 `scrape` 命令上，设置页的「测试连接」在 serve 进程里发请求，
Go 静态解析器在 Termux 上读不到 `/etc/resolv.conf`，回退 `[::1]:53` 直接拒绝连接。现在 serve 启动时也接管。

**重刮后浏览器仍显示旧图**：`/poster` 缓存 7 天且 URL 不随文件变化。图片 URL 现在带
`?v=<ScrapedAt>`，重刮一次就失效。

**启动补刮**：serve 启动 5 秒后自动补刮 `scraped_at=0` / `error` / 「成功但海报文件不在」的条目；
`miss` 与 `manual` 依旧不重试（避免烧配额）。渲染侧校验图片文件存在，占位符只在「没有真海报」时出现。

### v2.1（2026-11-09）—— 默认端口 8080 → 7788

8080 是最容易被占用的端口：开发服务器、路由器管理页、各种网页面板都默认抢它。
这个项目跑在手机上，端口撞车时排查成本远高于改个数。

- 默认监听改为 `0.0.0.0:7788`，示例配置、文档、bench 工具同步
- `serve` 启动失败且原因为端口占用时，单独提示「换一个端口」而不是只吐底层错误
  （`isAddrInUse` 同时匹配 `syscall.EADDRINUSE` 与 Windows 的 `WSAEADDRINUSE` 文案）
- **已有配置文件里写死的 `listen` 不受影响**：默认值只在字段为空时生效

### v2.0（2026-11-09）—— 项目更名 LiteMS，新增设置页

**更名**：`mediasrv` → **LiteMS**，命令 `litems`，配置目录 `~/.config/litems`，状态目录 `.litems/`，
module `github.com/Schallophy/LiteMS`。旧环境变量 `MEDIASRV_CONFIG` 与旧状态目录名仍被识别，升级不断档。

**新增 `GET/POST /settings`**：在浏览器里填 TMDB/OMDb 凭据，不必 SSH 上去改文件。

- 只改 `[scrape]` 段的白名单键，其余内容与注释原样保留；文件以 `0600` 写入、临时文件 + rename 落盘
- 「测试连接」用提交中的值跑一次自检，不落盘
- CSRF 防护用进程级随机令牌；只靠 Basic Auth 挡不住跨站表单提交
- 未启用认证时页面显式警告

**为什么不用「整份配置重写」**：配置文件是这个项目唯一可手工编辑的配置源，`toml.Encode` 会把注释、
注释掉的备选项、键顺序全部冲掉。宁可手写一段按行编辑的代码，也不破坏用户已有的文件。

### v1.9（2026-11-09）—— 前端重做，补上「图片引用可重建」

**背景**：功能已经跑通，但界面还是原型水准——灰底占位、无层级、缺图时整块空白。同时发现一个与「删库可重建」约定冲突的实现缺口。

**界面**

- 列表页：暖金强调色的深色主题、2:3 海报卡、评分角标、观看进度条、悬停播放按钮与上浮阴影；栅格宽度用 `clamp(150px,14vw,200px)`，390 / 834 / 1024 / 1440 四档实测无横向溢出
- 详情页：多层渐变头图、胶片颗粒（下缘渐隐避免硬边）、海报投影、`已观看 x / y` 进度条、主按钮金色渐变
- 缺图占位：细网格 + 标题首字（新增模板函数 `initial`，按 rune 取字）
- 无 `fanart.jpg` 时用海报做虚化背景（`.bg.blurred`）
- 单页时不再渲染分页控件

**修正**

| 问题 | 修法 |
| --- | --- |
| `scan` 不识别磁盘上已有的 `poster.jpg` / `fanart.jpg`，删库重建后海报消失 | 新增 `findImages`（剧集回上一层目录找），写入索引 |
| `UpsertItem` 的 `ON CONFLICT` 分支不更新图片列，重扫识别到的图写不进库 | 用 `COALESCE(NULLIF(...))` 更新，已刮削条目不被空值覆盖 |
| 模板函数在 `ParseFS` 之后才 `Funcs`，启动即报 `function "initial" not defined` | 改为 `template.New("").Funcs(tplFuncs).ParseFS(...)` |

### v1.8（2026-11-09）—— M2 刮削实现，默认源改回 TMDB

**触发**：v1.7 之后补测发现 `api.tmdb.org` 未被污染（13.2.5），推翻了「TMDB 必须走代理」的前提，进而推翻了「OMDb 当主源」的决策。

**新增代码**

- `nfo.go`：NFO 读写，`<art>` / `<ratings>` / `uniqueid` 按 Kodi schema 生成；`readNFO` 认 `lockdata` 与已有 ID
- `tmdb.go`：TMDB 客户端，限速 + 429 退避 + 字段级语言回落 + 错误信息抹掉 API Key
- `omdb.go`：兜底数据源，`Response=False` 当 miss 处理（HTTP 200 不是 404）
- `scrape.go`：`Scraper` 接口、硬门槛匹配、剧集分组缓存、图片与 NFO 落盘、`unmatched.txt`
- `net.go`：DNS 自检与三级回退（配置指定 → 系统 → 网关 → 公共 DNS）
- `scrape` 子命令：`--probe` / `--dry-run` / `--force` / `--limit` / `--provider`

**推翻或修正的旧结论**

| 旧说法 | 实测结果 |
| --- | --- |
| TMDB 全部域名被污染，必须走代理 | `api.tmdb.org` 可直连，401 + 标准 JSON；`image.tmdb.org` 200 |
| 默认源用 OMDb | 改为 TMDB。OMDb 无 language、无 fanart、分集只有标题 |
| `[net].dns = "auto"` 已在文档里描述 | 代码此前从未实现，首次 `scrape --probe` 就因 `[::1]:53` 失败。本版补上 `net.go` |
| `scan` 一并刮削 | 拆成 `scan`（不联网）+ `scrape`（联网），断网时索引仍可用 |

**测试**

- `httptest` 假 TMDB 跑通端到端：搜索 → 匹配 → 详情 → 写 NFO → 下载图片 → 回填索引 → 二次运行零变更
- 覆盖 `lockdata` 不覆盖、别名救回、`uniqueid` 优先级、文件名内嵌 ID

### v1.7（2026-11-09）—— M1 实现完成，附带 M3/M4/M5

**已完成**：`config.go` / `db.go` / `parse.go` / `scan.go` / `server.go` / `launch.go` + 两个页面模板，九平台构建通过（4m17s，13–14MB）。

**实测验证**

| 项 | 结果 |
| --- | --- |
| 文件名解析 | 3 个样本全对：电影（带 chs/cht 双字幕）、剧集（从 `Season 01` 回溯季号与剧名）、番剧（`- 01` 识别为 S1E1） |
| 增量扫描 | 第二次扫描 3 条全部跳过，另计「待刮削 3」 |
| `/healthz` | `{"db":"ok","items":3,...}` |
| `/api/items` | 分页、`q` 搜索、`sort`、`kind` 过滤正常 |
| `/stream/{id}` | Range 请求返回 `206`，1024 字节 |
| `/sub/{id}?lang=chs` | `application/x-subrip; charset=utf-8`，2048 字节 |
| `/api/progress` | 96% 判为完成：`finished=true` 且 `position_ms` 归零；`/api/continue` 随即清空；重开播自动取消完成态 |
| `/api/cmd/{id}` | 返回可直接粘贴的 mpv 命令（含 `--sub-file`） |
| 错误处理 | 非法 ID → 400，不存在的 ID → 404 |
| 九平台构建 | 全部成功，见 [14.3](#143-体积实测) |

**实现过程中修掉的四个问题**

1. **Go RE2 不接受 `\（`** —— 全角括号在字符类里不能转义，`regexp.MustCompile` 直接 panic。正则里凡涉及 `（）【】` 一律不加反斜杠
2. **`normalizeTitle` 的 Trim 吃掉了右括号** —— `strings.Trim(s, " -–—_[]()（）【】")` 会把「琅琊榜（TV版）」变成「琅琊榜（TV版」。括号的取舍已经由 `stripJunkBrackets` 决定过，这里不该再 Trim
3. **把 `scraped_at=0` 混进「是否重写索引」** —— 导致每次扫描都报「更新 3」，增量形同失效。拆成 `NeedsReindex`（只看文件属性）与 `NeedsScrape`（含元数据缺失），未刮削的单独计入 `PendingScrape`
4. **番剧的 `- NN` 被分隔符清洗吃掉** —— 原来按 `[\s._-]+` 切词再拼回，连字符一起没了，`某番名 - 01` 认不出来。改为「删标记词但保留两侧分隔符」，并把番剧判断提到折叠空白之前

**体积修正**：v1.6 估 11 MB，实测 13 MB。差值来自 `html/template`（约 3.5 MB）——它和 `embed` 是 v1.6 那次基准里没测的。

**尚未验证**：真实影片的刮削（M2）、浏览器里实际打开界面、helper 与真实 mpv 的联调、MIUI 后台存活、runit 开机自启。

### v1.6（2026-11-08）—— 四项基准回填，抓到一个真实性能缺陷

**完成的基准**：磁盘 IO、SQLite（modernc）、大文件推流、HTTP 长连接。全部数据见 [附录 C](#附录-c基准数据)。

**关键发现**

| # | 发现 | 影响 |
| --- | --- | --- |
| 1 | **列表分页 33.6 ms**（1.1 万条）——`ORDER BY mtime_unix DESC` 无索引，全表扫 + 排序 | 真实缺陷，已补 `idx_items_mtime` |
| 2 | modernc SQLite 只增加 **4.2 MB**，不是估的 15 MB | 体积从 22 MB 修正为 ~11 MB |
| 3 | 逐条提交 vs 批量事务差 **3.6 倍**（1964 vs 7143 条/秒） | 验证 4.2 的事务要求 |
| 4 | 推流 **31.7 MB/s**，双流 34（饱和）——瓶颈确在 Wi-Fi，存储在 205 MB/s | 9.5 换成实测表 |
| 5 | 单连接 WAL 无锁冲突，写入 1 ms/次 | 单连接串行方案成立 |

**改动**

- **5.1** 新增 `idx_items_mtime`，附注释说明列表默认排序依赖它
- **9.5** 性能段改为实测表格（含 Range 206 / 慢响应 / 双流饱和）
- **14.3** 重写为实测体积，说明「modernc +15MB」高估 3.5 倍
- **4.2** 修正「慢一个数量级」→「慢 3.6 倍（实测）」
- **新增附录 C**：完整基准数据（磁盘 / SQLite / 体积 / 推流）
- **附录 A** 目录结构补 `probe/` 与 `bench/`

**仍未验证**：真实影片的解析与刮削（Media 目录为空）、OMDb 实际返回、PWA/helper 浏览器链路、MIUI 后台存活、开机自启。

### v1.5（2026-11-08）—— CGO 与 GOOS 的对照实测

**背景**：v1.4 关于 CGO 的结论只测了 `go run`（android 目标 + CGO=1）。补做完整对照后发现，`GOOS` 与 `CGO_ENABLED` 是**两个独立维度**，组合出三种形态，DNS 行为各不相同。

**新事实**（详见 [13.2.3](#1323-三个组合的行为差异实测)）

| 组合 | ELF | 体积 | DNS 来源 |
| --- | --- | --- | --- |
| android + CGO=0 | `DYN` + INTERP（**不是真静态**） | 5.9M | `$PREFIX/etc/resolv.conf`（有补丁） |
| android + CGO=1 | `DYN` + INTERP + libc | 8.3M | netd（系统下发） |
| linux + CGO=0 | `EXEC`，无 INTERP（**真静态**） | 5.3M | `/etc/resolv.conf`（不存在，无补丁） |

**两组对照实验**

- 同一份坏 `resolv.conf`（指向 8.8.8.8）：CGO=1 无视它（DNS 10ms / HTTPS 501ms），CGO=0 严格读它（两项全 FAIL）
- `GOOS=linux` + `[net].dns`：DNS 5ms / HTTPS 200 442ms，完全可用

**另一项实用发现**：`/proc/net/route` 因 Android 10+ 侧信道防护而 `Permission denied`，`ip route` 空输出；但 UDP `connect` 无需权限即可取出口地址（实测 `192.168.137.193`），据此推断网关可行 → 于是有了 `dns = "auto"`。

**改动**

- **13.2.3 重写**：三种组合对照表 + 两组对照实验，明确「Termux 里 `CGO_ENABLED=0` 并非真静态」
- **13.2.5 重写**：改为 `[net].dns = "auto"` 方案，含 UDP connect 探测原理与源码片段
- **14.2 重写**：修正「`GOOS=android` 会引入 NDK 要求」这一不准确表述（实测 `CGO=0` 下不需要 NDK）；理由改为**真静态、可移植、DNS 可控**；补硬性要求
- **11 章**：`[net].dns` 默认值改 `"auto"`，注释说明三种取值
- **配置校验**：`auto` 探测失败即拒绝启动
- **17 章**：「DNS 配置漂移」从「高」降为「低」（有 auto 兜底）

结论未变：**仍用 `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`**，但理由和配套要求都换了。

### v1.4（2026-11-08）—— 依据真机实测重写网络与数据源

**背景**：在 K20 Pro 上实测了 Go 编译（Termux 自带 1.27.3）、DNS、代理与各数据源可达性。多项早期推断被推翻。

**被推翻的三条**

| 原结论 | 实测结果 |
| --- | --- |
| Go 纯解析器读不到 `/etc/resolv.conf`，必然失败 | Termux 的 golang 包**已 patch 成读 `$PREFIX/etc/resolv.conf`**，能读到，只是内容指向不可达的 8.8.8.8 |
| 写 `$PREFIX/etc/resolv.conf` 无效 | **错**，对这个包有效，且是唯一解 |
| ADR-004：CGO 产物在 bionic 上无法运行 | **不准确**，Termux 官方 Go 默认 CGO=1 且工作正常；真正理由是交叉编译成本（见 ADR-011） |

**实测结论**（详见 [13.2](#132-网络可达性已实测)）

- 外网 UDP 53 全部阻断（8.8.8.8 / 223.5.5.5 超时），仅本地网关 DNS 可用（8ms）
- `CGO_ENABLED=0` 默认配置下**无任何网络能力**，改对 `resolv.conf` 后完全正常（百度 HTTPS 200 / 662ms）
- TMDB 两个域名污染为 `0.0.0.0`；OMDb `401`、豆瓣 `302`、IMDb 图片 `200`、`datasets.imdbws.com` `200`
- SOCKS5 代理可救 TMDB，且**经代理时本地 DNS 完全不参与**

**结构调整**

- **13.2 整节重写**：从「DNS 原理推演」改为七小节实测记录（UDP 53 阻断 / resolv.conf 路径 / CGO 差异 / 域名可达性 / 最小配置 / 代理 / 自检命令）
- **8.2 重写**：「TMDB 调用」→「刮削后端（可插拔）」，含 `Scraper` 接口、三源对比表、跨源回查流程
- **8.3 调整**：「精确查 → 搜索打分」两步流程；补 OMDb 的破同分策略（无 popularity，改用票数）
- **新增 7.7**：`uniqueid` 优先级，明确豆瓣 ID 永不写入，以及无标准 ID 时的降级处理
- **11 章**：默认 `provider = "omdb"`；新增 `omdb_key` / `douban_enrich` / `daily_limit`；`[net]` 新增 `proxy`；配置校验新增三条
- **4.5 外部依赖表**重写，明确「OMDb 或 TMDB 二选一」
- **17 章重写**：新增「手机经 PC 热点上网」「DNS 随网络漂移」「OMDb 额度」「代理不稳定」「豆瓣接口失效」；删除已定论的 DNS 与 TMDB 条目
- **新增 ADR-010**（多源可插拔，默认 OMDb）、**ADR-011**（禁 CGO 的真实理由与实测代价）
- 新增 `probe/main.go`：网络自检工具，换网络环境后跑一次即可

### v1.3（2026-11-08）—— 媒体库改放 ext4

**背景**：这台手机只作服务器，**没有人亮屏使用**。原设计把媒体库放 `/storage/emulated/0`（共享目录），理由是「要能被手机上的 App 访问」——这个前提不成立。

**决策**：媒体库默认放 `$HOME/Media`（`/data/data/com.termux/files/home/Media`，ext4）。

**收益**：一次绕开五个 FUSE 限制。

| 问题              | 改前（`/storage/emulated/0`） | 改后（`$HOME`）  |
| --------------- | ------------------------ | ------------- |
| `sendfile`      | 退化为用户态 copy              | 内核零拷贝         |
| `fcntl` 文件锁     | 语义不完整，SQLite 可能损坏       | 完整            |
| inotify         | 不可用                      | 可用（Syncthing 依赖） |
| 符号链接 / 硬链接      | 不支持                      | 可用            |
| 存储权限            | 需 `MANAGE_EXTERNAL_STORAGE` | 不需要           |

**改动**：

- 5.4 标题从「数据库位置硬约束」改为「存储位置：全部落在 ext4」，数据库与媒体库一并论述
- 6.5 整节重写：从「分区存储注意事项」改为「为什么媒体库放 `$HOME` 而不是共享目录」，含五项对照表
- 9.5 删除「`sendfile` 可能不可用、吞吐靠实测」的论述，改为 ext4 零拷贝 + 瓶颈在 Wi-Fi
- 11 章 `[library].root` 默认值改为 `$HOME/Media`；配置校验新增「媒体根落在共享目录 → 警告但不阻止」
- 13.1 前置条件：`MANAGE_EXTERNAL_STORAGE` 从必需降为「仅第三方网盘服务需要」
- 13.9：Syncthing 一节从「FUSE 上的限制」改为「与存储分区的关系」——媒体库在 ext4 时它天然可用，反过来说明 6.5 的选择是对的
- 15 章删除两条已不成立的限制（SQLite 不能放 `/sdcard`、无符号链接/硬链接）
- 17 章删除「FUSE 性能低于预期」风险（已排除）
- 12.1 输出示例、13.2 冒烟命令里的路径同步更新

### v1.2（2026-11-08）—— 明确网盘边界

**背景**：手机同时要兼做网盘（OneDrive 类用途）。早期表述含糊，容易被理解为「LiteMS 要实现文件同步」。

**决策**：网盘/同步功能**不自研**，由同机的第三方单二进制服务提供，LiteMS 只负责媒体刮削与流服务：

| 需求        | 用什么                              |
| --------- | -------------------------------- |
| 浏览器文件管理   | Filebrowser（Go，单二进制）             |
| 真正的双向同步   | Syncthing                        |
| WebDAV 挂载 | `rclone serve webdav`            |

**改动**：

- 2.2「不做」表新增一条，指向 13.9
- 新增 13.9：端口规划（Filebrowser 默认 7788 与 LiteMS 冲突，必须显式改）、数据库禁止落 `/sdcard`、第三方写媒体目录与后台扫描的关系、多服务并发读 FUSE 的影响、Syncthing 在 FUSE 上的 inotify/原子 rename 限制、认证各自独立
- 关键结论：`[watch].enabled` 不能关，否则 Filebrowser 写入的文件永远不会进索引

### v1.1（2026-11-08）—— 设计评审修订

**结构性缺陷（会返工）**

| #   | 位置      | 问题                                                    | 修法                                        |
| --- | ------- | ----------------------------------------------------- | ----------------------------------------- |
| 1   | 5.1 / 9.7 | `progress` 表没有完成标记，`position_ms` 归零后无法区分「看完」与「未播」 | 加 `finished` 列；`/api/continue` 条件明确化       |
| 2   | 5.1 / 5.2 | `extra_roots` 与 `UNIQUE(rel_path)`、`sha1(rel_path)` 冲突，两根同名文件撞主键 | 加 `roots` 表、`root_idx`、`UNIQUE(root_idx, rel_path)`；ID 含根路径 |
| 3   | 6.3 / 6.4 | **清洗顺序颠倒**：`(2014)` 不含 CJK 被当组名丢弃，随后规则再去括号里找年份，永远失配 | 改为「先抽年份/季集，后清洗」；修正 rule 2 的尾部边界 |
| 4   | 6.3     | 缺 movie/episode 判定与剧集目录回溯规则                           | 新增判定流程与回溯规则，移入 M1                         |
| 5   | 5.1 / 9.6 | 单一 `sub_path` 装不下 chs/cht/eng 多字幕                    | 改 `sub_paths` JSON + `/sub?lang=`         |
| 6   | 5.3     | 增量判定不看 NFO 的 mtime，用户手改的 NFO 永不生效                     | 四元组加入 `nfo_mtime_unix`                    |
| 7   | 10.1 / 3.3 | `GET /play` 有副作用，任意网页可盲打拉起播放器；`no-cors` 过不了 PNA 预检 | 改 POST + Origin + token 三道校验；补 OPTIONS 预检头 |
| 8   | 9.2     | 缺 helper 失效时的降级通道                                     | 新增 `/api/cmd/{id}`                       |

**技术错误**

| #   | 位置   | 问题                              | 修法                                    |
| --- | ---- | ------------------------------- | ------------------------------------- |
| 9   | 8.2  | TMDB 限速写成「40 次/秒」，实际是「40 次/10 秒」，差一个数量级；且与「4×250ms」算出的速率不自洽 | 改默认 `concurrency=2`/`interval_ms=100`，按 429 + `Retry-After` 自适应 |
| 10  | 4.2  | 未写任何 SQLite pragma，`ON DELETE CASCADE` 实际不生效，后台扫描与推流并发会锁库 | 明确 DSN（WAL / busy_timeout / foreign_keys）+ 单连接 + 批量事务 |
| 11  | 4.3  | 未设 HTTP 超时；若误设 `WriteTimeout` 会切断大文件流 | 补 `ReadHeaderTimeout`/`IdleTimeout`，`WriteTimeout=0`，加优雅关闭 |
| 12  | 8.3  | 打分表中「类型匹配 +20」是恒定值，阈值 60 太低；二值「完全/包含」救不回港台译名 | 改硬门槛（kind + 相似度≥0.75 + 年份差≤1）+ Levenshtein 相似度 + `alternative_titles` |
| 13  | 7.1  | 用 legacy `<fanart><thumb>` 与 `<thumb aspect>`   | 改 `<art>` 块；补 `<ratings>` 结构             |
| 14  | 11   | `fallback_language` 的触发时机未定义，中文 `overview` 空值率高 | 补字段级回落规则（7.6）                         |
| 15  | 9.5  | 「实测 100 MB/s」是未验证数值冒充实测           | 改「预估 60–120 MB/s，M1 实测回填」              |
| 16  | 5.1  | 建了 `idx_items_title`，但 `LIKE '%x%'` 用不上它 | 删除该索引并说明原因                            |

**补充与一致性**

- 补 `--reindex`、`--unlock` 到 CLI（v1.0 在 5.2 / ADR-006 引用但未定义）
- 补 `[watch]` 配置段（v1.0 在 12.2 承诺「可配置关闭」但配置里没有该键）
- 修正 2.2「只读媒体库」的措辞，新增 2.3 写入边界表
- 新增 5.5 schema 迁移策略（删库重建）
- 新增 8.5 扫描互斥（文件锁）与扫描限速，避免扫盘拖垮推流
- 新增 8.6 `scrape_status` 语义表，明确 `error` 会被 `--only-missing` 重试而 `miss` 不会
- 新增 9.4 图片 `?w=` 与缓存头；9.2 补 `/healthz` 语义
- 新增 ADR-008（helper 选型的三种方案对比）、ADR-009（迁移策略）
- 13.4 runit 脚本改绝对路径（`$HOME` 在 runsv 环境下不可靠）；13.2 重写为 DNS 机理说明 + 程序内 `net.Resolver` 兜底；13.8 标注 `/sys` 权限问题
- 里程碑新增 M0（浏览器验证）、M1 纳入剧集，总估期 16 → 18 天
- 15 与 17 章去重：15 只留已接受的永久限制，17 只留待验证的不确定性

---

## 附录 A：目录结构

✅ 已实现　⏳ 待实现

```
litems/
├── main.go              ✅ 子命令入口、日志、CLI 参数
├── config.go            ✅ TOML 解析、默认值、启动校验
├── parse.go             ✅ 文件名解析、归属判定辅助、相似度
├── parse_test.go        ✅ 解析规则用例（覆盖 6.3 / 6.4 的边界）
├── scan.go              ✅ 遍历、增量判定、字幕匹配、清理消失条目
├── db.go                ✅ SQLite 封装（DSN/pragma/事务/迁移/查询）
├── server.go            ✅ 路由、Basic Auth、API、流、字幕、页面渲染
├── launch.go            ✅ helper（POST + Origin + token + PNA 预检）
├── settings.go          ✅ 设置页：凭据写回配置（白名单 + CSRF 令牌）
├── settings_test.go     ✅ 配置写回不破坏注释与其它段
├── net.go               ✅ DNS 自检与三级回退、代理注入
├── scrape.go            ✅ 刮削主流程、匹配门槛、剧集分组、落盘
├── scrape_test.go       ✅ 假 TMDB 端到端（含幂等与 lockdata）
├── tmdb.go              ✅ TMDB 客户端（限速/退避/语言回落）
├── omdb.go              ✅ OMDb 兜底后端
├── nfo.go               ✅ NFO 读写
├── nfo_test.go          ✅ NFO 结构与 uniqueid 优先级
├── web/
│   ├── index.html       ✅ 海报墙、搜索、分页
│   ├── item.html
│   └── settings.html        ✅ 详情页、播放按钮、复制命令
├── probe/               ✅ M0 网络自检（DNS / 可达性 / 代理）
├── bench/               ✅ 组件基准（SQLite / 推流，见附录 C）
├── docs/DESIGN.md       本文档
├── build.sh             ✅ 九平台构建
├── config.example.toml  ✅ 带注释的完整示例
└── README.md            ✅ 安装、坑位、命令速查
```

**相对 v1.0 设计的两处合并**：`http.go` / `api.go` / `stream.go` 合并为 `server.go`（都是 `net/http` 的 handler，拆三个文件反而要来回跳）；`progress.go` 并入 `db.go`（进度逻辑只有 upsert 与完成判定，共 40 行）。

## 附录 B：术语

| 术语              | 含义                          |
| --------------- | --------------------------- |
| 刮削（scrape）      | 从 TMDB 等数据源获取元数据并写入本地       |
| NFO             | Kodi 定义的 XML 元数据文件，跟随媒体文件存放 |
| 直连（direct play） | 服务器只发原文件，客户端自行解码            |
| remux           | 只更换容器，不重新编码                 |
| Range 请求        | HTTP 分段请求，播放器拖动进度条依赖它       |
| helper          | PC 侧的小服务，接收浏览器请求后启动本地播放器    |
| FUSE            | 用户态文件系统，`/sdcard` 即由此实现     |
| PNA             | Private Network Access，浏览器对「私网页面 → 本地回环」的访问限制 |

---

## 附录 C：基准数据

测试环境：Redmi K20 Pro / Snapdragon 855 / Termux / 内核 `4.14.180-perf` / Go 1.27.3 / 存储为 `/data`（ext4，129G 可用）/ 网络经 Windows 移动热点（`192.168.137.0/24`）。

所有数字均为本机实测，换设备或换网络需重测。

### C.1 磁盘

| 项 | 结果 |
| --- | --- |
| 顺序写 512 MB（`dd conv=fsync`） | **205 MB/s** |
| 顺序读（页缓存命中） | 5.7 GB/s —— 缓存值，仅作上限参考，不可信 |
| 4K 随机写 ×2000（`oflag=dsync`） | 5.2 MB/s，约 **0.78 ms/文件** |

含义：写 1000 个 NFO + 图片约 0.8 秒，不是瓶颈。顺序读的数字因页缓存不可信，忽略。

### C.2 SQLite（modernc，WAL，`SetMaxOpenConns(1)`）

| 项 | 结果 |
| --- | --- |
| 建表（5 表 + 5 索引） | 17.0 ms |
| 插入 1000 条（逐条提交） | 509 ms → 1964 条/秒 |
| 插入 10000 条（事务 100/批） | 1400 ms → **7143 条/秒** |
| `COUNT(*)`（1.1 万行） | 0.61 ms |
| 分页 60 条 `ORDER BY mtime_unix DESC`（**无索引**） | **33.6 ms** |
| 分页 60 条（加索引后） | 预期 < 1 ms（见下方结论 2） |
| `title LIKE '%关键词%'`（1.1 万行全表扫） | 3.3 ms |
| 状态过滤（走索引） | 0.45 ms |
| 进度 upsert ×100（逐条提交） | 102 ms → 约 **1 ms/次** |
| WAL 并发：持续写入时做 50 次读 | 121 ms，**无 `database is locked`** |
| 库体积（1.1 万条，含全文简介） | 5.71 MB（另有 WAL 4.36 MB 未 checkpoint） |

**三条直接结论**

1. **批量事务必须做**：7143 vs 1964，差 3.6 倍。4.2 节写的「慢一个数量级」夸张了，但方向没错。
2. **`mtime_unix` 必须有索引**：33.6 ms 对「打开首页」是可感知的卡顿，1.1 万条就已如此，量再涨会线性恶化。5.1 已补 `idx_items_mtime`。
3. **单连接串行不是问题**：WAL 下 50 次读里没有一次 locked，1 ms 级的写入延迟完全撑得住每页 60 条的浏览。

### C.3 二进制体积

| 产物 | 大小 | 说明 |
| --- | --- | --- |
| `probe-linux` | **5.3 MB** | 纯 Go + `net/http`，`GOOS=linux`，`-s -w` |
| `bench` | **9.5 MB** | 增加 `database/sql` + modernc sqlite |
| `probe`（android 目标） | 5.9 MB | `GOOS=android`，PIE |

### C.4 HTTP 推流

| 项 | 结果 |
| --- | --- |
| 单流 1 GB | **31.7 MB/s**（32.3 s） |
| 双流并发 2 GB | 34 MB/s（59 s）—— 仅比单流高 2.3，链路饱和 |
| Range 分段 1 MB | `206`，103 ms |
| 慢响应 20 秒 | 40/40 tick 全部送达（`WriteTimeout=0` 生效） |
| 局域网 `ping` | 17 ms |

这是**热点**环境下的数字。手机直连路由器后应重测——热点通常是瓶颈所在。

---

*文档结束。实现阶段如遇与本文冲突之处，以本文为准并同步更新。*
