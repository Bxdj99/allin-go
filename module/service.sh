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
# STAGE 是「网关 → root」的交接目录：网关往里放 zip，本脚本以 root 消费。
STAGE="$RUNDIR/stage"
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
# busybox 路径在探测函数里定，这里先占位
BUSYBOX=""

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
    # 在线更新：网关以 shell(2000) 运行，写不了 $MODDIR（root 所有）。
    # 它把更新包放到 $STAGE（shell 可写），由这里以 root 身份应用。
    apply_staged_update

    # 在线更新留下的 allin.pending → 替换正式二进制（网关不能覆盖正在运行的自己）
    if [ -f "$MODDIR/bin/allin.pending" ]; then
        log "发现待应用的更新 allin.pending"
        OLDPID=$(ss -tlnp 2>/dev/null | grep ":$PORT" | grep -oE 'pid=[0-9]+' | cut -d= -f2 | head -1)
        [ -n "$OLDPID" ] && kill -9 "$OLDPID" 2>/dev/null && sleep 1
        mv -f "$MODDIR/bin/allin.pending" "$MODDIR/bin/allin" 2>/dev/null
        log "已应用更新"
    fi
    # 二进制有更新（md5 不同）才复制；复制前杀旧进程避免 Text file busy
    NEWMD5=$(file_sum "$MODDIR/bin/allin")
    OLDMD5=$(file_sum "$RUNDIR/allin")
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

# 计算文件校验值。优先 md5sum，其次 busybox md5sum，都没有就退回大小+时间戳。
# 目的只是判断「二进制有没有变」，不需要密码学强度。
file_sum() {
    if [ ! -f "$1" ]; then
        echo "missing"
        return
    fi
    if command -v md5sum >/dev/null 2>&1; then
        md5sum "$1" 2>/dev/null | cut -d' ' -f1
        return
    fi
    if [ -n "$BUSYBOX" ]; then
        "$BUSYBOX" md5sum "$1" 2>/dev/null | cut -d' ' -f1
        return
    fi
    # 兜底：大小 + 修改时间
    set -- $(wc -c < "$1") $(date -r "$1" +%s 2>/dev/null || echo 0)
    echo "$1-$2"
}

# 定位 busybox。
#
# 不能直接写 `busybox`：不同 root 方案装的路径不同，PATH 里也未必有。
# 见过 Magisk / KernelSU / APatch 各自的目录，也见过系统自带 /system/bin 的。
# apply_staged_update 应用网关放下的在线更新包。
#
# 为什么要这个交接目录：网关以 uid 2000 运行，$MODDIR 属 root、权限 755，
# 网关根本写不进去。以前在线更新直接往 $MODDIR 写，必然 permission denied。
# 现在网关只下载到 $STAGE，这里以 root 解压覆盖，保留 data/ 与日志。
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

    # 校验：必须是我们自己的模块包
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

    # 备份 module.prop 以便失败回滚
    cp -f "$MODDIR/module.prop" "$STAGE/module.prop.bak" 2>/dev/null

    # 二进制写成 .pending，交给下面统一的替换逻辑（不覆盖正在运行的自己）
    if [ -f "$WORK/bin/allin" ]; then
        cp -f "$WORK/bin/allin" "$MODDIR/bin/allin.pending" 2>/dev/null &&
            chmod 755 "$MODDIR/bin/allin.pending" 2>/dev/null &&
            log "已放置新二进制（pending）"
    fi
    if [ -f "$WORK/module.prop" ]; then
        cp -f "$WORK/module.prop" "$MODDIR/module.prop" 2>/dev/null
    fi
    # 脚本类文件直接覆盖（不是运行中的可执行文件，可以直接替换）
    for f in service.sh action.sh uninstall.sh; do
        [ -f "$WORK/$f" ] && cp -f "$WORK/$f" "$MODDIR/$f" 2>/dev/null && chmod 755 "$MODDIR/$f" 2>/dev/null
    done

    log "在线更新包已应用"
    rm -rf "$ZIP" "$WORK"
}

find_busybox() {
    for c in \
        /data/adb/magisk/busybox \
        /data/adb/ksu/bin/busybox \
        /data/adb/ap/bin/busybox \
        /data/adb/modules/busybox-ndk/system/bin/busybox \
        /system/xbin/busybox \
        /system/bin/busybox \
        /vendor/bin/busybox
    do
        [ -x "$c" ] && { echo "$c"; return 0; }
    done
    # 最后退回 PATH 查找
    if command -v busybox >/dev/null 2>&1; then
        command -v busybox
        return 0
    fi
    return 1
}

# 以指定 uid 启动某个命令，尽量保持「降权运行」。
#
# 优先级：
#   1. busybox setuidgid（最通用）
#   2. su/setpriv 之类系统自带（少见）
#   3. 都没有就 warn 并从 root 启动 —— 可用性优先于权限隔离
start_as_uid() {
    uid="$1"; shift
    # setsid 也可能没有：没有就直接后台启动，
    # 好处是任何设备都能起来，代价是进程不脱离父会话。
    launcher=""
    command -v setsid >/dev/null 2>&1 && launcher="setsid"

    if [ -n "$BUSYBOX" ]; then
        $launcher "$BUSYBOX" setuidgid "$uid" "$@" >> "$RUNLOG" 2>&1 < /dev/null &
        return 0
    fi
    log "警告：找不到 busybox，无法降权到 uid=$uid，改为 root 启动（功能可用，但权限隔离失效）"
    $launcher "$@" >> "$RUNLOG" 2>&1 < /dev/null &
    return 0
}

start_gateway() {
    prepare || return 1
    log "启动网关（uid=$RUNAS）"
    start_as_uid "$RUNAS" "$RUNDIR/allin" -config "$RUNDATA/config.json"
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

# 从 /api/workbuddy/tasks 的返回里提炼「跑了哪些任务」的摘要。
#
# 不用 jq（很多设备没有），用 sed 粗暴提取任务名，够日志核对用。
# 输出形如：acc=11 report:ok checkin:already streak:locked ...
summarize_tasks() {
    RESP="$1"
    # 账号数
    n=$(echo "$RESP" | grep -o '"accountId"' | wc -l)
    # 任务名与状态配对。
    #
    # 不能用 sed 的 s///g：整份 JSON 是一行，gsub 会把所有匹配挤成一条，
    # 只剩最后一个任务。改成先用 grep -o 把所有片段切出来，再整理。
    PAIRS=$(echo "$RESP" | grep -o '"name":"[a-z-]*","outcome":{"status":"[a-z]*"' 2>/dev/null \
        | sed 's/"name":"\([a-z-]*\)","outcome":{"status":"\([a-z]*\)"/\1:\2/' \
        | head -10 | tr '\n' ' ')
    echo "acc=$n $PAIRS"
}

# 探测 busybox（找不到也不致命，会退化为 root 启动）
BUSYBOX=$(find_busybox)
if [ -n "$BUSYBOX" ]; then
    log "使用 busybox: $BUSYBOX"
else
    log "未找到 busybox，将以 root 身份启动网关"
fi

# ---- 等系统起来 ----
i=0
# getprop 也可能不存在：拿不到就别死等，直接往下走
if command -v getprop >/dev/null 2>&1; then
    while [ "$(getprop sys.boot_completed)" != "1" ] && [ $i -lt 150 ]; do
        sleep 2
        i=$((i+1))
    done
else
    log "未找到 getprop，跳过开机等待"
fi
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

        # 读自动任务开关。
        #
        # 字段在 schedule.autoCheckin，不是顶层 —— 面板写的是 schedule 对象。
        # 以前读顶层，永远读不到，于是无论面板怎么关都按「开启」处理。
        AC=$(sed -n 's/.*"autoCheckin"[[:space:]]*:[[:space:]]*\(true\|false\).*/\1/p' "$RUNDATA/config.json" 2>/dev/null | head -1)
        [ -z "$AC" ] && AC=true

        # 执行小时同样从 schedule.checkinHours 读，默认 9 与 21。
        # 面板上改过时间的话，这里要跟着变，否则改了没用。
        HOURS=$(sed -n 's/.*"checkinHours"[[:space:]]*:[[:space:]]*\[\([0-9, ]*\)\].*/\1/p' "$RUNDATA/config.json" 2>/dev/null | tr -d ' ' | head -1)
        [ -z "$HOURS" ] && HOURS="9,21"

        # 判断当前小时是否在计划内
        IN_HOURS=false
        for hh in $(echo "$HOURS" | tr ',' ' '); do
            # 去掉前导零再比，避免 08 与 8 不等
            hh=$((hh+0))
            if [ "$hh" = "$((H+0))" ]; then IN_HOURS=true; break; fi
        done
        if [ "$AC" = "true" ] && [ "$IN_HOURS" = "true" ] && [ "$last" != "$slot" ]; then
            K=$(sed -n 's/.*"authKey"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$RUNDATA/config.json" 2>/dev/null | head -1)
            if [ -n "$K" ]; then
                # 全套任务：一次请求跑完 report/checkin/streak/gift/comp/travel/lottery。
                # 每账号最多 3 分钟，账号多时整体会久一点，故 -m 给到 300。
                # 返回体落一行摘要进日志，方便事后核对谁成功谁跳过。
                RESP=$(curl -s -m 300 -X POST "$API" \
                    -H "Authorization: Bearer $K" \
                    -H 'Content-Type: application/json' -d '{}' 2>/dev/null)
                # 只记 ok 看不出跑了哪些任务 —— 用户反馈「自动任务好像不是全套」
                # 时无从核对。这里把每账号的任务名与结果都落一行摘要。
                case "$RESP" in
                    *'"ok"'*)
                        log "workbuddy tasks slot=$slot ok $(summarize_tasks "$RESP")"
                        ;;
                    *)
                        log "workbuddy tasks slot=$slot 返回异常: $(echo "$RESP" | cut -c1-200)"
                        ;;
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
