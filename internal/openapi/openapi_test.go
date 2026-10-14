package openapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/route"
)

func sampleEntries() []route.Entry {
	return []route.Entry{
		{Method: "GET", Path: "/api/v1/health", AuthMode: route.AuthNone, Summary: "健康检查", Tags: []string{"Meta"}},
		{Method: "POST", Path: "/api/v1/auth/login", AuthMode: route.AuthNone, Summary: "登录",
			Tags: []string{"Auth"}, Body: &route.Body{}},
		{Method: "POST", Path: "/api/v1/auth/logout", AuthMode: route.AuthSession, Summary: "登出", Tags: []string{"Auth"}},
		{Method: "GET", Path: "/api/v1/servers/{id}/metrics", AuthMode: route.AuthScope,
			Scope: apitoken.ScopeReadMetrics, Summary: "主机指标", Tags: []string{"Metrics"},
			Params: []route.Param{
				{Name: "since", Desc: "起始时间", SchemaType: "string", SchemaFmt: "date-time"},
				{Name: "step", Desc: "采样步长秒", Required: true, SchemaType: "integer", SchemaFmt: "int64", Example: "60"},
			}},
		{Method: "POST", Path: "/api/v1/servers/{id}/containers/{cid}/start", AuthMode: route.AuthScope,
			Scope: apitoken.ScopeWriteContainers, Summary: "启动容器", Tags: []string{"Containers"}, Status: http.StatusAccepted},
		{Method: "GET", Path: "/api/openapi.json", AuthMode: route.AuthNone, NoEnvelope: true,
			Summary: "spec 本体", Tags: []string{"Meta"}},
	}
}

func info() Info {
	return Info{Title: "DockDeck Hub API", Version: "0.1.0", Description: "test"}
}

func TestGenerateStructure(t *testing.T) {
	d := Generate(info(), sampleEntries())

	if d.OpenAPI != "3.0.3" {
		t.Fatalf("版本应 3.0.3，得 %s", d.OpenAPI)
	}
	if len(d.Paths) != 6 {
		t.Fatalf("应 6 个路径，得 %d：%v", len(d.Paths), keys(d.Paths))
	}
	// servers 不得含具体主机，避免把 tailnet 地址带进公开 spec。
	if len(d.Servers) != 1 || d.Servers[0].URL != "/" {
		t.Fatalf("servers 应为同域根，得 %+v", d.Servers)
	}

	// scope 端点：两种身份并列 + 描述里点名 scope。
	op := d.Paths["/api/v1/servers/{id}/metrics"].Get
	if op == nil {
		t.Fatal("metrics GET 缺失")
	}
	if len(op.Security) != 2 {
		t.Fatalf("scope 端点应同时允许会话与 Token，得 %d 条", len(op.Security))
	}
	if _, ok := op.Security[0][SchemeCookie]; !ok {
		t.Fatalf("第一条 security 应是 cookieAuth：%v", op.Security)
	}
	if _, ok := op.Security[1][SchemeBearer]; !ok {
		t.Fatalf("第二条 security 应是 bearerAuth：%v", op.Security)
	}
	if !strings.Contains(op.Description, apitoken.ScopeReadMetrics) {
		t.Fatalf("描述应写明所需 scope，得 %q", op.Description)
	}

	// 会话端点只允许 cookie。
	sess := d.Paths["/api/v1/auth/logout"].Post
	if len(sess.Security) != 1 {
		t.Fatalf("会话端点应只允许一种身份：%v", sess.Security)
	}
	if _, ok := sess.Security[0][SchemeCookie]; !ok {
		t.Fatalf("会话端点必须是 cookieAuth：%v", sess.Security)
	}
	if _, leaked := sess.Security[0][SchemeBearer]; leaked {
		t.Fatal("会话端点不得允许 bearerAuth，否则 Token 能调用登录/改密")
	}

	// 公开端点显式空数组，不能被继承成「需要身份」。
	health := d.Paths["/api/v1/health"].Get
	if len(health.Security) != 1 || len(health.Security[0]) != 0 {
		t.Fatalf("公开端点 security 应为 [{}]，得 %v", health.Security)
	}

	// 路径参数与查询参数都进 parameters。
	if got := len(op.Params); got != 3 { // id + since + step
		t.Fatalf("参数应 3 个，得 %d：%v", got, paramNames(op.Params))
	}
	var step *Param
	for i := range op.Params {
		if op.Params[i].Name == "step" {
			step = &op.Params[i]
		}
	}
	if step == nil || step.In != "query" || !step.Required || step.Schema.Type != "integer" || step.Schema.Format != "int64" {
		t.Fatalf("step 参数不对：%+v", step)
	}
	for _, p := range op.Params {
		if p.In == "path" && !p.Required {
			t.Fatalf("路径参数 %s 必须 required", p.Name)
		}
	}

	// 非 200 的成功码要如实写进 responses。
	start := d.Paths["/api/v1/servers/{id}/containers/{cid}/start"].Post
	if _, ok := start.Responses["202"]; !ok {
		t.Fatalf("202 应出现在 responses：%v", respKeys(start))
	}
	if _, ok := start.Responses["403"]; !ok {
		t.Fatal("scope 端点应声明 403")
	}
	if !strings.Contains(start.Responses["403"].Description, apitoken.ScopeWriteContainers) {
		t.Fatalf("403 说明应含 scope：%q", start.Responses["403"].Description)
	}

	// NoEnvelope 的端点不该引用信封。
	spec := d.Paths["/api/openapi.json"].Get
	sch := spec.Responses["200"].Content["application/json"].Schema
	if sch.Ref != "" {
		t.Fatalf("NoEnvelope 端点不应 $ref 信封，得 %q", sch.Ref)
	}

	// 组件齐备。
	cs := d.Components
	if cs.SecuritySchemes[SchemeCookie].Name != "dd_session" {
		t.Fatalf("cookie 方案名不对：%+v", cs.SecuritySchemes[SchemeCookie])
	}
	if cs.SecuritySchemes[SchemeBearer].Scheme != "bearer" {
		t.Fatal("bearer 方案不对")
	}
	if got := len(cs.Schemas["Scope"].Enum); got != len(apitoken.AllScopes) {
		t.Fatalf("Scope enum 应覆盖全部 scope，得 %d/%d", got, len(apitoken.AllScopes))
	}

	// tags 去重且有序。
	if len(d.Tags) == 0 {
		t.Fatal("tags 为空")
	}
	for i := 1; i < len(d.Tags); i++ {
		if d.Tags[i-1].Name >= d.Tags[i].Name {
			t.Fatalf("tags 未排序：%v", d.Tags)
		}
	}

	// operationId 唯一且不含非法字符。
	seen := map[string]bool{}
	for p, pi := range d.Paths {
		for m, op := range ops(pi) {
			if op == nil {
				continue
			}
			if seen[op.OperationID] {
				t.Fatalf("operationId 重复：%s %s %s", m, p, op.OperationID)
			}
			seen[op.OperationID] = true
			for _, bad := range []string{"{", "}", "/", "-", "_"} {
				if strings.Contains(op.OperationID, bad) {
					t.Fatalf("operationId %q 含非法字符 %q", op.OperationID, bad)
				}
			}
		}
	}
}

func TestOperationID(t *testing.T) {
	cases := []struct{ method, path, want string }{
		{"GET", "/api/v1/servers/{id}/metrics", "getApiV1ServersIdMetrics"},
		{"GET", "/api/v1/auth/me", "getApiV1AuthMe"},
		{"POST", "/api/v1/api-tokens", "postApiV1Apitokens"},
		{"DELETE", "/api/v1/api-tokens/{id}", "deleteApiV1ApitokensId"},
		{"GET", "/api/v1/servers/{id}/containers/{cid}/logs", "getApiV1ServersIdContainersCidLogs"},
		// 尾通配符 {ref...}（Go mux 容纳含 "/" 的镜像引用）：文档/标识符
		// 层剥掉 "..." 与 "."，与单段参数同规格呈现。
		{"DELETE", "/api/v1/servers/{id}/images/{ref...}", "deleteApiV1ServersIdImagesRef"},
	}
	for _, c := range cases {
		e := route.Entry{Method: c.method, Path: c.path}
		if got := OperationID(e); got != c.want {
			t.Errorf("OperationID(%s %s) = %q 期望 %q", c.method, c.path, got, c.want)
		}
	}
}

func TestMethodPathsMatchesEntries(t *testing.T) {
	entries := sampleEntries()
	d := Generate(info(), entries)
	mp := d.MethodPaths()
	if len(mp) != len(entries) {
		t.Fatalf("spec 覆盖 %d 条，登记表 %d 条", len(mp), len(entries))
	}
	for _, e := range entries {
		if !mp[e.Method+" "+e.Path] {
			t.Fatalf("spec 缺 %s %s", e.Method, e.Path)
		}
	}
}

func TestJSONIsValidAndStable(t *testing.T) {
	d := Generate(info(), sampleEntries())
	b, err := d.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	// 必须是合法 JSON。
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("spec 不是合法 JSON: %v", err)
	}
	// 同输入两次生成的字节要一致：spec 会被写进仓库比对，抖动就是噪音。
	b2, _ := Generate(info(), sampleEntries()).JSON()
	if string(b) != string(b2) {
		t.Fatal("两次生成的 spec 字节不一致")
	}
	// 关键字段存在。
	for _, k := range []string{"openapi", "info", "paths", "components", "tags"} {
		if _, ok := back[k]; !ok {
			t.Fatalf("spec 缺顶层字段 %q", k)
		}
	}
}

func TestFillRejectsUnknownMethod(t *testing.T) {
	var pi PathItem
	if err := fill(&pi, "TRACE", &Operation{}); err == nil {
		t.Fatal("未知方法应报错")
	}
}

func TestScopeDictCoversAllScopes(t *testing.T) {
	for _, s := range apitoken.AllScopes {
		if apitoken.ScopeDesc[s] == "" {
			t.Fatalf("scope %q 缺中文说明", s)
		}
	}
}

func keys(m map[string]PathItem) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func paramNames(ps []Param) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func respKeys(op *Operation) []string {
	var out []string
	for k := range op.Responses {
		out = append(out, k)
	}
	return out
}

func ops(pi PathItem) map[string]*Operation {
	return map[string]*Operation{
		"GET": pi.Get, "POST": pi.Post, "PUT": pi.Put, "PATCH": pi.Patch, "DELETE": pi.Delete,
	}
}

// TestNoDuplicateParameters 守住 S2.7 踩过的坑：Entry.Params 里声明与路径
// 同名的参数（id/cid/action——为补描述/示例顺手写的），生成器曾把它当作
// query 参数追加，与路径参数构成同一 name+in 两份——非法 OpenAPI，
// Swagger UI 直接报错。现在同名声明应用来补全路径参数，绝不重复追加。
func TestNoDuplicateParameters(t *testing.T) {
	es := []route.Entry{
		{Method: "GET", Path: "/api/v1/servers/{id}/containers/{cid}", AuthMode: route.AuthScope,
			Scope: apitoken.ScopeReadContainers, Summary: "容器详情", Tags: []string{"Containers"},
			Params: []route.Param{
				{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
				{Name: "cid", Desc: "容器 ID 或名字", Required: true, Example: "9f3a2b1c4d5e"},
				{Name: "all", Desc: "含已停止", SchemaType: "boolean"},
			}},
		{Method: "POST", Path: "/api/v1/servers/{id}/containers/{cid}/{action}", AuthMode: route.AuthScope,
			Scope: apitoken.ScopeWriteContainers, Summary: "容器操作", Tags: []string{"Containers"},
			Params: []route.Param{
				{Name: "id", Desc: "服务器 ID", Required: true, Example: "1", SchemaType: "integer", SchemaFmt: "int64"},
				{Name: "cid", Desc: "容器 ID 或名字", Required: true, Example: "9f3a2b1c4d5e"},
				{Name: "action", Desc: "动作", Required: true, Example: "restart"},
			}},
	}
	d := Generate(info(), es)
	for _, e := range es {
		op := ops(d.Paths[e.Path])[e.Method]
		if op == nil {
			t.Fatalf("%s %s：operation 缺失", e.Method, e.Path)
		}
		seen := map[string]int{}
		for _, p := range op.Params {
			seen[p.In+":"+p.Name]++
		}
		for k, n := range seen {
			if n > 1 {
				t.Errorf("%s %s：参数 %s 出现 %d 份（同一 name+in 重复是非法 OpenAPI）", e.Method, e.Path, k, n)
			}
		}
		// 路径占位符只允许 path 形态；Entry.Params 同名声明是补全，
		// 不得另生出 query 参数。
		for _, p := range op.Params {
			if p.In == "query" && (p.Name == "id" || p.Name == "cid" || p.Name == "action") {
				t.Errorf("%s %s：%s 是路径占位符，不得以 query 形态出现", e.Method, e.Path, p.Name)
			}
		}
		// 参数总数：路径参数各一份 + 真查询参数，一个不多一个不少。
		want := 3 // id + cid（或 action）路径 + all/action 各一；两条都是 3
		if got := len(op.Params); got != want {
			t.Errorf("%s %s：参数应 %d 个，得 %d：%v", e.Method, e.Path, want, got, paramNames(op.Params))
		}
		// 同名声明应用来补全：id 的 integer/int64 与描述必须落到路径参数上。
		var id *Param
		for i := range op.Params {
			if op.Params[i].Name == "id" {
				id = &op.Params[i]
			}
		}
		if id == nil || id.In != "path" || !id.Required {
			t.Fatalf("%s %s：id 路径参数不对：%+v", e.Method, e.Path, id)
		}
		if id.Schema == nil || id.Schema.Type != "integer" || id.Schema.Format != "int64" {
			t.Errorf("%s %s：id 未被声明补全为 integer/int64：%+v", e.Method, e.Path, id.Schema)
		}
		if id.Description != "服务器 ID" || id.Example != "1" {
			t.Errorf("%s %s：id 的描述/示例未从声明补全：%+v", e.Method, e.Path, id)
		}
	}
}

// TestPathParamsTailWildcard 覆盖 Go mux 尾通配符的文档名规范化：
// {ref...}（容纳含 "/" 的镜像引用，如 library/nginx:latest）在 OpenAPI
// 参数与路径模板里都呈现为 {ref}——"." 与 "..." 不是文档层词汇。
func TestPathParamsTailWildcard(t *testing.T) {
	got := pathParams("/api/v1/servers/{id}/images/{ref...}")
	want := []string{"id", "ref"}
	if len(got) != len(want) {
		t.Fatalf("pathParams = %v 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("参数 %d = %q 期望 %q", i, got[i], want[i])
		}
	}
}
