# switchyard-agent

把一台普通 Linux 主机接进 [Switchyard](https://github.com/xiao-si-qi/switchyard) 和 Home Assistant 的常驻代理。

> **Switchyard** 是铁路**编组站**——列车在那里被分类、编组、重新发车。
> 这个项目做的是同一件事：把家里各种设备统一登记、调度。
> **agent** 是设备侧代理：替这台主机在编组站里占一个车位。

一个静态二进制，无运行时依赖，支持 `amd64` / `arm64` / `armv7` / `armv6` / `loong64` / `riscv64`。

## 它做什么

| 动作 | 说明 |
|---|---|
| 上报状态 | IP（有线/无线）、信号、运行时长、CPU、内存、磁盘、可用内存、USB 插拔事件 |
| **自己注册 HA 实体** | 直接往 `homeassistant/...` 发 MQTT Discovery，**不经过 Switchyard 插件** |
| 提供重启按钮 | HA 里点一下 → 下发 `switchyard/<id>/ha/reboot/set` → 本机重启 |
| 远程排障 | 错误摘要 `diag`、按需回传日志 `log` |
| 接受配置 | 上报周期可通过 Switchyard 下发（retained `cfg`），带 `seq` 去重 |

**为什么实体由设备自己注册**：Switchyard 插件如果挂在实体注册路径上，它一挂新设备就不出现、升级带 bug 可能误改实体。设备自己发 discovery 后，插件只做台账和配置下发——它挂了完全不影响 HA 的实体和自动化。

```
本机 ──pub──▶ switchyard/<id>/state ──▶ HA（直接读）
                                   └─▶ Switchyard（只读，做台账）
```

## 一键安装

```sh
curl -fsSL https://raw.githubusercontent.com/xiao-si-qi/switchyard-agent/main/install.sh | sh
```

脚本会问 broker 地址、账号、设备名，然后自动完成：装二进制 → 写配置 → 注册并启动服务（systemd / OpenRC 自动识别）。

**无人值守**（批量部署）：

```sh
MQTT_HOST=ha.example.com MQTT_PORT=8883 MQTT_TLS=1 \
MQTT_USERNAME=user MQTT_PASSWORD=pass \
SY_DEVICE_NAME="客厅主机" \
sh install.sh
```

**离线机器 / 自己编译**：

```sh
git clone https://github.com/xiao-si-qi/switchyard-agent
cd switchyard-agent && make dist
SY_BIN=dist/switchyard-agent-linux-amd64 sh install.sh
```

其它覆盖项：

| 变量 | 默认 | 说明 |
|---|---|---|
| `SY_VERSION` | `latest` | 指定 release 版本 |
| `SY_PREFIX` | `/usr/local` | 安装前缀 |
| `SY_BIN` | — | 用本地二进制，跳过下载 |
| `SY_FORCE` | `0` | `1` = 覆盖已有配置 |
| `SY_NO_START` | `0` | `1` = 只装不启动 |
| `SY_MIRROR` | — | 下载加速前缀，如 `https://ghproxy.net/` |
| `SY_NO_MIRROR` | `0` | `1` = 只用直连，不试公共镜像 |

> **网络提示**：GitHub 的 release 二进制走 `objects.githubusercontent.com`，在国内经常下不动。
> 安装脚本会依次尝试「直连 → `SY_MIRROR` → 内置公共镜像（ghproxy.net / gh-proxy.com / ghfast.top）」，
> 单个源卡住 30 秒（低于 2KB/s）就自动换下一个。

## 配置

`/etc/switchyard/agent.conf`（权限 600，含密码），改完重启服务。

### 身份

| 变量 | 默认 | 说明 |
|---|---|---|
| `SY_DEVICE_ID` | 主机名规整 | MQTT 主题里那一段。小写，只含 `a-z 0-9 - _`，≤31 字符 |
| `SY_DEVICE_NAME` | `Linux 主机` | 台账和 HA 里显示的名字 |
| `SY_DEVICE_MODEL` | `Linux host` | HA 设备卡片的型号 |
| `SY_DEVICE_TYPE` | `host` | 设备类型 |
| `SY_INTERVAL` | `60` | 上报周期（秒），之后可被下发覆盖 |

### MQTT

| 变量 | 默认 | 说明 |
|---|---|---|
| `MQTT_HOST` | `localhost` | broker 地址 |
| `MQTT_PORT` | `1883` | broker 端口 |
| `MQTT_TLS` | `0` | `1` = TLS（`ssl://`） |
| `MQTT_CA_FILE` | 空 | 自定义 CA；留空用系统根证书 |
| `MQTT_INSECURE` | `0` | `1` = 跳过证书校验，**仅调试** |
| `MQTT_SERVER_NAME` | `MQTT_HOST` | 覆盖 SNI / 校验主机名 |
| `MQTT_USERNAME` / `MQTT_PASSWORD` | 空 | broker 凭据 |

> ⚠ **broker 在公网时必须开 TLS。** 明文 MQTT 会把用户名密码、系统状态、重启命令全部暴露。

### 可移植性

| 变量 | 默认 | 说明 |
|---|---|---|
| `SY_STATE_DIR` | `/var/lib/switchyard-agent` | seq / interval 持久化目录 |
| `SY_LOG_FILE` | `/var/log/switchyard-agent.log` | 日志文件（`get log` 回传的就是它） |
| `SY_REBOOT_CMD` | 自动探测 | `systemctl reboot` 或 `/sbin/reboot` |
| `SY_ETH_PREFIX` | `eth en` | 有线网卡名前缀（空格分隔） |
| `SY_WLAN_PREFIX` | `wlan wl` | 无线网卡名前缀 |
| `SY_USB_DETECT` | `1` | 可移动存储插拔探测，容器里设 `0` |

## HA 里会出现的实体

设备自己注册，不经过插件：

| 实体 | 类型 | 说明 |
|---|---|---|
| `sensor.*_fw` | sensor | 内核版本 |
| `sensor.*_ip_eth` / `*_ip_wlan` | sensor | 两个网卡的 IPv4 |
| `sensor.*_rssi` | sensor | WiFi 信号（有线设备为空） |
| `sensor.*_uptime` | sensor | 运行时长 |
| `sensor.*_cpu_load` | sensor | CPU 占用 % |
| `sensor.*_mem_used_pct` | sensor | 内存占用 % |
| `sensor.*_disk_used_pct` / `*_disk_free_mb` | sensor | 根分区占用 / 剩余 |
| `sensor.*_heap` | sensor | 可用内存 |
| `binary_sensor.*_online` | binary_sensor | 读 `availability`，LWT 维护 |
| `button.*_reboot` | button | 点一下重启本机 |

指标都归到 HA 设备的「诊断」分区，主界面只留有用的。

## 服务管理

```sh
# systemd
systemctl status switchyard-agent
tail -f /var/log/switchyard-agent.log
systemctl restart switchyard-agent

# OpenRC (Alpine)
rc-service switchyard-agent status
tail -f /var/log/switchyard-agent.log
rc-service switchyard-agent restart
```

## 从源码构建

```sh
make build     # 本机架构
make dist      # 全部架构 → dist/
make vet fmt   # 检查 / 格式化
```

需要 Go 1.24+。产物是静态二进制（`CGO_ENABLED=0`），拷到任何同架构的 Linux 都能直接跑。

## 卸载

```sh
curl -fsSL https://raw.githubusercontent.com/xiao-si-qi/switchyard-agent/main/uninstall.sh | sh
SY_PURGE=1 sh uninstall.sh    # 连配置、状态、日志一起删
```

## 协议

主题和 JSON 字段见 [Switchyard 的协议文档](https://github.com/xiao-si-qi/switchyard/blob/main/docs/PROTOCOL.md)。

```
上行  switchyard/<id>/state         状态快照（retained）
上行  switchyard/<id>/meta          设备声明：caps / metrics / ha_entities（retained）
上行  switchyard/<id>/availability  online + LWT offline
上行  switchyard/<id>/event         事件（非 retained）
上行  switchyard/<id>/diag          错误摘要（retained，1 条/分钟）
上行  switchyard/<id>/log           按需日志（非 retained）
上行  switchyard/<id>/ack           命令回执
下行  switchyard/<id>/cmd           插件独有命令（JSON + seq）
下行  switchyard/<id>/cfg           配置下发（retained）
下行  switchyard/<id>/ha/<obj>/set  HA 控制（纯值，如 PRESS）
上行  homeassistant/<comp>/switchyard_<id>/<obj>/config   自家的 HA 实体声明
```

## 许可

MIT，见 [LICENSE](LICENSE)。
