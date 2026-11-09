#!/data/data/com.termux/files/usr/bin/bash
# qBittorrent 下载完成 → 在媒体库里建符号链接 → 触发 LiteMS 索引与刮削。
#
# 为什么不直接把 qbt 的保存目录设成库目录：
#   LiteMS 会在视频旁边写 .nfo 和 poster/fanart，那样等于往正在做种的
#   种子目录里塞文件。
#
# 为什么不用硬链接：
#   实测这台 Android 上 link() 被禁（同一目录里 `ln` 都返回 Permission denied），
#   $PREFIX/tmp 也一样，硬链接这条路根本走不通。
#
# 所以用符号链接：qbt 在 ~/Downloads 里做种，链接指向它，文件只有一份。
#   - 下载目录：qbt 独占，litems 不写入一个字节
#   - 库目录：litems 自己写 .nfo / 海报，删链接不影响做种
#   - 前提：litems 要开 [library] follow_file_symlinks = true
#
# 参数（qbt 的「运行外部程序」会替换）：
#   $1 = %F 内容路径（单文件是文件路径，多文件是种子根目录）
#   $2 = %N 种子名

PATH=/data/data/com.termux/files/usr/bin:/system/bin
export PATH
HOME=/data/data/com.termux/files/home
export HOME

LITEMS="$HOME/litems/litems"
LIB="$HOME/Media/下载"
LOG="$HOME/litems-qbt-hook.log"
CFG="$HOME/.config/litems/config.toml"

log() { echo "[$(date '+%F %T')] $*" >>"$LOG"; }

src="$1"
name="${2:-$(basename "$1")}"

if [ -z "$src" ] || [ ! -e "$src" ]; then
    log "跳过：内容路径无效（\$1=$src）"
    exit 0
fi

mkdir -p "$LIB"

linked=0
failed=0
: >"$LOG.links"

if [ -d "$src" ]; then
    parent=$(dirname "$src")
    while IFS= read -r f; do
        rel=${f#"$parent"/}
        target="$LIB/$rel"
        mkdir -p "$(dirname "$target")" 2>/dev/null
        if ln -sfn "$f" "$target" 2>>"$LOG"; then
            echo ok >>"$LOG.links"
        else
            echo fail >>"$LOG.links"
            log "建链失败：$rel"
        fi
    done < <(find "$src" -type f 2>/dev/null)
else
    target="$LIB/$(basename "$src")"
    if ln -sfn "$src" "$target" 2>>"$LOG"; then
        echo ok >>"$LOG.links"
    else
        echo fail >>"$LOG.links"
        log "建链失败：$(basename "$src")"
    fi
fi

linked=$(grep -c '^ok$' "$LOG.links" 2>/dev/null) || linked=0
failed=$(grep -c '^fail$' "$LOG.links" 2>/dev/null) || failed=0
rm -f "$LOG.links"

log "已建链 $name：$linked 个文件，失败 $failed 个"

# 已有扫描在跑就跳过：litems 的 watch 定时扫描可能与这个钩子撞车
if pgrep -f "$LITEMS scan" >/dev/null 2>&1; then
    log "已有扫描在跑，本次不重复触发"
    exit 0
fi

"$LITEMS" scan >>"$LOG" 2>&1
log "扫描结束（退出码 $?）"

# 没有刮削凭据时 litems scrape 会直接报错退出，不必每次都制造一条错误日志
if [ -f "$CFG" ] && grep -Eq '^[[:space:]]*(tmdb_token|tmdb_key|omdb_key)[[:space:]]*=[[:space:]]*"[^"]+"' "$CFG"; then
    "$LITEMS" scrape --limit 20 >>"$LOG" 2>&1
    log "刮削结束（退出码 $?）"
else
    log "未配置刮削凭据，跳过 scrape（可在面板设置页填 TMDB token）"
fi

# 日志留最近 300 行
tail -n 300 "$LOG" >"$LOG.tmp" 2>/dev/null && mv "$LOG.tmp" "$LOG"
