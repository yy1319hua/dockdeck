// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package procscan 从宿主 /proc 读取「属于某个容器」的进程明细。
//
// 前置：Agent 容器以 pid:host 运行，其 /proc 即宿主 /proc，因此能看到
// 各容器进程。归属判定靠 /proc/<pid>/cgroup 里出现的容器 ID 子串——
// Docker 在 cgroup v1（/docker/<id>）与 v2（docker-<id>.scope 或 /docker/<id>）
// 都以完整 hex ID 出现，用子串匹配对两种布局都成立。
//
// 安全边界：本包只做「读 /proc 并解析」，不跟随 /proc/<pid>/root、cwd、exe
// 等 magic symlink（那是进程明细 v2 的事，且须经 fsapi 的 openat 守卫）。
// 归属过滤在 List 内一次完成：不属于该容器的 pid 一律不返回。
package procscan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// DefaultProcRoot 是宿主 procfs 的默认挂载点。
const DefaultProcRoot = "/proc"

// 明细（Detail）的哨兵错误。二者在 Agent 侧统一映射为 404（不泄漏 pid 是否存在），
// 内部可区分以便测试断言。
var (
	// ErrProcessNotFound 表示该 pid 不存在或读取失败。
	ErrProcessNotFound = errors.New("procscan: 进程不存在")
	// ErrNotOwned 表示该 pid 存在但不归属目标容器（越权读取）。
	ErrNotOwned = errors.New("procscan: 进程不属于该容器")
)

// List 返回属于 containerID 的进程列表，按 PID 升序。
//
// containerID 用 /proc/<pid>/cgroup 做子串匹配，必须是完整（或足够长的）
// hex ID。空 ID 返回空列表——绝不做「空 ID = 全量返回」，否则等于把宿主
// 进程全泄漏出去。
//
// 单个 pid 的读取失败不拖垮整次列举（进程随时在退出）：跳过它继续。
// 目标容器无进程时返回空切片而非 nil（JSON 序列化为 []）。
func List(ctx context.Context, procRoot, containerID string) ([]protocol.ContainerProcess, error) {
	out := []protocol.ContainerProcess{}
	if containerID == "" {
		return out, nil
	}
	if procRoot == "" {
		procRoot = DefaultProcRoot
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil || pid <= 0 {
			// 非数字目录（/proc/self、/proc/sys、/proc/net…）跳过。
			continue
		}
		p, ok := readProcess(procRoot, pid, containerID)
		if !ok {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// Detail 返回单个进程的补充明细（exe / cwd），仅当该 pid **归属目标容器**时。
//
// 安全红线：先读 /proc/<pid>/cgroup 做归属校验，不属于该容器一律 ErrNotOwned——
// 否则调用方可用任意 pid 读宿主任意进程的信息。exe/cwd 是 magic symlink，
// 用 Readlink 取目标路径（读不到就留空，不视为错误：进程可能刚退出/无权限）。
func Detail(ctx context.Context, procRoot, containerID string, pid int) (protocol.ContainerProcessDetail, error) {
	if containerID == "" || pid <= 0 {
		return protocol.ContainerProcessDetail{}, ErrProcessNotFound
	}
	if procRoot == "" {
		procRoot = DefaultProcRoot
	}
	if err := ctx.Err(); err != nil {
		return protocol.ContainerProcessDetail{}, err
	}
	dir := filepath.Join(procRoot, strconv.Itoa(pid))

	cg, err := os.ReadFile(filepath.Join(dir, "cgroup"))
	if err != nil {
		return protocol.ContainerProcessDetail{}, ErrProcessNotFound
	}
	if !cgroupBelongs(string(cg), containerID) {
		return protocol.ContainerProcessDetail{}, ErrNotOwned
	}

	d := protocol.ContainerProcessDetail{PID: pid}
	if p, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
		d.Exe = p
	}
	if p, err := os.Readlink(filepath.Join(dir, "cwd")); err == nil {
		d.Cwd = p
	}
	return d, nil
}

// readProcess 读取单个 pid 并判断是否归属 containerID。归属或必填字段
// 读不到时 ok=false（该 pid 被忽略）。
func readProcess(procRoot string, pid int, containerID string) (protocol.ContainerProcess, bool) {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))

	cg, err := os.ReadFile(filepath.Join(dir, "cgroup"))
	if err != nil {
		return protocol.ContainerProcess{}, false
	}
	if !cgroupBelongs(string(cg), containerID) {
		return protocol.ContainerProcess{}, false
	}

	statRaw, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return protocol.ContainerProcess{}, false
	}
	st, ok := parseStat(string(statRaw))
	if !ok {
		return protocol.ContainerProcess{}, false
	}

	p := protocol.ContainerProcess{
		PID:       pid,
		PPID:      st.ppid,
		Comm:      st.comm,
		State:     st.state,
		StartTime: st.startTime,
	}

	// cmdline：argv 以 \0 分隔；空表示内核线程或无权限，退回 [comm]。
	if cb, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		p.Command = joinCmdline(cb)
	}
	if p.Command == "" {
		p.Command = "[" + st.comm + "]"
	}

	// status：Uid / VmRSS / VmSize / Threads（读不到就留零值，不致命）。
	if sb, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
		parseStatus(string(sb), &p)
	}
	return p, true
}

// cgroupBelongs 判断 /proc/<pid>/cgroup 的文本是否归属 containerID。
//
// 用子串匹配而非精确解析：cgroup v1（"/docker/<id>"）与 v2
// （"docker-<id>.scope"）布局不同，但都以完整 hex ID 出现，子串对两者都成立。
func cgroupBelongs(cgroup, containerID string) bool {
	if containerID == "" {
		return false
	}
	return strings.Contains(cgroup, containerID)
}

// statInfo 是 /proc/<pid>/stat 中我们关心的字段。
type statInfo struct {
	comm      string
	state     string
	ppid      int
	startTime int64
}

// parseStat 解析 /proc/<pid>/stat。
//
// comm 在 "(...)" 内，可能含空格与括号（如 "(my proc)"），因此以**最后一个** ')'
// 为界切分：comm 取第一个 '(' 与最后一个 ')' 之间，其余字段按空白切分。
//
// 字段序号（1 起）：1 pid、2 comm、3 state、4 ppid、…、22 starttime。
// 切掉 "pid (comm)" 后，rest[0]=state、rest[1]=ppid、rest[19]=starttime。
func parseStat(s string) (statInfo, bool) {
	lp := strings.IndexByte(s, '(')
	rp := strings.LastIndexByte(s, ')')
	if lp < 0 || rp < 0 || rp < lp {
		return statInfo{}, false
	}
	comm := s[lp+1 : rp]
	rest := strings.Fields(strings.TrimSpace(s[rp+1:]))
	// 需要至少到 rest[19]（starttime），即 20 个字段。
	if len(rest) < 20 {
		return statInfo{}, false
	}
	ppid, _ := strconv.Atoi(rest[1])
	start, _ := strconv.ParseInt(rest[19], 10, 64)
	return statInfo{comm: comm, state: rest[0], ppid: ppid, startTime: start}, true
}

// parseStatus 解析 /proc/<pid>/status 里的展示字段。
func parseStatus(s string, p *protocol.ContainerProcess) {
	for _, line := range strings.Split(s, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "Uid":
			// "Uid:\t<real>\t<effective>\t<saved>\t<fs>"，取 real（第一个）。
			if f := strings.Fields(val); len(f) > 0 {
				p.User = f[0]
			}
		case "VmRSS":
			p.MemRSS = firstInt(val)
		case "VmSize":
			p.MemVMS = firstInt(val)
		case "Threads":
			p.Threads = int(firstInt(val))
		}
	}
}

// firstInt 取 " 1234 kB" 这类值的第一个整数。
func firstInt(v string) int64 {
	f := strings.Fields(v)
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(f[0], 10, 64)
	return n
}

// joinCmdline 把 \0 分隔的 argv 拼成展示串（空格连接）。
func joinCmdline(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	parts := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	return strings.Join(parts, " ")
}
