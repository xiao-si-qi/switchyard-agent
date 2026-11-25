package main

// MQTT 传输与发布。TLS 支持见 mqttEndpoint。

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

func buildState() StatePayload {
	totalKB, availKB := memInfo()
	var usedPct *float64
	if totalKB > 0 {
		p := float64(totalKB-availKB) / float64(totalKB) * 100
		p = float64(int(p*10)) / 10
		usedPct = &p
	}
	var load *float64
	if v, ok := cpuLoad.Load().(float64); ok {
		load = &v
	}
	freeMB, diskPct := diskUsage()
	return StatePayload{
		ID:          deviceID,
		Type:        devType,
		FW:          kernelVersion(),
		AgentVersion: currentVersion(),
		IP:          primaryIP(),
		IPEth:       ifaceIP(ethPrefixes),
		IPWlan:      ifaceIP(wlanPrefixes),
		RSSI:        wlanRSSI(),
		Uptime:      uptimeSeconds(),
		Heap:        availKB / 1024, // 统一用 MB，和 disk_free_mb 一致
		TS:          time.Now().Unix(),
		CPULoad:     load,
		MemTotalKB:  totalKB,
		MemFreeKB:   availKB,
		MemUsedPct:  usedPct,
		DiskFreeMB:  freeMB,
		DiskUsedPct: diskPct,
		USB:         usbPresent(),
		Interval:    interval.Load(),
	}
}

func buildMeta() MetaPayload {
	return MetaPayload{
		Type:       devType,
		Caps:       caps,
		Name:       deviceName,
		Model:      model,
		Interval:   interval.Load(),
		Metrics:    metrics,
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
		return fmt.Sprintf("tcp://%s:%d", host, port), nil, nil
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
	return fmt.Sprintf("ssl://%s:%d", host, port), tlsCfg, nil
}
