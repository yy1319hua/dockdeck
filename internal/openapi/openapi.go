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

// Server 不写死服务器：面板只绑 tailnet / 内网的具体 IP，把具体地址写进 spec 等于分发私密信息。
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
	Type                 string             `json:"type,omitempty"`
	Format               string             `json:"format,omitempty"`
	Description          string             `json:"description,omitempty"`
	Ref                  string             `json:"$ref,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	AdditionalProperties *Schema            `json:"additionalProperties,omitempty"`
	Enum                 []string           `json:"enum,omitempty"`
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
	"Servers":    "受管服务器纳管（UI 专属）。",
	"Metrics":    "服务器指标与监控历史，监控主线，App 可直连。",
	"Containers": "容器清单、操作与日志。",
	"Images":     "镜像与清理。",
	"Compose":    "Compose 项目与 YAML。",
	"Files":      "白名单根目录内的文件读写。",
	"Audit":      "审计日志查询与导出。",
	"Settings":   "面板安全设置：保护名单、TLS 指纹、整库快照——仅浏览器会话可用。",
	"Tokens":     "API Token 管理（仅浏览器会话可用）。",
	"Volumes":    "数据卷只读清单（Engine 只给使用计数，不给接入名单）。",
	"Networks":   "容器网络只读清单（含接入容器名单）。",
	"Meta":       "健康检查、spec 本体、scope 字典。",
	"System":     "Hub 构建元数据（版本 / commit / 构建时间），仅浏览器会话可用。",
}

// Generate 把登记表编译成 spec。entries 须来自同一张登记表。
func Generate(info Info, entries []route.Entry) *Doc {
	d := &Doc{
		OpenAPI: "3.0.3",
		Info:    info,
		Servers: []Server{
			{URL: "https://hub.example.com", Description: "生产入口（Cloudflare Tunnel，公网）"},
			{URL: "/", Description: "同域：本 spec 由运行中的 Hub 自身提供（tailnet 直连时用）"},
		},
		Paths: map[string]PathItem{},
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
		docPath := SpecPath(e.Path)
		pi := d.Paths[docPath]
		if err := fill(&pi, e.Method, operation(e)); err != nil {
			panic("openapi: " + err.Error()) // route.Add 已校验方法，走到这里说明是本包缺陷
		}
		d.Paths[docPath] = pi
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
			op.Params[i].Schema = &Schema{Type: typ, Format: p.SchemaFmt, Enum: p.Enum}
			continue
		}
		op.Params = append(op.Params, Param{
			Name: p.Name, In: "query", Required: p.Required,
			Description: p.Desc, Example: p.Example,
			Schema: &Schema{Type: typ, Format: p.SchemaFmt, Enum: p.Enum},
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
	// 端点声明了具体响应 schema 时优先用它：Envelope.data 是无类型 object，
	// 生成 SDK 时只能得到 Any，等于把「怎么做类型」推给调用方。
	if e.OKSchema != "" {
		okSchema = schemaRef(e.OKSchema)
	}
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
	case route.AuthJoin:
		// 一次性纳管邀请：Bearer，但非 scoped API Token，单独声明以免与 Token 混同。
		op.Security = []map[string][]string{{SchemeBearer: {}}}
		op.Description += "\n\n本端点以一次性 join_secret 作 Bearer（由 POST /api/v1/servers/invite 签发），非 API Token。"
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
		if e.AuthMode == route.AuthJoin {
			return "join_secret 无法识别"
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

// SpecPath 把 Go mux 的路径模板转成 OpenAPI 路径模板：尾通配符 {ref...} → {ref}。
// 文档层没有 "..." 这样的词汇，且路径模板里的变量名必须与声明的 path 参数名
// 完全一致——{ref...} 配一个叫 ref 的参数会被严格校验器判非法。
// 一致性测试用同一个函数把登记表路径规范化后再与 spec 比对，口径只此一处。
func SpecPath(p string) string { return strings.ReplaceAll(p, "...}", "}") }

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
		"ServerSummary": {
			Type:        "object",
			Description: "受管服务器。不含任何令牌字段。",
			Properties: map[string]*Schema{
				"ID":        {Type: "integer", Format: "int64"},
				"Name":      {Type: "string"},
				"BaseURL":   {Type: "string", Description: "HTTP 模式为 Agent 根地址；反向模式为 reverse://<host>"},
				"Arch":      {Type: "string"},
				"DockVer":   {Type: "string"},
				"Enabled":   {Type: "boolean"},
				"LastSeen":  {Type: "string", Description: "RFC3339；从未心跳时为空串"},
				"LastError": {Type: "string"},
			},
		},
		"ServerList": {
			Type:        "object",
			Description: "GET /api/v1/servers 的完整响应。",
			Properties: map[string]*Schema{
				"data": {Type: "array", Items: schemaRef("ServerSummary")},
			},
		},
		"ContainerSummary": {
			Type:        "object",
			Description: "docker ps 的一行（Engine 字段名大小写原样保留）。",
			Properties: map[string]*Schema{
				"Id":      {Type: "string"},
				"Names":   {Type: "array", Items: &Schema{Type: "string"}},
				"Image":   {Type: "string"},
				"ImageID": {Type: "string"},
				"Command": {Type: "string"},
				"Created": {Type: "integer", Format: "int64"},
				"State":   {Type: "string", Description: "running / exited / paused …"},
				"Status":  {Type: "string"},
				"Labels":  {Type: "object", AdditionalProperties: &Schema{Type: "string"}},
			},
		},
		"ContainerList": {
			Type:       "object",
			Properties: map[string]*Schema{"data": {Type: "array", Items: schemaRef("ContainerSummary")}},
		},
		"ContainerProcess": {
			Type:        "object",
			Description: "容器内单个进程（#91，来自宿主 /proc，按 cgroup 归属容器）。",
			Properties: map[string]*Schema{
				"pid":        {Type: "integer", Format: "int64"},
				"ppid":       {Type: "integer", Format: "int64"},
				"command":    {Type: "string"},
				"comm":       {Type: "string"},
				"state":      {Type: "string"},
				"user":       {Type: "string"},
				"mem_rss":    {Type: "integer", Format: "int64", Description: "常驻内存 KB"},
				"mem_vms":    {Type: "integer", Format: "int64", Description: "虚拟内存 KB"},
				"threads":    {Type: "integer"},
				"start_time": {Type: "integer", Format: "int64", Description: "相对系统启动的时钟滴答"},
			},
		},
		"ContainerProcessList": {
			Type:       "object",
			Properties: map[string]*Schema{"data": {Type: "array", Items: schemaRef("ContainerProcess")}},
		},
		"ContainerProcessDetail": {
			Type:        "object",
			Description: "单个进程的 exe/cwd/打开文件（#91 v2；pid 须归属该容器，否则 404）。",
			Properties: map[string]*Schema{
				"pid": {Type: "integer", Format: "int64"},
				"exe": {Type: "string", Description: "可执行文件路径（readlink /proc/<pid>/exe）"},
				"cwd": {Type: "string", Description: "工作目录（readlink /proc/<pid>/cwd）"},
				"open_files": {Type: "array", Items: &Schema{Type: "string"},
					Description: "打开文件的 fd 目标（readlink /proc/<pid>/fd/*，上限 128）"},
			},
		},
		"ImageSummary": {
			Type:        "object",
			Description: "本地镜像（Engine 字段名原样）。",
			Properties: map[string]*Schema{
				"Id":           {Type: "string"},
				"RepoTags":     {Type: "array", Items: &Schema{Type: "string"}},
				"RepoDigests":  {Type: "array", Items: &Schema{Type: "string"}},
				"Created":      {Type: "integer", Format: "int64"},
				"Size":         {Type: "integer", Format: "int64", Description: "字节"},
				"Architecture": {Type: "string"},
				"Os":           {Type: "string"},
				"Labels":       {Type: "object", AdditionalProperties: &Schema{Type: "string"}},
			},
		},
		"ImageList": {
			Type:       "object",
			Properties: map[string]*Schema{"data": {Type: "array", Items: schemaRef("ImageSummary")}},
		},
		"ComposeService": {
			Type: "object",
			Properties: map[string]*Schema{
				"name":         {Type: "string"},
				"state":        {Type: "string", Description: "running / exited / paused …"},
				"image":        {Type: "string"},
				"container":    {Type: "string", Description: "容器名（compose ps 的 Name/Names 首段）"},
				"container_id": {Type: "string", Description: "容器短 ID（compose ps 的 ID）"},
			},
		},
		"ComposeProject": {
			Type: "object",
			Properties: map[string]*Schema{
				"name":        {Type: "string", Description: "白名单内唯一标识：目录名；同目录多份 compose 时为「目录名-角色后缀」；再同名追加 -2 序号"},
				"path":        {Type: "string", Description: "项目目录绝对路径"},
				"config_file": {Type: "string", Description: "compose 文件绝对路径"},
				"status":      {Type: "string", Description: "running / partial / stopped / unknown"},
				"services":    {Type: "array", Items: schemaRef("ComposeService")},
			},
		},
		"ComposeList": {
			Type:       "object",
			Properties: map[string]*Schema{"data": {Type: "array", Items: schemaRef("ComposeProject")}},
		},
		"FileEntry": {
			Type: "object",
			Properties: map[string]*Schema{
				"name":     {Type: "string"},
				"path":     {Type: "string"},
				"is_dir":   {Type: "boolean"},
				"size":     {Type: "integer", Format: "int64", Description: "目录恒为 0"},
				"mode":     {Type: "string", Description: "八进制字符串，如 0644"},
				"mod_time": {Type: "string", Format: "date-time"},
			},
		},
		"FileRoot": {
			Type:        "object",
			Description: "一个文件管理根（--fs-root 白名单项）。",
			Properties: map[string]*Schema{
				"index":  {Type: "integer", Format: "int64"},
				"path":   {Type: "string", Description: "宿主机绝对路径"},
				"exists": {Type: "boolean", Description: "当前是否仍存在"},
			},
		},
		"FilesRoots": {
			Type:        "object",
			Description: "GET /files/roots 的 data。老 Agent 无此端点（404）。",
			Properties: map[string]*Schema{
				"roots": {Type: "array", Items: schemaRef("FileRoot")},
			},
		},
		"FilesList": {
			Type: "object",
			Description: "GET /files 的 data。truncated=true 时 entries 只是前 N 条，" +
				"客户端必须提示「还有更多」，不能当成完整清单。",
			Properties: map[string]*Schema{
				"path":      {Type: "string"},
				"entries":   {Type: "array", Items: schemaRef("FileEntry")},
				"truncated": {Type: "boolean"},
			},
		},
		"AuditLog": {
			Type: "object",
			Properties: map[string]*Schema{
				"id":         {Type: "integer", Format: "int64"},
				"ts":         {Type: "string"},
				"user_id":    {Type: "integer", Format: "int64"},
				"username":   {Type: "string"},
				"server_id":  {Type: "integer", Format: "int64"},
				"action":     {Type: "string"},
				"target":     {Type: "string"},
				"detail":     {Type: "string"},
				"result":     {Type: "string", Description: "ok | error"},
				"error":      {Type: "string"},
				"ip":         {Type: "string"},
				"request_id": {Type: "string"},
			},
		},
		"AuditLogsPage": {
			Type: "object",
			Properties: map[string]*Schema{
				"logs":        {Type: "array", Items: schemaRef("AuditLog")},
				"next_before": {Type: "integer", Format: "int64", Description: "下一页游标；0 表示没有更多"},
			},
		},
		"AuditLogsPageEnvelope": {
			Type:       "object",
			Properties: map[string]*Schema{"data": schemaRef("AuditLogsPage")},
		},
		"CPUStat": {
			Type: "object",
			Properties: map[string]*Schema{
				"cores":         {Type: "integer"},
				"usage_percent": {Type: "number", Description: "0–100，含 user/system/iowait/steal"},
				"user":          {Type: "integer", Format: "int64"},
				"system":        {Type: "integer", Format: "int64"},
				"iowait":        {Type: "integer", Format: "int64"},
				"steal":         {Type: "integer", Format: "int64"},
				"idle":          {Type: "integer", Format: "int64"},
			},
		},
		"MemoryStat": {
			Type:        "object",
			Description: "单位 byte。used = total - available（available 已含可回收 cache）。",
			Properties: map[string]*Schema{
				"total":      {Type: "integer", Format: "int64"},
				"used":       {Type: "integer", Format: "int64"},
				"available":  {Type: "integer", Format: "int64"},
				"free":       {Type: "integer", Format: "int64"},
				"buffers":    {Type: "integer", Format: "int64"},
				"cached":     {Type: "integer", Format: "int64"},
				"swap_total": {Type: "integer", Format: "int64"},
				"swap_used":  {Type: "integer", Format: "int64"},
			},
		},
		"DiskStat": {
			Type: "object",
			Properties: map[string]*Schema{
				"mount":        {Type: "string"},
				"fs_type":      {Type: "string"},
				"total":        {Type: "integer", Format: "int64"},
				"used":         {Type: "integer", Format: "int64"},
				"free":         {Type: "integer", Format: "int64"},
				"used_percent": {Type: "number"},
			},
		},
		"NetStat": {
			Type:        "object",
			Description: "累计值，不是速率；速率由 Hub 按两次快照时间差计算。",
			Properties: map[string]*Schema{
				"interface":  {Type: "string"},
				"rx_bytes":   {Type: "integer", Format: "int64"},
				"tx_bytes":   {Type: "integer", Format: "int64"},
				"rx_packets": {Type: "integer", Format: "int64"},
				"tx_packets": {Type: "integer", Format: "int64"},
				"rx_errors":  {Type: "integer", Format: "int64"},
				"tx_errors":  {Type: "integer", Format: "int64"},
			},
		},
		"LoadStat": {
			Type: "object",
			Properties: map[string]*Schema{
				"one": {Type: "number"}, "five": {Type: "number"}, "fifteen": {Type: "number"},
			},
		},
		"HostMetrics": {
			Type:        "object",
			Description: "服务器实时指标快照。CPU 由 Agent 对 /proc/stat 两次采样取差值。",
			Properties: map[string]*Schema{
				"collected_at": {Type: "string", Format: "date-time"},
				"cpu":          schemaRef("CPUStat"),
				"memory":       schemaRef("MemoryStat"),
				"disks":        {Type: "array", Items: schemaRef("DiskStat")},
				"network":      {Type: "array", Items: schemaRef("NetStat")},
				"load":         schemaRef("LoadStat"),
				"processes":    {Type: "integer"},
				"uptime_sec":   {Type: "integer", Format: "int64"},
			},
		},
		"ContainerStat": {
			Type:        "object",
			Description: "单容器实时占用（Agent 采集）。",
			Properties: map[string]*Schema{
				"id":          {Type: "string"},
				"name":        {Type: "string"},
				"cpu_percent": {Type: "number"},
				"mem_usage":   {Type: "integer", Format: "int64"},
				"mem_limit":   {Type: "integer", Format: "int64"},
				"mem_percent": {Type: "number"},
				"net_rx":      {Type: "integer", Format: "int64"},
				"net_tx":      {Type: "integer", Format: "int64"},
				"pids":        {Type: "integer", Format: "int64"},
			},
		},
		"MetricsSnapshot": {
			Type:        "object",
			Description: "GET /servers/{id}/metrics 的 data：服务器指标 + 每容器占用。",
			Properties: map[string]*Schema{
				"server_id": {Type: "integer", Format: "int64"},
				"ts":        {Type: "string", Format: "date-time"},
				"metrics":   schemaRef("HostMetrics"),
				"containers": {Type: "array", Items: schemaRef("ContainerStat"),
					Description: "null 表示该机没有容器指标（Agent 未接 Docker），与空数组语义不同"},
			},
		},
		"MetricsEnvelope": {
			Type:       "object",
			Properties: map[string]*Schema{"data": schemaRef("MetricsSnapshot")},
		},
		"ScopeList": {
			Type: "object",
			Properties: map[string]*Schema{
				"data": {Type: "array", Items: schemaRef("Scope")},
			},
		},
		"Scope": {
			Type:        "string",
			Description: "API Token 权限项。",
			Enum:        append([]string(nil), apitoken.AllScopes...),
		},
	}
}

// schemaRef 是 $ref 的简写，避免 map 字面量里到处拼长串。
func schemaRef(name string) *Schema { return &Schema{Ref: "#/components/schemas/" + name} }

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
