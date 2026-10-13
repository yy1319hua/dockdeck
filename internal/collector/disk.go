// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package collector

import (
	"errors"
	"strings"
	"syscall"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// fsTypeNames 把 statfs 的魔数映射成可读名字。
//
// 只列常见的几种；认不出的留空——留空只是少一列展示，猜错却会误导排障。
var fsTypeNames = map[int64]string{
	0xEF53:     "ext4",
	0x58465342: "xfs",
	0x9123683E: "btrfs",
	0x01021994: "tmpfs",
	0x794c7630: "overlayfs",
	0x2FC12FC1: "zfs",
	0x6969:     "nfs",
}

// diskUsage 用 statfs 逐个挂载点取容量。
//
// 用 Bfree 而非 Bavail：Used 与 Free 必须同源，否则 used + free ≠ total，
// 前端画环形图时会出现对不上的缝。
func (c *Collector) diskUsage() ([]protocol.DiskStat, error) {
	mounts := c.Mounts
	if len(mounts) == 0 {
		mounts = []string{"/"}
	}
	out := make([]protocol.DiskStat, 0, len(mounts))
	var errs []string
	for _, m := range mounts {
		var st syscall.Statfs_t
		if err := syscall.Statfs(m, &st); err != nil {
			errs = append(errs, m+": "+err.Error())
			continue
		}
		bsize := uint64(st.Bsize)
		total := uint64(st.Blocks) * bsize
		free := uint64(st.Bfree) * bsize
		var used uint64
		if total > free {
			used = total - free
		}
		var pct float64
		if total > 0 {
			pct = float64(used) / float64(total) * 100
		}
		out = append(out, protocol.DiskStat{
			Mount:       m,
			FSType:      fsTypeNames[int64(st.Type)],
			Total:       total,
			Used:        used,
			Free:        free,
			UsedPercent: pct,
		})
	}
	if len(errs) > 0 {
		// 已采到的挂载点仍要返回：一块盘读失败不该让其他盘一起消失。
		return out, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}
