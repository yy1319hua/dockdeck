// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.5：「误删存量」回归——把保护三重门在真实存量场景下的口径钉死。
//
// 背景（2026-10-15 用户定档）：保护名单缺省只有 `^dockdeck-` 一条；
// watchtower / komari 这类 TA 部署的存量容器**不进默认名单**（由 TA 自行删除），
// 运维若要保护须把它们追加进 settings.protected_containers。
//
// 与 S4.4 已有用例的分工：S4.4 证的是「门会拦」（400/403/500 与放行），
// 本文件证的是「拦下时存量真没被删」——每个用例都额外断言假 Agent 有没有
// 收到针对该容器的请求。只看状态码不够：Hub 漏转发同样会得到 4xx，
// 但那不等于存量安全。

package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/guard"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// stockDelete 对单个容器发一次删除请求，返回状态码、业务错误码，以及
// 假 Agent 是否收到过针对该容器的请求（true = 存量真的被删了）。
// protected 为空表示「不写 settings」，走 guard.DefaultPatterns 缺省名单。
func stockDelete(t *testing.T, protected, cid, confirm string) (int, string, bool) {
	t.Helper()
	h, sid, fa := hubWithAgent(t, true)
	tok := h.token(t, "rw", apitoken.ScopeWriteContainers)
	if protected != "" {
		if err := h.st.SetSetting(context.Background(), guard.SettingKey, protected); err != nil {
			t.Fatalf("写保护名单: %v", err)
		}
	}
	url := "/api/v1/servers/" + strconv.FormatInt(sid, 10) + "/containers/" + cid
	if confirm != "" {
		url += "?confirm=" + confirm
	}
	resp, body := h.do(t, http.MethodDelete, url, "", tok, "")

	reached := false
	for _, c := range fa.Calls() {
		if strings.Contains(c, cid) {
			reached = true
		}
	}
	return resp.StatusCode, errorCode(t, body), reached
}

// TestHubStockDefaultPatternsOnly 钉住缺省口径：只保护 `dockdeck-` 自身前缀，
// watchtower / komari 不进默认名单（2026-10-15 用户定档，ARCHITECTURE 已回填）。
func TestHubStockDefaultPatternsOnly(t *testing.T) {
	// 自身前缀受保护：confirm 完全正确也 403，且请求不出门。
	st, code, reached := stockDelete(t, "", "dockdeck-agent", "dockdeck-agent")
	if st != http.StatusForbidden || code != protocol.CodeForbidden {
		t.Errorf("缺省名单应保护 dockdeck-agent：状态 %d 码 %q", st, code)
	}
	if reached {
		t.Error("受保护的 dockdeck-agent 被转发到了 Agent——存量会被真删")
	}

	// 存量不在默认名单：confirm 正确即放行（删除与否由 TA 自行处理）。
	for _, cid := range []string{"watchtower", "komari"} {
		st, _, reached := stockDelete(t, "", cid, cid)
		if st != http.StatusOK {
			t.Errorf("%s 不进默认名单，confirm 正确应放行：状态 %d", cid, st)
		}
		if !reached {
			t.Errorf("%s 放行后没到达 Agent——转发链路断了", cid)
		}
	}
}

// TestHubStockEnrolledIsProtected 「误删存量」主线：存量录入名单后不可删——
// 即使 confirm 完全正确也 403，且请求根本不出门。
func TestHubStockEnrolledIsProtected(t *testing.T) {
	const list = `^watchtower$, ^komari$` // 故意带空格：顺带验 ParseList 去首尾空白
	for _, cid := range []string{"watchtower", "komari"} {
		st, code, reached := stockDelete(t, list, cid, cid)
		if st != http.StatusForbidden || code != protocol.CodeForbidden {
			t.Errorf("已录入名单的存量 %q 应 403：状态 %d 码 %q", cid, st, code)
		}
		if reached {
			t.Errorf("已录入名单的存量 %q 被转发到 Agent——会被真删", cid)
		}
	}

	// 对偶面：名单外的普通容器不受牵连（不该拦的别拦）。
	st, _, reached := stockDelete(t, list, "web1", "web1")
	if st != http.StatusOK {
		t.Errorf("名单外容器应放行：状态 %d", st)
	}
	if !reached {
		t.Error("名单外放行的删除没到达 Agent")
	}
}

// TestHubStockConfirmExact 存量删除的 confirm 必须与目标完全一致：
// 缺失 / 大小写不同 / 多字符 / 前缀相同 都不放行，且请求不出门。
func TestHubStockConfirmExact(t *testing.T) {
	for _, c := range []struct{ name, confirm string }{
		{"缺 confirm", ""},
		{"大小写不同", "WatchTower"},
		{"多后缀", "watchtower2"},
		{"只给前缀", "watch"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, code, reached := stockDelete(t, "", "watchtower", c.confirm)
			if st != http.StatusBadRequest || code != protocol.CodeBadRequest {
				t.Errorf("应 400：状态 %d 码 %q", st, code)
			}
			if reached {
				t.Errorf("confirm 不符却被转发——存量会被真删")
			}
		})
	}
}
