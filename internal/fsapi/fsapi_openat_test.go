// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package fsapi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 多跳软链（含相对目标）应逐段解析到最终真实文件——验证 descend 的递归下沉。
func TestGuard_OpenatMultiHopSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// l1 -> a（绝对），l2 -> l1（相对）：两跳都必须在白名单内正确解析。
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "l1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("l1", filepath.Join(root, "l2")); err != nil {
		t.Fatal(err)
	}
	g := newGuard(t, []string{root}, 5)

	res, err := g.Resolve(0, "l2/f.txt")
	if err != nil {
		t.Fatalf("多跳软链应解析: %v", err)
	}
	if want := filepath.Join(root, "a", "f.txt"); res.Abs != want {
		t.Errorf("Abs = %q，want %q", res.Abs, want)
	}
	if res.Rel != "a/f.txt" {
		t.Errorf("Rel = %q，want a/f.txt", res.Rel)
	}
}

// 相对目标经 ".." 逃逸出白名单：必须拒绝（symlinkTail 归一后 withinRoot 失败）。
func TestGuard_OpenatRelativeSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "s.txt"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(root, "esc")); err != nil {
		t.Fatal(err)
	}
	g := newGuard(t, []string{root}, 5)

	if _, err := g.Resolve(0, "esc/s.txt"); !errors.Is(err, ErrPathDenied) {
		t.Fatalf("相对软链逃逸应 ErrPathDenied，got %v", err)
	}
}

// 指向根自身的软链：解析回根，Rel 为空串——验证 symlinkTail 产出空段序列。
func TestGuard_OpenatSymlinkToRootItself(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(root, "self")); err != nil {
		t.Fatal(err)
	}
	g := newGuard(t, []string{root}, 5)

	res, err := g.Resolve(0, "self")
	if err != nil {
		t.Fatalf("指向根的软链应解析: %v", err)
	}
	if res.Rel != "" || res.Abs != root {
		t.Errorf("got Rel=%q Abs=%q，want 空串与 %q", res.Rel, res.Abs, root)
	}
}

// 自指软链：不得死循环，必须被 maxSymlinkHops 拦成 ErrPathDenied。
func TestGuard_OpenatSymlinkLoop(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "data")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(root, "loop")); err != nil {
		t.Fatal(err)
	}
	g := newGuard(t, []string{root}, 5)

	if _, err := g.Resolve(0, "loop"); !errors.Is(err, ErrPathDenied) {
		t.Fatalf("软链环应被拒（ErrPathDenied），got %v", err)
	}
}
