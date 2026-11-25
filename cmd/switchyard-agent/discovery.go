package main

// HA 实体自注册。
//
// 分界线：HA 能直接表达的（sensor / binary_sensor / switch / button / number）
// 由**设备自己**发标准 MQTT Discovery，不经过 Switchyard。这样插件挂了、
// 升级出 bug，HA 里的实体和自动化都不受影响。
//
// 只有 Switchyard 才能做的（OTA 分发、布局下发、批量配置）才走
// switchyard/<id>/cmd。本设备的 caps 因此是空的。

import (
	"fmt"
	"strings"
	"sync"
)

const haPrefix = "homeassistant"

// 上一轮发布过的**动态**实体（磁盘挂载），用于在挂载消失后清掉残留配置。
var (
	publishedDynamic   = map[string]bool{}
	publishedDynamicMu sync.Mutex
)

func haCmdTopic(obj string) string {
	return "switchyard/" + deviceID + "/ha/" + obj + "/set"
}

func haConfigTopic(component, obj string) string {
	return fmt.Sprintf("%s/%s/switchyard_%s/%s/config", haPrefix, component, deviceID, obj)
}

func haDeviceBlock() map[string]any {
	return map[string]any{
		"identifiers":  []string{"switchyard_" + deviceID},
		"name":         deviceName,
		"manufacturer": "Switchyard",
		"model":        model,
		"sw_version":   currentVersion(), // 固件=代理版本；内核版本走 fw 传感器
	}
}

func metricObject(m Metric) string {
	if m.Object != "" {
		return m.Object
	}
	return m.Key
}

// buildHAEntities 列出本设备往 HA 发布过哪些实体，供 Switchyard 删除设备时清理。
func buildHAEntities() []HAEntity {
	ms := allMetrics()
	out := make([]HAEntity, 0, len(ms)+3)
	for _, m := range ms {
		comp := "sensor"
		if m.Kind == "binary_sensor" {
			comp = "binary_sensor"
		}
		out = append(out, HAEntity{Component: comp, Object: metricObject(m)})
	}
	out = append(out, HAEntity{Component: "binary_sensor", Object: "online"})
	if allowReboot {
		out = append(out, HAEntity{Component: "button", Object: "reboot"})
	}
	out = append(out, HAEntity{Component: "update", Object: "update"})
	return out
}

// publishHADiscovery 发布全部实体配置（retained）。
// 连接建立时发一次即可；另有定时补发（discoveryRefresh）应对 broker 丢 retained。
func publishHADiscovery() {
	dev := haDeviceBlock()
	origin := map[string]any{"name": "switchyard-agent"}
	n := 0

	curDynamic := map[string]bool{}
	for _, m := range allMetrics() {
		obj := metricObject(m)
		kind := m.Kind
		if kind == "" {
			kind = "sensor"
		}
		cfg := map[string]any{
			"name":            m.Name,
			"unique_id":       "switchyard_" + deviceID + "_" + obj,
			"object_id":       deviceID + "_" + obj,
			"state_topic":     tState,
			"device":          dev,
			"origin":          origin,
			"entity_category": "diagnostic",
		}
		if kind == "binary_sensor" {
			cfg["value_template"] = fmt.Sprintf("{{ 'ON' if value_json.%s else 'OFF' }}", m.Key)
			cfg["payload_on"] = "ON"
			cfg["payload_off"] = "OFF"
		} else {
			cfg["value_template"] = fmt.Sprintf("{{ value_json.%s }}", m.Key)
			if m.Unit != "" {
				cfg["unit_of_measurement"] = m.Unit
				sc := m.StateClass
				if sc == "" {
					sc = "measurement"
				}
				cfg["state_class"] = sc
			}
		}
		if m.Icon != "" {
			cfg["icon"] = m.Icon
		}
		if m.DeviceClass != "" {
			cfg["device_class"] = m.DeviceClass
		}
		if m.Precision != nil {
			// HA 面板按这个精度显示，避免 10.1010101… 直接怼到界面上
			cfg["suggested_display_precision"] = *m.Precision
		}
		if publish(haConfigTopic(kind, obj), cfg, true) {
			n++
		}
		if strings.HasPrefix(m.Key, "disk_free_") || strings.HasPrefix(m.Key, "disk_used_") {
			curDynamic[kind+"/"+obj] = true
		}
	}

	// 挂载点消失（拔盘、卸载）后，把上一轮发过的对应实体清掉，避免留下僵尸
	publishedDynamicMu.Lock()
	for k := range publishedDynamic {
		if curDynamic[k] {
			continue
		}
		parts := strings.SplitN(k, "/", 2)
		if len(parts) == 2 {
			if publish(haConfigTopic(parts[0], parts[1]), "", true) {
				logf("[discovery] 清理已消失的磁盘实体 %s", k)
			}
		}
	}
	publishedDynamic = curDynamic
	publishedDynamicMu.Unlock()

	// 在线状态：读 availability（LWT 维护）
	online := map[string]any{
		"name":         "在线",
		"unique_id":    "switchyard_" + deviceID + "_online",
		"object_id":    deviceID + "_online",
		"state_topic":  tAvail,
		"payload_on":   "online",
		"payload_off":  "offline",
		"device_class": "connectivity",
		"device":       dev,
		"origin":       origin,
	}
	if publish(haConfigTopic("binary_sensor", "online"), online, true) {
		n++
	}

	// 重启按钮：HA 点一下 → 发到 ha/reboot/set
	// SY_ALLOW_REBOOT=0 时不发布，并把之前发过的清掉（避免残留一个还能点的按钮）
	if !allowReboot {
		publish(haConfigTopic("button", "reboot"), "", true)
	}
	reboot := map[string]any{
		"name":          "重启",
		"unique_id":     "switchyard_" + deviceID + "_reboot",
		"object_id":     deviceID + "_reboot",
		"command_topic": haCmdTopic("reboot"),
		"payload_press": "PRESS",
		"icon":          "mdi:restart",
		"device":        dev,
		"origin":        origin,
	}
	if allowReboot && publish(haConfigTopic("button", "reboot"), reboot, true) {
		n++
	}

	// 更新实体：HA 的「设置 → 更新」里会显示"已安装 → 可更新"，点「安装」发 INSTALL。
	// 已安装版本读 state.agent_version，最新版本读 switchyard/<id>/update。
	upd := map[string]any{
		"name":                   "switchyard-agent",
		"unique_id":              "switchyard_" + deviceID + "_update",
		"object_id":              deviceID + "_update",
		"device_class":           "firmware",
		"title":                  "switchyard-agent",
		"state_topic":            tState,
		"value_template":         "{{ value_json.agent_version }}",
		"latest_version_topic":   tUpdate,
		"latest_version_template": "{{ value_json.latest }}",
		"command_topic":          haCmdTopic("update"),
		"payload_install":        "INSTALL",
		"release_url":            "https://github.com/" + updateRepo + "/releases",
		"device":                 dev,
		"origin":                 origin,
	}
	if publish(haConfigTopic("update", "update"), upd, true) {
		n++
	}

	logf("[discovery] 发布 %d 个 HA 实体（不经过 Switchyard）", n)
}

// handleHACmd 处理 HA 下发的控制命令（主题 switchyard/<id>/ha/<obj>/set）。
// payload 用 HA 的惯例值（button 是 PRESS，update 是 INSTALL），
// 与 Switchyard 的 JSON 命令无关。
func handleHACmd(obj string, payload []byte) {
	body := strings.TrimSpace(string(payload))
	logf("[ha-cmd] %s = %q", obj, body)
	switch obj {
	case "reboot":
		if !allowReboot {
			logf("[ha-cmd] 已禁用重启（SY_ALLOW_REBOOT=0），忽略")
			publishDiag("收到重启请求但本机已禁用（SY_ALLOW_REBOOT=0）")
			return
		}
		logf("[ha-cmd] 执行重启")
		publishEvent("reboot_requested", "info", "ha")
		if err := doReboot(); err != nil {
			logf("[ha-cmd] 重启失败: %v", err)
			publishDiag("重启失败: " + err.Error())
		}

	case "update":
		if !strings.EqualFold(body, "INSTALL") {
			logf("[ha-cmd] 忽略非 INSTALL 的更新请求: %q", body)
			return
		}
		// 下载可能有几 MB，必须放到后台：在 MQTT 回调里阻塞会拖死 PINGREQ
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logf("[update] panic: %v", r)
					publishDiag(fmt.Sprintf("升级出错: %v", r))
				}
			}()
			latest, err := latestVersion()
			if err != nil {
				logf("[update] 查询最新版本失败: %v", err)
				publishDiag("查询最新版本失败: " + err.Error())
				return
			}
			if err := selfUpdate(latest); err != nil {
				logf("[update] 升级失败: %v", err)
				publishDiag("升级失败: " + err.Error())
			}
		}()

	default:
		logf("[ha-cmd] 未知对象: %s", obj)
	}
}
