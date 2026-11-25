package main

// 自更新。
//
// 设计原则：**HA 负责发现和执行，Switchyard 只展示状态**。
// 设备的版本以 MQTT `update` 实体暴露给 HA，HA 的「设置 → 更新」里会出现
// "已安装 v0.1.0 → 可更新 v0.2.0"，用户点一次「安装」就发
// INSTALL 到 switchyard/<id>/ha/update/set，设备自己完成替换和重启。
//
// 安全性：
//   - 只从本项目的 release 下载，且只接受版本号更大的版本（不接受外部 URL）
//   - 先写 <bin>.new，校验 ELF 头和架构后才原子 mv 覆盖；校验失败旧二进制不受影响
//   - 替换完用 syscall.Exec 原地重启，不依赖 systemd/OpenRC 把它拉起来

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const ghAPI = "https://api.github.com/repos/%s/releases/latest"

// 上次检查时间（unix nano），用于限流
var lastUpdateCheckAt atomic.Int64

// exePath 返回当前二进制的真实路径（解析符号链接），自更新要往这里写。
func exePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// updateAssetName 推导本机该用哪个 release 产物。
// armv6 用户需要显式指定 SY_UPDATE_ASSET（GOARCH 都是 arm，无法自动区分）。
func updateAssetName() string {
	if s := env("SY_UPDATE_ASSET", ""); s != "" {
		return s
	}
	return "switchyard-agent-linux-" + runtime.GOARCH
}

// httpGet 走直连 → 镜像的顺序取内容，返回 body 和最终的 URL（便于日志）。
func httpGet(url string, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	var lastErr error
	for _, u := range withMirrors(url) {
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "switchyard-agent/"+currentVersion())
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("%s 返回 HTTP %d", u, resp.StatusCode)
			continue
		}
		return body, nil
	}
	return nil, lastErr
}

// withMirrors 返回候选 URL 列表：直连 → SY_UPDATE_MIRROR → 内置公共镜像。
func withMirrors(url string) []string {
	out := []string{url}
	if m := env("SY_UPDATE_MIRROR", ""); m != "" {
		out = append(out, strings.TrimRight(m, "/")+"/"+url)
	}
	if !envBool("SY_NO_MIRROR", false) {
		for _, m := range []string{"https://ghproxy.net", "https://gh-proxy.com", "https://ghfast.top"} {
			out = append(out, m+"/"+url)
		}
	}
	return out
}

// latestVersion 查 GitHub 上最新 release 的版本号（去掉前导 v）。
func latestVersion() (string, error) {
	api := fmt.Sprintf(ghAPI, updateRepo)
	body, err := httpGet(api, 20*time.Second)
	if err != nil {
		return "", err
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", fmt.Errorf("解析 release JSON 失败: %w", err)
	}
	v := strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
	if v == "" {
		return "", fmt.Errorf("release 里没有 tag_name")
	}
	return v, nil
}

// parseVer 把 "1.2.3" 解析成可比较的数字序列。"1.2" 和 "1.2.0" 等价。
func parseVer(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// 去掉 -rc1 之类的后缀
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// versionGreater 判断 a 是否比 b 新。任一侧无法解析时返回 false（宁可不升级）。
func versionGreater(a, b string) bool {
	pa, pb := parseVer(a), parseVer(b)
	if pa == nil || pb == nil {
		return false
	}
	for len(pa) < len(pb) {
		pa = append(pa, 0)
	}
	for len(pb) < len(pa) {
		pb = append(pb, 0)
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

// elfMachine 返回 ELF 头的 e_machine，用来确认下载到的是本架构的二进制。
func elfMachine(b []byte) (uint16, bool) {
	if len(b) < 20 || b[0] != 0x7f || b[1] != 'E' || b[2] != 'L' || b[3] != 'F' {
		return 0, false
	}
	return uint16(b[18]) | uint16(b[19])<<8, true
}

// expectedMachine 是 runtime.GOARCH → ELF e_machine 的映射。
var expectedMachine = map[string]uint16{
	"amd64":   0x3E,
	"arm64":   0xB7,
	"arm":     0x28,
	"loong64": 0x102,
	"riscv64": 0xF3,
	"386":     0x03,
}

// checkUpdate 查最新版并发布到 switchyard/<id>/update（retained），供 HA 的
// update 实体读取 latest_version。返回最新版本号（查不到返回空）。
//
// 5 分钟内重复调用会被忽略：GitHub API 未认证只有 60 次/小时，
// 而「检查更新」按钮和 cmd:state 都可能连带触发。
func checkUpdate() string {
	if !envBool("SY_UPDATE_CHECK", true) {
		return ""
	}
	now := time.Now().UnixNano()
	if prev := lastUpdateCheckAt.Load(); prev != 0 && now-prev < int64(5*time.Minute) {
		return ""
	}
	lastUpdateCheckAt.Store(now)

	cur := currentVersion()
	if cur == "dev" {
		// 源码跑的开发版不参与升级，否则会拿一个假版本号和 release 比
		return ""
	}
	latest, err := latestVersion()
	if err != nil {
		logf("[update] 检查失败: %v", err)
		publishDiag("检查更新失败: " + err.Error())
		// 明确告诉 HA "我不知道"，避免它把旧的 latest 当成仍然有效
		publish(tUpdate, map[string]any{"latest": cur, "error": err.Error()}, true)
		return ""
	}
	logf("[update] 当前 %s，最新 %s", cur, latest)

	// 只有确实"更新"才把 latest 报给 HA。否则（版本相同、或本地比 release 还新，
	// 比如手工装了未发布的构建）HA 的 update 实体会认为"有更新"甚至提供降级。
	shown := latest
	if !versionGreater(latest, cur) {
		shown = cur
	}
	publish(tUpdate, map[string]any{
		"latest":  shown,
		"current": cur,
		"tag":     latest,
		"url":     fmt.Sprintf("https://github.com/%s/releases/tag/v%s", updateRepo, latest),
	}, true)
	return latest
}

// selfUpdate 执行升级：下载 → 校验 → 原子替换 → 原地重启。
// 任何一步失败都保持旧版本继续运行。
func selfUpdate(target string) error {
	cur := currentVersion()
	if !versionGreater(target, cur) {
		return fmt.Errorf("目标版本 %s 不比当前 %s 新，忽略", target, cur)
	}

	exe, err := exePath()
	if err != nil {
		return fmt.Errorf("定位自身路径失败: %w", err)
	}
	asset := updateAssetName()
	url := fmt.Sprintf("https://github.com/%s/releases/download/v%s/%s", updateRepo, target, asset)

	logf("[update] 开始下载 %s", url)
	body, err := httpGet(url, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("下载失败: %w", err)
	}
	sum := sha256.Sum256(body)
	logf("[update] 下载完成 %d 字节 sha256=%s", len(body), hex.EncodeToString(sum[:])[:16])

	// 校验架构：防止把错架构的二进制装上去导致起不来
	want, ok := expectedMachine[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("未知架构 %s，拒绝自更新", runtime.GOARCH)
	}
	got, isELF := elfMachine(body)
	if !isELF {
		return fmt.Errorf("下载到的不是 ELF 文件（可能被镜像返回了错误页）")
	}
	if got != want {
		return fmt.Errorf("架构不匹配：期望 0x%03x，实际 0x%03x", want, got)
	}

	// 写临时文件 → 原子替换
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, body, 0o755); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("设置可执行权限失败: %w", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换 %s 失败: %w", exe, err)
	}
	logf("[update] 已替换 %s，重启到 %s", exe, target)

	publishEvent("update_installed", "info", target)
	publish(tUpdate, map[string]any{"latest": target, "current": target}, true)

	// 原地重启：直接用新二进制接管当前进程。
	// 不退出等 service 拉起来 —— OpenRC 的 command_background 不会自动重启。
	time.Sleep(500 * time.Millisecond)
	argv := []string{exe}
	if len(os.Args) > 0 {
		argv = append(argv, os.Args[1:]...)
	}
	if err := syscall.Exec(exe, argv, os.Environ()); err != nil {
		// Exec 失败：磁盘上已经是新版本了，下次重启会生效
		logf("[update] 重启失败: %v（新版本已就位，重启服务后生效）", err)
		publishDiag("升级后重启失败: " + err.Error())
		return fmt.Errorf("重启失败: %w", err)
	}
	return nil // 正常不会到这里
}

// updateLoop 定时检查（默认 6 小时）。
func updateLoop() {
	iv := time.Duration(envInt("SY_UPDATE_INTERVAL_H", 6)) * time.Hour
	if iv <= 0 {
		return
	}
	time.Sleep(20 * time.Second) // 启动后稍等，别和首个 state 抢带宽
	for {
		if client != nil && client.IsConnectionOpen() {
			checkUpdate()
		}
		time.Sleep(iv)
	}
}
