// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package collector

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// readMemInfo 解析 /proc/meminfo，单位 kB，统一换算成 byte。
//
// Used = Total − Available：MemAvailable 已扣除可回收的 page cache，
// 比 Total − Free 更贴近真实占用。老内核没有 MemAvailable 时退化为
// Total − Free − Buffers − Cached，并在返回值上保持不变的语义。
func readMemInfo(path string) (protocol.MemoryStat, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return protocol.MemoryStat{}, err
	}
	vals := make(map[string]uint64, 16)
	for _, line := range strings.Split(string(b), "\n") {
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		fields := strings.Fields(line[idx+1:])
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		vals[key] = v * 1024
	}
	if _, ok := vals["MemTotal"]; !ok {
		return protocol.MemoryStat{}, errors.New("缺少 MemTotal")
	}
	total := vals["MemTotal"]
	var used uint64
	if avail, ok := vals["MemAvailable"]; ok {
		if total > avail {
			used = total - avail
		}
	} else {
		free := vals["MemFree"]
		reclaimable := vals["Buffers"] + vals["Cached"]
		if total > free+reclaimable {
			used = total - free - reclaimable
		} else if total > free {
			used = total - free
		}
	}
	// 逐项做上界判断：uint64 下溢会得到一个天文数字，比缺失更糟。
	swapTotal := vals["SwapTotal"]
	var swapUsed uint64
	if swapTotal > vals["SwapFree"] {
		swapUsed = swapTotal - vals["SwapFree"]
	}
	return protocol.MemoryStat{
		Total:     total,
		Used:      used,
		Available: vals["MemAvailable"],
		Free:      vals["MemFree"],
		Buffers:   vals["Buffers"],
		Cached:    vals["Cached"],
		SwapTotal: swapTotal,
		SwapUsed:  swapUsed,
	}, nil
}
