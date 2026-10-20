// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
)

// TestDeleteServerOverHTTP 覆盖删除端点的鉴权口径（仅会话）、级联效果与审计留痕。
func TestDeleteServerOverHTTP(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)

	// 纳管一台（invite→enroll 真实链路）。
	resp, body := h.do(t, http.MethodPost, "/api/v1/servers/invite", `{"name_hint":"web-01","ttl_min":30}`, "", cookie)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("invite 应 201，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var inv struct {
		Data struct {
			JoinSecret string `json:"join_secret"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatalf("解析 invite: %v", err)
	}
	resp, body = h.do(t, http.MethodPost, "/api/v1/servers/enroll",
		`{"name":"web-01","base_url":"https://10.0.0.5:9527","arch":"amd64","docker_ver":"29.1.3"}`, inv.Data.JoinSecret, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("enroll 应 201，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var en struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &en); err != nil {
		t.Fatalf("解析 enroll: %v", err)
	}
	path := "/api/v1/servers/" + strconv.FormatInt(en.Data.ID, 10)

	// 匿名 401、API Token 401（session-only，与 invite 同口径）。
	if resp, _ := h.do(t, http.MethodDelete, path, "", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("匿名删除应 401，得 %d", resp.StatusCode)
	}
	tok := h.token(t, "app", apitoken.AllScopes...)
	if resp, _ := h.do(t, http.MethodDelete, path, "", tok, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Token 删除应 401（session-only），得 %d", resp.StatusCode)
	}

	// 会话删除 → 200，列表不再有它。
	resp, body = h.do(t, http.MethodDelete, path, "", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除应 200，得 %d｜%s", resp.StatusCode, trim(body))
	}
	resp, body = h.do(t, http.MethodGet, "/api/v1/servers", "", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("列表应 200，得 %d", resp.StatusCode)
	}
	if n := strings.Count(string(body), `"web-01"`); n != 0 {
		t.Fatalf("删除后列表仍含 web-01（%d 次）", n)
	}

	// 再删 → 404。
	if resp, _ := h.do(t, http.MethodDelete, path, "", "", cookie); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复删除应 404，得 %d", resp.StatusCode)
	}

	// 审计留痕：server.delete 一行 ok，server_id 正确。
	var cnt int
	if err := h.st.DB.QueryRow(
		`SELECT COUNT(1) FROM audit_logs WHERE action=? AND server_id=? AND result=?`,
		"server.delete", en.Data.ID, "ok").Scan(&cnt); err != nil {
		t.Fatalf("查审计: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("审计应有 1 条 server.delete(ok)，得 %d", cnt)
	}
}
