// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package fsapi

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testTree 造一棵固定的白名单树，返回根路径。
//
// 形状（base = t.TempDir()）：
//
//	base/data/                      ← 白名单根
//	  notes.txt
//	  sub/inner.txt
//	  sub/.env                      ← 黑名单：段名
//	  sub/key.pem                   ← 黑名单：*.pem
//	  keys/server.pem
//	  keys/server.conf              ← 黑名单：整条相对路径 keys/server.conf
//	  cert.txt  -> keys/server.pem  ← 白名单内软链，真实路径命中黑名单
//	  inlink    -> sub              ← 白名单内软链，合法
//	  escape    -> base/outside     ← 软链指到白名单外
//	  evil      -> base/data-evil   ← 前缀混淆：data-evil 冒充 data
//	base/outside/secret.txt
//	base/data-evil/secret.txt
func testTree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "data")
	evil := filepath.Join(base, "data-evil")
	outside := filepath.Join(base, "outside")

	for _, d := range []string{root, evil, outside, filepath.Join(root, "sub"), filepath.Join(root, "keys")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("建目录 %s: %v", d, err)
		}
	}
	files := map[string]string{
		filepath.Join(root, "notes.txt"):           "notes\n",
		filepath.Join(root, "sub", "inner.txt"):    "inner\n",
		filepath.Join(root, "sub", ".env"):         "SECRET=1\n",
		filepath.Join(root, "sub", "key.pem"):      "pem\n",
		filepath.Join(root, "keys", "server.pem"):  "pem\n",
		filepath.Join(root, "keys", "server.conf"): "conf\n",
		filepath.Join(evil, "secret.txt"):          "evil\n",
		filepath.Join(outside, "secret.txt"):       "outside\n",
	}
	for p, content := range files {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("写文件 %s: %v", p, err)
		}
	}
	links := map[string]string{
		filepath.Join(root, "cert.txt"): filepath.Join(root, "keys", "server.pem"),
		filepath.Join(root, "inlink"):   filepath.Join(root, "sub"),
		filepath.Join(root, "escape"):   outside,
		filepath.Join(root, "evil"):     evil,
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("建软链 %s -> %s: %v", link, target, err)
		}
	}
	return root
}

// defaultTestDeny 是测试用的黑名单，覆盖三种 pattern 形态：
// 段名（.env / shadow）、通配（*.pem / *.key）、整条相对路径（keys/server.conf）。
var defaultTestDeny = []string{"id_rsa", "*.pem", "*.key", ".env", "shadow", "keys/server.conf"}

func newGuardLimits(t *testing.T, roots, deny []string, maxFile int64, maxDepth, maxEntries int) *Guard {
	t.Helper()
	g, err := NewGuard(roots, deny, maxFile, maxDepth, maxEntries)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}
	return g
}

func newGuard(t *testing.T, roots []string, maxDepth int) *Guard {
	t.Helper()
	return newGuardLimits(t, roots, defaultTestDeny, 1<<20, maxDepth, 100)
}

func TestGuard_ResolveRejectsEscapes(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	cases := []struct {
		name string
		rel  string
		want error
	}{
		{"绝对路径", "/etc/passwd", ErrPathEscape},
		{"绝对路径带斜杠前缀", "//etc/passwd", ErrPathEscape},
		{"上级穿越", "../outside/secret.txt", ErrPathEscape},
		{"中段穿越", "sub/../../notes.txt", ErrPathEscape},
		{"仅两点", "..", ErrPathEscape},
		{"尾段两点", "sub/..", ErrPathEscape},
		{"NUL", "notes\x00.txt", ErrPathEscape},
		{"换行", "notes\n.txt", ErrPathEscape},
		{"制表符", "no\ttes.txt", ErrPathEscape},
		{"超长", strings.Repeat("a", maxRelLen+1), ErrPathEscape},
		{"软链指向目录外", "escape/secret.txt", ErrPathDenied},
		{"软链自身指向目录外", "escape", ErrPathDenied},
		{"前缀混淆 data-evil", "evil/secret.txt", ErrPathDenied},
		{"黑名单段名", "sub/.env", ErrPathDenied},
		{"黑名单通配", "sub/key.pem", ErrPathDenied},
		{"黑名单整路径", "keys/server.conf", ErrPathDenied},
		{"软链真实路径命中黑名单", "cert.txt", ErrPathDenied},
		{"不存在", "sub/missing.txt", ErrNotFound},
		{"过深", "a/b/c/d.txt", ErrTooDeep},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := g.Resolve(0, tc.rel)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Resolve(%q) 错误 = %v，want %v", tc.rel, err, tc.want)
			}
			if err == nil {
				t.Fatalf("Resolve(%q) 应失败", tc.rel)
			}
		})
	}
}

func TestGuard_ResolveAcceptsInsidePaths(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	cases := []struct {
		name    string
		rel     string
		wantRel string
	}{
		{"普通文件", "notes.txt", "notes.txt"},
		{"子目录文件", "sub/inner.txt", "sub/inner.txt"},
		{"点段被规范化", "sub/./inner.txt", "sub/inner.txt"},
		{"重复斜杠被规范化", "sub//inner.txt", "sub/inner.txt"},
		{"压根自身", "", ""},
		{"压根自身斜杠写法", "/", ""},
		{"子目录本身", "sub", "sub"},
		{"白名单内软链", "inlink/inner.txt", "sub/inner.txt"},
		{"白名单内软链本身", "inlink", "sub"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := g.Resolve(0, tc.rel)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tc.rel, err)
			}
			if res.Rel != tc.wantRel {
				t.Errorf("Rel = %q，want %q", res.Rel, tc.wantRel)
			}
			if !filepath.IsAbs(res.Abs) {
				t.Errorf("Abs 应为绝对路径，got %q", res.Abs)
			}
			if !withinRoot(root, res.Abs) {
				t.Errorf("Abs %q 应在根 %q 内", res.Abs, root)
			}
		})
	}
}

// 深度上限按「相对根的层数」算：恰好等于上限放行，超一层即拒。
func TestGuard_ResolveEnforcesDepthBoundary(t *testing.T) {
	root := testTree(t)
	if err := os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := newGuard(t, []string{root}, 3)
	if _, err := g.Resolve(0, "a/b/c"); err != nil {
		t.Errorf("深度 3 = 上限 3，应放行，got %v", err)
	}
	if _, err := g.Resolve(0, "a/b/c/d"); !errors.Is(err, ErrTooDeep) {
		t.Errorf("深度 4 > 上限 3，应报 ErrTooDeep，got %v", err)
	}
}

func TestGuard_ResolveNewAllowsMissingTarget(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	ok := []struct {
		name    string
		rel     string
		wantRel string
	}{
		{"根下新文件", "new.txt", "new.txt"},
		{"子目录新文件", "sub/new.txt", "sub/new.txt"},
		{"软链目录下新文件", "inlink/new.txt", "sub/new.txt"},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			res, err := g.ResolveNew(0, tc.rel)
			if err != nil {
				t.Fatalf("ResolveNew(%q): %v", tc.rel, err)
			}
			if res.Rel != tc.wantRel {
				t.Errorf("Rel = %q，want %q", res.Rel, tc.wantRel)
			}
		})
	}

	bad := []struct {
		name string
		rel  string
		want error
	}{
		{"父目录不存在", "missing/new.txt", ErrNotFound},
		{"软链父目录出白名单", "escape/pwn.txt", ErrPathDenied},
		{"前缀混淆父目录", "evil/pwn.txt", ErrPathDenied},
		{"黑名单段名", "sub/.env", ErrPathDenied},
		{"越界", "../pwn.txt", ErrPathEscape},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := g.ResolveNew(0, tc.rel); !errors.Is(err, tc.want) {
				t.Fatalf("ResolveNew(%q) 错误 = %v，want %v", tc.rel, err, tc.want)
			}
		})
	}
}

// 根的路径本身也可能含软链（例如 --fs-root /srv/data，而 /srv 是软链）。
// 构造期必须把根也 realpath：否则「解引用后再比前缀」这一步会把每个请求
// 都误判成逃逸——不是安全漏洞（拒得过头），但会让功能完全不可用。
func TestGuard_RootPathWithSymlinkComponent(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}

	g := newGuard(t, []string{link}, 3)
	res, err := g.Resolve(0, "a.txt")
	if err != nil {
		t.Fatalf("根路径含软链时应能正常解析（构造期需 realpath 根）: %v", err)
	}
	if want := filepath.Join(realDir, "a.txt"); res.Abs != want {
		t.Errorf("Abs = %q，want %q", res.Abs, want)
	}
	if _, err := g.ResolveNew(0, "new.txt"); err != nil {
		t.Errorf("ResolveNew 也应正常: %v", err)
	}
}

// ResolveNew 只做路径校验，不判类型：rel "" 解析出来就是根目录本身。
// 「不能把目录当文件写」是操作层的事（ErrIsDir），不是解析层的事。
func TestGuard_ResolveNewRootItselfIsPathValid(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)
	res, err := g.ResolveNew(0, "")
	if err != nil {
		t.Fatalf("根自身应能通过路径校验: %v", err)
	}
	if res.Rel != "" || res.Abs != root {
		t.Errorf("got Rel=%q Abs=%q，want 空串与 %q", res.Rel, res.Abs, root)
	}
}

func TestGuard_ResolveRejectsBadRootIndex(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)
	for _, idx := range []int{-1, 1, 99} {
		if _, err := g.Resolve(idx, "notes.txt"); !errors.Is(err, ErrPathEscape) {
			t.Errorf("根序号 %d 应被拒绝，got %v", idx, err)
		}
	}
	if g.Roots() != 1 {
		t.Errorf("Roots() = %d，want 1", g.Roots())
	}
}

func TestGuard_MultipleRoots(t *testing.T) {
	base := t.TempDir()
	r1 := filepath.Join(base, "one")
	r2 := filepath.Join(base, "two")
	for _, d := range []string{r1, r2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(r2, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newGuard(t, []string{r1, r2}, 3)
	if g.Roots() != 2 {
		t.Fatalf("Roots() = %d，want 2", g.Roots())
	}
	res, err := g.Resolve(1, "b.txt")
	if err != nil {
		t.Fatalf("第二个根内应可解析: %v", err)
	}
	if res.RootIndex != 1 {
		t.Errorf("RootIndex = %d，want 1", res.RootIndex)
	}
	if _, err := g.Resolve(0, "b.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("第一个根内没有 b.txt，应 ErrNotFound，got %v", err)
	}
}

// 根目录的配置错误必须在启动时就炸，而不是等第一个请求。
func TestNewGuard_RejectsBadConfig(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := NewGuard(nil, nil, 1<<20, 3, 10); !errors.Is(err, ErrNoRoots) {
		t.Errorf("无根目录应 ErrNoRoots，got %v", err)
	}
	if _, err := NewGuard([]string{"relative/dir"}, nil, 1<<20, 3, 10); !errors.Is(err, ErrBadRoot) {
		t.Errorf("相对路径根应 ErrBadRoot，got %v", err)
	}
	if _, err := NewGuard([]string{filepath.Join(base, "nope")}, nil, 1<<20, 3, 10); !errors.Is(err, ErrBadRoot) {
		t.Errorf("不存在的根应 ErrBadRoot，got %v", err)
	}
	if _, err := NewGuard([]string{file}, nil, 1<<20, 3, 10); !errors.Is(err, ErrBadRoot) {
		t.Errorf("根是普通文件应 ErrBadRoot，got %v", err)
	}
	// 坏 pattern 必须在启动时被拒：留到请求期，「忽略 path.Match 错误」等于关掉保护。
	if _, err := NewGuard([]string{dir}, []string{"["}, 1<<20, 3, 10); err == nil {
		t.Error("非法黑名单 pattern 应在构造时报错")
	}
	for _, bad := range [][3]int{{0, 3, 10}, {1 << 20, 0, 10}, {1 << 20, 3, -1}} {
		if _, err := NewGuard([]string{dir}, nil, int64(bad[0]), bad[1], bad[2]); err == nil {
			t.Errorf("上限 %v 非正应报错", bad)
		}
	}
}

func TestWithinRootBoundary(t *testing.T) {
	sep := string(filepath.Separator)
	if withinRoot("/data", "/data-evil/x") {
		t.Error("/data-evil 不是 /data 的子路径（前缀混淆）")
	}
	if !withinRoot("/data", "/data") {
		t.Error("根自身应在白名单内")
	}
	if !withinRoot("/data", "/data/x/y") {
		t.Error("/data/x/y 应在 /data 内")
	}
	if !withinRoot(sep, "/etc/hosts") {
		t.Error("根为 / 时任何绝对路径都在其中")
	}
	if relOf("/data", "/data/a/b") != "a/b" {
		t.Errorf("relOf = %q，want a/b", relOf("/data", "/data/a/b"))
	}
	if relOf("/data", "/data") != "" {
		t.Errorf("根自身的 relOf 应为空串，got %q", relOf("/data", "/data"))
	}
	if relOf(sep, "/etc/hosts") != "etc/hosts" {
		t.Errorf("relOf(/, /etc/hosts) = %q，want etc/hosts", relOf(sep, "/etc/hosts"))
	}
}

func TestGuard_PathToRel(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 8)

	// 根自身：命中根序号 0，rel 为空串。
	idx, rel, ok := g.PathToRel(root)
	if !ok || idx != 0 || rel != "" {
		t.Errorf("根自身应命中 idx=0 rel=``，got (%d,%q,%v)", idx, rel, ok)
	}

	// 根内绝对路径：映射到正确 rel（分隔统一为 /）。
	idx, rel, ok = g.PathToRel(filepath.Join(root, "sub", "inner.txt"))
	if !ok || idx != 0 || rel != "sub/inner.txt" {
		t.Errorf("根内文件应命中，got (%d,%q,%v)", idx, rel, ok)
	}

	// 白名单外的绝对路径：不命中。
	outside := filepath.Join(filepath.Dir(root), "outside")
	if _, _, ok2 := g.PathToRel(filepath.Join(outside, "secret.txt")); ok2 {
		t.Error("白名单外不应命中")
	}

	// 前缀混淆：data-evil 不应被 data 当成自家孩子。
	evil := filepath.Join(filepath.Dir(root), "data-evil")
	if _, _, ok3 := g.PathToRel(filepath.Join(evil, "secret.txt")); ok3 {
		t.Error("data-evil 前缀混淆不应命中")
	}

	// 多根：落在第二根的绝对路径应映射到 idx=1。
	g2 := newGuard(t, []string{root, outside}, 8)
	idx, rel, ok = g2.PathToRel(filepath.Join(outside, "secret.txt"))
	if !ok || idx != 1 || rel != "secret.txt" {
		t.Errorf("第二根应命中 idx=1，got (%d,%q,%v)", idx, rel, ok)
	}
}
