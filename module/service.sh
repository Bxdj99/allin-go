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

# 单实例保护：service.sh 每次执行都会起一个常驻守护循环，
# 反复执行（或管理器重复触发）会堆出多个循环互相重启网关。
# 用 pid 文件 + 存活检查挡住。
LOCK="$MODDIR/run/service.pid"
mkdir -p "$MODDIR/run" 2>/dev/null
if [ -f "$LOCK" ]; then
    OLD=$(cat "$LOCK" 2>/dev/null)
    if [ -n "$OLD" ] && [ -d "/proc/$OLD" ]; then
        # 已有实例在跑，直接退出
        exit 0
    fi
fi
echo $$ > "$LOCK"
# 退出时清掉 pid 文件
trap 'rm -f "$LOCK"' EXIT INT TERM
RUNDIR=/data/local/tmp/allin-run
MODDATA="$MODDIR/data"
RUNDATA="$RUNDIR/data"
LOG="$MODDIR/allin.log"
GWLOG="$MODDIR/gateway.log"
RUNLOG="$RUNDIR/gateway.log"
# 端口从配置读（首次安装会自动选一个空闲端口，避开 Magpie 等占用的 3425）
PORT=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' /data/local/tmp/allin-run/data/config.json 2>/dev/null | head -1)
[ -z "$PORT" ] && PORT=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$MODDIR/data/config.json" 2>/dev/null | head -1)
[ -z "$PORT" ] && PORT=6868
# 任务中心接口：POST 跑全套（活跃上报→签到→连登兑换→领礼包→领补偿→补签→
# 猫猫旅行→抽奖）；只读汇总在 $API_STATUS，出问题时先打它看现场。
API="http://127.0.0.1:$PORT/api/workbuddy/tasks"
API_STATUS="http://127.0.0.1:$PORT/api/workbuddy/tasks/status"
RUNAS=2000

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >> "$LOG"; }

port_up() {
    ss -tln 2>/dev/null | grep -q ":$PORT" && return 0
    netstat -tln 2>/dev/null | grep -q ":$PORT" && return 0
    return 1
}

prepare() {
    mkdir -p "$RUNDIR" "$RUNDATA" 2>/dev/null
    # 自检：模块里的脚本与二进制版本是否对得上。
    # 之前踩过：改了 action.sh 的端口读取逻辑但忘了复制到模块目录，
    # 结果「运行」键一直去连旧端口。
    VER_FROM_BIN=$(strings "$MODDIR/bin/allin" 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+-go' | head -1)
    VER_FROM_PROP=$(sed -n 's/^version=//p' "$MODDIR/module.prop" 2>/dev/null | tr -d ' ')
    if [ -n "$VER_FROM_BIN" ] && [ -n "$VER_FROM_PROP" ] && [ "$VER_FROM_BIN" != "$VER_FROM_PROP" ]; then
        log "警告：二进制版本($VER_FROM_BIN) 与 module.prop($VER_FROM_PROP) 不一致，可能是升级没刷全"
    fi
    if [ ! -f "$MODDIR/bin/allin" ]; then
        log "缺少二进制：$MODDIR/bin/allin"
        return 1
    fi
    # 在线更新留下的 allin.pending → 替换正式二进制（网关不能覆盖正在运行的自己）
    if [ -f "$MODDIR/bin/allin.pending" ]; then
        log "发现待应用的更新 allin.pending"
        OLDPID=$(ss -tlnp 2>/dev/null | grep ":$PORT" | grep -oE 'pid=[0-9]+' | cut -d= -f2 | head -1)
        [ -n "$OLDPID" ] && kill -9 "$OLDPID" 2>/dev/null && sleep 1
        mv -f "$MODDIR/bin/allin.pending" "$MODDIR/bin/allin" 2>/dev/null
        log "已应用更新"
    fi
    # 二进制有更新（md5 不同）才复制；复制前杀旧进程避免 Text file busy
    NEWMD5=$(md5sum "$MODDIR/bin/allin" 2>/dev/null | cut -d' ' -f1)
    OLDMD5=$(md5sum "$RUNDIR/allin" 2>/dev/null | cut -d' ' -f1)
    if [ "$NEWMD5" != "$OLDMD5" ]; then
        log "检测到二进制更新，同步"
        OLDPID=$(ss -tlnp 2>/dev/null | grep ":$PORT" | grep -oE 'pid=[0-9]+' | cut -d= -f2 | head -1)
        [ -n "$OLDPID" ] && kill -9 "$OLDPID" 2>/dev/null && sleep 1
        cp -f "$MODDIR/bin/allin" "$RUNDIR/allin" 2>/dev/null
        chown "$RUNAS:$RUNAS" "$RUNDIR/allin" 2>/dev/null
    fi
    chmod 755 "$RUNDIR/allin"

    mkdir -p "$MODDATA" 2>/dev/null
    if [ -f "$MODDATA/config.json" ] && { [ ! -f "$RUNDATA/config.json" ] || [ "$MODDATA/config.json" -nt "$RUNDATA/config.json" ]; }; then
        cp -f "$MODDATA/config.json" "$RUNDATA/config.json" 2>/dev/null
        log "配置已同步到运行目录"
    fi
    # 首次刷入时两边都没配置：网关启动时会自己生成（随机口令），
    # 启动成功后由下面的 sync_back 立刻回写到模块目录，保证「运行」键能读到。

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
    sync_back
else
    start_gateway
    n=0
    while [ $n -lt 20 ]; do
        port_up && { log "网关就绪 $PORT（非 root 运行）"; break; }
        sleep 2
        n=$((n+1))
    done
    [ $n -ge 20 ] && log "40 秒内未就绪，看 $RUNLOG"
    # 网关起来后立刻把生成的 config.json 回写到模块目录，
    # 否则「运行」键读不到口令（首次刷入的常见困惑）
    port_up && sync_back
fi

# ---- 常驻守护 + 每日签到 + 配置回写 ----
(
    last=""
    ticks=0
    while true; do
        sleep 30
        ticks=$((ticks+1))

        # 配置里的端口变了（用户从面板改的）→ 重启让新端口生效
        CFGPORT=$(sed -n 's/.*"port"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/p' "$RUNDATA/config.json" 2>/dev/null | head -1)
        if [ -n "$CFGPORT" ] && [ "$CFGPORT" != "$PORT" ]; then
            log "端口配置变更 $PORT → $CFGPORT，重启网关"
            OLDPID=$(ss -tlnp 2>/dev/null | grep ":$PORT" | grep -oE 'pid=[0-9]+' | cut -d= -f2 | head -1)
            [ -n "$OLDPID" ] && kill -9 "$OLDPID" 2>/dev/null
            PORT="$CFGPORT"
            sleep 1
            start_gateway
        fi

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
        # 读自动签到开关（面板可改，未配置时默认开启）
        # config 里是 "autoCheckin": false / true；grep 不到就按开启处理
        AC=$(sed -n 's/.*"autoCheckin"[[:space:]]*:[[:space:]]*\(true\|false\).*/\1/p' "$RUNDATA/config.json" 2>/dev/null | head -1)
        [ -z "$AC" ] && AC=true
        if [ "$AC" = "true" ] && { [ "$H" = "09" ] || [ "$H" = "21" ]; } && [ "$last" != "$slot" ]; then
            K=$(sed -n 's/.*"authKey"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$RUNDATA/config.json" 2>/dev/null | head -1)
            if [ -n "$K" ]; then
                # 全套任务：一次请求跑完 report/checkin/streak/gift/comp/travel/lottery。
                # 每账号最多 3 分钟，账号多时整体会久一点，故 -m 给到 300。
                # 返回体落一行摘要进日志，方便事后核对谁成功谁跳过。
                RESP=$(curl -s -m 300 -X POST "$API" \
                    -H "Authorization: Bearer $K" \
                    -H 'Content-Type: application/json' -d '{}' 2>/dev/null)
                case "$RESP" in
                    *'"ok"'*) log "workbuddy tasks slot=$slot ok" ;;
                    *)        log "workbuddy tasks slot=$slot 返回异常: $(echo "$RESP" | cut -c1-200)" ;;
                esac
                sync_back
            else
                log "workbuddy tasks slot=$slot 跳过：配置里读不到 authKey"
            fi
            last="$slot"
        fi
    done
) &

exit 0
