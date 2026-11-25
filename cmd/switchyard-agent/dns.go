package main

// DNS 兼容性兜底。
//
// 背景：不少家用路由器/网关上的 dnsmasq 处理 **EDNS0** 有 bug —— 收到带 EDNS0
// 的查询会返回畸形报文（dig 会打印 "Message parser reports malformed message
// packet"），解析器于是报 "no such host"。glibc 默认不发 EDNS0，所以 ping /
// getent 正常，而 Go 的解析器默认发 EDNS0，于是只有 Go 程序挂掉。
//
// 现场判据：
//   dig +edns=0  example.com @<路由器>   → 畸形包 / 空
//   dig +noedns  example.com @<路由器>   → 正常返回
//   ping example.com                     → 正常（走 glibc）
//
// SY_DNS_NO_EDNS0=1 让 Go 退回不带 EDNS0 的查询。必须在**第一次 DNS 解析之前**
// 设置 GODEBUG，所以这个函数要在 main 的最开头调用。

import (
	"os"
	"strings"
)

func applyDNSWorkarounds() {
	if !envBool("SY_DNS_NO_EDNS0", false) {
		return
	}
	cur := os.Getenv("GODEBUG")
	if strings.Contains(cur, "netedns0=") {
		return // 用户自己已经设了，尊重它
	}
	next := "netedns0=0"
	if cur != "" {
		next = cur + ",netedns0=0"
	}
	if err := os.Setenv("GODEBUG", next); err != nil {
		logf("[dns] 设置 GODEBUG 失败: %v", err)
		return
	}
	logf("[dns] 已禁用 EDNS0（SY_DNS_NO_EDNS0=1）—— 用于兼容某些路由器的 DNS bug")
}
