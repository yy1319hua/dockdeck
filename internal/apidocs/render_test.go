// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package apidocs

import (
	"regexp"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/openapi"
)

func sampleDoc() *openapi.Doc {
	return &openapi.Doc{
		OpenAPI: "3.0.3",
		Info: openapi.Info{
			Title:       "DockDeck",
			Version:     "test",
			Description: "自托管 Docker 管理面板\n第二行也应被压成一行",
		},
		Tags: []openapi.Tag{
			{Name: "Servers", Description: "受管主机"},
			{Name: "Containers"},
		},
		Paths: map[string]openapi.PathItem{
			"/api/v1/servers": {
				Get: &openapi.Operation{
					Summary: "主机列表",
					Tags:    []string{"Servers"},
					Params:  []openapi.Param{{Name: "limit", In: "query", Description: "上限|含竖线"}},
					Responses: map[string]openapi.Response{
						"200": {Description: "成功"},
					},
				},
			},
			"/api/v1/servers/invite": {
				Post: &openapi.Operation{
					Summary:     "签发纳管邀请",
					Description: "生成一次性 join_secret",
					Tags:        []string{"Servers"},
					RequestBody: &openapi.RequestBody{Required: true, Content: map[string]openapi.MediaType{"application/json": {}}},
					Responses: map[string]openapi.Response{
						"201": {Description: "已创建"},
						"401": {Description: "未登录"},
					},
				},
			},
			"/api/v1/servers/{id}/containers": {
				Get: &openapi.Operation{
					Summary:   "容器列表",
					Tags:      []string{"Containers"},
					Responses: map[string]openapi.Response{"200": {Description: "成功"}},
				},
			},
		},
		Components: &openapi.Components{
			Schemas: map[string]*openapi.Schema{
				"ServerList": {Type: "object", Description: "主机列表响应"},
				"Untagged":   {Type: "object"},
			},
		},
	}
}

// TestEveryEndpointAppears 是 B4 的机检内核：文档与 spec 同源，
// 任何端点只要进 spec 就必须出现在两份文档里，否则就是漏渲染。
func TestEveryEndpointAppears(t *testing.T) {
	d := sampleDoc()
	llms := LLMSTxt(d)
	apimd := APIMD(d)

	for _, e := range flatten(d) {
		if !strings.Contains(llms, e.method+" "+e.path) {
			t.Errorf("llms.txt 漏了端点 %s %s", e.method, e.path)
		}
		if !strings.Contains(apimd, e.method+" "+e.path) {
			t.Errorf("api.md 漏了端点 %s %s", e.method, e.path)
		}
	}
}

func TestLLMSTxtStructure(t *testing.T) {
	out := LLMSTxt(sampleDoc())

	wants := []string{
		"# DockDeck",
		"## 认证",
		"Authorization: Bearer ddk_",
		"## 权限（scope）",
		"write:containers",
		"## 响应约定",
		"## 端点索引",
		"### Servers",
		"### Containers",
		"## 错误码",
		"`upstream_unavailable`",
		"## 权威来源",
		"/docs/openapi.json",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("llms.txt 缺少 %q", w)
		}
	}

	// 多行描述必须压成一行，否则「> 引用块」会被截断。
	if !strings.Contains(out, "自托管 Docker 管理面板 第二行也应被压成一行") {
		t.Error("描述未压成单行")
	}
	// 竖线要转义，否则表格列会错位。
	if strings.Contains(out, "上限|含竖线") {
		t.Error("表格竖线未转义")
	}
}

func TestAPIMDStructure(t *testing.T) {
	out := APIMD(sampleDoc())

	wants := []string{
		"# DockDeck",
		"## 认证",
		"## 错误码",
		"### `GET /api/v1/servers`",
		"**参数**",
		"| `limit` | query | 否 |",
		"**请求体**（必填）：application/json",
		"| 201 | 已创建 |",
		"## Schema",
		"`ServerList` — 主机列表响应",
		"`Untagged`",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("api.md 缺少 %q", w)
		}
	}
}

// TestNoHardcodedHost 守红线：生成器不得凭空造出任何链接。
//
// 用「输出里不许有 http(s)://」而不是把真实地址写成黑名单：
// sampleDoc 本身不含任何 URL，生成模板也不该硬编码主机——
// 一旦有人往模板里塞地址，这条会直接红。而黑名单会把地址字面量带进源码，
// 那本身就是一次泄露。
func TestNoHardcodedHost(t *testing.T) {
	d := sampleDoc()
	linkRe := regexp.MustCompile(`https?://`)
	for _, out := range []string{LLMSTxt(d), APIMD(d)} {
		if loc := linkRe.FindString(out); loc != "" {
			t.Errorf("文档出现硬编码链接（%s…），生成器不应自带主机", loc)
		}
	}
}
