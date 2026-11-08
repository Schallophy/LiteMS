# LiteMS

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

# 4. 配置
mkdir -p ~/.config/litems
cp config.example.toml ~/.config/litems/config.toml
$EDITOR ~/.config/litems/config.toml     # 至少填 tmdb_token 或 tmdb_key

# 5. 扫描 → 刮削 → 起服务
~/bin/litems scan --dry-run
~/bin/litems scan
~/bin/litems scrape --probe              # 先验凭据与网络，只发一次请求
~/bin/litems scrape
~/bin/litems serve
```

浏览器打开 `http://<手机IP>:8080`。

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
litems serve --listen 0.0.0.0:8080
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
