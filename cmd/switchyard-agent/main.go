package main

// switchyard-agent —— 把一台普通 Linux 主机接进 Switchyard / Home Assistant。
//
// 它做的两件事：
//   1. 自己往 homeassistant/<comp>/switchyard_<id>/<obj>/config 发实体声明，
//      HA 直接拿到指标和「重启」按钮，不经过 Switchyard 插件；
//   2. 往 switchyard/<id>/{state,meta,event} 上报，供 Switchyard 台账展示与远程排障。
//
// 一份静态二进制，无运行时依赖。配置全部走环境变量，见 README。

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

var (
	// 由 -ldflags "-X main.version=..." 注入；源码运行时是 dev
	version string

	client   mqtt.Client
	interval atomic.Int64 // 秒
	lastSeq  atomic.Int64

	cpuLoad    atomic.Value // float64
	lastDiagAt atomic.Int64 // unix nano
)

// 主题在 init 时按 deviceID 拼好，避免各处重复拼接。
var (
	tState = "switchyard/" + deviceID + "/state"
	tAvail = "switchyard/" + deviceID + "/availability"
	tMeta  = "switchyard/" + deviceID + "/meta"
	tEvent = "switchyard/" + deviceID + "/event"
	tCmd   = "switchyard/" + deviceID + "/cmd"
	tCfg   = "switchyard/" + deviceID + "/cfg"
	tDiag  = "switchyard/" + deviceID + "/diag"
	tLog   = "switchyard/" + deviceID + "/log"
	tAck   = "switchyard/" + deviceID + "/ack"
	tUpdate = "switchyard/" + deviceID + "/update" // 最新版本号，供 HA 的 update 实体读
	// HA 控制下行通配
	tHACmdWild = "switchyard/" + deviceID + "/ha/+/set"
	tHACmdPref = "switchyard/" + deviceID + "/ha/"
)

func main() {
	// 必须在任何 DNS 解析之前（preflight 就会解析域名）
	applyDNSWorkarounds()

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		logf("[warn] 无法创建状态目录 %s: %v（interval/seq 将无法持久化）", stateDir, err)
	}
	interval.Store(loadInterval())
	lastSeq.Store(loadSeq())

	go cpuSampler()

	setupPahoLogging()
	preflight()

	brokerURL, tlsCfg, err := mqttEndpoint()
	if err != nil {
		logf("[fatal] %v", err)
		os.Exit(1)
	}

	opts := mqtt.NewClientOptions()
	opts.AddBroker(brokerURL)
	if tlsCfg != nil {
		opts.SetTLSConfig(tlsCfg)
		logf("[mqtt] TLS 连接 %s（CA=%s，SNI=%s）",
			brokerURL, env("MQTT_CA_FILE", "系统根证书"), tlsCfg.ServerName)
	} else {
		logf("[mqtt] 明文连接 %s（仅建议用于可信内网）", brokerURL)
	}
	opts.SetClientID("switchyard-" + deviceID)
	opts.SetUsername(user)
	opts.SetPassword(pass)
	opts.SetKeepAlive(60 * time.Second)
	opts.SetPingTimeout(10 * time.Second)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	opts.SetCleanSession(true)
	opts.SetWill(tAvail, "offline", 1, true)

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		logf("[mqtt] 已连接 %s", brokerURL)
		publishMeta()
		publish(tAvail, "online", true)
		publishState()
		publishHADiscovery()
		c.SubscribeMultiple(
			map[string]byte{
				tCmd:       1,
				tCfg:       1,
				tHACmdWild: 1,
			},
			func(_ mqtt.Client, m mqtt.Message) {
				defer func() {
					if r := recover(); r != nil {
						logf("[error] 处理消息 panic: %v", r)
						publishDiag(fmt.Sprintf("处理消息出错: %v", r))
					}
				}()
				switch {
				case m.Topic() == tCmd:
					handleCmd(m)
				case m.Topic() == tCfg:
					handleCfg(m)
				case strings.HasPrefix(m.Topic(), tHACmdPref) && strings.HasSuffix(m.Topic(), "/set"):
					obj := strings.TrimSuffix(strings.TrimPrefix(m.Topic(), tHACmdPref), "/set")
					if obj != "" && !strings.Contains(obj, "/") {
						handleHACmd(obj, m.Payload())
					}
				}
			})
		publishEvent("boot", "fw", kernelVersion())
	})

	opts.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		logf("[mqtt] 连接断开: %v（自动重连）", err)
	})

	client = mqtt.NewClient(opts)
	v := version
	if v == "" {
		v = "dev"
	}
	logf("[agent] %s v%s (type=%s) → %s，每 %ds 上报", deviceID, v, devType, brokerURL, interval.Load())

	tok := client.Connect()
	if !tok.WaitTimeout(30 * time.Second) {
		// 注意：WaitTimeout 返回 false 表示"还在连"，此时不能调 tok.Error()
		// （它会阻塞到 token 完成）。旧代码用 && 短路，超时这条分支一条日志都不打，
		// 于是"连不上"看起来像是"没报错"。
		logf("[mqtt] 30 秒内未连上 %s（后台每 5 秒重试）—— 看上面的 preflight 结果", brokerURL)
	} else if err := tok.Error(); err != nil {
		logf("[mqtt] 首次连接失败: %v（后台继续重试）", err)
	}

	// 周期上报
	go func() {
		last := time.Now()
		for {
			time.Sleep(time.Second)
			if time.Since(last) >= time.Duration(interval.Load())*time.Second {
				last = time.Now()
				publishState()
			}
		}
	}()

	// 定时补发 HA discovery：broker 丢 retained、或别的组件误清时能自愈
	go func() {
		for {
			time.Sleep(discoveryRefresh)
			if client != nil && client.IsConnectionOpen() {
				publishHADiscovery()
			}
		}
	}()

	// 定时检查更新（默认 6 小时一次），结果发到 switchyard/<id>/update
	go updateLoop()

	// 连接看门狗：长时间连不上要能在日志里看见，而不是静默重试
	go func() {
		lastWarn := time.Time{}
		for {
			time.Sleep(30 * time.Second)
			if client != nil && client.IsConnectionOpen() {
				lastWarn = time.Time{}
				continue
			}
			if time.Since(lastWarn) >= 5*time.Minute {
				lastWarn = time.Now()
				logf("[mqtt] ⚠ 仍未连接到 %s（已重试多次）—— 检查 preflight 输出、防火墙与凭据", brokerURL)
			}
		}
	}()

	// 可移动存储插拔事件（可在容器里用 SY_USB_DETECT=0 关掉）
	if usbDetect {
		go func() {
			prev := usbPresent()
			for {
				time.Sleep(2 * time.Second)
				cur := usbPresent()
				if cur != prev {
					prev = cur
					if cur {
						publishEvent("usb_attached")
					} else {
						publishEvent("usb_detached")
					}
				}
			}
		}()
	}

	select {}
}
