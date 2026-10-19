// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package apidocs 从 OpenAPI spec 动态生成「AI 可读」与「人可读」两种文档形态：
//
//	GET /docs/llms.txt  —— 面向 LLM 的紧凑摘要（认证 / 端点索引 / 错误码）
//	GET /docs/api.md    —— 面向人的完整 Markdown（按标签分组的端点参考）
//
// 为什么生成而不是手写静态文件：
//   - 手写文档必然随路由漂移，而 spec 是由 route.Table 实时生成的唯一真相；
//   - 生成物与 /docs/openapi.json 同源，一致性检查不需要跨文件比对；
//   - 端点增减时文档自动跟上，没有「改了代码忘了改文档」的窗口。
//
// 唯一的静态内容只有错误码表：错误码是协议级稳定契约（见 internal/protocol），
// spec 的 responses 里只有 HTTP 状态码、没有业务 code 字符串，无法从中推导。
package apidocs

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/openapi"
)

// method 是一次「方法 + 取 Operation」的绑定，用于稳定顺序遍历。
type method struct {
	name string
	get  func(openapi.PathItem) *openapi.Operation
}

// methods 的顺序即文档里的展示顺序：读在前、写在后，与使用频率一致。
var methods = []method{
	{"GET", func(p openapi.PathItem) *openapi.Operation { return p.Get }},
	{"POST", func(p openapi.PathItem) *openapi.Operation { return p.Post }},
	{"PUT", func(p openapi.PathItem) *openapi.Operation { return p.Put }},
	{"PATCH", func(p openapi.PathItem) *openapi.Operation { return p.Patch }},
	{"DELETE", func(p openapi.PathItem) *openapi.Operation { return p.Delete }},
}

// endpoint 是压平后的一个端点，避免在渲染时反复查表。
type endpoint struct {
	method string
	path   string
	op     *openapi.Operation
}

// flatten 把 spec 的 paths 压成有序端点列表。
func flatten(d *openapi.Doc) []endpoint {
	paths := make([]string, 0, len(d.Paths))
	for p := range d.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make([]endpoint, 0, len(paths)*2)
	for _, p := range paths {
		item := d.Paths[p]
		for _, m := range methods {
			if op := m.get(item); op != nil {
				out = append(out, endpoint{method: m.name, path: p, op: op})
			}
		}
	}
	return out
}

// primaryTag 取端点的主标签，用于分组；无标签归入 Other。
func primaryTag(op *openapi.Operation) string {
	if len(op.Tags) > 0 && op.Tags[0] != "" {
		return op.Tags[0]
	}
	return "Other"
}

// groupByTag 保留 tag 在 spec 中声明的顺序（Tags 数组），未声明的按字典序追加。
func groupByTag(d *openapi.Doc, eps []endpoint) ([]string, map[string][]endpoint) {
	groups := make(map[string][]endpoint, 8)
	for _, e := range eps {
		t := primaryTag(e.op)
		groups[t] = append(groups[t], e)
	}

	order := make([]string, 0, len(groups))
	seen := make(map[string]bool, len(groups))
	for _, t := range d.Tags {
		if _, ok := groups[t.Name]; ok && !seen[t.Name] {
			order = append(order, t.Name)
			seen[t.Name] = true
		}
	}
	rest := make([]string, 0, len(groups))
	for t := range groups {
		if !seen[t] {
			rest = append(rest, t)
		}
	}
	sort.Strings(rest)
	return append(order, rest...), groups
}

// tagDescription 取标签说明；spec 没写就回空串。
func tagDescription(d *openapi.Doc, name string) string {
	for _, t := range d.Tags {
		if t.Name == name {
			return t.Description
		}
	}
	return ""
}

// sortedCodes 按数值升序返回响应码（"default" 排最后）。
func sortedCodes(resp map[string]openapi.Response) []string {
	codes := make([]string, 0, len(resp))
	for c := range resp {
		codes = append(codes, c)
	}
	sort.Slice(codes, func(i, j int) bool {
		ni, ei := strconv.Atoi(codes[i])
		nj, ej := strconv.Atoi(codes[j])
		if ei != nil || ej != nil {
			return codes[i] < codes[j]
		}
		return ni < nj
	})
	return codes
}

// cell 让文本能安全放进 Markdown 表格：竖线转义、换行压成空格。
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// plain 让摘要适合放在单行列表里（去换行、去多余空白）。
func plain(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// firstPara 取描述的第一段并压成一行。
// llms.txt 面向 LLM，整篇多段描述挤成一行既难读又浪费上下文；
// 首段已经说清「这是什么」，细节留给 api.md 与 spec。
func firstPara(s string) string {
	para, _, _ := strings.Cut(strings.TrimSpace(s), "\n\n")
	return plain(para)
}

// paragraphs 按空行切段，保留原有段落结构（api.md 用）。
func paragraphs(s string) []string {
	parts := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// LLMSTxt 生成面向 LLM 的紧凑文档。
//
// 刻意不含逐字段的 schema 细节：LLM 需要的是「怎么认证、有哪些端点、出错怎么办」，
// 结构化 schema 让它们去读 /docs/openapi.json——那才是机器可解析的权威形态。
func LLMSTxt(d *openapi.Doc) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s\n\n", d.Info.Title)
	fmt.Fprintf(&b, "> %s\n\n", firstPara(d.Info.Description))
	fmt.Fprintf(&b, "Version: %s · OpenAPI: %s\n\n", d.Info.Version, d.OpenAPI)

	b.WriteString("## 认证\n\n")
	b.WriteString("```http\nAuthorization: Bearer ddk_xxxxxxxx\n```\n\n")
	b.WriteString("- API Token 由管理员在面板「设置 → API Token」签发，**明文仅一次可见**，库里只存哈希。\n")
	b.WriteString("- 请求头不要带 Cookie；Cookie 会话只服务浏览器 UI。\n")
	b.WriteString("- 反向同理：API Token **不能**调 UI 专属端点（签发邀请、Token 管理、改密、备份）。\n")
	b.WriteString("- 服务端拒绝 `0.0.0.0` 绑定，只监听具体 IP；公网入口由 Cloudflare Tunnel 提供。\n\n")

	b.WriteString("## 权限（scope）\n\n")
	b.WriteString("| scope | 允许 |\n| --- | --- |\n")
	b.WriteString("| `read:metrics` | 读主机指标 |\n")
	b.WriteString("| `read:containers` | 读容器 / 镜像 / Compose / 卷 / 网络 |\n")
	b.WriteString("| `write:containers` | 启停删容器、拉镜像、操作 Compose |\n")
	b.WriteString("| `read:files` | 列目录、读文件 |\n")
	b.WriteString("| `write:files` | 上传、改写、删除文件 |\n")
	b.WriteString("| `read:audit` | 读审计日志 |\n\n")
	b.WriteString("**写不蕴含读**：既要读又要写就同时授两个 scope。缺 scope 时返回 403，`message` 里写明缺哪个。\n\n")

	b.WriteString("## 响应约定\n\n")
	b.WriteString("- 成功：`{\"data\": ...}`；失败：`{\"error\": {\"code\": \"...\", \"message\": \"...\"}}`。\n")
	b.WriteString("- 例外（裸流，无信封）：容器 / Compose 日志、文件下载、整库备份、`/api/openapi.json`。\n")
	b.WriteString("- 分页端点用 `limit` + 游标（见各自参数说明），游标为 0 表示没有更多。\n\n")

	tags, groups := groupByTag(d, flatten(d))
	b.WriteString("## 端点索引\n\n")
	for _, tag := range tags {
		fmt.Fprintf(&b, "### %s\n\n", tag)
		if desc := tagDescription(d, tag); desc != "" {
			fmt.Fprintf(&b, "%s\n\n", plain(desc))
		}
		for _, e := range groups[tag] {
			summary := plain(e.op.Summary)
			if summary == "" {
				summary = "(无摘要)"
			}
			fmt.Fprintf(&b, "- `%s %s` — %s\n", e.method, e.path, summary)
		}
		b.WriteString("\n")
	}

	b.WriteString("## 错误码\n\n")
	b.WriteString("| code | HTTP | 含义 |\n| --- | --- | --- |\n")
	for _, row := range errorTable {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", row[0], row[1], row[2])
	}
	b.WriteString("\n")

	b.WriteString("## 权威来源\n\n")
	b.WriteString("- 机器可解析：`/docs/openapi.json`（与 `/api/openapi.json` 同一份）\n")
	b.WriteString("- 人可读完整版：`/docs/api.md`\n")
	b.WriteString("- 可用 scope 实时清单：`/api/v1/scopes`\n")

	return b.String()
}

// APIMD 生成面向人的完整 Markdown 参考。
func APIMD(d *openapi.Doc) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s\n\n", d.Info.Title)
	for _, para := range paragraphs(d.Info.Description) {
		fmt.Fprintf(&b, "%s\n\n", para)
	}
	fmt.Fprintf(&b, "**版本** %s · **OpenAPI** %s\n\n", d.Info.Version, d.OpenAPI)

	b.WriteString("## 认证\n\n")
	b.WriteString("所有 `/api/v1/**` 端点（除标注公开的）都要求：\n\n")
	b.WriteString("```http\nAuthorization: Bearer ddk_xxxxxxxx\n```\n\n")
	b.WriteString("Token 在面板「设置 → API Token」签发，明文仅一次可见。可用 scope 见 `/api/v1/scopes`。\n\n")
	b.WriteString("失败时的统一结构：\n\n")
	b.WriteString("```json\n{\"error\": {\"code\": \"scope_denied\", \"message\": \"缺少 scope: write:containers\"}}\n```\n\n")

	b.WriteString("## 错误码\n\n")
	b.WriteString("| code | HTTP | 含义 |\n| --- | --- | --- |\n")
	for _, row := range errorTable {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", row[0], row[1], row[2])
	}
	b.WriteString("\n")

	tags, groups := groupByTag(d, flatten(d))
	for _, tag := range tags {
		fmt.Fprintf(&b, "## %s\n\n", tag)
		if desc := tagDescription(d, tag); desc != "" {
			fmt.Fprintf(&b, "%s\n\n", plain(desc))
		}
		for _, e := range groups[tag] {
			renderEndpoint(&b, e)
		}
	}

	b.WriteString("## Schema\n\n")
	b.WriteString("完整字段定义见 `/docs/openapi.json` 的 `components.schemas`。核心响应体：\n\n")
	names := make([]string, 0, len(d.Components.Schemas))
	for n := range d.Components.Schemas {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := d.Components.Schemas[n]
		desc := ""
		if s != nil {
			desc = plain(s.Description)
		}
		if desc == "" {
			fmt.Fprintf(&b, "- `%s`\n", n)
			continue
		}
		fmt.Fprintf(&b, "- `%s` — %s\n", n, desc)
	}
	b.WriteString("\n")

	return b.String()
}

func renderEndpoint(b *strings.Builder, e endpoint) {
	fmt.Fprintf(b, "### `%s %s`\n\n", e.method, e.path)
	if e.op.Summary != "" {
		fmt.Fprintf(b, "**%s**\n\n", plain(e.op.Summary))
	}
	if e.op.Description != "" {
		fmt.Fprintf(b, "%s\n\n", plain(e.op.Description))
	}
	if e.op.Deprecated {
		b.WriteString("> ⚠️ 已废弃，后续版本可能移除。\n\n")
	}

	if len(e.op.Params) > 0 {
		b.WriteString("**参数**\n\n")
		b.WriteString("| 名称 | 位置 | 必填 | 类型 | 说明 |\n| --- | --- | --- | --- | --- |\n")
		for _, p := range e.op.Params {
			typ := ""
			if p.Schema != nil {
				typ = p.Schema.Type
			}
			req := "否"
			if p.Required {
				req = "是"
			}
			fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s |\n", p.Name, p.In, req, cell(typ), cell(p.Description))
		}
		b.WriteString("\n")
	}

	if e.op.RequestBody != nil {
		ct := make([]string, 0, len(e.op.RequestBody.Content))
		for k := range e.op.RequestBody.Content {
			ct = append(ct, k)
		}
		sort.Strings(ct)
		req := "可选"
		if e.op.RequestBody.Required {
			req = "必填"
		}
		fmt.Fprintf(b, "**请求体**（%s）：%s\n\n", req, strings.Join(ct, ", "))
	}

	if len(e.op.Responses) > 0 {
		b.WriteString("**响应**\n\n")
		b.WriteString("| 状态 | 说明 |\n| --- | --- |\n")
		for _, code := range sortedCodes(e.op.Responses) {
			fmt.Fprintf(b, "| %s | %s |\n", code, cell(e.op.Responses[code].Description))
		}
		b.WriteString("\n")
	}
}

// errorTable 是协议级错误码契约（见 internal/protocol/errors.go）。
// spec 只携带 HTTP 状态码，推导不出业务 code，故静态维护。
var errorTable = [][3]string{
	{"unauthenticated", "401", "未提供或无效的凭据（Bearer 失效 / 会话过期）"},
	{"scope_denied", "403", "Token 缺少所需 scope，message 里写明缺哪个"},
	{"not_found", "404", "资源不存在（主机 / 容器 / 文件 / 端点）"},
	{"bad_request", "400", "请求体或参数不合法"},
	{"too_large", "413", "请求体超过上限（默认 1 MiB，文件上传 16 MiB）"},
	{"rate_limited", "429", "触发限流，按 Retry-After 退避"},
	{"upstream_unavailable", "502", "Agent 不可达（离线 / 反向连接不在线）"},
	{"upstream_timeout", "504", "Agent 响应超时"},
	{"internal", "500", "服务端内部错误，可凭 request_id 追查审计日志"},
}
