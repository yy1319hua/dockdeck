// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"errors"
	"testing"
)

// TestListMounts_ParsesMounts 验证 /containers/json 的 Mounts 解析：
// 选取器只依赖这份数据，字段错位会让「选错容器」静默发生。
func TestListMounts_ParsesMounts(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	f.containersJSON = []byte(`[
	  {"Id":"aaa111","Names":["/bbcc"],"State":"running",
	   "Mounts":[{"Type":"bind","Source":"/opt/bbcc","Destination":"/data","RW":true},
	             {"Type":"volume","Source":"/var/lib/docker/volumes/x/_data","Destination":"/cache","RW":true}]},
	  {"Id":"bbb222","Names":["/old"],"State":"exited","Mounts":[]}
	]`)
	c := New(sock)
	list, err := c.ListMounts(context.Background())
	if err != nil {
		t.Fatalf("ListMounts: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("容器数 = %d，期望 2", len(list))
	}
	if list[0].ID != "aaa111" || list[0].State != "running" || len(list[0].Mounts) != 2 {
		t.Fatalf("首个容器解析错误: %+v", list[0])
	}
	m := list[0].Mounts[0]
	if m.Type != "bind" || m.Source != "/opt/bbcc" || m.Destination != "/data" || !m.RW {
		t.Errorf("bind 挂载解析错误: %+v", m)
	}
	if list[1].ID != "bbb222" || len(list[1].Mounts) != 0 {
		t.Errorf("第二个容器解析错误: %+v", list[1])
	}
}

func bindMount(src, dst string, rw bool) Mount {
	return Mount{Type: "bind", Source: src, Destination: dst, RW: rw}
}

func ctr(id, state string, mounts ...Mount) ContainerMounts {
	return ContainerMounts{ID: id, State: state, Mounts: mounts}
}

// TestPickFrom_SubPathMapsIntoContainer 验证子路径映射：
// 宿主 /opt/bbcc/data 挂到容器 /data 时，对应容器内 /data/data。
func TestPickFrom_SubPathMapsIntoContainer(t *testing.T) {
	list := []ContainerMounts{ctr("aaa", "running", bindMount("/opt/bbcc", "/data", true))}

	got, err := PickFrom(list, "/opt/bbcc/data")
	if err != nil {
		t.Fatalf("PickFrom: %v", err)
	}
	if got.ContainerID != "aaa" || got.ContainerPath != "/data/data" {
		t.Errorf("borrow = %+v，期望 aaa → /data/data", got)
	}

	// 精确等于挂载源：容器内就是 Destination 本身。
	got, err = PickFrom(list, "/opt/bbcc")
	if err != nil {
		t.Fatalf("PickFrom(root): %v", err)
	}
	if got.ContainerPath != "/data" {
		t.Errorf("根路径映射 = %q，期望 /data", got.ContainerPath)
	}
}

// TestPickFrom_LongestSourceWins 验证最长前缀优先：
// 父目录挂载不能把子项目的访问引到错误的容器。
func TestPickFrom_LongestSourceWins(t *testing.T) {
	list := []ContainerMounts{
		ctr("parent", "running", bindMount("/opt", "/hostopt", true)),
		ctr("child", "running", bindMount("/opt/bbcc", "/data", true)),
	}
	got, err := PickFrom(list, "/opt/bbcc/www")
	if err != nil {
		t.Fatalf("PickFrom: %v", err)
	}
	if got.ContainerID != "child" || got.ContainerPath != "/data/www" {
		t.Errorf("borrow = %+v，期望 child → /data/www", got)
	}
}

// TestPickFrom_RunningPreferred 验证 running 优先：
// 同一目录被 running 与 exited 容器同时挂载时，选正在跑的那份。
func TestPickFrom_RunningPreferred(t *testing.T) {
	list := []ContainerMounts{
		ctr("exited-id", "exited", bindMount("/opt/bbcc", "/data", true)),
		ctr("running-id", "running", bindMount("/opt/bbcc", "/data", true)),
	}
	got, err := PickFrom(list, "/opt/bbcc/x")
	if err != nil {
		t.Fatalf("PickFrom: %v", err)
	}
	if got.ContainerID != "running-id" {
		t.Errorf("选中 %q，期望 running-id", got.ContainerID)
	}
}

// TestPickFrom_DeterministicByID 验证确定性：同优先级按容器 ID 字典序，
// 输入顺序变化不影响结果（面板刷新不能换借道对象）。
func TestPickFrom_DeterministicByID(t *testing.T) {
	a := ctr("aaa", "running", bindMount("/opt/bbcc", "/data", true))
	b := ctr("bbb", "running", bindMount("/opt/bbcc", "/data", true))
	for _, list := range [][]ContainerMounts{{a, b}, {b, a}} {
		got, err := PickFrom(list, "/opt/bbcc")
		if err != nil {
			t.Fatalf("PickFrom: %v", err)
		}
		if got.ContainerID != "aaa" {
			t.Errorf("输入顺序 %v 时选中 %q，期望 aaa", []string{list[0].ID, list[1].ID}, got.ContainerID)
		}
	}
}

// TestPickFrom_IgnoresNonBind 验证 volume / tmpfs 不参与借道：
// 它们的 Source 与用户看到的项目路径不是同一套命名。
func TestPickFrom_IgnoresNonBind(t *testing.T) {
	list := []ContainerMounts{
		ctr("vol", "running", Mount{Type: "volume", Source: "/opt/bbcc", Destination: "/data", RW: true}),
		ctr("tmp", "running", Mount{Type: "tmpfs", Source: "/opt/bbcc", Destination: "/data", RW: true}),
	}
	if _, err := PickFrom(list, "/opt/bbcc"); !errors.Is(err, ErrNoBorrower) {
		t.Fatalf("err = %v，期望 ErrNoBorrower", err)
	}
}

// TestPickFrom_NoBorrower 验证无覆盖时返回明确错误（调用方转「不可达」）。
func TestPickFrom_NoBorrower(t *testing.T) {
	list := []ContainerMounts{ctr("aaa", "running", bindMount("/opt/other", "/data", true))}
	if _, err := PickFrom(list, "/opt/bbcc"); !errors.Is(err, ErrNoBorrower) {
		t.Fatalf("err = %v，期望 ErrNoBorrower", err)
	}
	// 前缀相似但不能误判：/opt/bbcc2 不属于 /opt/bbcc。
	if _, err := PickFrom(list, "/opt/other2"); !errors.Is(err, ErrNoBorrower) {
		t.Fatalf("相似前缀 err = %v，期望 ErrNoBorrower", err)
	}
}

// TestPickFrom_ReadOnlyMarked 验证 :ro 挂载被标记，写操作可据此提前拒绝。
func TestPickFrom_ReadOnlyMarked(t *testing.T) {
	list := []ContainerMounts{ctr("aaa", "running", bindMount("/opt/bbcc", "/data", false))}
	got, err := PickFrom(list, "/opt/bbcc")
	if err != nil {
		t.Fatalf("PickFrom: %v", err)
	}
	if !got.ReadOnly {
		t.Error("只读挂载应标记 ReadOnly")
	}
}

// TestPickFrom_RejectsRelativePath 验证非绝对路径直接拒绝：
// 相对路径无法与挂载源做前缀比对，放过去只会匹配到奇怪的东西。
func TestPickFrom_RejectsRelativePath(t *testing.T) {
	list := []ContainerMounts{ctr("aaa", "running", bindMount("/opt/bbcc", "/data", true))}
	if _, err := PickFrom(list, "opt/bbcc"); err == nil {
		t.Fatal("相对路径应报错")
	} else if errors.Is(err, ErrNoBorrower) {
		t.Fatal("相对路径应是参数错误，而不是「不可达」")
	}
}
