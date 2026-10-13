// Package route 是 Hub 的 API 端点登记表——注册路由与生成 OpenAPI 用同一份声明，
// 从结构上保证「文档」与「实装」不会走偏。
package route

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// 鉴权模式。
const (
	AuthNone    = "none"    // 公开，无需身份
	AuthSession = "session" // 仅 cookie 会话（Token 不可调用）
	AuthScope   = "scope"   // cookie 会话，或携带所需 scope 的 API Token
)

// Param 声明一个查询参数（路径参数由 Path 里的 {name} 自动得出，不在此列）。
type Param struct {
	Name       string
	Desc       string
	Required   bool
	Example    string
	SchemaType string // string | integer | boolean
	SchemaFmt  string // int64 | date-time | ...
}

// Body 声明请求体内容类型，默认 application/json。
type Body struct {
	Type string // json | octet-stream
}

// Response 声明一个额外响应（401/403 与错误信封由生成器自动补）。
type Response struct {
	Status int
	Desc   string
}

// Entry 是一个 API 端点的完整声明。
type Entry struct {
	Method      string   // GET / POST / PUT / DELETE
	Path        string   // 必须以 /api/ 开头，可含 {param} 通配
	Summary     string   // 一句话说明（进 OpenAPI summary）
	Desc        string   // 补充说明，可省
	Scope       string   // AuthScope 时所需的 API Token scope
	AuthMode    string   // AuthNone | AuthSession | AuthScope
	Tags        []string // OpenAPI 分组
	Params      []Param  // 查询参数
	Body        *Body    // nil = 无请求体
	Responses   []Response
	Status      int  // 成功状态码，默认 200
	NoEnvelope  bool // true = 响应不是 {data,error} 信封
	RateLimited bool // true = 套 IP 令牌桶（登录/建号这类可爆破入口）
	Deprecated  bool
	InternalRef string // 内部实现定位串，不进 spec；供测试断言实装
}

// Table 持有登记表与底层 mux。
type Table struct {
	mux     *http.ServeMux
	wrap    func(Entry, http.Handler) http.Handler
	entries []Entry
}

// New 创建登记表。mux 须为路由尚未注册过的干净实例。
func New(mux *http.ServeMux, wrap func(Entry, http.Handler) http.Handler) *Table {
	if mux == nil {
		panic("route: mux 不能为 nil")
	}
	return &Table{mux: mux, wrap: wrap}
}

// Add 声明一个端点，并同时注册到 mux。声明错误一律 panic——
// 这类问题必须在启动时就炸，而不是等到某次请求返回 404。
func (t *Table) Add(e Entry, h http.HandlerFunc) {
	e.Method = strings.ToUpper(strings.TrimSpace(e.Method))
	e.Path = normalizeAPIPath(e.Path)
	e.AuthMode = strings.TrimSpace(e.AuthMode)

	if !isHTTPMethod(e.Method) {
		panic(fmt.Sprintf("route: 非法方法 %q (%s)", e.Method, e.Path))
	}
	if !strings.HasPrefix(e.Path, "/api/") {
		panic(fmt.Sprintf("route: 路径必须以 /api/ 开头：%s %s", e.Method, e.Path))
	}
	switch e.AuthMode {
	case AuthNone:
	case AuthSession:
	case AuthScope:
		if e.Scope == "" {
			panic(fmt.Sprintf("route: %s %s 为 scope 端点但未声明所需 scope", e.Method, e.Path))
		}
	default:
		panic(fmt.Sprintf("route: %s %s 鉴权模式非法：%q", e.Method, e.Path, e.AuthMode))
	}
	for _, prev := range t.entries {
		if prev.Method == e.Method && prev.Path == e.Path {
			panic(fmt.Sprintf("route: 端点重复声明：%s %s", e.Method, e.Path))
		}
	}
	if h == nil {
		panic(fmt.Sprintf("route: %s %s 未提供 handler", e.Method, e.Path))
	}
	if e.Status == 0 {
		e.Status = http.StatusOK
	}

	t.entries = append(t.entries, e)

	handler := http.Handler(http.HandlerFunc(h))
	if t.wrap != nil {
		handler = t.wrap(e, handler)
	}
	// Go 1.22+ 的 "METHOD /path/{wildcard}" 模式。
	t.mux.Handle(e.Method+" "+e.Path, handler)
}

// Entries 返回按路径、方法排序的声明副本。
func (t *Table) Entries() []Entry {
	out := make([]Entry, len(t.entries))
	copy(out, t.entries)
	for i := range out {
		out[i].Tags = append([]string(nil), out[i].Tags...)
		out[i].Params = append([]Param(nil), out[i].Params...)
		out[i].Responses = append([]Response(nil), out[i].Responses...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// Mux 返回底层 mux，供服务器装配。返回指针而非值：ServeMux 内含互斥锁，不可复制。
func (t *Table) Mux() *http.ServeMux { return t.mux }

// Keys 返回已声明端点的 "METHOD /path" 集合，供一致性测试与 spec 比对。
func (t *Table) Keys() map[string]Entry {
	m := make(map[string]Entry, len(t.entries))
	for _, e := range t.entries {
		m[e.Method+" "+e.Path] = e
	}
	return m
}

func normalizeAPIPath(p string) string {
	p = "/" + strings.Trim(p, "/")
	if p == "/api" {
		return "/api"
	}
	return p
}

func isHTTPMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}
