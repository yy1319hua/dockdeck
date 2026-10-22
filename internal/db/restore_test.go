// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package db

import (
	"os"
	"path/filepath"
	"testing"
)

// TestApplyStagedRestore 回归 #93：无 staged 不动作；有 staged 先备份当前库
// （含 -wal/-shm）再顶替，且原库不再有 WAL 附属件。
func TestApplyStagedRestore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dockdeck.db")

	// 场景 1：无 staged → no-op。
	applied, err := ApplyStagedRestore(dbPath)
	if err != nil || applied {
		t.Fatalf("无 staged 应 no-op, 得 applied=%v err=%v", applied, err)
	}

	// 造当前库 + WAL 附属件。
	if err := os.WriteFile(dbPath, []byte("current-db"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 造 staged。
	staged := filepath.Join(dir, "restore-staged.db")
	if err := os.WriteFile(staged, []byte("restored-db"), 0o600); err != nil {
		t.Fatal(err)
	}

	applied, err = ApplyStagedRestore(dbPath)
	if err != nil || !applied {
		t.Fatalf("应有替换, applied=%v err=%v", applied, err)
	}
	got, err := os.ReadFile(dbPath)
	if err != nil || string(got) != "restored-db" {
		t.Fatalf("主库应为 staged 内容, 得 %q, %v", got, err)
	}
	if _, err := os.Stat(dbPath + "-wal"); !os.IsNotExist(err) {
		t.Error("旧 WAL 附属件应已被挪走")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("staged 应消失（已换正）")
	}
	// 备份目录里应有原库。
	entries, _ := filepath.Glob(filepath.Join(dir, "backups", "pre-restore-boot-*", "dockdeck.db"))
	if len(entries) != 1 {
		t.Fatalf("原库应备份到 backups/pre-restore-boot-*/, 得 %v", entries)
	}
	if b, _ := os.ReadFile(entries[0]); string(b) != "current-db" {
		t.Errorf("备份内容应 = 原库, 得 %q", b)
	}
}

// TestLatestSchemaVersion 基本可用性：≥1 且两次调用一致。
func TestLatestSchemaVersion(t *testing.T) {
	v, err := LatestSchemaVersion()
	if err != nil || v < 1 {
		t.Fatalf("LatestSchemaVersion = %d, %v", v, err)
	}
	v2, _ := LatestSchemaVersion()
	if v2 != v {
		t.Errorf("两次调用不一致: %d vs %d", v, v2)
	}
}
