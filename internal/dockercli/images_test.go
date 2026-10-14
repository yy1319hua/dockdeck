// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"strings"
	"testing"
)

// TestListImages 走主链路：真 unix socket 假 Engine，样本按 Engine 29.x
// GET /images/json 真实形状裁剪——正常镜像 RepoTags 有值，dangling 镜像
// RepoTags 为 null（Engine 原样如此）。
func TestListImages(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)
	f.images = []byte(`[
		{"Id":"sha256:aaaa","RepoTags":["alpine:3.20"],"RepoDigests":["alpine@sha256:bbbb"],"Created":1750000000,"Size":7890000,"Architecture":"arm64","Os":"linux","Labels":{}},
		{"Id":"sha256:cccc","RepoTags":null,"RepoDigests":null,"Created":1740000000,"Size":12000000,"Architecture":"arm64","Os":"linux","Labels":null}
	]`)

	list, err := c.ListImages(context.Background(), false, false)
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	if list[0].RepoTags[0] != "alpine:3.20" || list[0].Size != 7890000 || list[0].Architecture != "arm64" {
		t.Errorf("首条解码不符: %+v", list[0])
	}
	// dangling 镜像：RepoTags 为 null → nil slice，len==0（UI 判定口径）。
	if len(list[1].RepoTags) != 0 {
		t.Errorf("dangling 镜像 RepoTags 应为空, got %v", list[1].RepoTags)
	}
	// all=false 显式进查询串（与 ContainerList 同口径：不依赖隐式默认）。
	uri := f.hitPaths()[len(f.hitPaths())-1]
	if !strings.Contains(uri, "all=false") {
		t.Errorf("查询串缺 all=false: %s", uri)
	}
	// dangling=false 时不带 filters（Engine 默认列全部）。
	if strings.Contains(uri, "filters") {
		t.Errorf("dangling=false 不该带 filters: %s", uri)
	}
}

func TestListImages_DanglingFilter(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)

	if _, err := c.ListImages(context.Background(), false, true); err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	uri := f.hitPaths()[len(f.hitPaths())-1]
	// filters 固定构造 + URL 编码形态（Engine 接受的编码后 JSON）。
	if !strings.Contains(uri, "filters=%7B%22dangling%22%3A%5B%22true%22%5D%7D") {
		t.Errorf("filters 未按固定值编码透传: %s", uri)
	}
	// 裸花括号意味着没编码——& 与 # 会截断它。
	if strings.Contains(uri, "filters={") {
		t.Errorf("filters 裸传未编码: %s", uri)
	}
}

func TestListImages_EmptyIsSliceNotNull(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)
	f.images = []byte("[]")

	list, err := c.ListImages(context.Background(), true, false)
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if list == nil {
		t.Error("空清单必须是 [] 非 null——前端据此区分「没有镜像」与「没拿到数据」")
	}
	uri := f.hitPaths()[len(f.hitPaths())-1]
	if !strings.Contains(uri, "all=true") {
		t.Errorf("查询串缺 all=true: %s", uri)
	}
}

// TestListImages_EngineError 验证非 200 路径：错误带状态码与请求路径。
// 假 Engine 的 /images/ 路由只认 GET /images/json，其余 404——
// 顺带把 getJSON 的非 200 分支测了。
func TestListImages_EngineError(t *testing.T) {
	_, sock := newFakeEngine(t, "1.51")
	c := New(sock)

	err := c.getJSON(context.Background(), "/images/nonexistent", &[]ImageSummary{})
	if err == nil {
		t.Fatal("不存在路径应报错")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("错误应带状态码: %v", err)
	}
	if !strings.Contains(err.Error(), "/images/nonexistent") {
		t.Errorf("错误应带请求路径: %v", err)
	}
}
