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
)

const haPrefix = "homeassistant"

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
		"sw_version":   kernelVersion(),
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
	out := make([]HAEntity, 0, len(metrics)+2)
	for _, m := range metrics {
		comp := "sensor"
		if m.Kind == "binary_sensor" {
			comp = "binary_sensor"
		}
		out = append(out, HAEntity{Component: comp, Object: metricObject(m)})
	}
	out = append(out,
		HAEntity{Component: "binary_sensor", Object: "online"},
		HAEntity{Component: "button", Object: "reboot"},
	)
	return out
}

// publishHADiscovery 发布全部实体配置（retained）。
// 连接建立时发一次即可；另有定时补发（discoveryRefresh）应对 broker 丢 retained。
func publishHADiscovery() {
	dev := haDeviceBlock()
	origin := map[string]any{"name": "switchyard-agent"}
	n := 0

	for _, m := range metrics {
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
	}

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
	if publish(haConfigTopic("button", "reboot"), reboot, true) {
		n++
	}

	logf("[discovery] 发布 %d 个 HA 实体（不经过 Switchyard）", n)
}

// handleHACmd 处理 HA 下发的控制命令（主题 switchyard/<id>/ha/<obj>/set）。
// payload 用 HA 的惯例值（button 是 PRESS），与 Switchyard 的 JSON 命令无关。
func handleHACmd(obj string, payload []byte) {
	body := strings.TrimSpace(string(payload))
	logf("[ha-cmd] %s = %q", obj, body)
	switch obj {
	case "reboot":
		logf("[ha-cmd] 执行重启")
		publishEvent("reboot_requested", "info", "ha")
		if err := doReboot(); err != nil {
			logf("[ha-cmd] 重启失败: %v", err)
			publishDiag("重启失败: " + err.Error())
		}
	default:
		logf("[ha-cmd] 未知对象: %s", obj)
	}
}
