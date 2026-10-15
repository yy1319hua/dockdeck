// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
)

// TestInviteAndEnrollOverHTTP 覆盖 GAP-1 纳管两阶段的全链路：
// invite（仅会话）→ enroll（join_secret）→ servers 列表可见且不泄密 → ServerConn 可解密。
func TestInviteAndEnrollOverHTTP(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)

	// invite 仅会话：匿名与 Token 都 401。
	if resp, _ := h.do(t, http.MethodPost, "/api/v1/servers/invite", `{"name_hint":"web-01"}`, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("匿名 invite 应 401，得 %d", resp.StatusCode)
	}
	tok := h.token(t, "app", apitoken.AllScopes...)
	if resp, _ := h.do(t, http.MethodPost, "/api/v1/servers/invite", `{}`, tok, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Token invite 应 401（session-only），得 %d", resp.StatusCode)
	}

	// 会话签发 → 一次性 join_secret（明文仅此一次）。
	resp, body := h.do(t, http.MethodPost, "/api/v1/servers/invite", `{"name_hint":"web-01","ttl_min":30}`, "", cookie)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("invite 应 201，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var inv struct {
		Data struct {
			JoinSecret string `json:"join_secret"`
			ExpiresIn  int    `json:"expires_in"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &inv); err != nil {
		t.Fatalf("解析 invite 响应: %v｜%s", err, trim(body))
	}
	if inv.Data.JoinSecret == "" || inv.Data.ExpiresIn != 1800 {
		t.Fatalf("invite 响应不对：%+v", inv.Data)
	}

	// enroll：无 Bearer / 坏 secret 都 401。
	if resp, _ := h.do(t, http.MethodPost, "/api/v1/servers/enroll",
		`{"name":"web-01","base_url":"https://10.0.0.5:9527"}`, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 join_secret 应 401，得 %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/v1/servers/enroll",
		`{"name":"web-01","base_url":"https://10.0.0.5:9527"}`, "bogus", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("坏 join_secret 应 401，得 %d", resp.StatusCode)
	}

	// 正确 enroll → 201，返回服务器 id。
	resp, body = h.do(t, http.MethodPost, "/api/v1/servers/enroll",
		`{"name":"web-01","base_url":"https://10.0.0.5:9527"}`, inv.Data.JoinSecret, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("enroll 应 201，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var en struct {
		Data struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &en); err != nil {
		t.Fatalf("解析 enroll 响应: %v｜%s", err, trim(body))
	}
	if en.Data.ID == 0 || en.Data.Name != "web-01" {
		t.Fatalf("enroll 响应不对：%+v", en.Data)
	}

	// 幂等重放 → 同 id。
	resp, body = h.do(t, http.MethodPost, "/api/v1/servers/enroll",
		`{"name":"web-01","base_url":"https://10.0.0.9:9527"}`, inv.Data.JoinSecret, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("重放 enroll 应 201，得 %d｜%s", resp.StatusCode, trim(body))
	}
	var en2 struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &en2)
	if en2.Data.ID != en.Data.ID {
		t.Fatalf("重放应返回同 id，得 %d vs %d", en2.Data.ID, en.Data.ID)
	}

	// 列表可见、base_url 已刷新，且绝不泄漏 join_secret。
	_, lb := h.get(t, "/api/v1/servers", "", cookie)
	if !strings.Contains(string(lb), "web-01") || !strings.Contains(string(lb), "10.0.0.9") {
		t.Fatalf("列表应含 web-01 且 base_url 已刷新：%s", trim(lb))
	}
	if strings.Contains(string(lb), inv.Data.JoinSecret) {
		t.Fatal("列表泄漏了 join_secret")
	}

	// 纳管后 join_secret 即该服务器的 Agent 令牌，Hub 可解密使用。
	conn, err := h.st.ServerConn(context.Background(), en.Data.ID)
	if err != nil {
		t.Fatalf("ServerConn: %v", err)
	}
	if conn.Token != inv.Data.JoinSecret {
		t.Fatal("服务器令牌应等于 join_secret")
	}
}

// TestEnrollExpiredInviteGone 覆盖过期邀请的 410 分支。
func TestEnrollExpiredInviteGone(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)
	// ttl_min 上限 24h，这里用最小正数；随后直接把库里的 expires_at 改到过去模拟过期。
	_, body := h.do(t, http.MethodPost, "/api/v1/servers/invite", `{"ttl_min":30}`, "", cookie)
	var inv struct {
		Data struct {
			JoinSecret string `json:"join_secret"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &inv)
	if inv.Data.JoinSecret == "" {
		t.Fatalf("应签发成功：%s", trim(body))
	}
	// 只改 expires_at，不动 join_hash：enroll 时该邀请仍未认领但已过期。
	if _, err := h.st.DB.ExecContext(context.Background(),
		`UPDATE server_invites SET expires_at=strftime('%Y-%m-%dT%H:%M:%SZ','now','-1 minute')`); err != nil {
		t.Fatalf("改过期: %v", err)
	}
	resp, eb := h.do(t, http.MethodPost, "/api/v1/servers/enroll",
		`{"name":"old","base_url":"https://o:1"}`, inv.Data.JoinSecret, "")
	if resp.StatusCode != http.StatusGone || errorCode(t, eb) != "invite_expired" {
		t.Fatalf("过期邀请应 410 invite_expired，得 %d｜%s", resp.StatusCode, trim(eb))
	}
}
