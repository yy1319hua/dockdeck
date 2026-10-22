// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"testing"
	"time"
)

func TestBackoffDelay(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{-3, linkRetryBase}, // 负数按 0 处理
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second}, // 32s 封顶到 30s
		{6, 30 * time.Second},
		{1000, 30 * time.Second}, // 大 attempt 不溢出、恒为封顶
	}
	for _, c := range cases {
		if got := backoffDelay(c.attempt, linkRetryBase, linkRetryMax); got != c.want {
			t.Errorf("backoffDelay(%d) = %v，want %v", c.attempt, got, c.want)
		}
	}
}

func TestJitterWithinBounds(t *testing.T) {
	d := 10 * time.Second
	lo, hi := 8*time.Second, 12*time.Second
	for i := 0; i < 200; i++ {
		got := jitter(d)
		if got < lo || got >= hi {
			t.Fatalf("jitter(%v) = %v，越界 [%v, %v)", d, got, lo, hi)
		}
	}
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %v，want 0", got)
	}
}
