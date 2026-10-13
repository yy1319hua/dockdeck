package db

import (
	"path/filepath"
	"testing"
)

func TestOpen_Migrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// schema_migrations 应记录已应用的迁移。
	var ver int
	if err := d.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&ver); err != nil {
		t.Fatalf("读版本: %v", err)
	}
	if ver < 1 {
		t.Fatalf("迁移版本 %d，期望 >=1", ver)
	}

	// 关键表应存在。
	for _, tbl := range []string{"users", "sessions", "servers", "audit_logs", "metrics_snapshots", "settings"} {
		var name string
		err := d.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
		if err != nil {
			t.Fatalf("表 %s 不存在: %v", tbl, err)
		}
	}
}

func TestOpen_Idempotent(t *testing.T) {
	// 对同一库二次 Open 不应报错，也不应重复应用迁移。
	path := filepath.Join(t.TempDir(), "test.db")
	d1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var v1 int
	_ = d1.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&v1)
	d1.Close()

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("二次 Open: %v", err)
	}
	defer d2.Close()
	var v2 int
	_ = d2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&v2)
	if v2 != v1 {
		t.Fatalf("迁移被重复应用: 首次 %d 行，二次 %d 行", v1, v2)
	}
}
