// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.3：Compose 文件读写客户端链路（ComposeFileGet/Diff/Put）。
// 关注点：方法与路径是否与 Agent 路由一字不差、请求体是否经信封 JSON 出门、
// 响应信封内层是否正确解码，以及"空项目名本地拦、不出门"。

package agentcli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

func TestClient_ComposeFileGet(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.RequestURI()
		writeEnvelope(w, http.StatusOK, protocol.ComposeFile{
			ConfigFile: "/opt/watchtower/docker-compose.yml",
			Content:    "services:\n  watchtower:\n",
		})
	}))
	defer srv.Close()

	got, err := quickClient(srv).ComposeFileGet(context.Background(), "watchtower")
	if err != nil {
		t.Fatalf("ComposeFileGet: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/v1/compose/watchtower/file" {
		t.Errorf("方法与路径不符 Agent 路由：%s %s", gotMethod, gotPath)
	}
	if got.ConfigFile != "/opt/watchtower/docker-compose.yml" || got.Content != "services:\n  watchtower:\n" {
		t.Errorf("信封内层解码不符：%+v", got)
	}
}

func TestClient_ComposeFileDiff(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeEnvelope(w, http.StatusOK, protocol.ComposeFileDiff{Diff: "@@ -1,1 +1,1 @@\n-a\n+b\n"})
	}))
	defer srv.Close()

	got, err := quickClient(srv).ComposeFileDiff(context.Background(), "watchtower", "b\n")
	if err != nil {
		t.Fatalf("ComposeFileDiff: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/compose/watchtower/diff" {
		t.Errorf("方法与路径不符 Agent 路由：%s %s", gotMethod, gotPath)
	}
	// 新内容经 {"content": ...} 出门。
	if !strings.Contains(gotBody, `"content"`) {
		t.Errorf("请求体应含 content 字段：%s", gotBody)
	}
	if got.Diff != "@@ -1,1 +1,1 @@\n-a\n+b\n" {
		t.Errorf("diff 解码不符：%q", got.Diff)
	}
}

func TestClient_ComposeFilePut(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		writeEnvelope(w, http.StatusOK, protocol.ComposeFileWritten{
			BackupFile: "/opt/watchtower/docker-compose.yml.bak.2026-10-15T00:50:00+08:00",
		})
	}))
	defer srv.Close()

	got, err := quickClient(srv).ComposeFilePut(context.Background(), "watchtower", "services:\n  watchtower:\n")
	if err != nil {
		t.Fatalf("ComposeFilePut: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/compose/watchtower/file" {
		t.Errorf("方法与路径不符 Agent 路由：%s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotBody, `"content"`) {
		t.Errorf("请求体应含 content 字段：%s", gotBody)
	}
	if got.BackupFile != "/opt/watchtower/docker-compose.yml.bak.2026-10-15T00:50:00+08:00" {
		t.Errorf("备份路径解码不符：%q", got.BackupFile)
	}
}

func TestClient_ComposeFileEmptyProjectRejected(t *testing.T) {
	// 三个方法都要在出门前拦住空项目名：既报错，也不产生任何请求。
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		writeEnvelope(w, http.StatusOK, protocol.ComposeFile{})
	}))
	defer srv.Close()
	c := quickClient(srv)

	if _, err := c.ComposeFileGet(context.Background(), ""); err == nil {
		t.Errorf("ComposeFileGet 空项目名应报错")
	}
	if _, err := c.ComposeFileDiff(context.Background(), "", "x"); err == nil {
		t.Errorf("ComposeFileDiff 空项目名应报错")
	}
	if _, err := c.ComposeFilePut(context.Background(), "", "x"); err == nil {
		t.Errorf("ComposeFilePut 空项目名应报错")
	}
	if called != 0 {
		t.Errorf("空项目名不应发出任何请求，got %d 次", called)
	}
}
