#!/bin/sh
# 交叉编译全部目标。用法：./build.sh [目标...]
# 不传目标则编译文档 14.1 的完整矩阵。
set -e

VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS="-s -w -X main.version=$VERSION"

TARGETS="$*"
if [ -z "$TARGETS" ]; then
  TARGETS="linux/amd64 linux/arm64 linux/arm linux/386
           darwin/amd64 darwin/arm64
           windows/amd64 windows/arm64
           freebsd/amd64"
fi

mkdir -p dist
ERRFILE=$(mktemp)

echo "版本: $VERSION"
echo

for t in $TARGETS; do
  GOOS=${t%/*}
  GOARCH=${t#*/}
  OUT="dist/litems-${GOOS}-${GOARCH}"
  [ "$GOOS" = "windows" ] && OUT="${OUT}.exe"

  printf '构建 %-16s' "$t"
  if CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH \
      go build -trimpath -ldflags="$LDFLAGS" -o "$OUT" . 2>"$ERRFILE"; then
    SIZE=$(ls -lh "$OUT" | awk '{print $5}')
    echo "  $SIZE"
  else
    echo "  失败"
    cat "$ERRFILE"
    rm -f "$ERRFILE"
    exit 1
  fi
done
rm -f "$ERRFILE"

echo
echo "产物在 dist/"
ls -lh dist | tail -n +2 | awk '{printf "  %-32s %s\n", $9, $5}'
