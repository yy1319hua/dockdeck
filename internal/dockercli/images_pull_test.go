// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSplitImageRef(t *testing.T) {
	cases := []struct {
		ref               string
		wantFrom, wantTag string
	}{
		{"alpine", "alpine", ""},
		{"alpine:3.20", "alpine", "3.20"},
		{"library/nginx:latest", "library/nginx", "latest"},
		{"registry.example.com:5000/team/app:v1.2.3", "registry.example.com:5000/team/app", "v1.2.3"},
		{"registry.example.com:5000/app", "registry.example.com:5000/app", ""},                 // 端口冒号不拆
		{"app@sha256:" + strings.Repeat("a", 64), "app@sha256:" + strings.Repeat("a", 64), ""}, // digest 不拆
		{"a/b/c", "a/b/c", ""},
	}
	for _, tc := range cases {
		gotFrom, gotTag := splitImageRef(tc.ref)
		if gotFrom != tc.wantFrom || gotTag != tc.wantTag {
			t.Errorf("splitImageRef(%q) = (%q,%q), want (%q,%q)", tc.ref, gotFrom, gotTag, tc.wantFrom, tc.wantTag)
		}
	}
}

func TestPull_SummarizesStream(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)
	// 样本按 Engine 29.x /images/create 真实事件形状：首条事件 id 是
	// tag 名（"3.20"），层事件 id 是 64 位 hex 摘要。
	layer1 := strings.Repeat("a", 64)
	layer2 := strings.Repeat("b", 64)
	f.pullStream = []byte(`{"status":"Pulling from library/alpine","id":"3.20"}
{"status":"Pulling fs layer","id":"` + layer1 + `"}
{"status":"Downloading","id":"` + layer1 + `","progress":"[==>] 2.8MB/3.4MB"}
{"status":"Download complete","id":"` + layer1 + `"}
{"status":"Pull complete","id":"` + layer1 + `","progress":"Digest: sha256:aaa"}
{"status":"Pull complete","id":"` + layer2 + `"}
{"status":"Downloaded newer image for alpine:3.20"}
`)

	res, err := c.Pull(context.Background(), "alpine:3.20")
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res.Err != "" {
		t.Errorf("Err 应为空: %q", res.Err)
	}
	if want := "Downloaded newer image for alpine:3.20"; res.Status != want {
		t.Errorf("Status = %q, want %q", res.Status, want)
	}
	// 层 ID 去重保序：layer1 出现四次只记一次；tag 名 "3.20" 不是层。
	if len(res.Layers) != 2 || res.Layers[0] != layer1 || res.Layers[1] != layer2 {
		t.Errorf("Layers = %v, want [%s %s]", res.Layers, layer1, layer2)
	}
	// fromImage/tag 拆成两个参数抵达 Engine。
	uri := f.hitPaths()[len(f.hitPaths())-1]
	if !strings.Contains(uri, "fromImage=alpine") || !strings.Contains(uri, "tag=3.20") {
		t.Errorf("查询串拆分不对: %s", uri)
	}
}

func TestPull_EngineErrorInStream(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)
	// HTTP 200 但流里带 error：Engine 把业务错误放在流里而非状态码。
	f.pullStream = []byte(`{"status":"Pulling from library/notexist"}
{"error":"pull access denied for notexist, repository does not exist"}
`)

	res, err := c.Pull(context.Background(), "notexist")
	if err != nil {
		t.Fatalf("Pull 本身不该失败（HTTP 200）: %v", err)
	}
	if res.Err == "" {
		t.Fatal("Err 应捕获流内错误")
	}
	if !strings.Contains(res.Err, "does not exist") {
		t.Errorf("Err 文本不对: %q", res.Err)
	}
}

func TestPull_HTTPError(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)
	f.pullStatus = 404

	if _, err := c.Pull(context.Background(), "alpine"); err == nil {
		t.Fatal("Engine 404 应报错")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("错误应带状态码: %v", err)
	}
}

func TestPull_BadRefRejectedLocally(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)

	for _, bad := range []string{"", "../etc", "a b", "alpine@sha256:short"} {
		if _, err := c.Pull(context.Background(), bad); err == nil {
			t.Errorf("非法 ref %q 应本地拒绝", bad)
		} else if !strings.Contains(err.Error(), "非法镜像引用") {
			t.Errorf("错误口径不对: %v", err)
		}
	}
	// 假 Engine 不该收到任何请求——本地校验发生在出门前。
	if paths := f.hitPaths(); len(paths) != 0 {
		t.Errorf("非法 ref 不该打到 Engine: %v", paths)
	}
}

func TestPull_CtxCancel(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)
	// 慢流：假 Engine 写完头后拖住——用已取消的 ctx 验证取消路径
	// 不挂死（Engine 请求失败即返）。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Pull(ctx, "alpine"); err == nil {
		t.Fatal("已取消的 ctx 应失败")
	}
	_ = f
}

func TestPull_DigestRefNotSplit(t *testing.T) {
	f, sock := newFakeEngine(t, "1.51")
	c := New(sock)
	digest := "app@sha256:" + strings.Repeat("b", 64)
	if _, err := c.Pull(context.Background(), digest); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	uri := f.hitPaths()[len(f.hitPaths())-1]
	if strings.Contains(uri, "tag=") {
		t.Errorf("digest 形态不该拆出 tag: %s", uri)
	}
	if !strings.Contains(uri, "fromImage=app%40sha256%3A"+strings.Repeat("b", 64)) {
		t.Errorf("digest 应整体作 fromImage 且编码: %s", uri)
	}
}

// TestPull_LongTimeoutConfigured 钉住PullTimeout 量级：回归防线——
// 谁把它改回 10s（默认请求超时）大镜像必挂，而这个 bug 只在真机
// 拉大镜像时才暴露，测试环境永远绿。
func TestPull_LongTimeoutConfigured(t *testing.T) {
	if PullTimeout < 5*time.Minute {
		t.Errorf("PullTimeout = %v，pull 长操作至少给 5 分钟", PullTimeout)
	}
}
