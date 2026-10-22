// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"math/rand"
	"time"
)

// 反向连接重连退避参数（S12.5 M2 ①）。
const (
	linkRetryBase = time.Second      // 首次重试基准 1s
	linkRetryMax  = 30 * time.Second // 封顶 30s
	stableSession = 60 * time.Second // 会话持续 ≥60s 视为稳定，重置退避
)

// backoffDelay 返回第 attempt 次（0 起）重连的基准等待：base<<attempt，封顶 max。
//
// 纯函数、无随机，便于单测；抖动由 jitter 单独施加。
func backoffDelay(attempt int, base, max time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := base
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		d = max
	}
	return d
}

// jitter 给基准等待叠加 ±20% 随机抖动，避免大量 Agent 在同一时刻重连（惊群）。
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	factor := 0.8 + rand.Float64()*0.4 // [0.8, 1.2)
	return time.Duration(float64(d) * factor)
}
