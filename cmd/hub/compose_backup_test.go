// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// 备份名用真实形态（RFC3339 带 +08:00 偏移）——「含 + 与 :」正是本模块要
// 处理的东西：它走查询串/请求体而不是路径段，就是因为放进路径段要处理
// 编码差异（S7.5 已踩过项目名与镜像 ref 的同类坑）。
const (
	bakOld = "docker-compose.yml.bak.2026-10-02T18:00:00+08:00"
	bakNew = "docker-compose.yml.bak.2026-10-03T09:00:00+08:00"
)

// TestComposeBackupsEndpoints 是 Hub→agentcli→Agent 的整链路：清单、读正文、
// 恢复三端点都要通，且**备份名必须原样抵达 Agent**（含 + 与 : 一个字符都不能少）。
func TestComposeBackupsEndpoints(t *testing.T) {
	h, id, fa := hubWithAgent(t, true)
	fa.SetComposeBackups([]protocol.ComposeBackup{
		{Name: bakNew, Size: 210, ModTime: "2026-10-03T09:00:00Z"},
		{Name: bakOld, Size: 180, ModTime: "2026-10-02T18:00:00Z"},
	})
	fa.SetBackupContents(map[string]string{bakOld: "services:\n  web:\n    image: nginx:1.24\n"})
	fa.SetBackupRestored(protocol.ComposeBackupRestored{Backup: "docker-compose.yml.bak.2026-10-05T11:00:00+08:00"})

	cookie := h.session(t)
	base := "/api/v1/servers/" + strconv.FormatInt(id, 10) + "/compose/web"

	// ---- 清单（只读）----
	resp, body := h.get(t, base+"/backups", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backups 应 200，得 %d｜%s", resp.StatusCode, body)
	}
	var list struct {
		Backups []protocol.ComposeBackup `json:"backups"`
	}
	decodeData(t, body, &list)
	if len(list.Backups) != 2 {
		t.Fatalf("备份数 = %d, want 2", len(list.Backups))
	}
	if list.Backups[0].Name != bakNew || list.Backups[1].Name != bakOld {
		t.Errorf("清单顺序/内容不符（须时间倒序）: %+v", list.Backups)
	}
	// Q4-A 回归：响应里不得出现绝对路径（不得含目录分隔符）。
	for _, b := range list.Backups {
		if strings.ContainsAny(b.Name, `/\`) {
			t.Errorf("备份名含路径分隔符（泄漏宿主机路径）: %q", b.Name)
		}
	}
	if strings.Contains(string(body), "/tmp/") || strings.Contains(string(body), "/var/") {
		t.Errorf("响应里出现了宿主机绝对路径: %s", body)
	}

	// ---- 读正文（备份名含 + 与 :，必须原样抵达）----
	resp, body = h.get(t, base+"/backups/content?file="+url.QueryEscape(bakOld), "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backups/content 应 200，得 %d｜%s", resp.StatusCode, body)
	}
	var content protocol.ComposeBackupContent
	decodeData(t, body, &content)
	if content.Name != bakOld {
		t.Errorf("备份名透传不符: %q, want %q（+ 与 : 被吃掉说明没转义）", content.Name, bakOld)
	}
	if !strings.Contains(content.Content, "nginx:1.24") {
		t.Errorf("备份正文不符: %q", content.Content)
	}
	if fa.LastBackupName() != bakOld {
		t.Errorf("Agent 收到的备份名 = %q, want %q", fa.LastBackupName(), bakOld)
	}

	// ---- 恢复（覆盖写）----
	req, _ := json.Marshal(protocol.ComposeBackupUpdate{File: bakOld})
	resp, body = h.do(t, http.MethodPost, base+"/backups/restore", string(req), "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("backups/restore 应 200，得 %d｜%s", resp.StatusCode, body)
	}
	var restored protocol.ComposeBackupRestored
	decodeData(t, body, &restored)
	if restored.Backup != "docker-compose.yml.bak.2026-10-05T11:00:00+08:00" {
		t.Errorf("恢复响应 backup = %q（应是恢复前新备份的文件名）", restored.Backup)
	}
	if fa.LastBackupName() != bakOld {
		t.Errorf("Agent 收到的恢复备份名 = %q, want %q", fa.LastBackupName(), bakOld)
	}
}

// TestComposeBackupsRestoreAudit 钉住恢复落审计——恢复是覆盖写，是审计表里
// 一等公民；同时也顺带确认清单也要留痕（本仓库读+写全量审计）。
func TestComposeBackupsRestoreAudit(t *testing.T) {
	h, id, fa := hubWithAgent(t, true)
	fa.SetComposeBackups([]protocol.ComposeBackup{{Name: bakOld, Size: 1}})
	fa.SetBackupRestored(protocol.ComposeBackupRestored{Backup: bakNew})

	cookie := h.session(t)
	before := len(auditRows(t, h))
	base := "/api/v1/servers/" + strconv.FormatInt(id, 10) + "/compose/web"

	h.get(t, base+"/backups", "", cookie)
	req, _ := json.Marshal(protocol.ComposeBackupUpdate{File: bakOld})
	h.do(t, http.MethodPost, base+"/backups/restore", string(req), "", cookie)

	rows := auditRows(t, h)
	added := rows[:len(rows)-before] // 倒序，新增在前
	if len(added) != 2 {
		t.Fatalf("新增审计行 = %d, want 2", len(added))
	}
	if added[0].Action != "compose.backups.restore" || added[1].Action != "compose.backups.list" {
		t.Errorf("审计动作不符（倒序应为 restore, list）: %q, %q", added[0].Action, added[1].Action)
	}
	for _, r := range added {
		if r.ServerID != id {
			t.Errorf("审计行 server_id = %d, want %d", r.ServerID, id)
		}
	}
}

// TestComposeBackupsAuth 鉴权矩阵：清单/读正文要 read:containers，
// **恢复要 write:containers**（它是覆盖写，与写 YAML 同款）——
// 只读 Token 不得借恢复改写 compose 文件。
func TestComposeBackupsAuth(t *testing.T) {
	h, id, fa := hubWithAgent(t, true)
	fa.SetComposeBackups([]protocol.ComposeBackup{{Name: bakOld, Size: 1}})
	fa.SetBackupRestored(protocol.ComposeBackupRestored{Backup: bakNew})
	base := "/api/v1/servers/" + strconv.FormatInt(id, 10) + "/compose/web"
	req, _ := json.Marshal(protocol.ComposeBackupUpdate{File: bakOld})

	// 会话直行（ServeSession 不查 scope）。
	if resp, body := h.get(t, base+"/backups", "", h.session(t)); resp.StatusCode != http.StatusOK {
		t.Errorf("会话读清单应 200，得 %d｜%s", resp.StatusCode, body)
	}
	readOnly := h.token(t, "ro", apitoken.ScopeReadContainers)
	// 只读 Token 能看清单…
	if resp, body := h.get(t, base+"/backups", readOnly, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("只读 Token 读清单应 200，得 %d｜%s", resp.StatusCode, body)
	}
	// …但不能恢复。
	if resp, body := h.do(t, http.MethodPost, base+"/backups/restore", string(req), readOnly, ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("只读 Token 恢复应 403，得 %d｜%s", resp.StatusCode, body)
	}
	// 带写权限的 Token 可以恢复。
	writer := h.token(t, "ops", apitoken.ScopeWriteContainers)
	if resp, body := h.do(t, http.MethodPost, base+"/backups/restore", string(req), writer, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("写 Token 恢复应 200，得 %d｜%s", resp.StatusCode, body)
	}
	// 未认证 401。
	if resp, _ := h.get(t, base+"/backups", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未认证应 401，得 %d", resp.StatusCode)
	}
}
