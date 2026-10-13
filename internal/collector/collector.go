// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package collector 从 /proc 采集主机指标。
//
// 只依赖标准库：/proc 文本读取 + statfs 系统调用，不引 gopsutil 这类库——
// 交叉编译体积与可控性优先，也避免第三方库把 /proc 语义封装成黑盒。
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

const (
	// DefaultProcRoot 是 /proc 的默认位置，测试时替换成样本目录。
	DefaultProcRoot = "/proc"
	// DefaultCPUSampleInterval 是 CPU 两次采样的间隔。
	// /proc/stat 给的是开机以来的累计 jiffies，只有取两次差值才能算出占用率。
	DefaultCPUSampleInterval = 200 * time.Millisecond
)

// Collector 采集主机指标，一次调用产出一个快照。
type Collector struct {
	// ProcRoot 指向 /proc；测试时指向存放样本文件的目录。
	ProcRoot string
	// Mounts 是要统计的挂载点，为空时只统计根。
	Mounts []string
	// Interval 是 CPU 两次采样之间的等待；置 0 可跳过等待（测试用）。
	Interval time.Duration
}

// New 构造采集器。
func New(mounts []string) *Collector {
	if len(mounts) == 0 {
		mounts = []string{"/"}
	}
	return &Collector{ProcRoot: DefaultProcRoot, Mounts: mounts, Interval: DefaultCPUSampleInterval}
}

// Collect 采集一次完整快照。
//
// 任何子项失败都降级为零值，只把原因汇总进返回的 error 作提示，不影响其他字段：
// 少一项指标只是 UI 少一条曲线，而整个采集失败会让 Hub 误判机器离线。
func (c *Collector) Collect() (protocol.HostMetrics, error) {
	root := c.ProcRoot
	if root == "" {
		root = DefaultProcRoot
	}
	var warns []string
	note := func(what string, err error) {
		if err != nil {
			warns = append(warns, what+": "+err.Error())
		}
	}

	cpu, err := c.cpuUsage()
	note("cpu", err)
	mem, err := readMemInfo(filepath.Join(root, "meminfo"))
	note("memory", err)
	disks, err := c.diskUsage()
	note("disk", err)
	nets, err := readNetDev(filepath.Join(root, "net", "dev"))
	note("network", err)
	load, err := readLoadAvg(filepath.Join(root, "loadavg"))
	note("load", err)
	procs, err := processCount(root)
	note("processes", err)
	up, err := readUptime(filepath.Join(root, "uptime"))
	note("uptime", err)

	m := protocol.HostMetrics{
		CollectedAt: time.Now().UTC(),
		CPU:         cpu,
		Memory:      mem,
		Disks:       disks,
		Network:     nets,
		Load:        load,
		Processes:   procs,
		UptimeSec:   up,
	}
	if len(warns) > 0 {
		return m, errors.New("部分指标采集失败（其余字段仍有效）: " + strings.Join(warns, "; "))
	}
	return m, nil
}

// readLoadAvg 解析 /proc/loadavg 的前三个数。
func readLoadAvg(path string) (protocol.LoadStat, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return protocol.LoadStat{}, err
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return protocol.LoadStat{}, errors.New("字段不足 3 个")
	}
	one, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return protocol.LoadStat{}, err
	}
	five, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return protocol.LoadStat{}, err
	}
	fifteen, err := strconv.ParseFloat(fields[2], 64)
	if err != nil {
		return protocol.LoadStat{}, err
	}
	return protocol.LoadStat{One: one, Five: five, Fifteen: fifteen}, nil
}

// processCount 统计 /proc 下以纯数字命名的目录数。
func processCount(root string) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if isAllDigits(e.Name()) {
			n++
		}
	}
	return n, nil
}

// readUptime 取 /proc/uptime 的第一个字段（秒）。
func readUptime(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, errors.New("内容为空")
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	if f < 0 {
		return 0, nil
	}
	return uint64(f), nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
