// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package collector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// 固定 /proc 样本：字段值与解析逻辑一一对应，改样本就必须改断言。
const (
	sampleStat = `cpu  1000 50 500 8000 100 10 5 0 0 0
cpu0 500 25 250 4000 50 5 2 0 0 0
cpu1 500 25 250 4000 50 5 3 0 0 0
intr 12345 10 0 0 0 0 0 0 0 0
ctxt 99999
`
	sampleMemInfo = `MemTotal:        8192000 kB
MemFree:         1000000 kB
MemAvailable:    5000000 kB
Buffers:          200000 kB
Cached:          3000000 kB
SwapTotal:       2097152 kB
SwapFree:        1000000 kB
`
	// 没有 MemAvailable 的老内核样本，用于验证退化路径。
	sampleMemInfoLegacy = `MemTotal:        8192000 kB
MemFree:         1000000 kB
Buffers:          200000 kB
Cached:          3000000 kB
`
	sampleNetDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:    1000      10    0    0    0     0          0         0     2000      20    0    0    0     0       0          0
  eth0:  123456    1000    2    0    0     0          0         0   654321    2000    1    0    0     0       0          0
`
	sampleLoadAvg = "0.50 0.40 0.30 1/200 12345\n"
	sampleUptime  = "3600.00 12345.00\n"
)

// newSampleProc 造一棵 /proc，并额外放三个数字目录作为进程。
func newSampleProc(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("stat", sampleStat)
	write("meminfo", sampleMemInfo)
	write("net/dev", sampleNetDev)
	write("loadavg", sampleLoadAvg)
	write("uptime", sampleUptime)
	for _, pid := range []string{"1", "2", "123"} {
		if err := os.MkdirAll(filepath.Join(dir, pid), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 非数字目录不应计入进程数。
	if err := os.MkdirAll(filepath.Join(dir, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadCPUStat(t *testing.T) {
	snap, err := readCPUStat(filepath.Join(newSampleProc(t), "stat"))
	if err != nil {
		t.Fatal(err)
	}
	if snap.cores != 2 {
		t.Errorf("cores = %d，want 2", snap.cores)
	}
	if snap.user != 1000 || snap.system != 500 || snap.iowait != 100 || snap.steal != 0 || snap.idle != 8000 {
		t.Errorf("字段解析有误：%+v", snap)
	}
	// total 必须含 nice/irq/softirq，否则占用率会系统性偏低。
	wantTotal := uint64(1000 + 50 + 500 + 8000 + 100 + 10 + 5 + 0)
	if snap.total != wantTotal {
		t.Errorf("total = %d，want %d", snap.total, wantTotal)
	}
}

func TestReadCPUStat_MissingSummaryLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte("intr 1 2 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCPUStat(filepath.Join(dir, "stat")); err == nil {
		t.Error("缺少 cpu 汇总行应报错")
	}
}

func TestUsageBetween(t *testing.T) {
	cases := []struct {
		name   string
		first  cpuSnapshot
		second cpuSnapshot
		want   float64
	}{
		{
			name:   "全忙",
			first:  cpuSnapshot{total: 100, idle: 50},
			second: cpuSnapshot{total: 200, idle: 50},
			want:   100,
		},
		{
			name:   "半忙",
			first:  cpuSnapshot{total: 100, idle: 50},
			second: cpuSnapshot{total: 200, idle: 100},
			want:   50,
		},
		{
			name:   "全空闲",
			first:  cpuSnapshot{total: 100, idle: 50},
			second: cpuSnapshot{total: 200, idle: 150},
			want:   0,
		},
		{
			// 同一份文件读两次，差值恒为 0，不能报 NaN 或负数。
			name:   "差值為零",
			first:  cpuSnapshot{total: 100, idle: 50},
			second: cpuSnapshot{total: 100, idle: 50},
			want:   0,
		},
		{
			// 计数器回绕：第二次总量反而更小。
			name:   "回绕",
			first:  cpuSnapshot{total: 200, idle: 100},
			second: cpuSnapshot{total: 100, idle: 50},
			want:   0,
		},
		{
			// idle 增量超过 total 增量（样本不合理），应被夹到 0 而非负数。
			name:   "idle 增量越界",
			first:  cpuSnapshot{total: 100, idle: 10},
			second: cpuSnapshot{total: 200, idle: 500},
			want:   0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := usageBetween(c.first, c.second); got != c.want {
				t.Errorf("usageBetween = %v，want %v", got, c.want)
			}
		})
	}
}

func TestReadMemInfo(t *testing.T) {
	dir := newSampleProc(t)
	mem, err := readMemInfo(filepath.Join(dir, "meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	const kB = uint64(1024)
	if mem.Total != 8192000*kB {
		t.Errorf("Total = %d", mem.Total)
	}
	// Used = Total − MemAvailable。
	if mem.Used != (8192000-5000000)*kB {
		t.Errorf("Used = %d，want %d", mem.Used, (8192000-5000000)*kB)
	}
	if mem.Available != 5000000*kB || mem.Free != 1000000*kB {
		t.Errorf("Available/Free 有误：%+v", mem)
	}
	if mem.SwapUsed != (2097152-1000000)*kB {
		t.Errorf("SwapUsed = %d", mem.SwapUsed)
	}
}

func TestReadMemInfo_LegacyKernelFallback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(p, []byte(sampleMemInfoLegacy), 0o644); err != nil {
		t.Fatal(err)
	}
	mem, err := readMemInfo(p)
	if err != nil {
		t.Fatal(err)
	}
	const kB = uint64(1024)
	// 没有 MemAvailable 时退化为 Total − Free − Buffers − Cached。
	want := (8192000 - 1000000 - 200000 - 3000000) * kB
	if mem.Used != want {
		t.Errorf("Used = %d，want %d", mem.Used, want)
	}
	if mem.Available != 0 {
		t.Errorf("无 MemAvailable 时应为 0，got %d", mem.Available)
	}
}

func TestReadMemInfo_MissingMemTotal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(p, []byte("MemFree: 100 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readMemInfo(p); err == nil {
		t.Error("缺少 MemTotal 应报错")
	}
}

func TestReadNetDev(t *testing.T) {
	nets, err := readNetDev(filepath.Join(newSampleProc(t), "net", "dev"))
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 2 {
		t.Fatalf("应有 2 张网卡，got %d", len(nets))
	}
	if nets[0].Interface != "lo" || nets[0].RxBytes != 1000 || nets[0].TxBytes != 2000 {
		t.Errorf("lo 解析有误：%+v", nets[0])
	}
	if nets[1].Interface != "eth0" || nets[1].RxBytes != 123456 || nets[1].TxBytes != 654321 {
		t.Errorf("eth0 解析有误：%+v", nets[1])
	}
	if nets[1].RxErrors != 2 || nets[1].TxErrors != 1 {
		t.Errorf("错误计数列偏移有误：rx=%d tx=%d", nets[1].RxErrors, nets[1].TxErrors)
	}
	if nets[1].RxPackets != 1000 || nets[1].TxPackets != 2000 {
		t.Errorf("包计数有误：%+v", nets[1])
	}
}

func TestReadLoadAvgAndUptime(t *testing.T) {
	dir := newSampleProc(t)
	load, err := readLoadAvg(filepath.Join(dir, "loadavg"))
	if err != nil {
		t.Fatal(err)
	}
	if load.One != 0.5 || load.Five != 0.4 || load.Fifteen != 0.3 {
		t.Errorf("load = %+v", load)
	}
	up, err := readUptime(filepath.Join(dir, "uptime"))
	if err != nil {
		t.Fatal(err)
	}
	if up != 3600 {
		t.Errorf("uptime = %d", up)
	}
}

func TestProcessCount(t *testing.T) {
	n, err := processCount(newSampleProc(t))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("进程数 = %d，want 3（非数字目录不计）", n)
	}
}

func TestDiskUsage_RealMountPoint(t *testing.T) {
	dir := t.TempDir()
	c := &Collector{ProcRoot: dir, Mounts: []string{dir}}
	disks, err := c.diskUsage()
	if err != nil {
		t.Fatalf("真实挂载点不应失败：%v", err)
	}
	if len(disks) != 1 {
		t.Fatalf("got %d 条，want 1", len(disks))
	}
	d := disks[0]
	if d.Mount != dir {
		t.Errorf("Mount = %q", d.Mount)
	}
	if d.Total == 0 {
		t.Error("Total 不应为 0")
	}
	// Used 与 Free 同源，必须能对上总数。
	if d.Used+d.Free != d.Total {
		t.Errorf("Used(%d) + Free(%d) ≠ Total(%d)", d.Used, d.Free, d.Total)
	}
	if d.UsedPercent < 0 || d.UsedPercent > 100 {
		t.Errorf("UsedPercent 越界：%v", d.UsedPercent)
	}
}

func TestDiskUsage_BadMountStillReturnsOthers(t *testing.T) {
	dir := t.TempDir()
	c := &Collector{ProcRoot: dir, Mounts: []string{dir, "/definitely/not/a/mount"}}
	disks, err := c.diskUsage()
	if err == nil {
		t.Error("存在失败挂载点时应返回提示")
	}
	if len(disks) != 1 {
		t.Errorf("坏的挂载点不该拖垮其他盘，got %d 条", len(disks))
	}
}

func TestCollect_AssemblesAllFields(t *testing.T) {
	dir := newSampleProc(t)
	c := &Collector{ProcRoot: dir, Mounts: []string{dir}, Interval: 0}
	m, err := c.Collect()
	if err != nil {
		t.Fatalf("样本齐全时不应有警告：%v", err)
	}
	if m.CPU.Cores != 2 {
		t.Errorf("cores = %d", m.CPU.Cores)
	}
	if m.Memory.Total != 8192000*1024 {
		t.Errorf("memory total = %d", m.Memory.Total)
	}
	if m.Load.One != 0.5 {
		t.Errorf("load = %+v", m.Load)
	}
	if m.Processes != 3 {
		t.Errorf("processes = %d", m.Processes)
	}
	if m.UptimeSec != 3600 {
		t.Errorf("uptime = %d", m.UptimeSec)
	}
	if len(m.Network) != 2 || len(m.Disks) != 1 {
		t.Errorf("network=%d disks=%d", len(m.Network), len(m.Disks))
	}
	if m.CollectedAt.IsZero() {
		t.Error("CollectedAt 不应为零值")
	}
}

// 降级原则：任何一项读不到都不能让整次采集失败，否则 Hub 会误判机器离线。
func TestCollect_DegradesWhenProcMissing(t *testing.T) {
	empty := t.TempDir()
	c := &Collector{ProcRoot: empty, Mounts: []string{empty}, Interval: 0}
	m, err := c.Collect()
	if err == nil {
		t.Error("全部读不到时应返回提示，供调用方记录")
	}
	if m.CPU.Cores != 0 || m.Memory.Total != 0 || m.UptimeSec != 0 || m.Processes != 0 {
		t.Errorf("缺失时应降级为零值，got %+v", m)
	}
	// 磁盘走真实 statfs，与 /proc 无关，仍应采到。
	if len(m.Disks) != 1 {
		t.Errorf("磁盘不依赖 /proc，仍应采到，got %d", len(m.Disks))
	}
}

func TestNew_DefaultsToRootMount(t *testing.T) {
	c := New(nil)
	if len(c.Mounts) != 1 || c.Mounts[0] != "/" {
		t.Errorf("默认挂载点应为 [\"/\"]，got %v", c.Mounts)
	}
	if c.ProcRoot != DefaultProcRoot {
		t.Errorf("ProcRoot = %q", c.ProcRoot)
	}
	if c.Interval != DefaultCPUSampleInterval {
		t.Errorf("Interval = %v", c.Interval)
	}
}

// HostMetrics 必须能被序列化：它是 Hub↔Agent 与对外 API 共用的结构。
func TestCollect_ResultIsSerializable(t *testing.T) {
	dir := newSampleProc(t)
	c := &Collector{ProcRoot: dir, Mounts: []string{dir}, Interval: 0}
	m, _ := c.Collect()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var back protocol.HostMetrics
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if back.Memory.Total != m.Memory.Total || len(back.Network) != len(m.Network) {
		t.Errorf("往返不一致：%+v", back)
	}
}
