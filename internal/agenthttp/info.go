// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agenthttp

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// procPath 返回 /proc 下的文件路径。
// 抽成变量是为了可测：/proc 的内容无法在单测里伪造，只能换掉根路径。
var procPath = func(name string) string { return filepath.Join("/proc", name) }

// Version 是构建时注入的版本串，未注入时保持 dev。
var Version = "dev"

// Info 返回 /info 的 handler，上报 Agent 自身与宿主机的静态信息。
//
// 这些信息变化极慢，与高频指标分开返回：Hub 纳管时拉一次即可，
// 不必让每条指标快照都重复携带服务器名和内核版本。
func Info() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, CollectInfo(time.Now))
	}
}

// CollectInfo 采集静态信息。now 可注入，便于测试 boot time 推算。
//
// 每一项失败都降级为零值并继续，不让 /info 因为拿不到内核版本就整个不可用——
// 少一个字段只是展示少一行，/info 挂掉会让 Hub 无法纳管这台机器。
func CollectInfo(now func() time.Time) protocol.AgentInfo {
	version := Version
	if strings.TrimSpace(version) == "" {
		version = "dev"
	}
	return protocol.AgentInfo{
		Hostname:        hostname(),
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		Kernel:          kernelVersion(),
		AgentVersion:    version,
		ProtocolVersion: protocol.ProtocolVersion,
		Cores:           runtime.NumCPU(),
		MemoryTotal:     memTotalBytes(),
		BootTime:        bootTime(now()),
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// kernelVersion 解析 /proc/version 的第三段。
// 非 Linux 或读不到时返回空串。
func kernelVersion() string {
	b, err := os.ReadFile(procPath("version"))
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return ""
	}
	return fields[2]
}

// memTotalBytes 解析 /proc/meminfo 的 MemTotal，单位 kB，换算成 byte。
func memTotalBytes() uint64 {
	b, err := os.ReadFile(procPath("meminfo"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// bootTime 由 /proc/uptime 的第一个字段反推开机时刻。
func bootTime(now time.Time) time.Time {
	b, err := os.ReadFile(procPath("uptime"))
	if err != nil {
		return time.Time{}
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return time.Time{}
	}
	up, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return time.Time{}
	}
	return now.Add(-time.Duration(up * float64(time.Second)))
}
