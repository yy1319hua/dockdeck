// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// getRaw 直接打 handler，不带任何凭据——文档三件套必须公开可读。
func getRaw(t *testing.T, h *hub, path string) (int, string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, req)
	res := w.Result()
	defer res.Body.Close()
	var b strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return res.StatusCode, res.Header.Get("Content-Type"), b.String()
}

// TestDocsTripletPublic 守 B3：文档公开只读，未登录也能取。
func TestDocsTripletPublic(t *testing.T) {
	h := newHub(t)

	cases := []struct {
		path string
		want string // Content-Type 前缀
	}{
		{"/docs/openapi.json", "application/json"},
		{"/docs/llms.txt", "text/plain"},
		{"/docs/api.md", "text/markdown"},
	}
	for _, c := range cases {
		code, ct, body := getRaw(t, h, c.path)
		if code != http.StatusOK {
			t.Errorf("%s 状态码 %d，期望 200", c.path, code)
		}
		if !strings.HasPrefix(ct, c.want) {
			t.Errorf("%s Content-Type %q，期望前缀 %q", c.path, ct, c.want)
		}
		if len(body) < 200 {
			t.Errorf("%s 内容过短（%d 字节），像是空壳", c.path, len(body))
		}
	}
}

// TestDocsSpecMatchesAPI 守 B4：/docs/openapi.json 与 /api/openapi.json 必须字节一致。
// 两者都调 s.spec()，一旦有人给文档端点单独存一份 spec，这条会红。
func TestDocsSpecMatchesAPI(t *testing.T) {
	h := newHub(t)

	_, _, docsSpec := getRaw(t, h, "/docs/openapi.json")
	_, _, apiSpec := getRaw(t, h, "/api/openapi.json")
	if docsSpec != apiSpec {
		t.Fatal("/docs/openapi.json 与 /api/openapi.json 不一致——文档不是同源生成")
	}
}

// TestDocsCoverEveryEndpoint 守「文档不落后于路由」：
// llms.txt 与 api.md 里出现的端点条目数必须等于 spec 的端点数。
func TestDocsCoverEveryEndpoint(t *testing.T) {
	h := newHub(t)

	_, _, specBody := getRaw(t, h, "/docs/openapi.json")
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal([]byte(specBody), &doc); err != nil {
		t.Fatalf("解析 spec: %v", err)
	}
	total := 0
	for _, item := range doc.Paths {
		for _, m := range []string{"get", "post", "put", "patch", "delete"} {
			if _, ok := item[m]; ok {
				total++
			}
		}
	}
	if total == 0 {
		t.Fatal("spec 里没有任何端点，测试前提不成立")
	}

	// llms.txt：每条端点索引是 "- `METHOD /path` — 摘要"
	_, _, llms := getRaw(t, h, "/docs/llms.txt")
	llmsCount := 0
	for _, line := range strings.Split(llms, "\n") {
		if strings.HasPrefix(line, "- `") && strings.Contains(line, " /api/") {
			llmsCount++
		}
	}
	if llmsCount != total {
		t.Errorf("llms.txt 端点条目 %d，spec 端点 %d——文档落后于路由", llmsCount, total)
	}

	// api.md：每个端点一个小节 "### `METHOD /path`"
	_, _, apimd := getRaw(t, h, "/docs/api.md")
	mdCount := strings.Count(apimd, "### `")
	if mdCount != total {
		t.Errorf("api.md 端点小节 %d，spec 端点 %d——文档落后于路由", mdCount, total)
	}
}

// TestDocsNoSensitiveData 守红线：公开文档只允许出现占位域名。
//
// 刻意用「白名单」而不是把真实地址写进黑名单——
// 一旦把部署机的域名/IP 字面量写进源码，它本身就成了一次泄露
// （也必然被 credscan 拦下）。白名单在语义上也更严：将来任何新增外链都会红。
func TestDocsNoSensitiveData(t *testing.T) {
	h := newHub(t)

	// 允许出现的 http(s) 域名：spec 占位域名、许可证站点、安装脚本的 raw 源。
	// 前三者都在入库文件里公开可见，不含部署信息。
	allowed := map[string]bool{
		"hub.example.com":           true,
		"polyformproject.org":       true,
		"raw.githubusercontent.com": true,
	}
	linkRe := regexp.MustCompile(`https?://([A-Za-z0-9.\-]+)`)

	for _, p := range []string{"/docs/openapi.json", "/docs/llms.txt", "/docs/api.md"} {
		_, _, body := getRaw(t, h, p)
		for _, m := range linkRe.FindAllStringSubmatch(body, -1) {
			if !allowed[m[1]] {
				t.Errorf("%s 出现未登记的域名 %q——文档不得泄露部署地址", p, m[1])
			}
		}
		// 内网段不依赖 host 白名单：即便以非 URL 形态出现也要拦住。
		if strings.Contains(body, "127.0.0.1") {
			t.Errorf("%s 出现回环地址", p)
		}
	}
}
