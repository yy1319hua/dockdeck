// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"strings"
	"testing"
)

func TestDelete_OK(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)

	if err := c.Delete(context.Background(), "alpine:3.20"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// ref 原样抵达 Engine 路径（冒号不过转义）。
	uri := f.hitPaths()[len(f.hitPaths())-1]
	if !strings.HasSuffix(uri, "/images/alpine:3.20") {
		t.Errorf("删除路径不对: %s", uri)
	}
}

func TestDelete_StatusMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantSubstr string
	}{
		{"不存在", 404, "不存在 (404)"},
		{"被占用冲突", 409, "(409)"},
		{"引擎异常", 500, "500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, sock := newFakeEngine(t, "1.51")
			c := New(sock)
			f.deleteStatus = tc.status
			err := c.Delete(context.Background(), "alpine")
			if err == nil {
				t.Fatalf("Engine %d 应报错", tc.status)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("错误文案缺 %q: %v", tc.wantSubstr, err)
			}
		})
	}
}

func TestDelete_BadRefRejectedLocally(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)

	for _, bad := range []string{"", "../etc", "a b", "x@sha256:short"} {
		if err := c.Delete(context.Background(), bad); err == nil {
			t.Errorf("非法 ref %q 应本地拒绝", bad)
		}
	}
	if paths := f.hitPaths(); len(paths) != 0 {
		t.Errorf("非法 ref 不该打到 Engine: %v", paths)
	}
}

func TestPrune_Result(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)
	// 样本按 Engine 29.x /images/prune 真实响应：Deleted 与 Untagged 两类条目。
	f.pruneBody = []byte(`{"ImagesDeleted":[{"Deleted":"sha256:cccc"},{"Untagged":"old-app:v1"}],"SpaceReclaimed":24000000}`)

	res, err := c.Prune(context.Background())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(res.ImagesDeleted) != 2 || res.SpaceReclaimed != 24000000 {
		t.Errorf("prune 结果解码不对: %+v", res)
	}
	if res.ImagesDeleted[0].Deleted != "sha256:cccc" {
		t.Errorf("Deleted 条目不对: %+v", res.ImagesDeleted[0])
	}
	if res.ImagesDeleted[1].Untagged != "old-app:v1" {
		t.Errorf("Untagged 条目不对: %+v", res.ImagesDeleted[1])
	}
	uri := f.hitPaths()[len(f.hitPaths())-1]
	if !strings.HasSuffix(uri, "/images/prune") {
		t.Errorf("prune 路径不对: %s", uri)
	}
}

func TestPrune_EngineError(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)
	f.pruneStatus = 500

	if _, err := c.Prune(context.Background()); err == nil {
		t.Fatal("Engine 500 应报错")
	} else if !strings.Contains(err.Error(), "500") {
		t.Errorf("错误应带状态码: %v", err)
	}
}
