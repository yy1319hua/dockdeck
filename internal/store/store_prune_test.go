// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package store

import (
	"context"
	"testing"
	"time"
)

// TestPruneAuditLogs 回归 #89：只删早于窗口的行，窗口内不动。
func TestPruneAuditLogs(t *testing.T) {
	s := newServerStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(ts time.Time, action string) {
		if _, err := s.DB.Exec(
			`INSERT INTO audit_logs(ts,action,result) VALUES(?,?,?)`,
			ts.UTC().Format(time.RFC3339), action, "ok"); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	mk(now.Add(-91*24*time.Hour), "old.1")
	mk(now.Add(-89*24*time.Hour), "keep.1")
	mk(now, "keep.2")

	n, err := s.PruneAuditLogs(ctx, 90*24*time.Hour, now)
	if err != nil || n != 1 {
		t.Fatalf("应删 1 行, 得 %d, %v", n, err)
	}
	var c int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action='old.1'`).Scan(&c); err != nil || c != 0 {
		t.Errorf("老行应已删除, 得 %d, %v", c, err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action LIKE 'keep%'`).Scan(&c); err != nil || c != 2 {
		t.Errorf("窗口内应保留 2 行, 得 %d, %v", c, err)
	}
	if _, err := s.PruneAuditLogs(ctx, 0, now); err == nil {
		t.Error("零窗口应报错")
	}
}
