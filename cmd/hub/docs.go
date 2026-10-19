// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"net/http"

	"github.com/yy1319hua/dockdeck/internal/apidocs"
)

// 文档三件套（PLAN-NODE-DOCS B）：公开只读，不含任何敏感信息。
//
// 为什么走 mux 而不是 route.Table：
//   - route.Table 是「JSON API 契约表」，会被生成进 spec，文档端点进 spec 只会
//     形成自引用，而且它们的响应不是信封 JSON；
//   - route.Table.Add 强制 /api/ 前缀，而文档要挂在 /docs（给人看的路径）。
//
// 与 Agent 反向连接握手同类：有意的、已审的例外，由
// TestNoRoutesBypassingTable 的白名单显式豁免。
//
// 三者都从同一份 spec 生成，路由增减时文档自动跟上——不存在「改了代码忘改文档」。
// Cache-Control: no-store 让面板升级后立刻看到新文档，避免代理缓存旧版误导。

func (s *server) docsSpec(w http.ResponseWriter, r *http.Request) {
	b, err := s.spec().JSON()
	if err != nil {
		http.Error(w, "spec 生成失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (s *server) docsLLMS(w http.ResponseWriter, r *http.Request) {
	body := apidocs.LLMSTxt(s.spec())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func (s *server) docsAPIMD(w http.ResponseWriter, r *http.Request) {
	body := apidocs.APIMD(s.spec())
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}
