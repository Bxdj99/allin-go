#!/system/bin/sh
# shellcheck shell=ash
# 管理器里的「运行」键：
#   1. 确保网关在跑
#   2. 把网关口令**打印出来**（首次使用最需要这个）
#   3. 顺手用带 #key= 的 URL 打开面板

MODDIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd)
# 端口从配置读（首次安装会自动选一个空闲端口，避开 Magpie 等占用的 3425）
PORT=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' /data/local/tmp/allin-run/data/config.json 2>/dev/null | head -1)
[ -z "$PORT" ] && PORT=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$MODDIR/data/config.json" 2>/dev/null | head -1)
[ -z "$PORT" ] && PORT=6667

port_up() {
    ss -tln 2>/dev/null | grep -q ":$PORT" && return 0
    netstat -tln 2>/dev/null | grep -q ":$PORT" && return 0
    return 1
}

# 配置可能先后出现在两个位置：运行时目录（权威）与模块目录（回写副本）
RUNDATA=/data/local/tmp/allin-run/data
MODDATA="$MODDIR/data"

find_config() {
    if [ -f "$RUNDATA/config.json" ]; then
        echo "$RUNDATA/config.json"
    elif [ -f "$MODDATA/config.json" ]; then
        echo "$MODDATA/config.json"
    else
        echo ""
    fi
}

read_key() {
    cfg="$1"
    [ -n "$cfg" ] || return 1
    sed -n 's/.*"authKey"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$cfg" 2>/dev/null | head -1
}

if ! port_up; then
    echo "网关未运行，正在拉起…"
    sh "$MODDIR/service.sh" > /dev/null 2>&1 &
    n=0
    while [ $n -lt 20 ]; do
        sleep 2
        port_up && break
        n=$((n+1))
    done
fi

if ! port_up; then
    echo "❌ 网关没起来"
    echo
    echo "排查："
    echo "  1. 看日志：cat $MODDIR/gateway.log"
    echo "  2. 手动启动：sh $MODDIR/service.sh"
    echo "  3. 检查二进制：ls -l $MODDIR/bin/allin"
    exit 1
fi

CFG=$(find_config)
KEY=$(read_key "$CFG")

echo "================================================"
echo " All In 网关已运行"
echo "================================================"
echo
if [ -n "$KEY" ]; then
    echo "网关口令（客户端 API Key 填这个）:"
    echo
    echo "    $KEY"
    echo
else
    echo "⚠️  还没读到网关口令"
    echo "    首次启动会自动生成，等几秒再点一次「运行」；"
    echo "    或手动查看：cat $RUNDATA/config.json"
    echo
fi
echo "面板地址:  http://127.0.0.1:$PORT/"
echo "接口地址:  http://127.0.0.1:$PORT/v1"
echo "配置文件:  ${CFG:-（尚未生成）}"
echo
echo "客户端这样填："
echo "    Base URL : http://127.0.0.1:$PORT/v1"
echo "    API Key  : 上面那串口令"
echo "    模型名   : workbuddy/glm-5.3"
echo "================================================"

# 带上口令直接打开面板（避免用户再手输一次）
if [ -n "$KEY" ]; then
    am start -a android.intent.action.VIEW -d "http://127.0.0.1:$PORT/#key=$KEY" >/dev/null 2>&1
else
    am start -a android.intent.action.VIEW -d "http://127.0.0.1:$PORT/" >/dev/null 2>&1
fi
