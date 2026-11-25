#!/bin/sh
# Switchyard Agent —— 一键安装 / 升级脚本
#
# 最简用法（交互式，会问 broker 地址和账号）：
#   curl -fsSL https://raw.githubusercontent.com/xiao-si-qi/switchyard-agent/main/install.sh | sh
#
# 无人值守（全用环境变量，适合批量部署）：
#   MQTT_HOST=ha.example.com MQTT_PORT=8883 MQTT_TLS=1 \
#   MQTT_USERNAME=u MQTT_PASSWORD=p SY_DEVICE_NAME="客厅主机" \
#   sh install.sh
#
# 用本地已编译好的二进制（离线机器 / 自建）：
#   SY_BIN=./switchyard-agent-linux-amd64 sh install.sh
#
# 其它可覆盖项：
#   SY_VERSION  指定版本（如 0.1.0；默认 latest）
#   SY_PREFIX   安装前缀，默认 /usr/local（二进制装到 $SY_PREFIX/bin）
#   SY_FORCE    1 = 覆盖已存在的 /etc/switchyard/agent.conf
#   SY_NO_START 1 = 只装不启动
#
# 支持：systemd（Debian/Ubuntu/CentOS/Arch…）与 OpenRC（Alpine）

set -eu

REPO="${SY_REPO:-xiao-si-qi/switchyard-agent}"
PREFIX="${SY_PREFIX:-/usr/local}"
BIN_DIR="$PREFIX/bin"
BIN="$BIN_DIR/switchyard-agent"
CONF_DIR="/etc/switchyard"
CONF="$CONF_DIR/agent.conf"
LOG_FILE="${SY_LOG_FILE:-/var/log/switchyard-agent.log}"
STATE_DIR="${SY_STATE_DIR:-/var/lib/switchyard-agent}"
VERSION="${SY_VERSION:-latest}"

# ---------------------------------------------------------------- 输出
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
    C_OK="\033[32m"; C_WARN="\033[33m"; C_ERR="\033[31m"; C_DIM="\033[2m"; C_END="\033[0m"
else
    C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_END=""
fi
info() { printf "${C_OK}==>${C_END} %s\n" "$*"; }
warn() { printf "${C_WARN}警告:${C_END} %s\n" "$*"; }
die()  { printf "${C_ERR}错误:${C_END} %s\n" "$*" >&2; exit 1; }
dim()  { printf "${C_DIM}%s${C_END}\n" "$*"; }

# ---------------------------------------------------------------- 环境检查
if [ "$(id -u)" -ne 0 ]; then
    cat >&2 <<'EOF'
错误: 需要 root 权限（要写 /etc、/usr/local、/var/log）

注意 sudo 只作用于它后面那一条命令，管道右边不会继承 ——
所以 `sudo curl … | sh` 里 sh 仍然是普通用户，必然失败。

正确写法（任选其一）：
  curl -fsSL <url> | sudo sh
  curl -fsSL <url> -o /tmp/sy-install.sh && sudo sh /tmp/sy-install.sh
  sudo sh -c 'curl -fsSL <url> | sh'
EOF
    exit 1
fi

have() { command -v "$1" >/dev/null 2>&1; }

# 交互读取：从 /dev/tty 读，避免 `curl | sh` 时 stdin 是脚本本身
ask() {
    prompt="$1"; default="$2"; var="$3"
    eval "cur=\${$var:-}"
    [ -n "$cur" ] && return 0
    if [ -r /dev/tty ]; then
        if [ -n "$default" ]; then
            printf "%s [%s]: " "$prompt" "$default" >/dev/tty
        else
            printf "%s: " "$prompt" >/dev/tty
        fi
        IFS= read -r ans < /dev/tty || ans=""
        [ -z "$ans" ] && ans="$default"
        eval "$var=\$ans"
    else
        eval "$var=\$default"
    fi
}

ask_secret() {
    prompt="$1"; var="$2"
    eval "cur=\${$var:-}"
    [ -n "$cur" ] && return 0
    if [ -r /dev/tty ]; then
        printf "%s: " "$prompt" >/dev/tty
        stty -echo < /dev/tty 2>/dev/null || true
        IFS= read -r ans < /dev/tty || ans=""
        stty echo < /dev/tty 2>/dev/null || true
        printf "\n" >/dev/tty
        eval "$var=\$ans"
    fi
}

# ---------------------------------------------------------------- 判断架构
detect_arch() {
    m="$(uname -m)"
    case "$m" in
        x86_64|amd64)      echo amd64 ;;
        aarch64|arm64)     echo arm64 ;;
        armv7l|armv7|armhf) echo arm ;;
        armv6l|armv6)      echo armv6 ;;
        loongarch64|loong64) echo loong64 ;;
        riscv64)           echo riscv64 ;;
        *) die "不支持的架构: $m（可以自己编译后用 SY_BIN 指定）" ;;
    esac
}

# ---------------------------------------------------------------- 判断 init
detect_init() {
    if [ -d /run/systemd/system ] && have systemctl; then
        echo systemd
    elif have rc-update && have rc-service; then
        echo openrc
    else
        echo none
    fi
}

# ---------------------------------------------------------------- 下载
# 候选源顺序：直连 → SY_MIRROR → 内置公共镜像
candidates() {
    url="$1"
    printf '%s\n' "$url"
    [ -n "${SY_MIRROR:-}" ] && printf '%s\n' "${SY_MIRROR%/}/$url"
    if [ "${SY_NO_MIRROR:-0}" != "1" ]; then
        for m in https://ghproxy.net https://gh-proxy.com https://ghfast.top; do
            printf '%s\n' "$m/$url"
        done
    fi
    return 0
}

# 下载前先探测：每个源只取 1KB、最多 6 秒，选响应最快的那个。
# 这样不会在"能连但几乎不动"的直连上白等好几分钟。
# 输出 "耗时 URL"；没有可用源时输出空。
pick_source() {
    url="$1"
    best=""; best_t=""
    for u in $(candidates "$url"); do
        r=$(curl -sL --http1.1 --max-time 6 --range 0-1023 -o /dev/null \
             -w '%{http_code} %{time_total}' "$u" 2>/dev/null) || continue
        code=${r%% *}
        t=${r##* }
        case "$code" in
            200|206) ;;
            *) continue ;;
        esac
        if [ -z "$best_t" ] || awk "BEGIN{exit !($t < $best_t)}"; then
            best="$u"; best_t="$t"
        fi
    done
    [ -n "$best" ] && printf '%s %s' "$best_t" "$best"
}

# 单个 URL 取文件。带"低速自动放弃"：30 秒内低于 2KB/s 就当它死了。
fetch() {
    url="$1"; out="$2"
    if have curl; then
        # --http1.1：部分加速镜像的 HTTP/2 会半路断开
        # （curl: (92) HTTP/2 stream not closed cleanly），降级更稳
        curl -fsSL --http1.1 --connect-timeout 10 --max-time 600 \
             --speed-limit 2048 --speed-time 30 \
             -o "$out" "$url"
    elif have wget; then
        wget -q -O "$out" --timeout=30 --tries=1 "$url"
    else
        die "需要 curl 或 wget 来下载二进制"
    fi
}

download() {
    url="$1"; out="$2"

    # 先探测挑源（wget-only 环境没有 --range，跳过直接顺序试）
    if have curl; then
        sel=$(pick_source "$url")
        if [ -n "$sel" ]; then
            st=${sel%% *}
            su=${sel#* }
            dim "  测速后选用：$(printf '%s' "$su" | cut -c1-68)…（${st}s）"
            if fetch "$su" "$out" && [ -s "$out" ]; then
                return 0
            fi
            rm -f "$out"
            warn "首选源下载失败，逐个回退"
        else
            warn "所有源探测都不通，尝试顺序下载"
        fi
    fi

    i=0
    for u in $(candidates "$url"); do
        i=$((i + 1))
        dim "  尝试 #$i：$(printf '%s' "$u" | cut -c1-68)…"
        if fetch "$u" "$out" && [ -s "$out" ]; then
            return 0
        fi
        rm -f "$out"
    done
    return 1
}

# ---------------------------------------------------------------- 主流程
ARCH="$(detect_arch)"
INIT="$(detect_init)"

info "架构 linux/$ARCH，init: $INIT，版本: $VERSION"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

if [ -n "${SY_BIN:-}" ]; then
    [ -f "$SY_BIN" ] || die "SY_BIN 指向的文件不存在: $SY_BIN"
    info "使用本地二进制：$SY_BIN"
    cp "$SY_BIN" "$TMP/switchyard-agent"
else
    if [ "$VERSION" = "latest" ]; then
        URL="https://github.com/$REPO/releases/latest/download/switchyard-agent-linux-$ARCH"
    else
        URL="https://github.com/$REPO/releases/download/v$VERSION/switchyard-agent-linux-$ARCH"
    fi
    info "下载 $URL"
    download "$URL" "$TMP/switchyard-agent" || die "下载失败：所有源都拿不到二进制。
  解决办法（任选其一）：
    1) 指定镜像：SY_MIRROR=https://ghproxy.net/ sh install.sh
    2) 在能上网的机器上编译后，用 SY_BIN=<文件> 重跑本脚本
    3) git clone 后执行 make dist，再用 SY_BIN 指定产物"
fi

chmod 0755 "$TMP/switchyard-agent"

# ---- 安装二进制（先写临时名再 mv，避免覆盖正在运行的二进制失败）----
mkdir -p "$BIN_DIR"
install -m 0755 "$TMP/switchyard-agent" "$BIN.new"
mv -f "$BIN.new" "$BIN"
info "已安装 $BIN"

# ---- 目录 ----
mkdir -p "$CONF_DIR" "$STATE_DIR"
chmod 0755 "$CONF_DIR" "$STATE_DIR"
touch "$LOG_FILE" && chmod 0644 "$LOG_FILE"

# ---- 配置文件 ----
if [ -f "$CONF" ] && [ "${SY_FORCE:-0}" != "1" ]; then
    info "保留已有配置 $CONF（要覆盖请 SY_FORCE=1）"
else
    info "生成配置 $CONF"
    ask "MQTT broker 地址" "ha.example.com" MQTT_HOST
    ask "MQTT 端口（公网请用 8883 + TLS）" "1883" MQTT_PORT
    ask "启用 TLS？（1=是 0=否）" "$([ "$MQTT_PORT" = "8883" ] && echo 1 || echo 0)" MQTT_TLS
    ask "MQTT 用户名" "" MQTT_USERNAME
    ask_secret "MQTT 密码" MQTT_PASSWORD
    hostname_default="$(hostname 2>/dev/null | tr 'A-Z' 'a-z' | tr -cs 'a-z0-9_-' '-' | sed 's/^-*//;s/-*$//' | cut -c1-31)"
    ask "设备 ID（台账里的标识，小写）" "${hostname_default:-linux-host}" SY_DEVICE_ID
    ask "设备显示名" "$(hostname 2>/dev/null || echo Linux 主机)" SY_DEVICE_NAME

    umask 077
    cat > "$CONF" <<EOF
# Switchyard Agent 配置（由 install.sh 生成）
# 改完执行：$( [ "$INIT" = systemd ] && echo "systemctl restart switchyard-agent" || echo "rc-service switchyard-agent restart" )

MQTT_HOST=$MQTT_HOST
MQTT_PORT=$MQTT_PORT
MQTT_TLS=$MQTT_TLS
MQTT_USERNAME=$MQTT_USERNAME
MQTT_PASSWORD=$MQTT_PASSWORD

SY_DEVICE_ID=$SY_DEVICE_ID
SY_DEVICE_NAME="$SY_DEVICE_NAME"
SY_INTERVAL=${SY_INTERVAL:-60}
EOF
    umask 022
    chmod 600 "$CONF"
fi

# ---- 服务 ----
case "$INIT" in
systemd)
    info "安装 systemd 单元"
    cat > /etc/systemd/system/switchyard-agent.service <<EOF
[Unit]
Description=Switchyard device agent
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$CONF
ExecStart=/bin/sh -c 'exec $BIN >> $LOG_FILE 2>&1'
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable switchyard-agent >/dev/null 2>&1 || true
    if [ "${SY_NO_START:-0}" = "1" ]; then
        dim "已跳过启动（SY_NO_START=1），手动启动：systemctl start switchyard-agent"
    else
        systemctl restart switchyard-agent
        sleep 2
        systemctl is-active --quiet switchyard-agent && info "服务已启动" || warn "服务未启动，查看：journalctl -u switchyard-agent -n 50"
    fi
    ;;
openrc)
    info "安装 OpenRC 服务"
    cat > /etc/init.d/switchyard-agent <<EOF
#!/sbin/openrc-run
description="Switchyard device agent"
command="$BIN"
command_background=true
command_user="root"
pidfile="/run/switchyard-agent.pid"
output_log="$LOG_FILE"
error_log="$LOG_FILE"

depend() {
    need net
    after firewall
}

start_pre() {
    mkdir -p /run /var/log "$STATE_DIR"
    if [ -f "$CONF" ]; then
        set -a
        . "$CONF"
        set +a
    fi
}
EOF
    chmod 0755 /etc/init.d/switchyard-agent
    rc-update add switchyard-agent default >/dev/null 2>&1 || true
    if [ "${SY_NO_START:-0}" = "1" ]; then
        dim "已跳过启动（SY_NO_START=1），手动启动：rc-service switchyard-agent start"
    else
        rc-service switchyard-agent restart >/dev/null 2>&1 || rc-service switchyard-agent start >/dev/null 2>&1 || true
        sleep 2
        rc-service switchyard-agent status >/dev/null 2>&1 && info "服务已启动" || warn "服务未启动，查看：tail -50 $LOG_FILE"
    fi
    ;;
none)
    warn "没有检测到 systemd 或 OpenRC，二进制已安装但未注册服务。"
    dim "可以手动前台运行试试："
    dim "  set -a; . $CONF; set +a; $BIN"
    ;;
esac

# ---- 结果 ----
printf "\n"
info "安装完成"
dim "  二进制 : $BIN"
dim "  配置   : $CONF"
dim "  日志   : $LOG_FILE"
dim "  状态   : $STATE_DIR"
printf "\n"
dim "常用命令："
if [ "$INIT" = systemd ]; then
    dim "  查看状态  systemctl status switchyard-agent"
    dim "  查看日志  tail -f $LOG_FILE"
    dim "  改配置后  systemctl restart switchyard-agent"
elif [ "$INIT" = openrc ]; then
    dim "  查看状态  rc-service switchyard-agent status"
    dim "  查看日志  tail -f $LOG_FILE"
    dim "  改配置后  rc-service switchyard-agent restart"
fi
printf "\n"
dim "接入 HA 后会出现这些实体（由本机自己注册，不经过 Switchyard 插件）："
dim "  固件 / 有线 IP / 无线 IP / 信号 / 运行时长 / CPU / 内存 / 存储 / 可用内存"
dim "  在线状态 + 重启按钮"
printf "\n"
dim "卸载：curl -fsSL https://raw.githubusercontent.com/$REPO/main/uninstall.sh | sh"
