#!/system/bin/sh
# shellcheck shell=ash
# 卸载时保留配置（data/ 里有账号与密钥），只清日志。
MODDIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd)
rm -f "$MODDIR/allin.log" "$MODDIR/gateway.log"
exit 0
