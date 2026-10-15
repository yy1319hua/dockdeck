// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package fsapi

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// 列举的基本口径：看不见的是「不可操作的」，看得见的都能操作。
func TestGuard_ListHidesInaccessibleEntries(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	entries, truncated, err := g.List(0, "", 1)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if truncated {
		t.Error("小目录不该被截断")
	}

	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	for _, want := range []string{"notes.txt", "sub", "keys", "inlink"} {
		if !names[want] {
			t.Errorf("列举结果缺少 %q：%v", want, names)
		}
	}
	// escape 指向白名单外、evil 是前缀混淆、cert.txt 的真实路径命中黑名单：
	// 三者都必须不出现，否则用户点了会 403。
	for _, bad := range []string{"escape", "evil", "cert.txt"} {
		if names[bad] {
			t.Errorf("不可操作项 %q 不该出现在列举里", bad)
		}
	}

	// 软链按真实落点展示：Name 是链接名，Path 是解引用后的相对路径。
	paths := map[string]string{}
	for _, e := range entries {
		paths[e.Name] = e.Path
	}
	if paths["inlink"] != "sub" {
		t.Errorf("inlink 的 Path = %q，want sub（软链展示真实落点）", paths["inlink"])
	}
	if paths["notes.txt"] != "notes.txt" {
		t.Errorf("notes.txt 的 Path = %q", paths["notes.txt"])
	}
	// 目录体积约定为 0，模式串是四位八进制。
	for _, e := range entries {
		if e.IsDir && e.Size != 0 {
			t.Errorf("目录 %q 的 Size 应为 0，got %d", e.Name, e.Size)
		}
		if len(e.Mode) != 4 {
			t.Errorf("%q 的 Mode = %q，want 四位八进制", e.Name, e.Mode)
		}
	}
}

func TestGuard_ListDepth(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	depth1, _, err := g.List(0, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(depth1); contains(got, "sub/inner.txt") {
		t.Errorf("depth=1 不该递归，got %v", got)
	}

	// depth=2：进 sub 一层。sub/.env 与 sub/key.pem 被黑名单挡住，
	// keys/server.conf 与 keys/server.pem 同样——所以 keys 下没有任何条目。
	depth2, _, err := g.List(0, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"keys", "notes.txt", "sub", "sub", "sub/inner.txt"}
	if got := paths(depth2); !equalStrings(got, want) {
		t.Errorf("depth=2 结果 = %v，want %v", got, want)
	}
}

// 空目录必须返回「空切片」而不是 nil：HTTP 层直接把 Entries 序列化出去，
// nil 会变成 "entries":null，与容器/镜像清单的 [] 口径不一致。
func TestGuard_ListEmptyIsNeverNil(t *testing.T) {
	root := testTree(t)
	if err := os.MkdirAll(filepath.Join(root, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := newGuard(t, []string{root}, 3)

	entries, truncated, err := g.List(0, "emptydir", 1)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if truncated {
		t.Error("空目录不该标截断")
	}
	if entries == nil {
		t.Fatal("空目录应返回空切片而非 nil（否则对外会序列化成 null）")
	}
	if len(entries) != 0 {
		t.Errorf("len(entries) = %d，want 0", len(entries))
	}
}

func TestGuard_ListTruncatesAtMaxEntries(t *testing.T) {
	root := testTree(t)
	for _, name := range []string{"e1.txt", "e2.txt", "e3.txt", "e4.txt", "e5.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g := newGuardLimits(t, []string{root}, defaultTestDeny, 1<<20, 3, 3)
	entries, truncated, err := g.List(0, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("条目数超过 maxEntries 时应置 truncated")
	}
	if len(entries) != 3 {
		t.Errorf("len(entries) = %d，want 3（上限即上限，不多返回）", len(entries))
	}
}

func TestGuard_ListRejectsBadTarget(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	if _, _, err := g.List(0, "notes.txt", 1); !errors.Is(err, ErrNotDir) {
		t.Errorf("列一个普通文件应 ErrNotDir，got %v", err)
	}
	if _, _, err := g.List(0, "missing", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("列不存在的目录应 ErrNotFound，got %v", err)
	}
	if _, _, err := g.List(0, "sub/.env", 1); !errors.Is(err, ErrPathDenied) {
		t.Errorf("黑名单目标应 ErrPathDenied，got %v", err)
	}
	if _, _, err := g.List(0, "escape", 1); !errors.Is(err, ErrPathDenied) {
		t.Errorf("逃逸软链应 ErrPathDenied，got %v", err)
	}
}

func TestGuard_Stat(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	meta, err := g.Stat(0, "notes.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if meta.Path != "notes.txt" || meta.Size != int64(len("notes\n")) {
		t.Errorf("meta = %+v", meta)
	}
	if !strings.HasPrefix(meta.ContentType, "text/plain") {
		t.Errorf("ContentType = %q，want text/plain 系", meta.ContentType)
	}
	if meta.Truncated {
		t.Error("小文件不该标截断")
	}
	if meta.ModTime.IsZero() {
		t.Error("ModTime 应为真实时间")
	}

	// 目录走 List，不走 Stat。
	if _, err := g.Stat(0, "sub"); !errors.Is(err, ErrIsDir) {
		t.Errorf("Stat 目录应 ErrIsDir，got %v", err)
	}
	if _, err := g.Stat(0, "missing.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat 不存在应 ErrNotFound，got %v", err)
	}
	if _, err := g.Stat(0, "sub/key.pem"); !errors.Is(err, ErrPathDenied) {
		t.Errorf("Stat 黑名单应 ErrPathDenied，got %v", err)
	}

	// 超过单文件上限：如实标 Truncated，而不是假装文件就这么大。
	small := newGuardLimits(t, []string{root}, defaultTestDeny, 4, 3, 100)
	meta, err = small.Stat(0, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Truncated || meta.Size != int64(len("notes\n")) {
		t.Errorf("超限时应 Truncated=true 且 Size 为真实大小，got %+v", meta)
	}
}

func TestGuard_OpenStreamsContent(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	r, err := g.Open(0, "notes.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if string(body) != "notes\n" {
		t.Errorf("正文 = %q，want notes\\n", body)
	}
	if r.Meta.Truncated {
		t.Error("小文件不该标截断")
	}

	// 软链走的也是真实内容与真实相对路径。
	rl, err := g.Open(0, "inlink/inner.txt")
	if err != nil {
		t.Fatalf("Open 软链: %v", err)
	}
	defer rl.Close()
	if rl.Meta.Path != "sub/inner.txt" {
		t.Errorf("Meta.Path = %q，want sub/inner.txt", rl.Meta.Path)
	}
	bl, _ := io.ReadAll(rl.Body)
	if string(bl) != "inner\n" {
		t.Errorf("正文 = %q，want inner\\n", bl)
	}

	if _, err := g.Open(0, "sub"); !errors.Is(err, ErrIsDir) {
		t.Errorf("打开目录应 ErrIsDir，got %v", err)
	}
	if _, err := g.Open(0, "missing.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("打开不存在应 ErrNotFound，got %v", err)
	}
	if _, err := g.Open(0, "escape/secret.txt"); !errors.Is(err, ErrPathDenied) {
		t.Errorf("打开逃逸软链应 ErrPathDenied，got %v", err)
	}
}

// 截断必须精确落在 maxFile 上：多读一个字节就等于上限失效。
func TestGuard_OpenTruncatesAtMaxFile(t *testing.T) {
	root := testTree(t)
	g := newGuardLimits(t, []string{root}, defaultTestDeny, 4, 3, 100)

	r, err := g.Open(0, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "note" {
		t.Errorf("正文 = %q，want note（截在 4 字节）", body)
	}
	if !r.Meta.Truncated {
		t.Error("应标 Truncated")
	}
	if r.Meta.Size != int64(len("notes\n")) {
		t.Errorf("Size 应为真实大小 %d，got %d", len("notes\n"), r.Meta.Size)
	}
}

// 嗅探过的字节必须还给调用方：这是流式读取最容易丢开头的地方。
func TestGuard_ContentTypeSniffingKeepsBytes(t *testing.T) {
	root := testTree(t)
	g := newGuard(t, []string{root}, 3)

	cases := []struct {
		name        string
		content     string
		wantType    string
		wantContent string
	}{
		{"data.json", `{"a":1}`, "application/json", `{"a":1}`},
		{"plain", "hello world", "text/plain", "hello world"},
		{"blob", "\x00\x01\x02\x03", "application/octet-stream", "\x00\x01\x02\x03"},
		{"empty", "", "application/octet-stream", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(root, tc.name)
			if err := os.WriteFile(p, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			meta, err := g.Stat(0, tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(meta.ContentType, tc.wantType) {
				t.Errorf("Stat ContentType = %q，want %q 系", meta.ContentType, tc.wantType)
			}
			r, err := g.Open(0, tc.name)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if !strings.HasPrefix(r.Meta.ContentType, tc.wantType) {
				t.Errorf("Open ContentType = %q，want %q 系", r.Meta.ContentType, tc.wantType)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != tc.wantContent {
				t.Errorf("正文 = %q，want %q（嗅探后必须回退到文件头）", body, tc.wantContent)
			}
		})
	}
}

// paths 抽出 Path 列表，便于整体断言顺序与内容。
func paths(entries []protocol.FileEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
