#!/system/bin/sh
# shellcheck shell=ash
# 守护循环的看门狗。
#
# 为什么需要它：自动任务（签到等）靠 guard.sh 里那个 60 秒循环驱动。
# 循环是普通后台子 shell，没有会话保护，实测会在断电、系统内存回收、
# 在线更新重启等情况下静默消失 —— 而网关进程本身还在跑。
# 结果就是「网关看起来一切正常，但自动签到就是不执行」。
# 线上真实发生过：11-24 21:56 跑完最后一轮后循环消失，
# 第二天 9 点整个签到窗口没有任何进程触发，13 小时后才被人发现。
#
# 本脚本只做一件事：每 5 分钟看一眼 guard.sh 是否活着，没了就重拉。
# 开销极小（纯 shell + /proc 检查），不碰网络、不碰网关。

MODDIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd)
LOG="$MODDIR/allin.log"
LOCK="$MODDIR/run/watchdog.pid"
GUARD_LOCK="$MODDIR/run/guard.pid"

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') [watchdog] $*" >> "$LOG"; }

mkdir -p "$MODDIR/run" 2>/dev/null

# 单实例：已有的看门狗还活着就直接退出
if [ -f "$LOCK" ]; then
    OLD=$(cat "$LOCK" 2>/dev/null)
    if [ -n "$OLD" ] && [ -d "/proc/$OLD" ]; then
        exit 0
    fi
fi
echo $$ > "$LOCK"
trap 'rm -f "$LOCK"' EXIT INT TERM

# 守护循环是否存活：guard.pid 存在且指向的进程还在。
guard_alive() {
    [ -f "$GUARD_LOCK" ] || return 1
    pid=$(cat "$GUARD_LOCK" 2>/dev/null)
    [ -n "$pid" ] || return 1
    [ -d "/proc/$pid" ] || return 1
    return 0
}

log "看门狗启动（pid $$）"

while true; do
    sleep 300
    if ! guard_alive; then
        # 清掉过期 pid 文件，否则 guard.sh 的单实例保护会让它直接退出
        [ -f "$GUARD_LOCK" ] && {
            pid=$(cat "$GUARD_LOCK" 2>/dev/null)
            [ -n "$pid" ] && [ -d "/proc/$pid" ] || rm -f "$GUARD_LOCK"
        }
        log "守护循环不在，重新拉起 guard.sh"
        setsid sh "$MODDIR/guard.sh" > /dev/null 2>&1 < /dev/null &
        sleep 10
        if guard_alive; then
            log "守护循环已恢复"
        else
            log "恢复失败，5 分钟后重试"
        fi
    fi
done
