// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"fmt"
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

// TestWriteBackupExclusive 验证「同名不覆盖」是内核保证，不靠时钟精度：目标
// 已存在时自动换 .N 后缀，且既有文件内容一个字节都不能被动。旧实现用
// os.WriteFile，撞名即清空重写——这正是缺陷①的根。
func TestWriteBackupExclusive(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "docker-compose.yml.bak.2026-01-01T00:00:00.000000000+08:00")

	got, err := writeBackupExclusive(base, []byte("first"))
	if err != nil {
		t.Fatalf("首次写备份: %v", err)
	}
	if got != base {
		t.Errorf("首次应返回原名: got %q, want %q", got, base)
	}

	got, err = writeBackupExclusive(base, []byte("second"))
	if err != nil {
		t.Fatalf("二次写备份: %v", err)
	}
	if want := base + ".1"; got != want {
		t.Errorf("撞名应换后缀: got %q, want %q", got, want)
	}
	orig, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if string(orig) != "first" {
		t.Errorf("既有备份被覆盖: %q, want first（O_EXCL 应保证不覆盖）", orig)
	}
	suffix, err := os.ReadFile(base + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if string(suffix) != "second" {
		t.Errorf(".1 内容 = %q, want second", suffix)
	}

	got, err = writeBackupExclusive(base, []byte("third"))
	if err != nil {
		t.Fatalf("三次写备份: %v", err)
	}
	if want := base + ".2"; got != want {
		t.Errorf("连续撞名应递增: got %q, want %q", got, want)
	}
}

// TestWriteFileBackupNoOverwrite 是缺陷①的端到端回归：连续两次写入绝不能只
// 留下一份备份。旧实现秒级 RFC3339，同秒两次写算出同名、后一次清空重写，用户
// 静默丢一份；新实现靠 O_EXCL，无论时间戳是否相同都落两份。
func TestWriteFileBackupNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewComposeClient(nil)

	if _, err := c.WriteFile(context.Background(), file, "v1"); err != nil {
		t.Fatalf("第一次写入: %v", err)
	}
	if _, err := c.WriteFile(context.Background(), file, "v2"); err != nil {
		t.Fatalf("第二次写入: %v", err)
	}

	names, err := backupNames(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("两次写入应留两份备份，实际 %d 份: %v（同秒覆盖回归）", len(names), names)
	}
	// 两份内容分别是写入前的 v0 与 v1，顺序无关，但都要在。
	seen := map[string]bool{}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		seen[string(b)] = true
	}
	if !seen["v0"] || !seen["v1"] {
		t.Errorf("两份备份内容应为 v0 与 v1，实际 %v（少一份即静默丢失）", seen)
	}
}

// TestBackupNamesMixedFormatOrder 验证新旧备份格式混存时顺序仍对：清单与轮转
// 都靠「文件名倒字典序＝时间倒序」，混排一旦乱就会列错/删错。
func TestBackupNamesMixedFormatOrder(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	names := []string{
		"docker-compose.yml.bak.2026-01-01T00:00:00+08:00",           // 旧格式，最早
		"docker-compose.yml.bak.2026-01-01T00:00:00.000000001+08:00", // 新格式
		"docker-compose.yml.bak.2026-01-01T00:00:00.000000009+08:00", // 新格式
		"docker-compose.yml.bak.2026-01-01T00:00:01+08:00",           // 旧格式，最晚
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 噪音：别的文件的备份、无关文件、目录形态的同名项——都须被过滤。
	if err := os.WriteFile(filepath.Join(dir, "other.yml.bak.2026-01-01T00:00:00+08:00"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "docker-compose.yml.bak.2026-01-01T00:00:02+08:00"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := backupNames(file)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"docker-compose.yml.bak.2026-01-01T00:00:01+08:00",
		"docker-compose.yml.bak.2026-01-01T00:00:00.000000009+08:00",
		"docker-compose.yml.bak.2026-01-01T00:00:00.000000001+08:00",
		"docker-compose.yml.bak.2026-01-01T00:00:00+08:00",
	}
	if len(got) != len(want) {
		t.Fatalf("备份数 = %d, want %d（噪音/目录须被过滤）: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("第 %d 个 = %q, want %q（新旧格式混排须仍按时间倒序）", i, got[i], w)
		}
	}
}

// TestRotateComposeBackups 验证缺陷②：超过 maxComposeBackups 份时删最旧、留
// 最新，且绝不碰 compose 文件本身、别的文件的备份、无关文件与目录形态项。
func TestRotateComposeBackups(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(file, []byte("cur"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 造 maxComposeBackups+5 份，序号越大越新（定宽，字典序＝时间序）。
	total := maxComposeBackups + 5
	for i := 1; i <= total; i++ {
		n := fmt.Sprintf("docker-compose.yml.bak.2026-01-01T00:00:00.%09d+08:00", i)
		if err := os.WriteFile(filepath.Join(dir, n), []byte("b"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 噪音：别的文件的备份、无关文件、目录形态的「备份」——都不能被删。
	if err := os.WriteFile(filepath.Join(dir, "other.yml.bak.2026-01-01T00:00:00.000000001+08:00"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirBackup := filepath.Join(dir, "docker-compose.yml.bak.2026-01-01T00:00:00.000000000+08:00")
	if err := os.Mkdir(dirBackup, 0o755); err != nil {
		t.Fatal(err)
	}

	rotateComposeBackups(file)

	names, err := backupNames(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != maxComposeBackups {
		t.Fatalf("轮转后应剩 %d 份，实际 %d 份", maxComposeBackups, len(names))
	}
	for i, n := range names {
		want := fmt.Sprintf("docker-compose.yml.bak.2026-01-01T00:00:00.%09d+08:00", total-i)
		if n != want {
			t.Errorf("第 %d 份 = %q, want %q（须留最新、删最旧）", i, n, want)
		}
	}
	for seq := 1; seq <= total-maxComposeBackups; seq++ {
		n := fmt.Sprintf("docker-compose.yml.bak.2026-01-01T00:00:00.%09d+08:00", seq)
		if _, err := os.Lstat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("旧备份 %s 应已被轮转删除", n)
		}
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("compose 文件本身被误伤: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Errorf("无关文件被误伤: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.yml.bak.2026-01-01T00:00:00.000000001+08:00")); err != nil {
		t.Errorf("别的文件的备份被误伤: %v", err)
	}
	if _, err := os.Stat(dirBackup); err != nil {
		t.Errorf("目录形态的「备份」被误伤（轮转只删普通文件）: %v", err)
	}
}
