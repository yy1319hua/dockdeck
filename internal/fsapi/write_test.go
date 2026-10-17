// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package fsapi

import (
	"errors"
	"fmt"
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

// 轮转：超过 maxFileBackups 份后自动删最旧的，保留最新的。
//
// 预置的 25 份时间戳全部早于运行时刻（2020 年），backup() 新生成的
// 那份必然排在最前，删除集合完全确定、不赌时钟。其中掺了 5 份
// 定宽奈秒新格式，与秒级旧格式混排：整秒相同时旧格式（'+' 结尾）
// 应排在新格式（'.' 结尾）之前——更早生成的排更前。
func TestGuard_WriteBackupRotation(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	// 20 份秒级旧格式 + 5 份定宽奈秒新格式。
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("notes.txt.bak.2020-01-01T00:00:%02d+08:00", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("notes.txt.bak.2020-01-01T00:00:%02d.%09d+08:00", i-1, i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := g.Write(0, "notes.txt", strings.NewReader("rotated\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	backups := backupsOf(t, root, "notes.txt")
	if len(backups) != maxFileBackups {
		t.Fatalf("备份份数 = %d，want %d：%v", len(backups), maxFileBackups, backups)
	}
	// 字典序最小的 6 份应被删掉：00:00/00:01/00:02 的旧新各一份。
	for _, gone := range []string{
		"notes.txt.bak.2020-01-01T00:00:00+08:00",
		"notes.txt.bak.2020-01-01T00:00:00.000000001+08:00",
		"notes.txt.bak.2020-01-01T00:00:01+08:00",
		"notes.txt.bak.2020-01-01T00:00:01.000000002+08:00",
		"notes.txt.bak.2020-01-01T00:00:02+08:00",
		"notes.txt.bak.2020-01-01T00:00:02.000000003+08:00",
	} {
		if _, err := os.Lstat(filepath.Join(root, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("最旧的 %s 应被轮转删除，got err=%v", gone, err)
		}
	}
	// 留下的应含第 3 秒起的旧格式与最后两份新格式。
	for _, keep := range []string{
		"notes.txt.bak.2020-01-01T00:00:03+08:00",
		"notes.txt.bak.2020-01-01T00:00:19+08:00",
		"notes.txt.bak.2020-01-01T00:00:03.000000004+08:00",
		"notes.txt.bak.2020-01-01T00:00:04.000000005+08:00",
	} {
		if _, err := os.Lstat(filepath.Join(root, keep)); err != nil {
			t.Errorf("较新的 %s 应保留，got err=%v", keep, err)
		}
	}
	if got := mustRead(t, filepath.Join(root, "notes.txt")); got != "rotated\n" {
		t.Errorf("目标内容 = %q，want rotated\\n", got)
	}
}

// 轮转不误伤：撞进 <base>.bak. 前缀的目录/软链形态项，以及别文件的
// 备份、原文件本身，都不在删除范围内。非 regular 项被跳过时还会
// 占掉一个名额——宁肯少删一个，也不删错。
func TestGuard_WriteBackupRotationSkipsNonRegular(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	// 20 份整，不超限；外加一个目录和一个软链，名字都排在所有
	// 备份之前（'!'/'0' < '2'），一进待删区就会被 IsRegular 拦下。
	for i := 0; i < maxFileBackups; i++ {
		name := fmt.Sprintf("notes.txt.bak.2020-01-01T00:00:%02d+08:00", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "notes.txt.bak.!dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other.txt", filepath.Join(root, "notes.txt.bak.0link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	otherBackup := "other.txt.bak.2020-01-01T00:00:00+08:00"
	if err := os.WriteFile(filepath.Join(root, otherBackup), []byte("other-old"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := g.Write(0, "notes.txt", strings.NewReader("kept\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// 23 个前缀项（20 预置 + 2 非 regular + 1 新生成）超限 3 个，
	// 待删的是字典序最旧 3 个：00:00:00 备份（regular，真删）+
	// 目录/软链（跳过）。即 regular 备份最终保留 20 份——跳过非
	// regular 项但名额照占，与「只对 regular 计数」等价。
	all := backupsOf(t, root, "notes.txt")
	if len(all) != 22 {
		t.Errorf("前缀项 = %d，want 22：%v", len(all), all)
	}
	if _, err := os.Lstat(filepath.Join(root, "notes.txt.bak.2020-01-01T00:00:00+08:00")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("00:00:00 是最旧项之一，应被删，got err=%v", err)
	}
	for _, keep := range []string{
		"notes.txt.bak.!dir",
		"notes.txt.bak.0link",
		"notes.txt.bak.2020-01-01T00:00:01+08:00",
		"notes.txt.bak.2020-01-01T00:00:19+08:00",
		"notes.txt",
		otherBackup,
	} {
		if _, err := os.Lstat(filepath.Join(root, keep)); err != nil {
			t.Errorf("%s 不该被轮转碰掉，got err=%v", keep, err)
		}
	}
}

// 并发覆盖写同一文件：选名与占名是同一个 O_EXCL 调用，没有
// 「检查后又被抢占」的窗口——每个 goroutine 都该成功，且备份
// 一份不少（竞态撞名自动换 .N，不是失败）。
func TestGuard_WriteBackupConcurrentKeepsAll(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			_, err := g.Write(0, "notes.txt", strings.NewReader(fmt.Sprintf("v%d\n", i)))
			errs <- err
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("并发 Write #%d: %v", i, err)
		}
	}
	backups := backupsOf(t, root, "notes.txt")
	if len(backups) != n {
		t.Errorf("备份份数 = %d，want %d：%v", len(backups), n, backups)
	}
}
