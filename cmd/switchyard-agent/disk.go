package main

// 磁盘挂载自动发现。
//
// 从 /proc/mounts 扫描，只保留**本地块设备**上的文件系统：
//   - 排除伪文件系统（proc/sysfs/tmpfs/cgroup/overlay…）
//   - 排除网络文件系统（nfs/cifs/sshfs）和 FUSE —— statfs 打到一个挂死的
//     NFS 上会把整个进程卡在 D 状态，等于 agent 失联。要看它们请显式开
//     SY_DISK_INCLUDE_NET=1
//   - 按**设备**去重：bind mount 和同一个 fs 的多个挂载点只统计一次
//   - 只统计有实际容量的挂载（blocks > 0）
//
// 根分区沿用历史字段 disk_free_mb / disk_used_pct（实体 ID 不变），
// 其它挂载生成 disk_free_<slug> / disk_used_<slug>。

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 本地文件系统白名单：只有这些才敢 statfs。
var localFSTypes = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true,
	"xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "jfs": true, "reiserfs": true, "bcachefs": true,
	"vfat": true, "fat": true, "exfat": true, "msdos": true,
	"ntfs": true, "ntfs3": true, "fuseblk": true,
	"hfsplus": true, "hfs": true, "ufs": true, "apfs": true,
}

// 网络文件系统：默认跳过（statfs 可能长时间阻塞）
var netFSTypes = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smbfs": true, "smb3": true,
	"sshfs": true, "davfs": true, "ceph": true, "glusterfs": true,
	"9p": true, "afs": true, "ncpfs": true,
}

type mountUsage struct {
	Device  string
	Mount   string
	FSType  string
	FreeMB  int64
	UsedPct float64
}

const maxMounts = 12

type mountCacheT struct {
	sync.Mutex
	at   time.Time
	list []mountUsage
}

var mountCache mountCacheT

// mountSlug 把挂载点变成合法的 metric key 片段。
//
//	"/"            → "root"
//	"/data"        → "data"
//	"/mnt/disk2"   → "mnt_disk2"
//	"/media/我的盘" → "media"（非 ASCII 会被丢掉）
func mountSlug(mount string) string {
	if mount == "/" {
		return "root"
	}
	clean := strings.Trim(mount, "/")
	var b strings.Builder
	for _, r := range strings.ToLower(clean) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '/' || r == '-' || r == '.' || r == ' ':
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "_")
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	if s == "" {
		return "mnt"
	}
	if len(s) > 24 {
		s = s[:24]
	}
	return s
}

// scanMounts 返回当前所有值得上报的本地挂载，按可用空间从大到小排序。
func scanMounts() []mountUsage {
	includeNet := envBool("SY_DISK_INCLUDE_NET", false)

	raw, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return nil
	}

	seenDev := map[string]bool{}
	var out []mountUsage

	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		device, mount, fstype := f[0], f[1], f[2]

		isLocal := localFSTypes[fstype]
		isNet := netFSTypes[fstype] || strings.HasPrefix(fstype, "fuse")
		if !isLocal && !(includeNet && isNet) {
			continue
		}
		// 同一个设备只统计一次（bind mount / 多挂载点）
		if seenDev[device] {
			continue
		}
		if device == "none" || device == "" {
			continue
		}

		var st syscall.Statfs_t
		if err := syscall.Statfs(mount, &st); err != nil {
			continue
		}
		frsize := float64(st.Frsize)
		total := float64(st.Blocks) * frsize
		if st.Blocks == 0 || total <= 0 {
			continue // 空设备/未挂好
		}
		free := float64(st.Bavail) * frsize

		seenDev[device] = true
		out = append(out, mountUsage{
			Device:  device,
			Mount:   mount,
			FSType:  fstype,
			FreeMB:  int64(free / 1048576),
			UsedPct: round1((total - free) / total * 100),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].FreeMB > out[j].FreeMB })
	if len(out) > maxMounts {
		out = out[:maxMounts]
	}
	return out
}

func round1(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return float64(int(v*10+0.5)) / 10
}

// mounts 带缓存：state 每几十秒发一次，没必要每次都重新 statfs。
func mounts() []mountUsage {
	ttl := time.Duration(envInt("SY_DISK_SCAN_SECONDS", 60)) * time.Second
	mountCache.Lock()
	defer mountCache.Unlock()
	if time.Since(mountCache.at) < ttl && mountCache.list != nil {
		return mountCache.list
	}
	mountCache.list = scanMounts()
	mountCache.at = time.Now()
	return mountCache.list
}

// rootUsage 返回根分区的用量（沿用历史字段）。
func rootUsage() (freeMB *int64, usedPct *float64) {
	for _, m := range mounts() {
		if m.Mount == "/" {
			f, p := m.FreeMB, m.UsedPct
			return &f, &p
		}
	}
	// / 不在白名单里（极少见）时退回直接 statfs
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return nil, nil
	}
	frsize := float64(st.Frsize)
	total := float64(st.Blocks) * frsize
	free := float64(st.Bavail) * frsize
	f := int64(free / 1048576)
	if total > 0 {
		p := round1((total - free) / total * 100)
		return &f, &p
	}
	return &f, nil
}

// diskMetrics 为**非根**挂载生成 metric 声明。根分区走历史字段，不重复生成。
func diskMetrics() []Metric {
	var out []Metric
	for _, m := range mounts() {
		if m.Mount == "/" {
			continue
		}
		slug := mountSlug(m.Mount)
		out = append(out,
			Metric{
				Key:   "disk_free_" + slug,
				Name:  fmt.Sprintf("存储剩余 (%s)", m.Mount),
				Unit:  "MB",
				Icon:  "mdi:database",
				Precision: iptr(0),
			},
			Metric{
				Key:   "disk_used_" + slug,
				Name:  fmt.Sprintf("存储占用 (%s)", m.Mount),
				Unit:  "%",
				Icon:  "mdi:harddisk",
				Precision: iptr(1),
			},
		)
	}
	return out
}
