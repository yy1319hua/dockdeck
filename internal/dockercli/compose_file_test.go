// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.3：Compose 文件读写（ReadFile/Diff/WriteFile）的单元测试。
// 文件操作不碰 docker CLI，直接走真实文件系统（t.TempDir），验证读原样、
// diff 正确、写前备份（备份内容==原文、备份名带 .bak.<ts>、原文件已更新）。

package dockercli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposeReadFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "docker-compose.yml")
	const content = "services:\n  web:\n    image: nginx\n"
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewComposeClient(nil)
	got, err := c.ReadFile(context.Background(), f)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != content {
		t.Errorf("ReadFile 内容不符: got %q want %q", got, content)
	}
}

func TestComposeReadFileNotFound(t *testing.T) {
	c := NewComposeClient(nil)
	if _, err := c.ReadFile(context.Background(), filepath.Join(t.TempDir(), "nope.yml")); err == nil {
		t.Errorf("读取不存在的文件应报错")
	}
}

func TestComposeDiff(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(f, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewComposeClient(nil)

	// 内容相同 → 空 diff、无错。
	if d, err := c.Diff(context.Background(), f, "a\nb\nc\n"); err != nil || d != "" {
		t.Errorf("相同内容 diff 应为空且无错: d=%q err=%v", d, err)
	}
	// 有改动 → 非空且含预期 +/- 标记。
	d, err := c.Diff(context.Background(), f, "a\nB\nc\n")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(d, "- b") || !strings.Contains(d, "+ B") {
		t.Errorf("diff 未含预期变更标记: %q", d)
	}
}

func TestComposeWriteFileBacksUp(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "docker-compose.yml")
	const orig = "services:\n  web:\n    image: nginx\n"
	const next = "services:\n  web:\n    image: nginx:1.27\n"
	if err := os.WriteFile(f, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewComposeClient(nil)

	bak, err := c.WriteFile(context.Background(), f, next)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 备份文件名：<file>.bak.<ts>。
	if !strings.HasPrefix(bak, f+".bak.") {
		t.Errorf("备份路径不符: %q", bak)
	}
	// 备份内容 == 原文（回滚基线）。
	bdata, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("读备份文件: %v", err)
	}
	if string(bdata) != orig {
		t.Errorf("备份内容应为原文: got %q want %q", bdata, orig)
	}
	// 原文件已更新为新内容。
	ndata, err := os.ReadFile(f)
	if err != nil {
		t.Fatalf("读原文件: %v", err)
	}
	if string(ndata) != next {
		t.Errorf("原文件未更新: got %q want %q", ndata, next)
	}
}

func TestComposeWriteFileMissingSource(t *testing.T) {
	// 原文件不存在 → 读失败 → 直接中止，不创建文件（防止「无备份的写」）。
	// 只编辑已发现的 compose 项目，不通过此路径新建文件。
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.yml")
	c := NewComposeClient(nil)
	if _, err := c.WriteFile(context.Background(), missing, "x\n"); err == nil {
		t.Errorf("原文件不存在时 WriteFile 应报错")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("原文件不存在时不应被创建")
	}
}
