// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"strings"
	"testing"
)

// TestListVolumes 钉住 /volumes 的**对象包数组**形态与两个易错点：
//  1. 外层是 {"Volumes":[...],"Warnings":...}——直接解成数组会静默得空清单；
//  2. UsageData 为 null 时必须是 nil 指针，不能退化成零值：
//     「Engine 没统计」被读成「0 个容器在用」会让用户误判卷可删。
func TestListVolumes(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)
	f.volumes = []byte(`{
		"Volumes": [
			{"Name":"pgdata","Driver":"local","Scope":"local","CreatedAt":"2026-01-02T03:04:05.123456789Z",
			 "Labels":{"app":"db"},"Options":{},"UsageData":{"Size":104857600,"RefCount":1}},
			{"Name":"unused","Driver":"local","Scope":"local","CreatedAt":"2026-02-03T04:05:06Z",
			 "Labels":null,"Options":null,"UsageData":null}
		],
		"Warnings": null
	}`)

	list, err := c.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2（外层对象包数组没解开的话这里是 0）", len(list))
	}
	first := list[0]
	if first.Name != "pgdata" || first.Driver != "local" || first.Scope != "local" {
		t.Errorf("首条基础字段解码不符: %+v", first)
	}
	if first.Labels["app"] != "db" {
		t.Errorf("Labels 解码不符: %+v", first.Labels)
	}
	if first.UsageData == nil {
		t.Fatalf("首条 UsageData 应为非 nil")
	}
	if first.UsageData.Size != 104857600 || first.UsageData.RefCount != 1 {
		t.Errorf("UsageData 解码不符: %+v", first.UsageData)
	}

	// 关键反例：UsageData 为 null → nil 指针（不是 &{0,0}）。
	if list[1].UsageData != nil {
		t.Errorf("UsageData 为 null 时应解成 nil，got %+v（非 nil 会把「没统计」误报成「0 个在用」）", list[1].UsageData)
	}
	// Labels/Options 为 null → nil map，取键安全、len 为 0。
	if len(list[1].Labels) != 0 || len(list[1].Options) != 0 {
		t.Errorf("null 的 Labels/Options 应解成 nil map: %+v %+v", list[1].Labels, list[1].Options)
	}

	uri := f.hitPaths()[len(f.hitPaths())-1]
	if !strings.Contains(uri, "/volumes") {
		t.Errorf("请求路径不对: %s", uri)
	}
}

// TestListVolumes_Empty 钉住「无卷」时 Engine 给的对象包空数组：
// Volumes 为 []、Warnings 为 null——两者都不得让解码报错或产出 nil 之外的怪值。
func TestListVolumes_Empty(t *testing.T) {
	// f 不需要：本用例走假引擎的缺省响应（volumes 为 nil → 对象包空数组）。
	_, sock := newFakeEngine(t, "1.51")
	c := New(sock)

	list, err := c.ListVolumes(context.Background())
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("len = %d, want 0", len(list))
	}
}

// 样本里的地址段用 RFC 5737 文档段（203.0.113.0/24）而非 Docker 真实默认的
// 网桥私有段：后者落在内网 IP 形态里，会被 pre-commit 的凭据扫描拦下——那是
// 误报（公开文档值、不是本项目任何服务器地址），但规矩不允许 --no-verify 绕过，
// 故改用文档段。本用例只验证字段透传、不验证地址本身，故不损失断言价值。

// TestListNetworks 钉住 /networks 的**裸数组**形态（与 /volumes 的对象包数组
// 相反）。
//
// 接入容器名单的来源钉在**详情端点**：Engine 29 起清单端点的 Containers 一律
// 为 null，名单只在 GET /networks/{id} 给。本用例的清单响应刻意把 Containers
// 写成 null——若实现退回复制清单里的 Containers，这条测试会红（它断言名单
// 来自详情）。这正是线上「面板一律显示未接入容器」的成因，必须钉住。
func TestListNetworks(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)
	// 清单：Containers 为 null（Engine 29 实况）。
	f.networks = []byte(`[
		{"Id":"netaaa","Name":"bridge","Driver":"bridge","Scope":"local","Created":"2026-01-02T03:04:05Z",
		 "Internal":false,"Attachable":false,"Ingress":false,"EnableIPv6":false,"Labels":{},
		 "IPAM":{"Driver":"default","Config":[{"Subnet":"203.0.113.0/24","Gateway":"203.0.113.1"}]},
		 "Containers":null},
		{"Id":"netbbb","Name":"orphan","Driver":"bridge","Scope":"local","Created":"2026-02-03T04:05:06Z",
		 "Internal":true,"Attachable":false,"Ingress":false,"EnableIPv6":false,"Labels":null,
		 "IPAM":{"Driver":"default","Config":null},"Containers":null}
	]`)
	// 详情：只有 netaaa 有接入容器；netbbb 无（详情也返回 null）。
	f.networkDetails = map[string][]byte{
		"netaaa": []byte(`{"Id":"netaaa","Name":"bridge","Driver":"bridge","Scope":"local",
		 "Created":"2026-01-02T03:04:05Z","Internal":false,"Attachable":false,"Ingress":false,
		 "EnableIPv6":false,"Labels":{},"IPAM":{"Driver":"default","Config":null},
		 "Containers":{"cid111":{"Name":"web","EndpointID":"e1","MacAddress":"02:42:ac:11:00:02",
		                         "IPv4Address":"203.0.113.2/24","IPv6Address":""}}}`),
		"netbbb": []byte(`{"Id":"netbbb","Name":"orphan","Driver":"bridge","Scope":"local",
		 "Created":"2026-02-03T04:05:06Z","Internal":true,"Attachable":false,"Ingress":false,
		 "EnableIPv6":false,"Labels":null,"IPAM":{"Driver":"default","Config":null},"Containers":null}`),
	}

	list, err := c.ListNetworks(context.Background())
	if err != nil {
		t.Fatalf("ListNetworks: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	first := list[0]
	if first.Name != "bridge" || first.ID != "netaaa" || first.Driver != "bridge" {
		t.Errorf("首条基础字段解码不符: %+v", first)
	}
	if len(first.IPAM.Config) != 1 || first.IPAM.Config[0].Subnet != "203.0.113.0/24" {
		t.Errorf("IPAM.Config 解码不符: %+v", first.IPAM)
	}
	// 接入容器名单：来自详情端点，键是容器 ID。
	c1, ok := first.Containers["cid111"]
	if !ok {
		t.Fatalf("Containers 缺 cid111（名单应来自详情端点）: %+v", first.Containers)
	}
	if c1.Name != "web" || c1.IPv4Address != "203.0.113.2/24" || c1.MacAddress != "02:42:ac:11:00:02" {
		t.Errorf("接入容器条目解码不符: %+v", c1)
	}

	// 详情里 Containers 为 null → nil map：range 零次迭代，前端「无容器接入」分支。
	if len(list[1].Containers) != 0 {
		t.Errorf("Containers 为 null 应解成 nil map, got %+v", list[1].Containers)
	}
	if !list[1].Internal {
		t.Errorf("Internal 解码不符: %+v", list[1])
	}

	// 必须打清单 + 每个网络各一次详情。
	paths := f.hitPaths()
	var sawList, sawDetail bool
	for _, u := range paths {
		if strings.Contains(u, "/networks/netaaa") {
			sawDetail = true
		}
		if strings.Contains(u, "/networks") && !strings.Contains(u, "/networks/") {
			sawList = true
		}
	}
	if !sawList {
		t.Errorf("没请求网络清单端点: %v", paths)
	}
	if !sawDetail {
		t.Errorf("没请求网络详情端点（接入名单就无从取得）: %v", paths)
	}
}

// TestListNetworks_DetailFailsKeepsMetadata 钉住「单个详情失败不拖垮整次列举」：
// 详情拿不到只丢该行的接入名单，网络元信息（名称/子网）仍要拿得到——
// 否则 Engine 一抖动，整页就空了。
func TestListNetworks_DetailFailsKeepsMetadata(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)
	f.networks = []byte(`[
		{"Id":"netccc","Name":"app","Driver":"bridge","Scope":"local","Created":"2026-03-04T05:06:07Z",
		 "Internal":false,"Attachable":false,"Ingress":false,"EnableIPv6":false,"Labels":{},
		 "IPAM":{"Driver":"default","Config":[{"Subnet":"198.51.100.0/24"}]},"Containers":null}
	]`)
	// 不注册 netccc 的详情 → 详情端点 404。

	list, err := c.ListNetworks(context.Background())
	if err != nil {
		t.Fatalf("详情失败不该让整次列举报错: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d, want 1", len(list))
	}
	if list[0].Name != "app" {
		t.Errorf("详情失败时元信息应保留: %+v", list[0])
	}
	if len(list[0].IPAM.Config) != 1 {
		t.Errorf("详情失败时子网应保留: %+v", list[0].IPAM)
	}
	if len(list[0].Containers) != 0 {
		t.Errorf("详情失败时不应编造接入名单: %+v", list[0].Containers)
	}
}
