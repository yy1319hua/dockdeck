// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/config"
)

func TestEnrollToHubSuccess(t *testing.T) {
	var gotAuth, gotName, gotBase string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var b struct {
			Name    string `json:"name"`
			BaseURL string `json:"base_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotName, gotBase = b.Name, b.BaseURL
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"id":7,"name":"whatever"}}`)
	}))
	defer srv.Close()

	cfg := config.AgentConfig{
		ListenAddr:     "10.0.0.5:9527",
		TLSMode:        config.TLSModeNone,
		HubURL:         srv.URL,
		JoinSecret:     "sekret",
		EnrollInsecure: true,
	}
	if err := enrollToHub(cfg); err != nil {
		t.Fatalf("enrollToHub: %v", err)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("Authorization 不对：%q", gotAuth)
	}
	if gotBase != "http://10.0.0.5:9527" {
		t.Fatalf("base_url 应由 scheme+ListenAddr 得出，得 %q", gotBase)
	}
	if gotName == "" {
		t.Fatal("name 应取 hostname，不应为空")
	}
}

func TestEnrollToHubRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `{"error":{"code":"invite_expired","message":"过期"}}`)
	}))
	defer srv.Close()
	cfg := config.AgentConfig{
		ListenAddr: "127.0.0.1:9527", TLSMode: config.TLSModeNone,
		HubURL: srv.URL, JoinSecret: "x", EnrollInsecure: true,
	}
	if err := enrollToHub(cfg); err == nil {
		t.Fatal("Hub 拒绝纳管时应返回 error")
	}
}
