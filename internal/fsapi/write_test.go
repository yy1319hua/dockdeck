// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package fsapi

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/iotest"
)

// backupsOf 列出 dir 下 base 的所有备份文件名（base.bak.*）。
func backupsOf(t *testing.T, dir, base string) []string {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		if strings.HasPrefix(it.Name(), base+".bak.") {
			out = append(out, it.Name())
		}
	}
	sort.Strings(out)
	return out
}

// leftoversOf 列出流式写入可能遗留的临时文件。
func leftoversOf(t *testing.T, dir string) []string {
	t.Helper()
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		if strings.HasPrefix(it.Name(), ".dd-upload-") {
			out = append(out, it.Name())
		}
	}
	return out
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}

func TestGuard_WriteCreatesNewFile(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	res, err := g.Write(0, "fresh.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Path != "fresh.txt" || res.Size != 5 || !res.Created {
		t.Errorf("结果 = %+v，want path=fresh.txt size=5 created=true", res)
	}
	if res.BackupFile != "" {
		t.Errorf("新文件不该有备份，got %q", res.BackupFile)
	}
	target := filepath.Join(root, "fresh.txt")
	if got := mustRead(t, target); got != "hello" {
		t.Errorf("落盘内容 = %q，want hello", got)
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("权限 = %04o，want 0644", st.Mode().Perm())
	}
}

// 写前备份是红线：目标已存在时必须先有备份，且备份里是**旧内容**。
func TestGuard_WriteBacksUpExistingFile(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	res, err := g.Write(0, "notes.txt", strings.NewReader("new content\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Created {
		t.Error("覆盖已有文件不该标 Created")
	}
	if res.BackupFile == "" {
		t.Fatal("覆盖已有文件必须产生备份")
	}
	if strings.HasPrefix(res.BackupFile, "/") {
		t.Errorf("备份路径不能是绝对路径（对外不暴露宿主机路径），got %q", res.BackupFile)
	}
	if got := mustRead(t, filepath.Join(root, res.BackupFile)); got != "notes\n" {
		t.Errorf("备份内容 = %q，want 旧内容 notes\\n", got)
	}
	if got := mustRead(t, filepath.Join(root, "notes.txt")); got != "new content\n" {
		t.Errorf("目标内容 = %q，want 新内容", got)
	}
}

// 失败路径的核心断言：原文件一个字节都没动、没有半成品、没有孤儿临时文件。
func TestGuard_WriteFailureLeavesNothingBehind(t *testing.T) {
	root := testTree(t)
	g := newGuardLimits(t, []string{root}, defaultTestDeny, 4, 3, 100)

	if _, err := g.Write(0, "notes.txt", strings.NewReader("0123456789")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("超过上限应 ErrTooLarge，got %v", err)
	}
	if got := mustRead(t, filepath.Join(root, "notes.txt")); got != "notes\n" {
		t.Errorf("超限后原文件被改动：%q", got)
	}
	if got := backupsOf(t, root, "notes.txt"); len(got) != 0 {
		t.Errorf("上限检查应在备份之前，不该产生备份：%v", got)
	}
	if got := leftoversOf(t, root); len(got) != 0 {
		t.Errorf("遗留临时文件：%v", got)
	}

	// 读取中途失败：同样不许留下半成品。
	broken := iotest.ErrReader(errors.New("boom"))
	if _, err := g.Write(0, "notes.txt", broken); err == nil {
		t.Fatal("读取失败应报错")
	}
	if got := mustRead(t, filepath.Join(root, "notes.txt")); got != "notes\n" {
		t.Errorf("出错后原文件被改动：%q", got)
	}
	if got := leftoversOf(t, root); len(got) != 0 {
		t.Errorf("遗留临时文件：%v", got)
	}
}

// 上传物不可执行：即使覆盖的是原本 0755 的文件，也必须落成 0644。
func TestGuard_WriteDropsExecutableBit(t *testing.T) {
	root := testTree(t)
	target := filepath.Join(root, "script.sh")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := newGuard(t, []string{root}, 3)
	if _, err := g.Write(0, "script.sh", strings.NewReader("#!/bin/sh\necho hi\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("权限 = %04o，want 0644（不留可执行位）", st.Mode().Perm())
	}
}

// 目标是软链时必须拒绝：否则「写 cert.txt」会绕过黑名单，
// 把受保护文件的内容搬进白名单里的新文件。
func TestGuard_WriteRejectsSymlinkTarget(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	if _, err := g.Write(0, "cert.txt", strings.NewReader("pwn")); !errors.Is(err, ErrPathDenied) {
		t.Fatalf("写软链目标应 ErrPathDenied，got %v", err)
	}
	if got := mustRead(t, filepath.Join(root, "keys", "server.pem")); got != "pem\n" {
		t.Errorf("受保护文件被改写：%q", got)
	}
	if got := mustRead(t, filepath.Join(root, "cert.txt")); got != "pem\n" {
		t.Errorf("软链目标被改写：%q", got)
	}
	if _, err := g.Write(0, "inlink", strings.NewReader("x")); !errors.Is(err, ErrPathDenied) {
		t.Errorf("写目录软链应 ErrPathDenied，got %v", err)
	}
}

func TestGuard_WriteRejectsBadTargets(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	if _, err := g.Write(0, "sub", strings.NewReader("x")); !errors.Is(err, ErrIsDir) {
		t.Errorf("写目录应 ErrIsDir，got %v", err)
	}
	if _, err := g.Write(0, "", strings.NewReader("x")); !errors.Is(err, ErrIsDir) {
		t.Errorf("写根目录应 ErrIsDir，got %v", err)
	}
	if _, err := g.Write(0, "missing/x.txt", strings.NewReader("x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("父目录不存在应 ErrNotFound，got %v", err)
	}
	if _, err := g.Write(0, "sub/.env", strings.NewReader("x")); !errors.Is(err, ErrPathDenied) {
		t.Errorf("黑名单目标应 ErrPathDenied，got %v", err)
	}
	if _, err := g.Write(0, "../pwn.txt", strings.NewReader("x")); !errors.Is(err, ErrPathEscape) {
		t.Errorf("越界目标应 ErrPathEscape，got %v", err)
	}
}

func TestGuard_Mkdir(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	res, err := g.Mkdir(0, "newdir")
	if err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if res.Path != "newdir" {
		t.Errorf("Path = %q", res.Path)
	}
	st, err := os.Stat(filepath.Join(root, "newdir"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.IsDir() || st.Mode().Perm() != 0o755 {
		t.Errorf("目录状态 = %v %04o，want dir 0755", st.IsDir(), st.Mode().Perm())
	}

	if _, err := g.Mkdir(0, "newdir"); !errors.Is(err, ErrExists) {
		t.Errorf("已存在应 ErrExists，got %v", err)
	}
	if _, err := g.Mkdir(0, "missing/sub"); !errors.Is(err, ErrNotFound) {
		t.Errorf("父目录不存在应 ErrNotFound，got %v", err)
	}
	if _, err := g.Mkdir(0, "sub/.env"); !errors.Is(err, ErrPathDenied) {
		t.Errorf("黑名单应 ErrPathDenied，got %v", err)
	}
	if _, err := g.Mkdir(0, "escape/x"); !errors.Is(err, ErrPathDenied) {
		t.Errorf("逃逸软链应 ErrPathDenied，got %v", err)
	}
	if _, err := g.Mkdir(0, ""); !errors.Is(err, ErrExists) {
		t.Errorf("根目录已存在，应 ErrExists，got %v", err)
	}
}

func TestGuard_Delete(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	if _, err := g.Delete(0, "notes.txt"); err != nil {
		t.Fatalf("Delete 文件: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Error("文件应已删除")
	}

	if _, err := g.Mkdir(0, "emptydir"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Delete(0, "emptydir"); err != nil {
		t.Errorf("删除空目录应成功，got %v", err)
	}

	if _, err := g.Delete(0, "sub"); !errors.Is(err, ErrNotEmpty) {
		t.Errorf("非空目录应 ErrNotEmpty，got %v", err)
	}
	if _, err := g.Delete(0, "missing.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在应 ErrNotFound，got %v", err)
	}
	if _, err := g.Delete(0, ""); !errors.Is(err, ErrPathDenied) {
		t.Errorf("删根目录应 ErrPathDenied，got %v", err)
	}
	if _, err := g.Delete(0, "sub/key.pem"); !errors.Is(err, ErrPathDenied) {
		t.Errorf("黑名单应 ErrPathDenied，got %v", err)
	}
	// 删软链要拒：否则「删 inlink」会删掉真目录 sub。
	if _, err := g.Delete(0, "inlink"); !errors.Is(err, ErrPathDenied) {
		t.Errorf("删软链应 ErrPathDenied，got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "inner.txt")); err != nil {
		t.Errorf("真目录不该被删：%v", err)
	}
}

func TestGuard_Rename(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	res, err := g.Rename(0, "notes.txt", "renamed.txt")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if res.Path != "notes.txt" || res.Dest != "renamed.txt" {
		t.Errorf("结果 = %+v", res)
	}
	if got := mustRead(t, filepath.Join(root, "renamed.txt")); got != "notes\n" {
		t.Errorf("改名后内容 = %q", got)
	}

	// 目录连同内容一起改名是可以的（os.Rename 语义），但不覆盖已有目标。
	// 注意顺序：改掉 sub 之后，指向它的软链 inlink 就成了悬空链，
	// 后面几条与 inlink 相关的断言必须在改名之前做。
	if _, err := g.Rename(0, "renamed.txt", "sub"); !errors.Is(err, ErrExists) {
		t.Errorf("目标已存在应 ErrExists（不覆盖），got %v", err)
	}
	if _, err := g.Rename(0, "renamed.txt", "renamed.txt"); !errors.Is(err, ErrExists) {
		t.Errorf("源与目标相同应 ErrExists，got %v", err)
	}
	if _, err := g.Rename(0, "missing.txt", "x.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("源不存在应 ErrNotFound，got %v", err)
	}
	if _, err := g.Rename(0, "renamed.txt", "missing/x.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("目标父目录不存在应 ErrNotFound，got %v", err)
	}
	if _, err := g.Rename(0, "", "x.txt"); !errors.Is(err, ErrPathDenied) {
		t.Errorf("改根目录名应 ErrPathDenied，got %v", err)
	}
	if _, err := g.Rename(0, "inlink", "link2"); !errors.Is(err, ErrPathDenied) {
		t.Errorf("改软链名应 ErrPathDenied（会动到真实目标），got %v", err)
	}
	if _, err := g.Rename(0, "renamed.txt", "sub/.env"); !errors.Is(err, ErrPathDenied) {
		t.Errorf("目标命中黑名单应 ErrPathDenied，got %v", err)
	}

	if _, err := g.Rename(0, "sub", "sub2"); err != nil {
		t.Errorf("改目录名应成功，got %v", err)
	}
	if got := mustRead(t, filepath.Join(root, "sub2", "inner.txt")); got != "inner\n" {
		t.Errorf("改名后子文件内容 = %q", got)
	}
}

// 同一秒内连写两次不能把上一份备份覆盖掉——备份的意义就是还能回去。
func TestGuard_WriteBackupCollisionKeepsBoth(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	if _, err := g.Write(0, "notes.txt", strings.NewReader("first\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Write(0, "notes.txt", strings.NewReader("second\n")); err != nil {
		t.Fatal(err)
	}
	backups := backupsOf(t, root, "notes.txt")
	if len(backups) != 2 {
		t.Fatalf("应有 2 份备份（同秒撞名要另起名字），got %v", backups)
	}
	contents := map[string]bool{}
	for _, b := range backups {
		contents[mustRead(t, filepath.Join(root, b))] = true
	}
	for _, want := range []string{"notes\n", "first\n"} {
		if !contents[want] {
			t.Errorf("备份里缺少 %q：%v", want, contents)
		}
	}
	if got := mustRead(t, filepath.Join(root, "notes.txt")); got != "second\n" {
		t.Errorf("目标内容 = %q，want second\\n", got)
	}
}
