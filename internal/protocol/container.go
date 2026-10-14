// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

import (
	"strings"
	"time"
)

// ContainerInfo 是容器清单里的一条，对应 Agent 侧 Docker Engine API 的只读投影。
//
// 字段语义以真实引擎为准，S3 完成后会拿 ARM 宿主机上的真实返回值逐字段核对并回填
// docs/ARCHITECTURE.md 的「接口契约」。在此之前，本结构的字段只作占位，
// 不允许据此推断引擎行为。
type ContainerInfo struct {
	// ID 是容器完整 ID。
	ID string `json:"id"`
	// Name 不带前导斜杠，与 docker ps 的 NAMES 列一致。
	Name    string `json:"name"`
	Image   string `json:"image"`
	ImageID string `json:"image_id"`
	// State 是引擎给出的状态词（running/exited/paused 等）。
	State string `json:"state"`
	// Status 是人类可读的状态串（如 Up 3 hours），仅用于展示，不用于判断。
	Status    string            `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
	Labels    map[string]string `json:"labels,omitempty"`
	Ports     []PortBinding     `json:"ports,omitempty"`
}

// PortBinding 是一条端口映射。
//
// 端口用字符串而非整数：引擎可能返回 "3000/tcp" 这类带协议的复合值，
// 用整数会在解析阶段就丢信息。
type PortBinding struct {
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      string `json:"host_port"`
	ContainerPort string `json:"container_port"`
	Protocol      string `json:"protocol"`
}

// ContainerSummary 是容器清单的一条，对应 Agent 侧 Docker Engine API
// GET /containers/json 的原始投影。
//
// 定义放在 protocol 而非 dockercli：这个形状出现在 Hub↔Agent 线路上
// （Agent 原样转发 Engine 字段，Hub 解码），两侧必须共用同一份定义，
// 各自造 struct 必有字段悄悄对不上的一天。dockercli 保留同名别名，
// S3.2 的既有代码零改动。
//
// Names 是数组（一个容器可有多个名字），取用方自己决定取哪个；
// 本结构不做「取第一个名字」这种裁剪——那属于语义猜测。
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Command string            `json:"Command"`
	Created int64             `json:"Created"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
}

// ValidContainerID 判断容器 ID/名字是否合法。
//
// 规则与 Engine 命名规则一致：hex ID、ID 前缀与名字——
// 首字符必须字母数字（Docker 名字即此规则，顺带拒掉 "." / ".." 这类
// URL dot-segment，杜绝它们以路径段形态拼进 URL），其余字符
// [a-zA-Z0-9_.-]，总长 1–128 字节，不带前导斜杠。
// 容器 ID 会拼进 URL 路径，不校验等于给路径拼接注入开门：
// Hub（拼转发路径前）与 Agent（拼 Engine 路径前）两侧都必须在本地过它，
// 单点定义防止两侧字符集漂移。
func ValidContainerID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	// 首字符字母数字：Engine 规则，同时拒 dot-segment（"."/".."）。
	first := id[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')) {
		return false
	}
	for i := 1; i < len(id); i++ {
		ch := id[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '_' || ch == '.' || ch == '-':
		default:
			return false
		}
	}
	return true
}

// allowedSignals 是 Hub/Agent 两侧允许的 kill 信号集。
// 信号是攻击者可注入的字符串，白名单之外一律拒。
var allowedSignals = map[string]bool{
	"SIGTERM": true, "SIGKILL": true, "SIGINT": true,
	"SIGHUP": true, "SIGQUIT": true, "SIGUSR1": true, "SIGUSR2": true,
}

// NormalizeSignal 规范化 kill 信号名（大写）并校验白名单。
//
// 空串是合法输入：表示用引擎默认（SIGKILL），此时返回 ("", true)，
// 调用方不应把空信号拼进请求。Hub（客户端本地校验）与 Agent
// （落 Engine 前校验）共用同一份集合，单点定义防漂移。
func NormalizeSignal(signal string) (string, bool) {
	if signal == "" {
		return "", true
	}
	up := strings.ToUpper(signal)
	return up, allowedSignals[up]
}

// LogsOptions 是容器日志的查询参数，Hub→Agent 与 Agent→Engine 同一份契约。
//
// 定义放在 protocol：查询串是 Hub 拼、Agent 解析的，两侧必须共用字段名与
// 合法值规则；dockercli 保留同名别名，S3.3 的既有代码零改动。
type LogsOptions struct {
	// Follow 跟随输出（= docker logs -f）。跟随时客户端不施加请求超时，
	// 流的生命周期由调用方 ctx 决定。
	Follow bool
	// Tail 是「末尾 N 行」（"all"或数字；空 = 引擎默认全部）。
	Tail string
	// Since / Until 是 RFC3339 或 Unix 秒（原样透传，不猜语义）。
	//  Hub→Agent 拼查询串时必须 URL 编码："+08:00" 里的 + 不转义会被
	// 解析成空格，时区信息静默丢失。
	Since, Until string
	// Timestamps 在每行前加 Engine 时间戳。
	Timestamps bool
	// Export 仅存在于 Hub→Agent 契约：要 Engine 原始字节流（下载用）而非
	// 合流文本。Agent→Engine 无此参——合流/原始是 Agent 层的呈现选择。
	Export bool
}

// ValidTail 判断 tail 参数是否合法：空、"all" 或纯数字。
// Agent 解析侧与 Hub 拼装侧共用同一份规则，别把怪字符串透传给 Engine。
func ValidTail(tail string) bool {
	if tail == "" || tail == "all" {
		return true
	}
	for i := 0; i < len(tail); i++ {
		if tail[i] < '0' || tail[i] > '9' {
			return false
		}
	}
	return true
}

// ContainerStat 是单个容器的实时资源占用。
//
// 与 HostMetrics 一样只报当前值；MemUsage/MemLimit 单位 byte，
// CPUPercent/MemPercent 是 0–100。MemLimit 为 0 表示未设内存上限，
// 此时 MemPercent 无意义，UI 不应展示百分比。
type ContainerStat struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpu_percent"`
	MemUsage   uint64  `json:"mem_usage"`
	MemLimit   uint64  `json:"mem_limit"`
	MemPercent float64 `json:"mem_percent"`
	NetRx      uint64  `json:"net_rx"`
	NetTx      uint64  `json:"net_tx"`
	// PIDS 是容器内进程数。
	PIDS uint64 `json:"pids"`
}
