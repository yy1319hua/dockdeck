// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package procscan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeProc 在 fake procRoot 下造一个 pid 目录，写入给定文件内容。
// files 的键是文件名（cgroup/stat/status/cmdline）。
func writeProc(t *testing.T, procRoot string, pid string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(procRoot, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建 pid 目录 %s: %v", dir, err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s/%s: %v", pid, name, err)
		}
	}
}

// 一个字段完整、comm 含空格的 stat（验证「以最后一个 ) 为界」的解析）。
const goodStat = "100 (my proc) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 12345 0 0\n"

func TestCgroupBelongs(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		cg   string
		want bool
	}{
		{"cgroup v1", "11:cpu,cpuacct:/docker/" + id + "\n12:memory:/docker/" + id + "\n", true},
		{"cgroup v2", "0::/system.slice/docker-" + id + ".scope\n", true},
		{"cgroupfs v2", "0::/docker/" + id + "\n", true},
		{"他容器", "0::/docker/ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff\n", false},
		{"空", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cgroupBelongs(tc.cg, id); got != tc.want {
				t.Errorf("cgroupBelongs(%q) = %v，want %v", tc.cg, got, tc.want)
			}
		})
	}
}

func TestParseStat(t *testing.T) {
	// 正常：comm 含空格。
	st, ok := parseStat(goodStat)
	if !ok {
		t.Fatal("parseStat 失败")
	}
	if st.comm != "my proc" {
		t.Errorf("comm = %q，want %q", st.comm, "my proc")
	}
	if st.state != "S" {
		t.Errorf("state = %q，want S", st.state)
	}
	if st.ppid != 1 {
		t.Errorf("ppid = %d，want 1", st.ppid)
	}
	if st.startTime != 12345 {
		t.Errorf("startTime = %d，want 12345", st.startTime)
	}

	// comm 含 ')'：以最后一个 ')' 为界，comm 保留内部的括号。
	st2, ok := parseStat("200 (a)b) R 3 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 999 0 0\n")
	if !ok {
		t.Fatal("parseStat(含括号 comm) 失败")
	}
	if st2.comm != "a)b" {
		t.Errorf("comm = %q，want %q", st2.comm, "a)b")
	}

	// 字段不足：拒绝。
	if _, ok := parseStat("100 (x) S 1 2 3\n"); ok {
		t.Error("字段不足应返回 false")
	}
	// 无括号：拒绝。
	if _, ok := parseStat("garbage"); ok {
		t.Error("无括号应返回 false")
	}
}

func TestListAttribution(t *testing.T) {
	procRoot := t.TempDir()
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// 归属目标容器：完整数据。
	writeProc(t, procRoot, "100", map[string]string{
		"cgroup":  "0::/docker/" + id + "\n",
		"stat":    goodStat,
		"status":  "Name:\tfoo\nUid:\t1000\t1000\t1000\t1000\nVmRSS:\t 2048 kB\nVmSize:\t 4096 kB\nThreads:\t3\n",
		"cmdline": "/bin/sh\x00-c\x00sleep 1\x00",
	})
	// 归属目标容器：还有一条，验证排序。
	writeProc(t, procRoot, "50", map[string]string{
		"cgroup":  "0::/docker/" + id + "\n",
		"stat":    "50 (second) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 111 0 0\n",
		"status":  "Uid:\t0\t0\t0\t0\n",
		"cmdline": "/usr/bin/nginx\x00\n",
	})
	// 他容器：应被排除。
	writeProc(t, procRoot, "200", map[string]string{
		"cgroup": "0::/docker/" + other + "\n",
		"stat":   goodStat,
	})
	// 非数字目录：应被跳过。
	if err := os.MkdirAll(filepath.Join(procRoot, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}

	procs, err := List(context.Background(), procRoot, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(procs) != 2 {
		t.Fatalf("应返回 2 个进程，got %d: %+v", len(procs), procs)
	}
	// 排序：PID 升序。
	if procs[0].PID != 50 || procs[1].PID != 100 {
		t.Fatalf("应按 PID 升序，got %d,%d", procs[0].PID, procs[1].PID)
	}
	p := procs[1]
	if p.PPID != 1 || p.Comm != "my proc" || p.State != "S" {
		t.Errorf("基本字段不符: %+v", p)
	}
	if p.User != "1000" || p.MemRSS != 2048 || p.MemVMS != 4096 || p.Threads != 3 {
		t.Errorf("status 字段不符: %+v", p)
	}
	if p.StartTime != 12345 {
		t.Errorf("StartTime = %d，want 12345", p.StartTime)
	}
	if p.Command != "/bin/sh -c sleep 1" {
		t.Errorf("Command = %q", p.Command)
	}
}

func TestListEmptyIDReturnsEmpty(t *testing.T) {
	procRoot := t.TempDir()
	writeProc(t, procRoot, "100", map[string]string{
		"cgroup": "0::/docker/x\n",
		"stat":   goodStat,
	})
	procs, err := List(context.Background(), procRoot, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(procs) != 0 {
		t.Fatalf("空 containerID 应返回空列表（绝不返回全量），got %d", len(procs))
	}
}

func TestListMissingProcRoot(t *testing.T) {
	if _, err := List(context.Background(), filepath.Join(t.TempDir(), "nope"), "x"); err == nil {
		t.Error("procRoot 不存在应返回错误")
	}
}

func TestJoinCmdline(t *testing.T) {
	if got := joinCmdline([]byte("a\x00b\x00c\x00")); got != "a b c" {
		t.Errorf("joinCmdline = %q，want %q", got, "a b c")
	}
	if got := joinCmdline(nil); got != "" {
		t.Errorf("空 cmdline 应为空串，got %q", got)
	}
}

func TestDetail(t *testing.T) {
	procRoot := t.TempDir()
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// 归属目标的进程：cgroup 命中，exe/cwd 为 magic symlink。
	dir := filepath.Join(procRoot, "100")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte("0::/docker/"+id+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/foo", filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/app", filepath.Join(dir, "cwd")); err != nil {
		t.Fatal(err)
	}

	d, err := Detail(context.Background(), procRoot, id, 100)
	if err != nil {
		t.Fatalf("Detail(归属进程): %v", err)
	}
	if d.PID != 100 || d.Exe != "/usr/bin/foo" || d.Cwd != "/app" {
		t.Errorf("Detail = %+v，want exe=/usr/bin/foo cwd=/app", d)
	}

	// 越权 pid：存在但属于他容器 → ErrNotOwned（红线）。
	if err := os.MkdirAll(filepath.Join(procRoot, "200"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(procRoot, "200", "cgroup"), []byte("0::/docker/"+other+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Detail(context.Background(), procRoot, id, 200); !errors.Is(err, ErrNotOwned) {
		t.Errorf("越权 pid 应 ErrNotOwned，got %v", err)
	}

	// 不存在的 pid。
	if _, err := Detail(context.Background(), procRoot, id, 999); !errors.Is(err, ErrProcessNotFound) {
		t.Errorf("不存在 pid 应 ErrProcessNotFound，got %v", err)
	}

	// 空容器 ID 一律拒（绝不退化成任意进程可读）。
	if _, err := Detail(context.Background(), procRoot, "", 100); !errors.Is(err, ErrProcessNotFound) {
		t.Errorf("空容器ID 应 ErrProcessNotFound，got %v", err)
	}
}
