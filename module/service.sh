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
# 自动任务哨兵：网关每次保存配置时写的单行 key=value，供下面的守护循环读取。
SCHED_STATE="$RUNDATA/schedule.state"
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
    # 上次在线更新留下的脚本，先落地（不能在运行中覆盖自己）
    if [ -f "$MODDIR/service.sh.pending" ]; then
        cp -f "$MODDIR/service.sh.pending" "$MODDIR/service.sh" 2>/dev/null &&
            chmod 755 "$MODDIR/service.sh" 2>/dev/null &&
            rm -f "$MODDIR/service.sh.pending" 2>/dev/null &&
            log "已应用新的 service.sh"
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
    # 运行目录里没有哨兵（首次升级 / 被清掉）但模块目录有 → 补回去
    if [ ! -f "$SCHED_STATE" ] && [ -f "$MODDATA/schedule.state" ]; then
        cp -f "$MODDATA/schedule.state" "$SCHED_STATE" 2>/dev/null &&
            chown "$RUNAS:$RUNAS" "$SCHED_STATE" 2>/dev/null
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
    # 脚本类文件覆盖。
    #
    # service.sh 例外：本进程正在执行它，覆盖会让「正在跑的旧逻辑」
    # 和「磁盘上的新脚本」不一致，下次开机行为才切换，容易让人以为更新没生效。
    # 所以先写成 .pending，下次启动时由下面统一替换。
    if [ -f "$WORK/service.sh" ]; then
        cp -f "$WORK/service.sh" "$MODDIR/service.sh.pending" 2>/dev/null &&
            chmod 755 "$MODDIR/service.sh.pending" 2>/dev/null &&
            log "已放置新 service.sh（pending，下次启动替换）"
    fi
    for f in action.sh uninstall.sh; do
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
    # 哨兵也回写一份到模块目录，便于重启后即使运行目录被清掉也能恢复
    if [ -f "$SCHED_STATE" ]; then
        cp -f "$SCHED_STATE" "$MODDATA/schedule.state" 2>/dev/null
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
    # 按账号统计签到结果。
    #
    # 之前把所有账号的任务状态混成一行，结果只剩最后一个账号的，
    # 7 个成功 4 个失败会显示成「全是 failed」，看着像没签到。
    # 现在分开数：新签=本次签到成功 已签=今天已签过 失败=失败
    #
    # 注意：上游返回的 JSON 带空格（"name": "report"），
    # 正则必须允许冒号后有空格，否则一条都匹配不到。
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
#
# 逻辑全部搬到了同目录的 guard.sh，原因见该文件头部注释：
# 原来这里是一个匿名后台子 shell，父脚本 exit 之后靠 init 收养，
# 没有会话保护，被系统回收时会静默消失 —— 而网关进程还在跑，
# 于是「网关看起来正常、自动签到就是不执行」。
# 线上真实发生过：守护循环消失后，第二天 9 点的签到窗口没人触发。
#
# 现在 guard.sh 独立运行、自己写 pid 文件、由 watchdog.sh 兜底重启。
if [ -f "$MODDIR/guard.sh" ]; then
    setsid sh "$MODDIR/guard.sh" > /dev/null 2>&1 < /dev/null &
else
    log "缺少 guard.sh，自动任务不会执行（请重刷模块）"
fi

# 看门狗：守护循环万一还是没了，5 分钟内把它拉回来。
if [ -f "$MODDIR/watchdog.sh" ]; then
    setsid sh "$MODDIR/watchdog.sh" > /dev/null 2>&1 < /dev/null &
fi

exit 0
