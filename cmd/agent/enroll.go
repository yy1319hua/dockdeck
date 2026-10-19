// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
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

// enrollError 把失败分成「值得重试」与「重试也没用」两类。
//
// 为什么需要这个区分：Hub 与 Agent 常常同一份 compose 一起拉起，
// Agent 先起来时 Hub 还没监听——那是瞬时失败，退避重试几秒就好；
// 而 join_secret 过期（410）或凭据错（401）重试一万次也不会变，
// 只会让运维在一堆重试日志里找不到真正的原因。
type enrollError struct {
	msg string
	// status 是 Hub 返回的 HTTP 状态码；网络层失败时为 0。
	status int
	// permanent 为 true = 凭据/邀请/命名冲突类问题，必须人工修正。
	permanent bool
}

func (e *enrollError) Error() string { return e.msg }

// permanentEnrollStatus 判断该状态码是否属于「改配置才能好」。
// 408/429 是明确的临时态，其余 4xx 都要求人介入。
func permanentEnrollStatus(code int) bool {
	if code < 400 || code >= 500 {
		return false
	}
	return code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
}

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
	name, _ := os.Hostname()
	baseURL := scheme + "://" + cfg.ListenAddr
	if cfg.Reverse {
		// 反向模式没有可被 Hub 拨号的地址：用标记串占位，
		// 让 Hub 明确「这台只能走长连接」，而不是去撞一个不存在的端口。
		baseURL = "reverse://" + name
	}

	payload, _ := json.Marshal(map[string]string{"name": name, "base_url": baseURL})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(cfg.HubURL, "/")+"/api/v1/servers/enroll", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("构造纳管请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// 反向模式下用户往往只提供 token（token 本身就是 join_secret），
	// 若 JoinSecret 为空就用 Token 兜底，避免「有凭证却因字段没填被拒」。
	secret := cfg.JoinSecret
	if secret == "" {
		secret = cfg.Token
	}
	req.Header.Set("Authorization", "Bearer "+secret)

	resp, err := enrollClient(cfg).Do(req)
	if err != nil {
		return fmt.Errorf("连接 Hub 失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return &enrollError{
			msg:       fmt.Sprintf("Hub 拒绝纳管：HTTP %d｜%s", resp.StatusCode, strings.TrimSpace(string(body))),
			status:    resp.StatusCode,
			permanent: permanentEnrollStatus(resp.StatusCode),
		}
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

// enrollWithRetry 按指数退避重试纳管，直到成功、判定为永久失败或 ctx 结束。
//
// maxAttempts<=0 表示无限重试（反向模式用）：连接本身是 Agent 的命脉，
// Hub 暂时不可达时保持进程存活、持续尝试，比退出等编排器重启更可控。
// 非反向模式用有限次数：Agent 已在正常服务，纳管只是加分项，不值得无限刷屏。
func enrollWithRetry(ctx context.Context, cfg config.AgentConfig, maxAttempts int) error {
	// 生产口径：2s 起、30s 封顶。测试走 enrollRetryEvery 传更小的值，
	// 免得为了验重试逻辑真等半分钟。
	return enrollRetryEvery(ctx, cfg, maxAttempts, 2*time.Second, 30*time.Second)
}

// enrollRetryEvery 是 enrollWithRetry 的可注入版本（退避参数外置，便于测试）。
func enrollRetryEvery(ctx context.Context, cfg config.AgentConfig, maxAttempts int, base, maxBackoff time.Duration) error {
	if base <= 0 {
		base = time.Second
	}
	if maxBackoff < base {
		maxBackoff = base
	}
	backoff := base
	var last error
	for i := 1; ; i++ {
		err := enrollToHub(cfg)
		if err == nil {
			return nil
		}
		last = err

		var ee *enrollError
		if errors.As(err, &ee) && ee.permanent {
			return err
		}
		if maxAttempts > 0 && i >= maxAttempts {
			break
		}
		limit := "∞"
		if maxAttempts > 0 {
			limit = fmt.Sprintf("%d", maxAttempts)
		}
		log.Printf("agent: 纳管未成功（第 %d/%s 次）：%v；%s 后重试", i, limit, err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	return fmt.Errorf("重试 %d 次仍未纳管成功: %w", maxAttempts, last)
}

func enrollClient(cfg config.AgentConfig) *http.Client {
	tr := &http.Transport{}
	if strings.HasPrefix(strings.ToLower(cfg.HubURL), "https") && cfg.EnrollInsecure {
		// Hub 用自签证书：默认跳过校验（运维可用 --enroll-insecure=false 改强制校验）。
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 见 AgentConfig.EnrollInsecure 注释
	}
	return &http.Client{Transport: tr, Timeout: enrollTimeout}
}
