// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package collector

import (
	"os"
	"strconv"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// readNetDev 解析 /proc/net/dev。
//
// 文件前两行是表头，之后每行形如 "  eth0: rx_bytes rx_packets ..."：
// 接收侧 8 列（bytes packets errs drop fifo frame compressed multicast），
// 发送侧 8 列（bytes packets errs drop fifo colls carrier compressed）。
//
// 只上报累计值，不在这里算速率：速率由 Hub 按两次快照的时间差推算，
// 这样 Agent 不必保存上一次采样，重装后速率也能立刻恢复。
func readNetDev(path string) ([]protocol.NetStat, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.NetStat, 0, 4)
	for i, line := range strings.Split(string(b), "\n") {
		if i < 2 {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:idx])
		if iface == "" {
			continue
		}
		fields := strings.Fields(line[idx+1:])
		if len(fields) < 11 {
			continue
		}
		out = append(out, protocol.NetStat{
			Interface: iface,
			RxBytes:   atou(fields[0]),
			RxPackets: atou(fields[1]),
			RxErrors:  atou(fields[2]),
			TxBytes:   atou(fields[8]),
			TxPackets: atou(fields[9]),
			TxErrors:  atou(fields[10]),
		})
	}
	return out, nil
}

// atou 把字段转成 uint64；解析失败记 0。
// 单列异常不该让整张网卡的数据消失。
func atou(s string) uint64 {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}
