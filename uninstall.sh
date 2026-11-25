#!/bin/sh
# Switchyard Agent —— 卸载脚本
#
#   curl -fsSL https://raw.githubusercontent.com/xiao-si-qi/switchyard-agent/main/uninstall.sh | sh
#
# 默认保留配置和日志，方便重装。全删：
#   SY_PURGE=1 sh uninstall.sh

set -eu

BIN="${SY_PREFIX:-/usr/local}/bin/switchyard-agent"
CONF_DIR="/etc/switchyard"
STATE_DIR="${SY_STATE_DIR:-/var/lib/switchyard-agent}"
LOG_FILE="${SY_LOG_FILE:-/var/log/switchyard-agent.log}"

[ "$(id -u)" -eq 0 ] || { echo "需要 root 权限" >&2; exit 1; }

info() { printf "==> %s\n" "$*"; }

# ---- 停服务 ----
if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    info "停止并禁用 systemd 服务"
    systemctl stop switchyard-agent 2>/dev/null || true
    systemctl disable switchyard-agent 2>/dev/null || true
    rm -f /etc/systemd/system/switchyard-agent.service
    systemctl daemon-reload 2>/dev/null || true
elif command -v rc-service >/dev/null 2>&1; then
    info "停止并移除 OpenRC 服务"
    rc-service switchyard-agent stop 2>/dev/null || true
    rc-update del switchyard-agent default 2>/dev/null || true
    rm -f /etc/init.d/switchyard-agent
fi

# ---- 删二进制 ----
if [ -e "$BIN" ]; then
    info "删除 $BIN"
    rm -f "$BIN"
fi

# ---- 可选清理 ----
if [ "${SY_PURGE:-0}" = "1" ]; then
    info "清理配置、状态和日志（SY_PURGE=1）"
    rm -rf "$CONF_DIR" "$STATE_DIR" "$LOG_FILE"
else
    info "保留配置 $CONF_DIR、状态 $STATE_DIR、日志 $LOG_FILE"
    printf "    要全部删除请重跑：SY_PURGE=1 sh uninstall.sh\n"
fi

info "卸载完成"
printf "    HA 里的实体由设备自己注册，设备不再上线后会显示为不可用。\n"
printf "    彻底移除实体：在 Switchyard 面板里「删除设备」，或去 HA 的 MQTT 集成里删。\n"
