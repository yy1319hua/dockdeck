// Package openapi 从 route 登记表生成 OpenAPI 3.0.3 文档。
// 生成物与实装同源，所以「文档写了但没实现」「实现了但没进文档」在结构上不会发生，
// 只由一致性测试兜住人为绕过登记表直接 mux.Handle 的情况。
package openapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/route"
)

type Doc struct {
	OpenAPI    string              `json:"openapi"`
	Info       Info                `json:"info"`
	Servers    []Server            `json:"servers"`
	Paths      map[string]PathItem `json:"paths"`
	Components *Components         `json:"components"`
	Tags       []Tag               `json:"tags"`
}

type Info struct {
	Title       string   `json:"title"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	License     *License `json:"license,omitempty"`
}

type License struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// Server 不写死主机：面板只绑 tailnet IP，把具体地址写进 spec 等于分发私密信息。
type Server struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

type Tag struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type PathItem struct {
	Get    *Operation `json:"get,omitempty"`
	Post   *Operation `json:"post,omitempty"`
	Put    *Operation `json:"put,omitempty"`
	Patch  *Operation `json:"patch,omitempty"`
	Delete *Operation `json:"delete,omitempty"`
}

type Operation struct {
	OperationID string                `json:"operationId"`
	Summary     string                `json:"summary"`
	Description string                `json:"description,omitempty"`
	Tags        []string              `json:"tags,omitempty"`
	Params      []Param               `json:"parameters,omitempty"`
	RequestBody *RequestBody          `json:"requestBody,omitempty"`
	Responses   map[string]Response   `json:"responses"`
	Security    []map[string][]string `json:"security,omitempty"`
	Deprecated  bool                  `json:"deprecated,omitempty"`
}

type Param struct {
	Name        string  `json:"name"`
	In          string  `json:"in"`
	Required    bool    `json:"required"`
	Description string  `json:"description,omitempty"`
	Example     string  `json:"example,omitempty"`
	Schema      *Schema `json:"schema"`
}

type RequestBody struct {
	Required bool                 `json:"required"`
	Content  map[string]MediaType `json:"content"`
}

type MediaType struct {
	Schema *Schema `json:"schema"`
}

type Response struct {
	Description string               `json:"description"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

type Schema struct {
	Type        string             `json:"type,omitempty"`
	Format      string             `json:"format,omitempty"`
	Description string             `json:"description,omitempty"`
	Ref         string             `json:"$ref,omitempty"`
	Properties  map[string]*Schema `json:"properties,omitempty"`
	Items       *Schema            `json:"items,omitempty"`
	Enum        []string           `json:"enum,omitempty"`
}

type Components struct {
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes"`
	Schemas         map[string]*Schema        `json:"schemas"`
}

type SecurityScheme struct {
	Type        string `json:"type"`
	In          string `json:"in,omitempty"`
	Name        string `json:"name,omitempty"`
	Scheme      string `json:"scheme,omitempty"`
	Description string `json:"description,omitempty"`
}

// 安全方案与 schema 引用名。
const (
	SchemeCookie = "cookieAuth"
	SchemeBearer = "bearerAuth"
	EnvelopeRef  = "#/components/schemas/Envelope"
	ErrorRef     = "#/components/schemas/ErrorBody"
)

// tagDesc 是分组的说明文字。未登记的 tag 只出现在 UI 分组里，不报错。
var tagDesc = map[string]string{
	"Auth":       "登录、建号、改密——仅浏览器会话可用，API Token 不能调用。",
	"Servers":    "受管主机纳管（UI 专属）。",
	"Metrics":    "主机指标与监控历史，监控主线，App 可直连。",
	"Containers": "容器清单、操作与日志。",
	"Images":     "镜像与清理。",
	"Compose":    "Compose 项目与 YAML。",
	"Files":      "白名单根目录内的文件读写。",
	"Audit":      "审计日志查询与导出。",
	"Tokens":     "API Token 管理（仅浏览器会话可用）。",
	"Meta":       "健康检查、spec 本体、scope 字典。",
}

// Generate 把登记表编译成 spec。entries 须来自同一张登记表。
func Generate(info Info, entries []route.Entry) *Doc {
	d := &Doc{
		OpenAPI: "3.0.3",
		Info:    info,
		Servers: []Server{{URL: "/", Description: "同域：本 spec 由运行中的 Hub 自身提供"}},
		Paths:   map[string]PathItem{},
		Components: &Components{
			SecuritySchemes: map[string]SecurityScheme{
				SchemeCookie: {
					Type: "apiKey", In: "cookie", Name: "dd_session",
					Description: "浏览器会话 cookie，由 POST /api/v1/auth/login 签发。",
				},
				SchemeBearer: {
					Type: "http", Scheme: "bearer",
					Description: "API Token（" + apitoken.Prefix + " 前缀），Authorization: Bearer <token>。" +
						"能做什么由签发时勾选的 scope 决定，scope 字典见 GET /api/v1/scopes。",
				},
			},
			Schemas: schemas(),
		},
	}

	tagSeen := map[string]bool{}
	for _, e := range entries {
		for _, t := range e.Tags {
			tagSeen[t] = true
		}
		pi := d.Paths[e.Path]
		if err := fill(&pi, e.Method, operation(e)); err != nil {
			panic("openapi: " + err.Error()) // route.Add 已校验方法，走到这里说明是本包缺陷
		}
		d.Paths[e.Path] = pi
	}

	for t := range tagSeen {
		d.Tags = append(d.Tags, Tag{Name: t, Description: tagDesc[t]})
	}
	sort.Slice(d.Tags, func(i, j int) bool { return d.Tags[i].Name < d.Tags[j].Name })
	return d
}

func operation(e route.Entry) *Operation {
	op := &Operation{
		OperationID: OperationID(e),
		Summary:     e.Summary,
		Description: e.Desc,
		Tags:        append([]string(nil), e.Tags...),
		Deprecated:  e.Deprecated,
		Responses:   map[string]Response{},
	}

	// 路径参数由 {name} 得出，一律必填。
	for _, name := range pathParams(e.Path) {
		op.Params = append(op.Params, Param{
			Name: name, In: "path", Required: true, Schema: &Schema{Type: "string"},
		})
	}
	for _, p := range e.Params {
		typ := p.SchemaType
		if typ == "" {
			typ = "string"
		}
		// 同名参数若已作为路径参数存在（Entry.Params 里顺手声明了 {id}
		// 的描述/示例/类型），用声明补全那一条，不重复追加——同一
		// name+in 出现两份是非法 OpenAPI，Swagger UI 直接报错。
		// （S2.7 的指标端点就因此把 id 生成过双份。）
		if i := indexOfParam(op.Params, p.Name, "path"); i >= 0 {
			op.Params[i].Description = p.Desc
			op.Params[i].Example = p.Example
			op.Params[i].Schema = &Schema{Type: typ, Format: p.SchemaFmt}
			continue
		}
		op.Params = append(op.Params, Param{
			Name: p.Name, In: "query", Required: p.Required,
			Description: p.Desc, Example: p.Example,
			Schema: &Schema{Type: typ, Format: p.SchemaFmt},
		})
	}

	if e.Body != nil {
		ct, mt := "application/json", MediaType{Schema: &Schema{Type: "object"}}
		if e.Body.Type == "octet-stream" {
			ct, mt = "application/octet-stream", MediaType{Schema: &Schema{Type: "string", Format: "binary"}}
		}
		op.RequestBody = &RequestBody{Required: true, Content: map[string]MediaType{ct: mt}}
	}

	// Status 为 0 时按 200 处理：route.Add 会补默认值，但 Generate 是公开入口，
	// 调用方（含测试）可能直接构造 Entry。
	status := e.Status
	if status == 0 {
		status = http.StatusOK
	}
	okDesc := "成功"
	if status != http.StatusOK {
		okDesc = http.StatusText(status)
	}
	okSchema := &Schema{Ref: EnvelopeRef}
	okCT := "application/json"
	if e.NoEnvelope {
		okSchema = &Schema{Type: "object"}
		// 裸流端点（日志等）声明真实内容类型：文档对实装说谎比没有文档更糟。
		// 动态内容类型（export 时的 octet-stream）在 Desc 里说明，spec 取主形态。
		if e.ContentType != "" {
			okCT = e.ContentType
			if strings.HasPrefix(e.ContentType, "text/") {
				okSchema = &Schema{Type: "string"}
			}
		}
	}
	op.Responses[strconv.Itoa(status)] = Response{Description: okDesc, Content: map[string]MediaType{
		okCT: {Schema: okSchema},
	}}
	for _, c := range [4]int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		op.Responses[strconv.Itoa(c)] = Response{Description: statusDesc(c, e), Content: map[string]MediaType{
			"application/json": {Schema: &Schema{Ref: ErrorRef}},
		}}
	}
	for _, extra := range e.Responses {
		// 错误状态码的载荷是 {error:{code,message}}，不是信封——声明错了
		// 比不声明更糟：客户端代码生成器会照着造解析结构。
		// （此前不分状态一律 EnvelopeRef，S3.4 起区分。）
		schema := &Schema{Ref: EnvelopeRef}
		if extra.Status >= 400 {
			schema = &Schema{Ref: ErrorRef}
		}
		op.Responses[strconv.Itoa(extra.Status)] = Response{Description: extra.Desc, Content: map[string]MediaType{
			"application/json": {Schema: schema},
		}}
	}

	switch e.AuthMode {
	case route.AuthNone:
		// 显式空数组覆盖全局要求，声明「这里不需要身份」。
		op.Security = []map[string][]string{{}}
	case route.AuthSession:
		op.Security = []map[string][]string{{SchemeCookie: {}}}
	default:
		// 会话或 Token 任一即可（两条都是合法的 security 要求，OR 语义）。
		// OpenAPI 3.0 不能在 securityScheme 上表达 scope 依赖，因此 scope 写进描述。
		op.Security = []map[string][]string{{SchemeCookie: {}}, {SchemeBearer: {}}}
		if e.Scope != "" {
			op.Description += "\n\nAPI Token 须具备 scope `" + e.Scope + "`；浏览器会话不受此限制。"
		}
	}
	return op
}

func statusDesc(code int, e route.Entry) string {
	switch code {
	case http.StatusBadRequest:
		return "请求体或参数不合法"
	case http.StatusUnauthorized:
		if e.AuthMode == route.AuthNone {
			return "公开端点，不使用"
		}
		return "未登录，或凭据无效/已过期"
	case http.StatusForbidden:
		if e.AuthMode == route.AuthScope && e.Scope != "" {
			return "API Token 缺少所需 scope：" + e.Scope
		}
		return "身份成立但无权执行"
	case http.StatusNotFound:
		return "目标不存在"
	}
	return http.StatusText(code)
}

func pathParams(p string) []string {
	var out []string
	for {
		i := strings.IndexByte(p, '{')
		if i < 0 {
			return out
		}
		p = p[i+1:]
		j := strings.IndexByte(p, '}')
		if j < 0 {
			return out
		}
		// Go mux 尾通配符 {ref...} 的文档名剥掉 "..."（规范里就是 {ref}）。
		out = append(out, strings.TrimSuffix(p[:j], "..."))
		p = p[j+1:]
	}
}

// indexOfParam 找 name+in 的参数下标；找不到返回 -1。
func indexOfParam(ps []Param, name, in string) int {
	for i, p := range ps {
		if p.Name == name && p.In == in {
			return i
		}
	}
	return -1
}

// OperationID 由方法+路径稳定推导：GET /api/v1/servers/{id}/metrics → getApiV1ServersIdMetrics。
// 去掉 { } - _ 等非法标识符字符，保证客户端代码生成器可直接用作方法名。
func OperationID(e route.Entry) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(e.Method))
	for _, seg := range strings.Split(e.Path, "/") {
		seg = strings.NewReplacer("{", "", "}", "", "-", "", "_", "", ".", "").Replace(seg)
		if seg == "" {
			continue
		}
		b.WriteString(strings.ToUpper(seg[:1]) + seg[1:])
	}
	return b.String()
}

func fill(pi *PathItem, method string, op *Operation) error {
	switch method {
	case http.MethodGet:
		pi.Get = op
	case http.MethodPost:
		pi.Post = op
	case http.MethodPut:
		pi.Put = op
	case http.MethodPatch:
		pi.Patch = op
	case http.MethodDelete:
		pi.Delete = op
	default:
		return &methodError{method: method}
	}
	return nil
}

type methodError struct{ method string }

func (e *methodError) Error() string { return "未支持的方法 " + e.method }

func schemas() map[string]*Schema {
	return map[string]*Schema{
		"Envelope": {
			Type:        "object",
			Description: "统一响应信封：成功只有 data，失败只有 error。客户端先看 HTTP 码再看 code。",
			Properties: map[string]*Schema{
				"data":  {Description: "端点各自载荷，可为对象或数组"},
				"error": {Ref: ErrorRef},
			},
		},
		"ErrorBody": {
			Type: "object",
			Description: "错误载荷。code 是稳定的机器可读标识；message 是给人看的中文，可能随版本改动，" +
				"客户端应判 code 而非文案。",
			Properties: map[string]*Schema{
				"code":    {Type: "string", Description: "如 invalid_credentials / scope_denied / not_found"},
				"message": {Type: "string"},
			},
		},
		"Scope": {
			Type:        "string",
			Description: "API Token 权限项。",
			Enum:        append([]string(nil), apitoken.AllScopes...),
		},
	}
}

// JSON 输出缩进 spec。map 键序由 encoding/json 保证（按字典序），因此字节稳定。
func (d *Doc) JSON() ([]byte, error) { return json.MarshalIndent(d, "", "  ") }

// MethodPaths 返回 spec 覆盖到的 "METHOD /path" 集合，供一致性测试与登记表比对。
func (d *Doc) MethodPaths() map[string]bool {
	out := map[string]bool{}
	for p, pi := range d.Paths {
		for m, op := range map[string]*Operation{
			"GET": pi.Get, "POST": pi.Post, "PUT": pi.Put, "PATCH": pi.Patch, "DELETE": pi.Delete,
		} {
			if op != nil {
				out[m+" "+p] = true
			}
		}
	}
	return out
}
