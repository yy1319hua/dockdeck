// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// resEnvelope 只取 data 原样：Hub 侧信封是 {data:...}，具体类型由各用例自己解。
// 不用 httpx.Envelope——它的 Data 是 interface{}，解码后是 []interface{}，
// 断言不到字段。
type resEnvelope struct {
	Data json.RawMessage `json:"data"`
}

func decodeData(t *testing.T, body []byte, dst interface{}) {
	t.Helper()
	var env resEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析信封失败: %v｜%s", err, body)
	}
	if len(env.Data) == 0 {
		t.Fatalf("响应无 data 段: %s", body)
	}
	if err := json.Unmarshal(env.Data, dst); err != nil {
		t.Fatalf("解析 data 失败: %v｜%s", err, body)
	}
}

// TestResourcesForward 是 Hub→agentcli→Agent 的整链路透传：
// 两条只读端点都要 200，且 Engine 的两个关键形态必须原样抵达前端——
// ① 卷的 UsageData 为 null 时仍是 null（不是 0），② 网络的 Containers 名单完整
// （这是「谁接入了该网络」零额外请求的前提）。
func TestResourcesForward(t *testing.T) {
	h, id, fa := hubWithAgent(t, true)
	fa.SetVolumeList([]protocol.VolumeSummary{
		{
			Name: "pgdata", Driver: "local", Scope: "local",
			Labels:    map[string]string{"app": "db"},
			UsageData: &protocol.VolumeUsageData{Size: 104857600, RefCount: 1},
		},
		// UsageData 留 nil：模拟 Engine 未统计的卷。
		{Name: "unused", Driver: "local", Scope: "local"},
	})
	fa.SetNetworkList([]protocol.NetworkSummary{
		{
			ID: "netaaa", Name: "bridge", Driver: "bridge",
			IPAM: protocol.NetworkIPAM{
				Driver: "default",
				Config: []protocol.NetworkIPAMConfig{{Subnet: "203.0.113.0/24", Gateway: "203.0.113.1"}},
			},
			Containers: map[string]protocol.NetworkContainer{
				"cid111": {Name: "web", IPv4Address: "203.0.113.2/24", MacAddress: "02:42:ac:11:00:02"},
			},
		},
		// Containers 留 nil：无容器接入的网络。
		{ID: "netbbb", Name: "orphan", Driver: "bridge", Internal: true},
	})

	cookie := h.session(t)
	base := "/api/v1/servers/" + strconv.FormatInt(id, 10)

	// ---- 卷 ----
	resp, body := h.get(t, base+"/volumes", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("volumes 应 200，得 %d｜%s", resp.StatusCode, body)
	}
	var vols []protocol.VolumeSummary
	decodeData(t, body, &vols)
	if len(vols) != 2 {
		t.Fatalf("卷数 = %d, want 2", len(vols))
	}
	if vols[0].Name != "pgdata" || vols[0].Driver != "local" || vols[0].Labels["app"] != "db" {
		t.Errorf("卷首条透传不符: %+v", vols[0])
	}
	if vols[0].UsageData == nil || vols[0].UsageData.Size != 104857600 || vols[0].UsageData.RefCount != 1 {
		t.Errorf("UsageData 透传不符: %+v", vols[0].UsageData)
	}
	// 关键：JSON 往返后 null 必须还是 null——变成 &{0,0} 会让 UI 把
	// 「Engine 没统计」显示成「0 个容器在用」，用户据此误判卷可删。
	if vols[1].UsageData != nil {
		t.Errorf("UsageData 为 null 的卷经往返后应仍为 nil，got %+v", vols[1].UsageData)
	}

	// ---- 网络 ----
	resp, body = h.get(t, base+"/networks", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("networks 应 200，得 %d｜%s", resp.StatusCode, body)
	}
	var nets []protocol.NetworkSummary
	decodeData(t, body, &nets)
	if len(nets) != 2 {
		t.Fatalf("网络数 = %d, want 2", len(nets))
	}
	if nets[0].Name != "bridge" || nets[0].ID != "netaaa" {
		t.Errorf("网络首条透传不符: %+v", nets[0])
	}
	if len(nets[0].IPAM.Config) != 1 || nets[0].IPAM.Config[0].Subnet != "203.0.113.0/24" {
		t.Errorf("IPAM 透传不符: %+v", nets[0].IPAM)
	}
	c1, ok := nets[0].Containers["cid111"]
	if !ok {
		t.Fatalf("接入容器名单丢失: %+v", nets[0].Containers)
	}
	if c1.Name != "web" || c1.IPv4Address != "203.0.113.2/24" {
		t.Errorf("接入容器条目透传不符: %+v", c1)
	}
	if len(nets[1].Containers) != 0 || !nets[1].Internal {
		t.Errorf("无容器接入的网络透传不符: %+v", nets[1])
	}
}

// TestResourcesAudit 钉住读端点也留痕（与 container.list / image.list 同口径：
// 本仓库 S3.6 起读+写全量审计）。区别于审计查询与设置读端点——那两处刻意不审，
// 理由是高频读会淹掉真操作记录；卷网络读是人工点开才发生，同镜像清单。
func TestResourcesAudit(t *testing.T) {
	h, id, fa := hubWithAgent(t, true)
	fa.SetVolumeList([]protocol.VolumeSummary{{Name: "v1"}})
	fa.SetNetworkList([]protocol.NetworkSummary{{Name: "n1"}})

	// 会话只取一次并复用：每次 h.session(t) 都是一次真实登录，自身会落一条
	// auth.login——取三次就会多出两条无关审计行，把「端点各留痕一次」的断言搅浑。
	cookie := h.session(t)
	before := len(auditRows(t, h))

	base := "/api/v1/servers/" + strconv.FormatInt(id, 10)
	h.get(t, base+"/volumes", "", cookie)
	h.get(t, base+"/networks", "", cookie)

	rows := auditRows(t, h)
	added := rows[:len(rows)-before] // auditRows 按 id 倒序，新增在前
	if len(added) != 2 {
		t.Fatalf("新增审计行 = %d, want 2", len(added))
	}
	if added[0].Action != "network.list" || added[1].Action != "volume.list" {
		t.Errorf("审计动作不符（倒序应为 network.list, volume.list）: %q, %q", added[0].Action, added[1].Action)
	}
	for _, r := range added {
		if r.ServerID != id {
			t.Errorf("审计行 server_id = %d, want %d", r.ServerID, id)
		}
		if r.Result != "ok" {
			t.Errorf("审计行 result = %q, want ok", r.Result)
		}
	}
}

// TestResourcesAuth 鉴权矩阵：会话直行（ServeSession 不查 scope），
// Token 按 scope 受限——泄漏的只读 Token 不该顺手看清全站卷与网络。
func TestResourcesAuth(t *testing.T) {
	h, id, fa := hubWithAgent(t, true)
	fa.SetVolumeList([]protocol.VolumeSummary{{Name: "v1"}})
	base := "/api/v1/servers/" + strconv.FormatInt(id, 10) + "/volumes"

	if resp, body := h.get(t, base, "", h.session(t)); resp.StatusCode != http.StatusOK {
		t.Errorf("会话用户应 200，得 %d｜%s", resp.StatusCode, body)
	}
	// 无 read:containers 的 Token → 403 scope_denied。
	noScope := h.token(t, "ro", apitoken.ScopeReadMetrics)
	if resp, body := h.get(t, base, noScope, ""); resp.StatusCode != http.StatusForbidden || errorCode(t, body) != "scope_denied" {
		t.Errorf("无 read:containers 的 Token 应 403 scope_denied，得 %d｜%s", resp.StatusCode, body)
	}
	withScope := h.token(t, "ops", apitoken.ScopeReadContainers)
	if resp, body := h.get(t, base, withScope, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("带 read:containers 的 Token 应 200，得 %d｜%s", resp.StatusCode, body)
	}
	if resp, _ := h.get(t, base, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未认证应 401，得 %d", resp.StatusCode)
	}
}

// TestResourcesServerStates 钉住两个非 200 分支：禁用服务器一律拒绝实时转发
// （向已下线的 Agent 发请求没意义），非法 ID 在路由层就拦掉。
func TestResourcesServerStates(t *testing.T) {
	h, id, _ := hubWithAgent(t, false)
	base := "/api/v1/servers/" + strconv.FormatInt(id, 10)
	if resp, body := h.get(t, base+"/volumes", "", h.session(t)); resp.StatusCode != http.StatusForbidden {
		t.Errorf("禁用服务器的卷清单应 403，得 %d｜%s", resp.StatusCode, body)
	}
	if resp, body := h.get(t, base+"/networks", "", h.session(t)); resp.StatusCode != http.StatusForbidden {
		t.Errorf("禁用服务器的网络清单应 403，得 %d｜%s", resp.StatusCode, body)
	}

	h2 := newHub(t)
	if resp, body := h2.get(t, "/api/v1/servers/abc/volumes", "", h2.session(t)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法服务器 ID 应 400，得 %d｜%s", resp.StatusCode, body)
	}
}
