// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/config"
)

const enrollTimeout = 15 * time.Second

// enrollToHub 在 Agent 启动时向 Hub 自注册（GAP-1 纳管第二阶段）。
// best-effort：失败只返回 error 由调用方告警，不阻断 Agent 服务；登记可在此后重启时
// 重试（enroll 幂等——同一 join_secret 重复调用返回既有服务器 id 并刷新 base_url）。
func enrollToHub(cfg config.AgentConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), enrollTimeout)
	defer cancel()

	scheme := "https"
	if cfg.TLSMode == config.TLSModeNone {
		scheme = "http"
	}
	// base_url 用 Agent 实际监听地址：运维已把 DD_AGENT_LISTEN 设为 Hub 可达地址（非回环）。
	baseURL := scheme + "://" + cfg.ListenAddr
	name, _ := os.Hostname()

	payload, _ := json.Marshal(map[string]string{"name": name, "base_url": baseURL})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.HubURL, "/")+"/api/v1/servers/enroll", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("构造纳管请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.JoinSecret)

	resp, err := enrollClient(cfg).Do(req)
	if err != nil {
		return fmt.Errorf("连接 Hub 失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("Hub 拒绝纳管：HTTP %d｜%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Data struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("解析纳管响应失败: %w", err)
	}
	log.Printf("agent: 已纳管为 Hub 服务器 #%d（name=%s base_url=%s）", out.Data.ID, out.Data.Name, baseURL)
	return nil
}

func enrollClient(cfg config.AgentConfig) *http.Client {
	tr := &http.Transport{}
	if strings.HasPrefix(strings.ToLower(cfg.HubURL), "https") && cfg.EnrollInsecure {
		// Hub 用自签证书：默认跳过校验（运维可用 --enroll-insecure=false 改强制校验）。
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 见 AgentConfig.EnrollInsecure 注释
	}
	return &http.Client{Transport: tr, Timeout: enrollTimeout}
}
