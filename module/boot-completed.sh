#!/system/bin/sh
# shellcheck shell=ash
# KernelSU / Magisk 在开机完成后调用本脚本。
#
# service.sh 由 KernelSU 在 post-fs-data 阶段启动，会自己拉起 guard.sh
# 与 watchdog.sh。这里只做一次「开机后补检查」：
# 万一 post-fs-data 那一轮有东西没起来，这里补一把。

MODDIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd)
LOG="$MODDIR/allin.log"
GUARD_LOCK="$MODDIR/run/guard.pid"
WD_LOCK="$MODDIR/run/watchdog.pid"

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') [boot] $*" >> "$LOG"; }

# 等系统真正就绪，避免和 service.sh 的启动过程抢资源
i=0
if command -v getprop >/dev/null 2>&1; then
    while [ "$(getprop sys.boot_completed)" != "1" ] && [ $i -lt 150 ]; do
        sleep 2
        i=$((i+1))
    done
fi
sleep 8

alive() {
    [ -f "$1" ] || return 1
    pid=$(cat "$1" 2>/dev/null)
    [ -n "$pid" ] && [ -d "/proc/$pid" ] || return 1
    return 0
}

if ! alive "$GUARD_LOCK"; then
    [ -f "$GUARD_LOCK" ] && rm -f "$GUARD_LOCK"
    log "开机补拉 guard.sh"
    setsid sh "$MODDIR/guard.sh" > /dev/null 2>&1 < /dev/null &
    sleep 5
fi

if ! alive "$WD_LOCK"; then
    [ -f "$WD_LOCK" ] && rm -f "$WD_LOCK"
    log "开机补拉 watchdog.sh"
    setsid sh "$MODDIR/watchdog.sh" > /dev/null 2>&1 < /dev/null &
fi
