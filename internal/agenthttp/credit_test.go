// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agenthttp

import (
	"testing"
	"time"
)

func TestCreditAcquireAndAdd(t *testing.T) {
	c := newCredit(10)
	if !c.acquire(8, time.Second) {
		t.Fatal("初始信用足够，应成功")
	}
	// 剩余 2：再要 8 应等待超时失败。
	if c.acquire(8, 30*time.Millisecond) {
		t.Fatal("信用不足应超时失败")
	}
	// 补充 10 → 12，再要 12 应成功。
	c.add(10)
	if !c.acquire(12, time.Second) {
		t.Fatal("补充后应成功")
	}
	// 用光后再要 1 应失败。
	if c.acquire(1, 20*time.Millisecond) {
		t.Fatal("信用用尽应失败")
	}
}

func TestCreditCloseUnblocks(t *testing.T) {
	c := newCredit(0)
	go func() {
		time.Sleep(20 * time.Millisecond)
		c.close()
	}()
	if c.acquire(1, time.Second) {
		t.Fatal("关闭后 acquire 应失败")
	}
}

func TestCreditAddWakesWaiter(t *testing.T) {
	c := newCredit(0)
	done := make(chan bool, 1)
	go func() { done <- c.acquire(5, time.Second) }()
	time.Sleep(20 * time.Millisecond)
	c.add(5)
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("补充后应成功")
		}
	case <-time.After(time.Second):
		t.Fatal("补充后未被唤醒")
	}
}
