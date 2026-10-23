// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agenthttp

import (
	"sync"
	"time"
)

// creditWait 是等待对端补充信用的上限：超时即结束该流，避免消费端彻底不读时
// 把处理 goroutine 永久挂住（背压不能被无限期阻塞）。
const creditWait = 60 * time.Second

// credit 是流式响应的发送信用（字节，S12.5 M2 ④）。
//
// Agent 初始拿到 CreditWindow 字节信用；每发一块 data 扣减，Hub 消费端每读走
// n 字节回一个 credit 帧补充。用通知通道而非 sync.Cond，便于带超时等待。
type credit struct {
	mu     sync.Mutex
	avail  int64
	closed bool
	notify chan struct{}
}

func newCredit(initial int64) *credit {
	return &credit{avail: initial, notify: make(chan struct{}, 1)}
}

func (c *credit) signal() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// acquire 扣减 n 字节信用；不足时等待补充，超时或流关闭返回 false。
func (c *credit) acquire(n int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return false
		}
		if c.avail >= n {
			c.avail -= n
			c.mu.Unlock()
			return true
		}
		c.mu.Unlock()

		d := time.Until(deadline)
		if d <= 0 {
			return false
		}
		select {
		case <-c.notify:
		case <-time.After(d):
			return false
		}
	}
}

// add 补充信用并唤醒等待方。
func (c *credit) add(n int64) {
	c.mu.Lock()
	c.avail += n
	c.mu.Unlock()
	c.signal()
}

// close 关闭信用窗口，唤醒所有等待方令其失败。
func (c *credit) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.signal()
}
