#!/bin/sh
# 一键编译 + 打包 KernelSU 模块
#
# 用法：
#   ./build.sh              # 交叉编译 linux/arm64 并打包模块
#   ./build.sh test         # 只跑测试
#   ./build.sh clean        # 清理产物
set -e
cd "$(dirname "$0")"

ARCH=${ARCH:-arm64}
OUT=allin
MODZIP=AllIn-Go.zip

case "$1" in
  test)
    echo "==> go vet"
    go vet ./...
    echo "==> go test"
    go test ./...
    exit 0
    ;;
  clean)
    rm -f "$OUT" "$MODZIP" module/bin/allin
    echo "已清理"
    exit 0
    ;;
esac

# 从 module.prop 读版本号注入二进制（面板「检查更新」要用它比对）
VER=$(grep '^version=' module/module.prop | cut -d= -f2 | tr -d ' ')
echo "==> 编译 linux/${ARCH} 静态二进制（版本 ${VER}）"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" \
  go build -trimpath -ldflags "-s -w -X allin/internal/gateway.BuildVersion=${VER}" -o "$OUT" .

SIZE=$(ls -lh "$OUT" | awk '{print $5}')
echo "    $OUT  $SIZE"

echo "==> 打包模块"
mkdir -p module/bin
cp "$OUT" module/bin/allin
chmod 755 module/bin/allin
rm -f "$MODZIP"
( cd module && zip -q -r "../$MODZIP" module.prop service.sh action.sh uninstall.sh bin/allin )

ZSIZE=$(ls -lh "$MODZIP" | awk '{print $5}')
echo "    $MODZIP  $ZSIZE"
echo
echo "完成。刷入 $MODZIP 即可，或直接运行 ./$OUT"
