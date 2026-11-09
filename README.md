# LiteMS

**LiteMS** is a single-binary personal media library for Android/Termux. It scans a local
video collection, fetches metadata from TMDB, writes it back as Kodi-compatible `.nfo`
files plus poster/fanart images, indexes everything in SQLite, and serves the files over
HTTP with `Range` support. Playback is delegated to a local player (mpv/VLC) on the PC —
there is no transcoding, no web player, and no runtime dependency.

Personal, non-commercial, single-user. Metadata is cached to disk and requests back off on
HTTP 429; nothing is collected or redistributed.

---

跑在 Android/Termux 上的家庭媒体库服务：负责扫描、刮削、索引，把文件按 HTTP 发出去；解码交给客户端。

一个静态二进制，无运行时依赖，`CGO_ENABLED=0` 交叉编译到九个平台。

设计文档见 [`docs/DESIGN.md`](docs/DESIGN.md)。

## 状态

M1–M5 已实现：扫描、刮削（TMDB/OMDb）、HTTP 服务、Web UI、进度续播、九平台构建。
刮削的真实匹配率还没验过，等一个 TMDB API key。

## 它做什么

- 递归扫描媒体目录，按文件名识别电影 / 剧集
- 刮削元数据（TMDB 为主源，OMDb 可切换作兜底），写 `movie.nfo` / `tvshow.nfo` / 单集 nfo 与海报、背景图
- SQLite 索引（纯缓存，删了能从文件重建）
- HTTP 服务：海报墙、详情页、搜索、`Range` 视频流、外挂字幕、播放进度
- PC 侧 helper：点网页上的播放按钮，用本地 mpv/VLC 打开

## 它不做什么

不转码、不在浏览器里播放、不实现 Jellyfin API、不做 DLNA/SMB。网盘与同步交给同机的 Filebrowser / Syncthing，本项目只负责媒体库。

## 快速开始（Termux）

```bash
# 1. 装 Go（首次约 38MB 下载、229MB 安装）
pkg install -y golang

# 2. 国内必须换模块镜像，否则 go mod tidy 会卡住
go env -w GOPROXY=https://goproxy.cn,direct GOSUMDB=off

# 3. 编译
git clone https://github.com/Schallophy/LiteMS && cd LiteMS
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags="-s -w" -o ~/bin/litems .

# 4. 先把服务起起来，凭据在网页里填（首次没有配置文件也能跑）
~/bin/litems scan --dry-run
~/bin/litems scan
~/bin/litems serve
```

浏览器打开 `http://<手机IP>:7788`，点右上角的设置按钮填 TMDB 凭据，点「测试连接」通过后保存。
也可以跳过网页，直接拷一份完整配置手工改：

```bash
mkdir -p ~/.config/litems
cp config.example.toml ~/.config/litems/config.toml
$EDITOR ~/.config/litems/config.toml     # 至少填 tmdb_token 或 tmdb_key
~/bin/litems scrape --probe              # 先验凭据与网络，只发一次请求
~/bin/litems scrape
```

设置页写入的是同一份文件，**只改 `[scrape]` 段里的几个键**，你手写的注释和别的段落不会被覆盖。

PC 上想一键播放，再跑一个 helper：

```bash
litems launch        # 监听 127.0.0.1:9876
```

## 三个必须知道的坑

**1. 媒体库和数据库都要放 `$HOME`，不要放 `/storage/emulated/0`。**

共享存储是 FUSE：`sendfile` 退化成用户态拷贝、`fcntl` 锁不完整会让 SQLite 报 `database is locked`、inotify 不可用。放在 `$HOME`（ext4）一次绕开全部问题。代价只是手机上的相册看不到这些文件——但播放走 HTTP，本来也不需要。

**2. `GOOS=linux` 的产物必须配 `[net].dns`。**

纯静态二进制读 `/etc/resolv.conf`，而 Android 没有这个文件，会回退到 `[::1]:53` 然后连接被拒。表现是「编译成功、运行无网络」。默认 `dns = "auto"` 会自动探测网关，通常不用改。

**3. `api.themoviedb.org` 被污染，但 `api.tmdb.org` 可以直连。**

实测（K20 Pro + Windows 热点）：外网 UDP 53 全被阻断，只有网关 DNS 可用；`api.themoviedb.org` 与 `api.bgm.tv` 都被解析成 `127.0.0.1`，而同一后端的 `api.tmdb.org` 与图片 CDN `image.tmdb.org` 正常。所以默认配置把 `tmdb_base` 指向 `https://api.tmdb.org/3`，**不需要代理**。如果哪天这个域名也被污染，把 `[net].proxy` 指向局域网内可用的代理即可。

## 常用命令

```bash
litems scan --dry-run        # 只看会做什么，不写库
litems scan --force          # 忽略增量，全部重扫
litems scan --reindex        # 删库重建后全量扫描
litems scrape --probe        # 验凭据与网络（一次请求）
litems scrape --dry-run      # 列出待刮削条目，不调 API
litems scrape --limit 5      # 先试 5 条，看效果再全量
litems scrape --force        # 全部重刮（仍尊重 lockdata）
litems serve --listen 0.0.0.0:7788
litems launch --player mpv
```

刮削是独立的一步：`scan` 只碰文件系统、不联网；`scrape` 才调 API。这样断网时索引照样能用。

## 目录结构约定

```
媒体根/
├── 电影/
│   └── 星际穿越 (2014)/
│       ├── 星际穿越 (2014).mkv
│       ├── movie.nfo
│       ├── poster.jpg
│       └── 星际穿越 (2014).chs.srt
└── 剧集/
    └── 切尔诺贝利 (2019)/
        ├── tvshow.nfo
        └── Season 01/
            ├── 切尔诺贝利 S01E01.mkv
            └── 切尔诺贝利 S01E01.nfo
```

解析规则：**先抽年份/季集，再清洗发布标记**。顺序反过来会把 `(2014)` 当噪声删掉——这是本项目最容易写错的一处地方。

## 与 qBittorrent 共存（做种不受影响）

典型需求：qbt 下片并做种，litems 负责索引与播放。这两件事能同时成立，但**不能让 litems 往下载目录写文件**——
它会在视频旁边生成 `.nfo`、`poster.jpg`、`fanart.jpg`，而那个目录正被 qbt 用于做种。

**推荐做法：设置下载目录，让 serve 自动镜像**。设置页「媒体库」里填上下载目录
（或配置文件 `[library].download_dir = "~/Downloads"`），然后重启服务。之后：

- serve 每分钟检查下载目录，出现新内容就在媒体根的 `Downloads/` 子目录建立符号链接，
  自动扫描并刮削——**下载工具侧零配置**，不需要钩子脚本
- 下载目录一个字节都不会被写入；文件只有一份，不占双倍空间
- 未完成文件（`.!qB`、`.part`、隐藏目录）自动跳过，下完的那轮自然入库
- 在 qbt 里删了种子，对应的链接与只剩元数据的空目录会在一分钟内自动清掉
- 想要完整副本而不是链接（下载目录随时会被卸载/清空时）：`download_mode = "copy"`，
  代价是双倍空间，副本不会自动清理

布局示例（下载目录 `~/Downloads`，媒体根 `~/Media`）：

```
~/Downloads/<种子>/...            qbt 独占，做种用；litems 一个字节都不写
~/Media/Downloads/<种子>/...  ->   上面那些文件，符号链接
~/Media/Downloads/<种子>/*.nfo     litems 写在这里（链接旁边，不是原文件旁边）
```

**两个前提**：

- 媒体根要开 `follow_file_symlinks = true`（设置页勾选），否则链接不会被索引
- 下载目录不能与媒体根互相包含，设置页会直接拒绝这种写法

**硬链接不可用**：这台设备上 `link()` 被 Android SELinux 拦掉，同一目录里 `ln` 都是
`Permission denied`，所以零拷贝方案只有符号链接；它跨文件系统、指向哪里都行。

## 开发

```bash
go test ./...                                    # 解析规则有完整用例
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build . # 必须能编过（CI 守门）
./build.sh                                       # 九平台矩阵
```

`probe/` 与 `bench/` 是网络与组件自检工具，换环境后用来诊断，不参与主程序构建。

## 已知限制

| 限制 | 说明 |
| --- | --- |
| 无转码 | 片源必须客户端能解，用 mpv/VLC |
| 浏览器无法直接播放 | MKV 容器、多音轨、ASS 字幕都不支持，所以播放交给本地播放器 |
| 剧集匹配弱于 TMDB | OMDb 分集只有标题与评分，无剧情无剧照；作兜底用 |
| 豆瓣未接入 | 中文增强留待后续；当前中文数据全部来自 TMDB `zh-CN` |
| 单用户 | 一个 Basic Auth 账号 |
| `api.tmdb.org` 也可能被墙 | 届时需配 `[net].proxy` |
