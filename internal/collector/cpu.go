// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package collector

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// cpuSnapshot 是 /proc/stat 的一次采样。
//
// total 含全部 jiffies（user/nice/system/idle/iowait/irq/softirq/steal），
// 而 DTO 只暴露其中五个——算占用率必须用全量，否则会系统性偏低。
type cpuSnapshot struct {
	user    uint64
	system  uint64
	iowait  uint64
	steal   uint64
	idle    uint64
	total   uint64
	cores   int
	hasCore bool
}

// readCPUStat 解析 /proc/stat 的 cpu 汇总行，并统计在线 CPU 数。
func readCPUStat(path string) (cpuSnapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return cpuSnapshot{}, err
	}
	var snap cpuSnapshot
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		// "cpu" 后紧跟数字的是单核行（cpu0、cpu1…），用来数核数。
		rest := line[3:]
		if rest != "" && rest[0] >= '0' && rest[0] <= '9' {
			snap.cores++
			continue
		}
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		// cpu user nice system idle iowait irq softirq steal [guest guest_nice]
		if len(fields) < 9 {
			return cpuSnapshot{}, errors.New("cpu 行字段不足 9 个")
		}
		vals := make([]uint64, 0, 8)
		for _, f := range fields[1:9] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return cpuSnapshot{}, err
			}
			vals = append(vals, v)
		}
		snap.user = vals[0]
		snap.system = vals[2]
		snap.iowait = vals[4]
		snap.steal = vals[7]
		snap.idle = vals[3]
		for _, v := range vals {
			snap.total += v
		}
		snap.hasCore = true
	}
	if !snap.hasCore {
		return cpuSnapshot{}, errors.New("未找到 cpu 汇总行")
	}
	return snap, nil
}

// physicalCores 统计物理核数：/proc/cpuinfo 里不同的 core id 的个数。
//
// 为什么需要它：Cores 数的是逻辑线程（cpu0/cpu1…），开了超线程的机器上
// 两个线程共享一个物理核——「1 物理核 / 2 线程」的机器 Cores=2，
// 用户看到「2 核」却以为是 2 个物理核。物理核数是这个误解的正解。
//
// 读 /proc/cpuinfo 而不是 /sys 的 thread_siblings_list：前者在容器里
// 必然可读（Agent 本来就读 /proc/stat），后者依赖 /sys 透传，
// 容器内常常读不到——读不到就返回 0（未知），绝不拿 Cores 顶替。
//
// core id 缺失（部分 ARM/虚拟化环境不提供）时返回 0：此时无法判定物理核，
// 显示「未知」比给个假数字诚实。
func physicalCores(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	ids := make(map[string]bool)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) != "core id" {
			continue
		}
		// core id 是全局唯一的物理核编号（同一核的多个线程共享它），
		// 故不同 core id 的个数即物理核数。
		if v = strings.TrimSpace(v); v != "" {
			ids[v] = true
		}
	}
	return len(ids)
}

// cpuUsage 对 /proc/stat 采两次，用差值算占用率。
//
// idle 只计 idle、不计 iowait：按架构规定「占用含 user/system/iowait/steal」，
// 等 IO 的时间也算被占用。
func (c *Collector) cpuUsage() (protocol.CPUStat, error) {
	root := c.ProcRoot
	if root == "" {
		root = DefaultProcRoot
	}
	path := filepath.Join(root, "stat")

	first, err := readCPUStat(path)
	if err != nil {
		return protocol.CPUStat{}, err
	}
	if c.Interval > 0 {
		time.Sleep(c.Interval)
	}
	second, err := readCPUStat(path)
	if err != nil {
		return protocol.CPUStat{}, err
	}

	stat := protocol.CPUStat{
		Cores:  first.cores,
		User:   second.user,
		System: second.system,
		IOWait: second.iowait,
		Steal:  second.steal,
		Idle:   second.idle,
	}
	stat.UsagePercent = usageBetween(first, second)
	// 物理核数读 /proc/cpuinfo（与 /proc/stat 同根，容器内必然可读）。
	stat.CoresPhysical = physicalCores(filepath.Join(root, "cpuinfo"))
	// 占用核数 = 整机百分比 × 逻辑线程数。usage_percent 是整机归一口径，
	// 乘线程数才得到「吃掉了几个核」，与容器侧的 cpu_cores 同量纲。
	stat.BusyCores = stat.UsagePercent / 100 * float64(stat.Cores)
	return stat, nil
}

// usageBetween 由两次采样算占用率（0–100）。
//
// 抽成纯函数是为了可测：读同一份文件时两次采样完全相同，差值恒为 0，
// 光靠 Collect 永远覆盖不到这段计算。
func usageBetween(first, second cpuSnapshot) float64 {
	if second.total <= first.total {
		// 差值为 0（同一份样本）或计数器回绕，占用率记 0，不报错。
		return 0
	}
	totalDelta := second.total - first.total
	idleDelta := second.idle - first.idle
	if idleDelta > totalDelta {
		idleDelta = totalDelta
	}
	pct := float64(totalDelta-idleDelta) / float64(totalDelta) * 100
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}
