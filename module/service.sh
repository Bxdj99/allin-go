#!/system/bin/sh
# shellcheck shell=ash
# All In（Go 版）模块启动脚本
#
# 关键设计：网关进程**不以 root 运行**。
#   - 模块目录（/data/adb/modules/allin）只有 root 能访问，用来存放权威配置与二进制
#   - 运行时把二进制和配置同步到 /data/local/tmp/allin-run，chown 给 shell(2000)
#   - 用 setuidgid 2000 启动，网关只有 shell 权限
# 这样即使网关被攻破，拿到的也只是 shell uid，拿不到 root；
# 同时它读不到 /data/adb，看不到模块与 KernelSU 的任何痕迹。
#
# 注意：模块目录里改完配置后需要重启模块（或运行一次本脚本）才会同步到运行目录。

MODDIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd)
RUNDIR=/data/local/tmp/allin-run
MODDATA="$MODDIR/data"
RUNDATA="$RUNDIR/data"
LOG="$MODDIR/allin.log"
GWLOG="$MODDIR/gateway.log"
RUNLOG="$RUNDIR/gateway.log"
PORT=3425
API="http://127.0.0.1:$PORT/api/workbuddy/checkin"
RUNAS=2000

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >> "$LOG"; }

port_up() {
    ss -tln 2>/dev/null | grep -q ":$PORT" && return 0
    netstat -tln 2>/dev/null | grep -q ":$PORT" && return 0
    return 1
}

prepare() {
    mkdir -p "$RUNDIR" "$RUNDATA" 2>/dev/null
    if [ ! -f "$MODDIR/bin/allin" ]; then
        log "缺少二进制：$MODDIR/bin/allin"
        return 1
    fi
    cp -f "$MODDIR/bin/allin" "$RUNDIR/allin" 2>/dev/null
    chmod 755 "$RUNDIR/allin"

    mkdir -p "$MODDATA" 2>/dev/null
    if [ ! -f "$MODDATA/config.json" ]; then
        # 首次：让程序自己生成默认配置（先跑一次 -h 触发不了，直接留空即可）
        log "配置不存在，将在首次启动时生成"
    elif [ ! -f "$RUNDATA/config.json" ] || [ "$MODDATA/config.json" -nt "$RUNDATA/config.json" ]; then
        cp -f "$MODDATA/config.json" "$RUNDATA/config.json" 2>/dev/null
        log "配置已同步到运行目录"
    fi

    chown -R "$RUNAS:$RUNAS" "$RUNDIR" 2>/dev/null
    return 0
}

start_gateway() {
    prepare || return 1
    log "启动网关（uid=$RUNAS）"
    setsid busybox setuidgid "$RUNAS" "$RUNDIR/allin" -config "$RUNDATA/config.json" \
        >> "$RUNLOG" 2>&1 < /dev/null &
}

# 把运行目录里的配置回写到模块（保存签到记录等）
sync_back() {
    if [ -f "$RUNDATA/config.json" ]; then
        cp -f "$RUNDATA/config.json" "$MODDATA/config.json" 2>/dev/null
    fi
    if [ -s "$RUNLOG" ]; then
        tail -n 500 "$RUNLOG" >> "$GWLOG" 2>/dev/null
        : > "$RUNLOG" 2>/dev/null
    fi
}

# ---- 等系统起来 ----
i=0
while [ "$(getprop sys.boot_completed)" != "1" ] && [ $i -lt 150 ]; do
    sleep 2
    i=$((i+1))
done
sleep 5

if port_up; then
    log "网关已在运行"
else
    start_gateway
    n=0
    while [ $n -lt 20 ]; do
        port_up && { log "网关就绪 $PORT（非 root 运行）"; break; }
        sleep 2
        n=$((n+1))
    done
    [ $n -ge 20 ] && log "40 秒内未就绪，看 $RUNLOG"
fi

# ---- 常驻守护 + 每日签到 + 配置回写 ----
(
    last=""
    ticks=0
    while true; do
        sleep 30
        ticks=$((ticks+1))

        if ! port_up; then
            log "网关掉线，重拉"
            start_gateway
        fi

        # 每 10 分钟把运行配置与日志回写模块目录
        if [ $((ticks % 20)) -eq 0 ]; then
            sync_back
        fi

        H=$(date +%H)
        slot="$(date +%Y%m%d)-$H"
        if { [ "$H" = "09" ] || [ "$H" = "21" ]; } && [ "$last" != "$slot" ]; then
            K=$(sed -n 's/.*"authKey"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$RUNDATA/config.json" 2>/dev/null | head -1)
            if [ -n "$K" ]; then
                curl -s -m 120 -o /dev/null -X POST "$API" \
                    -H "Authorization: Bearer $K" \
                    -H 'Content-Type: application/json' -d '{}' 2>/dev/null
                log "workbuddy checkin slot=$slot"
                sync_back
            fi
            last="$slot"
        fi
    done
) &

exit 0
