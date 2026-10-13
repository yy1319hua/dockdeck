// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/protocol"
	"github.com/yy1319hua/dockdeck/internal/tlsx"
)

const smokeToken = "smoke-token"

// TestRun_RejectsMissingToken 覆盖启动失败路径：
// 没有 token 必须启动失败，绝不能退化成无鉴权监听。
func TestRun_RejectsMissingToken(t *testing.T) {
	t.Setenv("DD_AGENT_TOKEN", "")
	if err := run([]string{"-listen", "127.0.0.1:9527"}); err == nil {
		t.Fatal("缺少 token 时 run 应返回错误")
	}
}

// TestBuildHandler_Unauthenticated 验证生产中间件链确实挡住了无凭证请求。
func TestBuildHandler_Unauthenticated(t *testing.T) {
	srv := httptest.NewServer(buildHandler(smokeToken))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/api/v1/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无凭证访问 /api/v1/info 应返回 401，got %d", resp.StatusCode)
	}
}

// TestBuildHandler_Authenticated 走一次完整签名流程，确认 /info 可被 Hub 正常读取。
func TestBuildHandler_Authenticated(t *testing.T) {
	srv := httptest.NewServer(buildHandler(smokeToken))
	defer srv.Close()

	target := "/api/v1/info"
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequest(http.MethodGet, srv.URL+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+smokeToken)
	req.Header.Set("X-DD-Timestamp", stamp)
	req.Header.Set("X-DD-Signature",
		crypto.SignRequest([]byte(smokeToken), stamp, http.MethodGet, target, nil))

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("正确签名应返回 200，got %d", resp.StatusCode)
	}
	var env struct {
		Data protocol.AgentInfo `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if env.Data.Cores <= 0 || env.Data.OS == "" || env.Data.Arch == "" {
		t.Errorf("/info 返回内容异常：%+v", env.Data)
	}
	if env.Data.ProtocolVersion != protocol.ProtocolVersion {
		t.Errorf("协议版本 = %q", env.Data.ProtocolVersion)
	}
}

// TestSelfSignedCertUsable 验证 Agent 的 TLS 接线：证书能落盘并可直接用于握手。
func TestSelfSignedCertUsable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "certs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cert, fingerprint, err := tlsx.EnsureSelfSigned(dir, "127.0.0.1", 9527)
	if err != nil {
		t.Fatalf("生成自签证书失败：%v", err)
	}
	if fingerprint == "" {
		t.Error("指纹不应为空，Hub 需要它做首次信任")
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv := httptest.NewUnstartedServer(buildHandler(smokeToken))
	srv.TLS = cfg
	srv.StartTLS()
	defer srv.Close()

	// 客户端不校验证书（自签），只确认 TLS 握手与鉴权链路通畅。
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	target := "/api/v1/info"
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+target, nil)
	req.Header.Set("Authorization", "Bearer "+smokeToken)
	req.Header.Set("X-DD-Timestamp", stamp)
	req.Header.Set("X-DD-Signature",
		crypto.SignRequest([]byte(smokeToken), stamp, http.MethodGet, target, nil))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("TLS 请求失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("TLS 下正确签名应返回 200，got %d", resp.StatusCode)
	}
}
