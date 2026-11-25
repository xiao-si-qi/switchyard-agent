package main

// 采集层：全部读 /proc 和 /sys，不依赖任何发行版特有的工具
// （唯一的例外是 `ip` 命令，用来取网卡地址；不存在时优雅降级）。

import (
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func logf(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func clampInterval(v int64) int64 {
	if v < int64(intervalMin) {
		return int64(intervalMin)
	}
	if v > int64(intervalMax) {
		return int64(intervalMax)
	}
	return v
}

func kernelVersion() string {
	if v := readFile("/proc/sys/kernel/osrelease"); v != "" {
		return v
	}
	return "unknown"
}

func uptimeSeconds() int64 {
	fields := strings.Fields(readFile("/proc/uptime"))
	if len(fields) == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	return int64(f)
}

// procStat 读 /proc/stat 第一行，返回 (总时间, 空闲时间)。
func procStat() (total, idle uint64, ok bool) {
	line := strings.SplitN(readFile("/proc/stat"), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var vals []uint64
	for _, f := range fields[1:] {
		n, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			break
		}
		vals = append(vals, n)
	}
	if len(vals) < 4 {
		return 0, 0, false
	}
	var sum uint64
	for _, v := range vals {
		sum += v
	}
	idle = vals[3]
	if len(vals) > 4 {
		idle += vals[4] // iowait 也算空闲，否则会把等 IO 误判成忙
	}
	return sum, idle, true
}

// cpuSampler 每秒采一次 /proc/stat 算占用率，然后歇 9 秒。
// 取 1 位小数：计数器是整数，直接除会得到 10.1010101… 这种值。
func cpuSampler() {
	for {
		t1, i1, ok1 := procStat()
		time.Sleep(time.Second)
		t2, i2, ok2 := procStat()
		if !ok1 || !ok2 || t2 <= t1 {
			continue
		}
		dt := float64(t2 - t1)
		di := float64(i2 - i1)
		pct := 100.0 * (dt - di) / dt
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		cpuLoad.Store(math.Round(pct*10) / 10)
		time.Sleep(9 * time.Second)
	}
}

// wlanRSSI 从 /proc/net/wireless 取信号强度。
// 有线设备没有这个文件，返回 nil（HA 里该实体显示为未知，不影响其它指标）。
func wlanRSSI() *int {
	for _, line := range strings.Split(readFile("/proc/net/wireless"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ":")
		if !hasAnyPrefix(name, wlanPrefixes) {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimRight(fields[2], "."), 64)
		if err != nil {
			continue
		}
		v := int(q/2 - 100)
		return &v
	}
	return nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

var ipRe = regexp.MustCompile(`^\d+:\s+(\S+)\s+inet\s+(\d+\.\d+\.\d+\.\d+)`)

// ifaceIP 返回名字匹配任一前缀的第一块网卡的 IPv4。
// 优先用 `ip` 命令；没有 iproute2 时退回 ioctl 读网卡地址。
func ifaceIP(prefixes []string) *string {
	if out, err := exec.Command("ip", "-4", "-o", "addr", "show", "scope", "global").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			m := ipRe.FindStringSubmatch(line)
			if m != nil && hasAnyPrefix(m[1], prefixes) {
				addr := m[2]
				return &addr
			}
		}
		return nil
	}
	// 兜底：用 net 包直接枚举网卡（不依赖外部命令）
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !hasAnyPrefix(e.Name(), prefixes) {
			continue
		}
		if addr := ifaceAddr(e.Name()); addr != "" {
			return &addr
		}
	}
	return nil
}

// ifaceAddr 取指定网卡的第一个非回环 IPv4。
func ifaceAddr(name string) string {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
	}
	return ""
}

// primaryIP：有线优先，没有有线才用无线。
func primaryIP() *string {
	if ip := ifaceIP(ethPrefixes); ip != nil {
		return ip
	}
	return ifaceIP(wlanPrefixes)
}

func memInfo() (totalKB, availKB int64) {
	for _, line := range strings.Split(readFile("/proc/meminfo"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		n, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			totalKB = n
		case "MemAvailable:":
			availKB = n
		}
	}
	return
}

// diskUsage 返回根分区剩余 MB 和已用百分比。
func diskUsage() (freeMB *int64, usedPct *float64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return nil, nil
	}
	frsize := float64(st.Frsize)
	total := float64(st.Blocks) * frsize
	free := float64(st.Bavail) * frsize
	f := int64(free / 1048576)
	freeMB = &f
	if total > 0 {
		p := (total - free) / total * 100
		p = float64(int(p*10)) / 10
		usedPct = &p
	}
	return freeMB, usedPct
}

// usbPresent 判断有没有可移动存储（U 盘 / SD 卡）。
//
// 不能用 /dev/sda 判断：在普通 PC 上那通常是系统盘，会永远返回 true。
// 这里读 sysfs 的 removable 标志，并额外识别 removable=0 的 USB 硬盘。
func usbPresent() bool {
	if !usbDetect {
		return false
	}
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") ||
			strings.HasPrefix(name, "zram") || strings.HasPrefix(name, "sr") ||
			strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "md") {
			continue
		}
		if v := readFile("/sys/block/" + name + "/removable"); v == "1" {
			return true
		}
		// 有些 U 盘/移动硬盘报告 removable=0，但 sysfs 路径里带 usb
		if link, err := os.Readlink("/sys/block/" + name); err == nil &&
			strings.Contains(strings.ToLower(link), "usb") {
			return true
		}
		if link, err := os.Readlink("/sys/block/" + name + "/device"); err == nil &&
			strings.Contains(strings.ToLower(link), "usb") {
			return true
		}
	}
	return false
}

// doReboot 执行配置好的重启命令。需要 root（systemd 下也可用 polkit 授权）。
func doReboot() error {
	if len(rebootArgs) == 0 {
		return fmt.Errorf("重启命令为空")
	}
	cmd := exec.Command(rebootArgs[0], rebootArgs[1:]...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("执行 %s 失败: %w", strings.Join(rebootArgs, " "), err)
	}
	return nil
}
