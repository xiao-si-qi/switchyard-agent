package main

// 全部配置来自环境变量 —— 同一份二进制在龙芯板子、PC、NAS、树莓派上都能跑，
// 不需要重新编译。可移植性相关的项（路径、网卡名、重启命令）也在这里。

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// defaultRebootCmd 挑一个当前系统能用的重启命令。
// systemd 优先（普通发行版），其次 /sbin/reboot（Alpine 等），最后裸 reboot。
func defaultRebootCmd() string {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return "systemctl reboot"
	}
	if _, err := os.Stat("/sbin/reboot"); err == nil {
		return "/sbin/reboot"
	}
	return "reboot"
}

// defaultDeviceID 用主机名兜底，并规整成合法 ID。
// 协议要求：小写、只含 a-z 0-9 - _，≤31 字符（会进 MQTT 主题和 HA object_id）。
func defaultDeviceID() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "linux-host"
	}
	h = strings.ToLower(h)
	var b strings.Builder
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == '.' || r == ' ':
			b.WriteRune('-')
		}
	}
	id := strings.Trim(b.String(), "-_")
	if id == "" {
		return "linux-host"
	}
	if len(id) > 31 {
		id = id[:31]
	}
	return id
}

var (
	// ---- 身份 ----
	deviceID   = env("SY_DEVICE_ID", defaultDeviceID())
	deviceName = env("SY_DEVICE_NAME", "Linux 主机")
	model      = env("SY_DEVICE_MODEL", "Linux host")
	devType    = env("SY_DEVICE_TYPE", "host")

	// caps：只放「必须由 Switchyard 操作、HA 做不到」的能力。
	// 主机类设备没有 OTA 双分区，重启也由 HA 的 button 直接下发，所以是空表。
	caps = []string{}

	// ---- MQTT ----
	host = env("MQTT_HOST", "localhost")
	port = envInt("MQTT_PORT", 1883)
	user = env("MQTT_USERNAME", "")
	pass = env("MQTT_PASSWORD", "")

	// ---- 路径（可移植性：不再是写死的 /var/lib）----
	stateDir = env("SY_STATE_DIR", "/var/lib/switchyard-agent")
	logFile  = env("SY_LOG_FILE", "/var/log/switchyard-agent.log")

	cfgFile = filepath.Join(stateDir, "cfg.json")
	seqFile = filepath.Join(stateDir, "seq.json")

	// ---- 平台相关 ----
	// 重启命令按空格切分，允许带参数（如 "systemctl reboot"）
	rebootArgs = strings.Fields(env("SY_REBOOT_CMD", defaultRebootCmd()))
	// 网卡名在不同发行版差别很大：eth0 / ens18 / enp3s0 / wlan0 / wlp2s0。
	// 用"前缀列表"匹配，空格分隔。
	ethPrefixes  = strings.Fields(env("SY_ETH_PREFIX", "eth en"))
	wlanPrefixes = strings.Fields(env("SY_WLAN_PREFIX", "wlan wl"))
	// U 盘探测：某些环境（容器、无 /sys）要关掉
	usbDetect = envBool("SY_USB_DETECT", true)

	// ---- 行为 ----
	intervalMin = 15
	intervalMax = 3600
	logTailN    = 40
	diagGap     = 60 * time.Second
	discoveryRefresh = 10 * time.Minute
)

// metrics 是只读指标清单。加一个指标 = 这里加一行 + buildState 里补字段。
var metrics = []Metric{
	{Key: "fw", Name: "固件", Icon: "mdi:chip"},
	{Key: "rssi", Name: "信号", Unit: "dBm", Icon: "mdi:wifi", DeviceClass: "signal_strength",
		Precision: iptr(0)},
	{Key: "uptime", Name: "运行时长", Unit: "s", Icon: "mdi:clock-outline",
		DeviceClass: "duration", StateClass: "total_increasing", Precision: iptr(0)},
	{Key: "ip_eth", Name: "有线 IP", Icon: "mdi:ethernet"},
	{Key: "ip_wlan", Name: "无线 IP", Icon: "mdi:wifi"},
	{Key: "cpu_load", Name: "CPU 占用", Unit: "%", Icon: "mdi:cpu-64-bit", Precision: iptr(1)},
	{Key: "mem_used_pct", Name: "内存占用", Unit: "%", Icon: "mdi:memory", Precision: iptr(1)},
	{Key: "disk_used_pct", Name: "存储占用", Unit: "%", Icon: "mdi:harddisk", Precision: iptr(1)},
	{Key: "disk_free_mb", Name: "存储剩余", Unit: "MB", Icon: "mdi:database", Precision: iptr(0)},
	{Key: "heap", Name: "可用内存", Unit: "KB", Icon: "mdi:memory", Precision: iptr(0)},
}
