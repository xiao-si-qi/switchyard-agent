package main

import "encoding/json"

// 与 broker 交换的 JSON 结构。字段名就是协议的一部分：
// Switchyard 台账和 HA 的 value_template 都按这些 key 取值，改名等于破坏协议。

// StatePayload 是 switchyard/<id>/state 的内容（retained）。
type StatePayload struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	FW          string   `json:"fw"`
	IP          *string  `json:"ip"`      // 主 IP（有线优先），台账用
	IPEth       *string  `json:"ip_eth"`  // 有线网卡的 IPv4
	IPWlan      *string  `json:"ip_wlan"` // 无线网卡的 IPv4
	RSSI        *int     `json:"rssi"`
	Uptime      int64    `json:"uptime"`
	Heap        int64    `json:"heap"`
	Battery     *int     `json:"battery"`
	TS          int64    `json:"ts"`
	CPULoad     *float64 `json:"cpu_load"`
	MemTotalKB  int64    `json:"mem_total_kb"`
	MemFreeKB   int64    `json:"mem_free_kb"`
	MemUsedPct  *float64 `json:"mem_used_pct"`
	DiskFreeMB  *int64   `json:"disk_free_mb"`
	DiskUsedPct *float64 `json:"disk_used_pct"`
	USB         bool     `json:"usb"`
	Interval    int64    `json:"interval"`
}

// Metric 声明一个只读指标。同一份表同时驱动两件事：
//  1. 设备自己发布 HA Discovery（HA 直接拿到实体，不经过 Switchyard）
//  2. 写进 meta.metrics 供 Switchyard 界面展示
type Metric struct {
	Key         string `json:"key"`                    // state JSON 里的字段名，也是 object_id 后缀
	Name        string `json:"name"`                   // 显示名
	Kind        string `json:"kind,omitempty"`         // 空 = sensor；binary_sensor 表示布尔
	Unit        string `json:"unit,omitempty"`         // 单位
	Icon        string `json:"icon,omitempty"`         // mdi 图标
	DeviceClass string `json:"device_class,omitempty"` // HA device_class
	StateClass  string `json:"state_class,omitempty"`  // HA state_class
	Object      string `json:"object,omitempty"`       // 覆盖 object_id 后缀
	Precision   *int   `json:"precision,omitempty"`    // HA suggested_display_precision
}

func iptr(n int) *int { return &n }

// HAEntity 记录本设备往 HA 发过哪些实体，供 Switchyard「删除设备」时照着清。
type HAEntity struct {
	Component string `json:"component"`
	Object    string `json:"object"`
}

// MetaPayload 是 switchyard/<id>/meta 的内容（retained）。
// 它只用于台账展示，**不用于生成 HA 实体**。
type MetaPayload struct {
	Type       string     `json:"type"`
	Caps       []string   `json:"caps"`
	Name       string     `json:"name"`
	Model      string     `json:"model"`
	Interval   int64      `json:"interval"`
	Metrics    []Metric   `json:"metrics"`
	HAEntities []HAEntity `json:"ha_entities"`
}

type EventPayload struct {
	Ev   string `json:"ev"`
	TS   int64  `json:"ts"`
	FW   string `json:"fw,omitempty"`
	Info string `json:"info,omitempty"`
}

type AckPayload struct {
	Seq int64  `json:"seq"`
	OK  bool   `json:"ok"`
	Err string `json:"err,omitempty"`
}

// Command 是 switchyard/<id>/cmd 上的下行命令。
type Command struct {
	Seq   *int64          `json:"seq"`
	Cmd   string          `json:"cmd"`
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}
