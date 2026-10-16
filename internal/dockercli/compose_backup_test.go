// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// backupDir 造一个 compose 目录：当前文件 + 若干备份 + 若干噪音文件。
// 备份名用真实形态（RFC3339 带时区偏移），因为「含 + 与 :」正是本模块
// 要处理的东西——拿没有 + 的名字测，等于没测到编码这条路径。
func backupDir(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte("current"), 0o644); err != nil {
		t.Fatalf("写 compose 文件: %v", err)
	}
	backups := map[string]string{
		"docker-compose.yml.bak.2026-10-01T10:00:00+08:00": "old1",
		"docker-compose.yml.bak.2026-10-03T10:00:00+08:00": "old3",
		"docker-compose.yml.bak.2026-10-02T10:00:00+08:00": "old2",
	}
	for n, c := range backups {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatalf("写备份 %s: %v", n, err)
		}
	}
	// 噪音：另一个 compose 文件的备份、无关文件、以及一个目录。
	if err := os.WriteFile(filepath.Join(dir, "other.yml.bak.2026-10-01T10:00:00+08:00"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "docker-compose.yml.bak.2026-10-04T10:00:00+08:00"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, file
}

// TestComposeListBackups 钉住三件事：只认本文件的备份（别的 compose 文件的
// .bak 不算）、目录同名不算、以及**按时间倒序**（排序用文件名字典序，
// 不解析时间——所以这条用例就是那个假设的回归）。
func TestComposeListBackups(t *testing.T) {
	_, file := backupDir(t)
	c := NewComposeClient(nil)

	list, err := c.ListBackups(context.Background(), file)
	if err != nil {
		t.Fatalf("ListBackups: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("备份数 = %d, want 3（噪音与目录同名项都要被过滤）: %+v", len(list), list)
	}
	want := []string{
		"docker-compose.yml.bak.2026-10-03T10:00:00+08:00",
		"docker-compose.yml.bak.2026-10-02T10:00:00+08:00",
		"docker-compose.yml.bak.2026-10-01T10:00:00+08:00",
	}
	for i, w := range want {
		if list[i].Name != w {
			t.Errorf("第 %d 条 = %q, want %q（须时间倒序）", i, list[i].Name, w)
		}
	}
	// 只回文件名，不回目录——Q4-A 的第一道防线。
	for _, b := range list {
		if strings.ContainsAny(b.Name, `/\`) {
			t.Errorf("备份名含路径分隔符（泄漏宿主机路径）: %q", b.Name)
		}
	}
	if list[0].Size != 4 {
		t.Errorf("Size = %d, want 4", list[0].Size)
	}
	if list[0].ModTime == "" {
		t.Error("ModTime 不该为空")
	}
}

// TestComposeResolveBackup 钉住路径关卡（Q5-A）：不是本文件的备份、带路径
// 分隔符、以及**软链指向白名单目录之外**都要被拒——最后一条最要命，备份是
// 文件副本，被换成软链就能借「读备份 / 恢复」读到或写进任意文件。
func TestComposeResolveBackup(t *testing.T) {
	dir, file := backupDir(t)
	good := "docker-compose.yml.bak.2026-10-02T10:00:00+08:00"

	if _, err := resolveBackup(file, good); err != nil {
		t.Errorf("合法备份名被拒: %v", err)
	}

	// 越界的软链（名字合法、指向目录外）。
	outside := filepath.Join(t.TempDir(), "secret.yml")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := "docker-compose.yml.bak.2026-10-09T10:00:00+08:00"
	if err := os.Symlink(outside, filepath.Join(dir, link)); err != nil {
		t.Fatalf("造软链: %v", err)
	}
	if _, err := resolveBackup(file, link); err == nil {
		t.Error("指向目录外的软链备份应当被拒（否则能借读备份读到任意文件）")
	}

	for _, bad := range []string{
		"", // 空
		"other.yml.bak.2026-10-01T10:00:00+08:00",              // 不是这个 compose 文件的备份
		"docker-compose.yml",                                   // 当前文件本身不是备份
		"../docker-compose.yml.bak.2026-10-01T10:00:00+08:00",  // 含 ..
		"sub/docker-compose.yml.bak.2026-10-01T10:00:00+08:00", // 含分隔符
		"docker-compose.yml.bak.nonexistent",                   // 前缀对但文件不存在
	} {
		if _, err := resolveBackup(file, bad); err == nil {
			t.Errorf("非法备份名 %q 应当被拒", bad)
		}
	}
}

// TestComposeRestoreBackup 是 Q2-A 的回归：恢复本身是一次覆盖写，必须先把
// 「恢复前的当前内容」备份下来——否则回滚是单向门，恢复到旧版本后就回不去了。
func TestComposeRestoreBackup(t *testing.T) {
	dir, file := backupDir(t)
	c := NewComposeClient(nil)
	pick := "docker-compose.yml.bak.2026-10-02T10:00:00+08:00"

	got, err := c.RestoreBackup(context.Background(), file, pick)
	if err != nil {
		t.Fatalf("RestoreBackup: %v", err)
	}
	if got == "" || strings.ContainsAny(got, `/\`) {
		t.Fatalf("返回的备份名不合法（应只回文件名）: %q", got)
	}
	// 当前文件现在是备份的内容。
	cur, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != "old2" {
		t.Errorf("当前文件 = %q, want old2", cur)
	}
	// 且恢复前的当前内容（"current"）被存进了新备份——回滚不是单向门。
	saved, err := os.ReadFile(filepath.Join(dir, got))
	if err != nil {
		t.Fatalf("读恢复前的新备份: %v", err)
	}
	if string(saved) != "current" {
		t.Errorf("恢复前的新备份内容 = %q, want current（没有它，恢复就不可逆）", saved)
	}
	// 恢复用的那份备份仍在（读备份不消耗备份）。
	if _, err := os.ReadFile(filepath.Join(dir, pick)); err != nil {
		t.Errorf("恢复用的备份不应被挪走: %v", err)
	}
}
