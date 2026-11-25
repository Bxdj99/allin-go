#!/system/bin/sh
# shellcheck shell=ash
# 常驻守护循环：每分钟一轮，负责
#   1. 应用网关放下的在线更新包（$STAGE）
#   2. 端口配置变更后重启网关
#   3. 网关掉线重拉
#   4. 每 10 分钟回写配置与日志
#   5. 到点跑 WorkBuddy 全套自动任务（签到等）
#
# 单独抽成一个脚本、而不是 service.sh 里的匿名后台子 shell，
# 是因为匿名子 shell 没有会话保护：父脚本 exit 后就靠 init 收养，
# 一旦被系统回收就静默消失，而网关进程还在 ——
# 表现就是「网关正常但自动签到不跑」。抽出来之后可以
# 独立 setsid、独立写 pid 文件、被 watchdog 独立检测与重启。

MODDIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd)

RUNDIR=/data/local/tmp/allin-run
STAGE="$RUNDIR/stage"
MODDATA="$MODDIR/data"
RUNDATA="$RUNDIR/data"
LOG="$MODDIR/allin.log"
GWLOG="$MODDIR/gateway.log"
RUNLOG="$RUNDIR/gateway.log"
SCHED_STATE="$RUNDATA/schedule.state"

PORT=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$RUNDATA/config.json" 2>/dev/null | head -1)
[ -z "$PORT" ] && PORT=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$MODDATA/config.json" 2>/dev/null | head -1)
[ -z "$PORT" ] && PORT=6868

API="http://127.0.0.1:$PORT/api/workbuddy/tasks"
RUNAS=2000
BUSYBOX=""
for c in /data/adb/magisk/busybox /data/adb/ksu/bin/busybox /data/adb/ap/bin/busybox /system/xbin/busybox /system/bin/busybox; do
    [ -x "$c" ] && { BUSYBOX="$c"; break; }
done
[ -z "$BUSYBOX" ] && command -v busybox >/dev/null 2>&1 && BUSYBOX=$(command -v busybox)

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >> "$LOG"; }

port_up() {
    ss -tln 2>/dev/null | grep -q ":$PORT" && return 0
    netstat -tln 2>/dev/null | grep -q ":$PORT" && return 0
    return 1
}

file_sum() {
    if [ ! -f "$1" ]; then echo "missing"; return; fi
    if command -v md5sum >/dev/null 2>&1; then
        md5sum "$1" 2>/dev/null | cut -d' ' -f1; return
    fi
    if [ -n "$BUSYBOX" ]; then
        "$BUSYBOX" md5sum "$1" 2>/dev/null | cut -d' ' -f1; return
    fi
    set -- $(wc -c < "$1") $(date -r "$1" +%s 2>/dev/null || echo 0)
    echo "$1-$2"
}

# 只搬网关 stdout 日志，不动配置。
# 签到这种「跑完就要看结果」的场景单独调它，避免等 10 分钟一轮的 sync_back。
flush_runlog() {
    if [ -s "$RUNLOG" ]; then
        cat "$RUNLOG" >> "$GWLOG" 2>/dev/null
        : > "$RUNLOG" 2>/dev/null
    fi
}

sync_back() {
    if [ -f "$RUNDATA/config.json" ]; then
        cp -f "$RUNDATA/config.json" "$MODDATA/config.json" 2>/dev/null
    fi
    if [ -f "$SCHED_STATE" ]; then
        cp -f "$SCHED_STATE" "$MODDATA/schedule.state" 2>/dev/null
    fi
    if [ -s "$RUNLOG" ]; then
        # 全量搬，不再只 tail 500 行。
        # 签到失败时要看上游原话，尾部截断会正好把 [CHECKIN] 那些行切掉。
        cat "$RUNLOG" >> "$GWLOG" 2>/dev/null
        : > "$RUNLOG" 2>/dev/null
    fi
}

summarize_tasks() {
    RESP="$1"
    count_status() {
        echo "$RESP" | grep -o "\"name\": *\"checkin\", *\"outcome\": *{[^}]*\"status\": *\"$1\"" 2>/dev/null | wc -l
    }
    n=$(echo "$RESP" | grep -o "\"name\": *\"checkin\"" 2>/dev/null | wc -l)
    ok=$(count_status ok)
    done_n=$(count_status already)
    fail=$(count_status failed)
    skip=$(count_status skipped)

    SUMMARY="acc=$n"
    [ "$ok" -gt 0 ] && SUMMARY="$SUMMARY 新签=$ok"
    [ "$done_n" -gt 0 ] && SUMMARY="$SUMMARY 已签=$done_n"
    [ "$fail" -gt 0 ] && SUMMARY="$SUMMARY 失败=$fail"
    [ "$skip" -gt 0 ] && SUMMARY="$SUMMARY 跳过=$skip"
    echo "$SUMMARY"
}

start_gateway() {
    if [ -n "$BUSYBOX" ]; then
        setsid "$BUSYBOX" setuidgid "$RUNAS" "$RUNDIR/allin" \
            -config "$RUNDATA/config.json" >> "$RUNLOG" 2>&1 < /dev/null &
    else
        setsid "$RUNDIR/allin" -config "$RUNDATA/config.json" >> "$RUNLOG" 2>&1 < /dev/null &
    fi
}

apply_staged_update() {
    ZIP="$STAGE/allin-update.zip"
    [ -f "$ZIP" ] || return 0
    log "发现待应用的在线更新包"

    WORK="$STAGE/x"
    rm -rf "$WORK"
    mkdir -p "$WORK" 2>/dev/null
    if [ -n "$BUSYBOX" ]; then
        "$BUSYBOX" unzip -o -q "$ZIP" -d "$WORK" 2>/dev/null
    else
        unzip -o -q "$ZIP" -d "$WORK" 2>/dev/null
    fi

    if [ ! -f "$WORK/module.prop" ] || [ ! -f "$WORK/bin/allin" ]; then
        log "更新包不完整，丢弃"
        rm -rf "$ZIP" "$WORK"
        return 0
    fi
    if ! grep -q '^id=allin' "$WORK/module.prop" 2>/dev/null; then
        log "更新包 id 不是 allin，拒绝安装"
        rm -rf "$ZIP" "$WORK"
        return 0
    fi

    cp -f "$MODDIR/module.prop" "$STAGE/module.prop.bak" 2>/dev/null

    if [ -f "$WORK/bin/allin" ]; then
        cp -f "$WORK/bin/allin" "$MODDIR/bin/allin.pending" 2>/dev/null &&
            chmod 755 "$MODDIR/bin/allin.pending" 2>/dev/null &&
            log "已放置新二进制（pending）"
    fi
    cp -f "$WORK/module.prop" "$MODDIR/module.prop" 2>/dev/null

    if [ -f "$WORK/service.sh" ]; then
        cp -f "$WORK/service.sh" "$MODDIR/service.sh.pending" 2>/dev/null &&
            chmod 755 "$MODDIR/service.sh.pending" 2>/dev/null &&
            log "已放置新 service.sh（pending，下次启动替换）"
    fi
    for f in action.sh uninstall.sh watchdog.sh boot-completed.sh guard.sh; do
        [ -f "$WORK/$f" ] && cp -f "$WORK/$f" "$MODDIR/$f" 2>/dev/null && chmod 755 "$MODDIR/$f" 2>/dev/null
    done

    log "在线更新包已应用"
    rm -rf "$ZIP" "$WORK"
}

# ---- 单实例保护 ----
LOCK="$MODDIR/run/guard.pid"
mkdir -p "$MODDIR/run" 2>/dev/null
if [ -f "$LOCK" ]; then
    OLD=$(cat "$LOCK" 2>/dev/null)
    if [ -n "$OLD" ] && [ -d "/proc/$OLD" ]; then
        exit 0
    fi
fi
echo $$ > "$LOCK"
trap 'rm -f "$LOCK"' EXIT INT TERM

log "守护循环启动（pid $$）"

last=""
ticks=0
while true; do
    sleep 60
    ticks=$((ticks+1))

    if [ -f "$STAGE/allin-update.zip" ]; then
        log "守护循环发现更新包，开始应用"
        apply_staged_update
        if [ -f "$MODDIR/bin/allin.pending" ]; then
            OLDPID=$(ss -tlnp 2>/dev/null | grep ":$PORT" | grep -oE 'pid=[0-9]+' | cut -d= -f2 | head -1)
            [ -n "$OLDPID" ] && kill -9 "$OLDPID" 2>/dev/null && sleep 1
            mv -f "$MODDIR/bin/allin.pending" "$MODDIR/bin/allin" 2>/dev/null
            cp -f "$MODDIR/bin/allin" "$RUNDIR/allin" 2>/dev/null
            chown "$RUNAS:$RUNAS" "$RUNDIR/allin" 2>/dev/null
            chmod 755 "$RUNDIR/allin" 2>/dev/null
            log "更新已应用，重启网关"
            start_gateway
            n=0
            while [ $n -lt 20 ]; do
                port_up && { log "网关已重启 $PORT"; break; }
                sleep 2
                n=$((n+1))
            done
        fi
        sync_back
    fi

    CFGPORT=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$RUNDATA/config.json" 2>/dev/null | head -1)
    if [ -n "$CFGPORT" ] && [ "$CFGPORT" != "$PORT" ]; then
        log "端口配置变更 $PORT → $CFGPORT，重启网关"
        OLDPID=$(ss -tlnp 2>/dev/null | grep ":$PORT" | grep -oE 'pid=[0-9]+' | cut -d= -f2 | head -1)
        [ -n "$OLDPID" ] && kill -9 "$OLDPID" 2>/dev/null
        PORT="$CFGPORT"
        API="http://127.0.0.1:$PORT/api/workbuddy/tasks"
        sleep 1
        start_gateway
    fi

    if ! port_up; then
        log "网关掉线，重拉"
        start_gateway
    fi

    if [ $((ticks % 10)) -eq 0 ]; then
        sync_back
        TDAY=$(date +%Y%m%d)
        if ! grep -q "签到汇总 $TDAY" "$LOG" 2>/dev/null; then
            DONE=$(grep "workbuddy tasks slot=$TDAY-" "$LOG" 2>/dev/null | tail -1)
            if [ -n "$DONE" ]; then
                echo "$(date '+%Y-%m-%d %H:%M:%S') 签到汇总 $TDAY: $(echo "$DONE" | sed 's/.*slot=[0-9-]* //')" >> "$LOG"
            fi
        fi
    fi

    H=$(date +%H)
    slot="$(date +%Y%m%d)-$H"

    # 读自动任务开关与执行小时。
    # 不直接从 config.json 用 sed 抠：config.json 是多行缩进 JSON，
    # 早期写法用了 BRE 的 \| 分支（\(true\|false\)），而 busybox sed 的 BRE
    # 不支持 \|，于是开关永远读不到、永远回退成 true，
    # 面板上「关闭自动任务」从来不起作用；checkinHours 同样不可靠。
    AC=true
    HOURS="9,21"
    if [ -f "$SCHED_STATE" ]; then
        _ac=$(sed -n 's/^autoCheckin=//p' "$SCHED_STATE" 2>/dev/null | head -1 | tr -d ' \r')
        case "$_ac" in
            0|false|False|FALSE) AC=false ;;
            1|true|True|TRUE)    AC=true ;;
        esac
        _hrs=$(sed -n 's/^checkinHours=//p' "$SCHED_STATE" 2>/dev/null | head -1 | tr -d ' \r')
        [ -n "$_hrs" ] && HOURS="$_hrs"
    else
        # 兜底：刚升级上来、网关还没写过哨兵时退回读 JSON。
        # 必须用 [-E] 扩展正则，busybox sed 的 BRE 不认 \|。
        _ac=$(sed -nE 's/.*"autoCheckin"[[:space:]]*:[[:space:]]*(true|false).*/\1/p' "$RUNDATA/config.json" 2>/dev/null | head -1)
        case "$_ac" in
            false) AC=false ;;
        esac
        _hrs=$(sed -nE 's/.*"checkinHours"[[:space:]]*:[[:space:]]*\[([0-9, ]*)\].*/\1/p' "$RUNDATA/config.json" 2>/dev/null | tr -d ' ' | head -1)
        [ -n "$_hrs" ] && HOURS="$_hrs"
    fi

    IN_HOURS=false
    for hh in $(echo "$HOURS" | tr ',' ' '); do
        hh=$((hh+0))
        if [ "$hh" = "$((H+0))" ]; then IN_HOURS=true; break; fi
    done
    if [ "$AC" = "true" ] && [ "$IN_HOURS" = "true" ] && [ "$last" != "$slot" ]; then
        K=$(sed -n 's/.*"authKey"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$RUNDATA/config.json" 2>/dev/null | head -1)
        if [ -n "$K" ]; then
            RESP=$(curl -s -m 300 -X POST "$API" \
                -H "Authorization: Bearer $K" \
                -H 'Content-Type: application/json' -d '{}' 2>/dev/null)
            case "$RESP" in
                *'"ok":true'*)
                    log "workbuddy tasks slot=$slot ok $(summarize_tasks "$RESP")"
                    ;;
                *'"ok":false'*)
                    log "workbuddy tasks slot=$slot 有任务失败 $(summarize_tasks "$RESP")"
                    ;;
                *)
                    log "workbuddy tasks slot=$slot 返回异常: $(echo "$RESP" | cut -c1-200)"
                    ;;
            esac
            # 跑完立刻把网关日志搬进 gateway.log。
            # 否则 sync_back 的日志搬运要等 ticks%10（最多 10 分钟），
            # 用户跑完马上来看日志会看不到任何 [CHECKIN] 记录。
            flush_runlog
            sync_back
        else
            log "workbuddy tasks slot=$slot 跳过：配置里读不到 authKey"
        fi
        last="$slot"
    fi
done
