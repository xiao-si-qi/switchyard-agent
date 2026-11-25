package main

// 命令处理与持久化。
//
// seq 语义：设备记住已执行的最后 seq，只执行更大的。这样重连后 broker 上
// 残留的 retained 命令不会被重复执行。

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// ---- 持久化 ----

func loadInterval() int64 {
	b, err := os.ReadFile(cfgFile)
	if err != nil {
		return int64(envInt("SY_INTERVAL", 60))
	}
	var cfg struct {
		Interval int64 `json:"interval"`
	}
	if json.Unmarshal(b, &cfg) == nil && cfg.Interval > 0 {
		return clampInterval(cfg.Interval)
	}
	return int64(envInt("SY_INTERVAL", 60))
}

func saveInterval(v int64) {
	_ = os.MkdirAll(stateDir, 0o755)
	b, _ := json.Marshal(map[string]int64{"interval": v})
	_ = os.WriteFile(cfgFile, b, 0o644)
}

func loadSeq() int64 {
	b, err := os.ReadFile(seqFile)
	if err != nil {
		return 0
	}
	var s struct {
		Seq int64 `json:"seq"`
	}
	if json.Unmarshal(b, &s) == nil {
		return s.Seq
	}
	return 0
}

func saveSeq(v int64) {
	_ = os.MkdirAll(stateDir, 0o755)
	b, _ := json.Marshal(map[string]int64{"seq": v})
	_ = os.WriteFile(seqFile, b, 0o644)
}

// ---- 命令处理 ----

func handleCmd(msg mqtt.Message) {
	raw := strings.TrimSpace(string(msg.Payload()))
	if raw == "" {
		return
	}
	logf("[cmd] %s (retain=%v)", raw, msg.Retained())

	var cmd Command
	if err := json.Unmarshal([]byte(raw), &cmd); err != nil {
		publishDiag("收到非 JSON 命令: " + raw)
		return
	}

	if cmd.Seq != nil && *cmd.Seq <= lastSeq.Load() {
		logf("[cmd] 忽略过期命令 seq=%d (last=%d)", *cmd.Seq, lastSeq.Load())
		return
	}

	ok, errMsg := true, ""

	switch cmd.Cmd {
	case "state":
		go checkUpdate() // 顺便刷新一次"最新版本"，让 HA 的 update 实体马上有值
		publishMeta()
		publishState()

	case "reboot":
		if !allowReboot {
			ok, errMsg = false, "本机已禁用重启（SY_ALLOW_REBOOT=0）"
			publishDiag("收到 cmd:reboot 但本机已禁用重启")
			break
		}
		if cmd.Seq != nil {
			publishAck(*cmd.Seq, true, "")
			lastSeq.Store(*cmd.Seq)
			saveSeq(*cmd.Seq)
		}
		logf("[cmd] 执行重启")
		time.Sleep(time.Second)
		if err := doReboot(); err != nil {
			logf("[cmd] 重启失败: %v", err)
			publishDiag("重启失败: " + err.Error())
		}
		return

	case "get":
		switch strings.ToLower(cmd.Key) {
		case "log":
			publishLog()
		case "cfg":
			publish(tCfg, map[string]int64{"interval": interval.Load()}, true)
		default:
			ok, errMsg = false, "未知的 get key: "+cmd.Key
		}

	case "set":
		if strings.ToLower(cmd.Key) == "interval" {
			var v int64
			if json.Unmarshal(cmd.Value, &v) != nil {
				ok, errMsg = false, "interval 必须是整数"
			} else {
				v = clampInterval(v)
				interval.Store(v)
				saveInterval(v)
				publish(tCfg, map[string]int64{"interval": v}, true)
				logf("[cfg] interval => %d", v)
			}
		} else {
			ok, errMsg = false, "未知的 set key: "+cmd.Key
		}

	case "ota":
		ok, errMsg = false, "host 类型设备不支持 OTA"

	default:
		ok, errMsg = false, "不支持的命令: "+cmd.Cmd
	}

	if cmd.Seq != nil {
		lastSeq.Store(*cmd.Seq)
		saveSeq(*cmd.Seq)
		publishAck(*cmd.Seq, ok, errMsg)
	}
	if msg.Retained() {
		publish(tCmd, "", true) // 执行完清掉 retained 命令（协议 §5）
	}
}

func handleCfg(msg mqtt.Message) {
	raw := strings.TrimSpace(string(msg.Payload()))
	if raw == "" {
		return
	}
	var cfg struct {
		Interval int64 `json:"interval"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		publishDiag("cfg 不是合法 JSON")
		return
	}
	if cfg.Interval > 0 {
		v := clampInterval(cfg.Interval)
		interval.Store(v)
		saveInterval(v)
		logf("[cfg] 下发 interval=%d", v)
	}
	publishEvent("cfg_applied")
}
