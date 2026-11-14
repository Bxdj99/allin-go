#!/system/bin/sh
# shellcheck shell=ash
# 管理器里的「运行」键：打开 All In 面板（自动带上网关口令）。

MODDIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd)
DATA="$MODDIR/data"
PORT=3425

port_up() {
    ss -tln 2>/dev/null | grep -q ":$PORT" && return 0
    netstat -tln 2>/dev/null | grep -q ":$PORT" && return 0
    return 1
}

if ! port_up; then
    echo "网关未运行，正在拉起…"
    sh "$MODDIR/service.sh" > /dev/null 2>&1 &
    n=0
    while [ $n -lt 15 ]; do
        sleep 2
        port_up && break
        n=$((n+1))
    done
fi

if ! port_up; then
    echo "网关没起来，检查 $MODDIR/gateway.log"
    exit 1
fi

K=$(sed -n 's/.*"authKey"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$DATA/config.json" 2>/dev/null | head -1)

if [ -n "$K" ]; then
    am start -a android.intent.action.VIEW -d "http://127.0.0.1:$PORT/#key=$K" >/dev/null 2>&1
else
    am start -a android.intent.action.VIEW -d "http://127.0.0.1:$PORT/" >/dev/null 2>&1
fi

echo "面板: http://127.0.0.1:$PORT/"
echo "接口: http://127.0.0.1:$PORT/v1"
echo "配置: $DATA/config.json"
