package main

// MQTT 传输与发布。TLS 支持见 mqttEndpoint。

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

func buildState() map[string]any {
	totalKB, availKB := memInfo()
	var memPct any
	if totalKB > 0 {
		memPct = round1(float64(totalKB-availKB) / float64(totalKB) * 100)
	}
	var load any
	if v, ok := cpuLoad.Load().(float64); ok {
		load = v
	}
	freeMB, diskPct := rootUsage()

	st := map[string]any{
		"id":      deviceID,
		"type":    devType,
		"fw":      kernelVersion(), // 内核版本，不是代理版本
		"agent_version": currentVersion(),
		"ip":       primaryIP(),
		"ip_eth":   ifaceIP(ethPrefixes),
		"ip_wlan":  ifaceIP(wlanPrefixes),
		"rssi":     wlanRSSI(),
		"uptime":   uptimeSeconds(),
		"heap":     availKB / 1024, // MB，与 disk_free_* 单位统一
		"ts":       time.Now().Unix(),
		"cpu_load": load,
		"mem_total_kb":  totalKB,
		"mem_free_kb":   availKB,
		"mem_used_pct":  memPct,
		"disk_free_mb":  freeMB,  // 根分区（历史字段名，保持不变）
		"disk_used_pct": diskPct,
		"usb":      usbPresent(),
		"interval": interval.Load(),
	}
	// 自动发现出来的其它本地挂载：/data → disk_free_data / disk_used_data
	for _, m := range mounts() {
		if m.Mount == "/" {
			continue
		}
		slug := mountSlug(m.Mount)
		st["disk_free_"+slug] = m.FreeMB
		st["disk_used_"+slug] = m.UsedPct
	}
	return st
}

func buildMeta() MetaPayload {
	return MetaPayload{
		Type:       devType,
		Caps:       caps,
		Name:       deviceName,
		Model:      model,
		Interval:   interval.Load(),
		Metrics:    allMetrics(),
		HAEntities: buildHAEntities(),
	}
}

// publish 统一发布入口：字符串原样发，其它类型 JSON 序列化。
func publish(topic string, v any, retain bool) bool {
	if client == nil || !client.IsConnectionOpen() {
		return false
	}
	var payload []byte
	switch t := v.(type) {
	case string:
		payload = []byte(t)
	case []byte:
		payload = t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return false
		}
		payload = b
	}
	tok := client.Publish(topic, 1, retain, payload)
	tok.WaitTimeout(5 * time.Second)
	return tok.Error() == nil
}

func publishState() bool { return publish(tState, buildState(), true) }
func publishMeta() bool  { return publish(tMeta, buildMeta(), true) }

func publishEvent(ev string, extra ...string) bool {
	e := EventPayload{Ev: ev, TS: time.Now().Unix()}
	for i := 0; i+1 < len(extra); i += 2 {
		switch extra[i] {
		case "fw":
			e.FW = extra[i+1]
		case "info":
			e.Info = extra[i+1]
		}
	}
	logf("[event] %s", ev)
	return publish(tEvent, e, false)
}

// publishDiag 错误摘要：retained，但限流 1 条/分钟，避免刷屏。
func publishDiag(text string) bool {
	now := time.Now()
	last := time.Unix(0, lastDiagAt.Load())
	if now.Sub(last) < diagGap {
		return false
	}
	lastDiagAt.Store(now.UnixNano())
	if len(text) > 1000 {
		text = text[:1000]
	}
	return publish(tDiag, map[string]any{"line": text, "ts": now.Unix()}, true)
}

// publishLog 按需回传最近 N 行日志（非 retained）。
func publishLog() bool {
	lines := strings.Split(readFile(logFile), "\n")
	if len(lines) > logTailN {
		lines = lines[len(lines)-logTailN:]
	}
	logf("[log] 回传 %d 行", len(lines))
	return publish(tLog, map[string]any{"lines": strings.Join(lines, "\n"), "ts": time.Now().Unix()}, false)
}

func publishAck(seq int64, ok bool, errMsg string) bool {
	return publish(tAck, AckPayload{Seq: seq, OK: ok, Err: errMsg}, false)
}

// mqttEndpoint 按配置决定明文还是 TLS。
//
// 明文只在 broker 与设备同处一个可信网络时才算安全；一旦跨公网，
// 凭据、状态、重启命令全是明文，必须开 TLS。
//
//	MQTT_TLS=1            启用 TLS（默认 0 = 明文）
//	MQTT_CA_FILE=路径     自定义 CA（自签/企业内网）；留空用系统根证书
//	MQTT_INSECURE=1       跳过证书与主机名校验，**仅调试用**
//	MQTT_SERVER_NAME=xxx  覆盖 SNI / 证书校验主机名，默认用 MQTT_HOST
func mqttEndpoint() (string, *tls.Config, error) {
	if !envBool("MQTT_TLS", false) {
		return fmt.Sprintf("tcp://%s", net.JoinHostPort(host, strconv.Itoa(port))), nil, nil
	}

	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: env("MQTT_SERVER_NAME", host),
	}

	if caFile := env("MQTT_CA_FILE", ""); caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return "", nil, fmt.Errorf("读取 CA 文件失败: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return "", nil, fmt.Errorf("CA 文件里没有可用的证书: %s", caFile)
		}
		tlsCfg.RootCAs = pool
	}

	if envBool("MQTT_INSECURE", false) {
		tlsCfg.InsecureSkipVerify = true
		logf("[mqtt] ⚠ 已禁用证书校验（MQTT_INSECURE=1），仅限调试")
	}

	// paho 用 ssl:// 表示"TCP + TLS"
	return fmt.Sprintf("ssl://%s", net.JoinHostPort(host, strconv.Itoa(port))), tlsCfg, nil
}

// ---------------------------------------------------------------- 可观测性

// pahoLogger 把 paho 的内部日志接到我们自己的日志里。
// paho 默认把 ERROR/CRITICAL/WARN 全部丢进 NOOPLogger —— 连接失败、证书错误、
// 认证被拒这些全都不出声，排障时等于瞎的。
type pahoLogger struct{ prefix string }

// 这些是 paho 内部存储的常规噪音（QoS1 重发后清理），对排障没用
var pahoNoise = []string{"store]", "memorystore"}

func pahoIsNoise(s string) bool {
	for _, n := range pahoNoise {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func (p pahoLogger) Println(v ...any) {
	s := fmt.Sprintln(v...)
	if pahoIsNoise(s) {
		return
	}
	logf("%s%s", p.prefix, strings.TrimRight(s, "\n"))
}

func (p pahoLogger) Printf(format string, v ...any) {
	if pahoIsNoise(format) {
		return
	}
	logf(p.prefix+strings.TrimRight(format, "\n"), v...)
}

// setupPahoLogging 打开 paho 的错误输出；SY_MQTT_DEBUG=1 时连协议细节也打。
func setupPahoLogging() {
	mqtt.ERROR = pahoLogger{"[paho:error] "}
	mqtt.CRITICAL = pahoLogger{"[paho:critical] "}
	mqtt.WARN = pahoLogger{"[paho:warn] "}
	if envBool("SY_MQTT_DEBUG", false) {
		mqtt.DEBUG = pahoLogger{"[paho:debug] "}
		logf("[mqtt] 已开启 paho 协议级日志（SY_MQTT_DEBUG=1）")
	}
}

// preflight 在建立 MQTT 之前先做一次 DNS + TCP 探测，把结果打进日志。
// 连不上时能立刻分清是"域名解析不出来"还是"端口不通"，还是"TLS/认证被拒"。
// 不阻塞主流程：失败只记日志，真正的连接仍由 paho 重试。
func preflight() {
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	ips, err := net.LookupHost(host)
	if err != nil {
		logf("[preflight] ❌ 域名解析失败: %v", err)
		return
	}
	logf("[preflight] %s 解析到 %s", host, strings.Join(ips, ", "))

	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		logf("[preflight] ❌ TCP 连不上 %s: %v（检查防火墙 / 端口 / 出网策略）", addr, err)
		return
	}
	local := conn.LocalAddr().String()
	remote := conn.RemoteAddr().String()
	_ = conn.Close()
	logf("[preflight] ✅ TCP 可达 %s（本地 %s → 远端 %s）", addr, local, remote)
}
