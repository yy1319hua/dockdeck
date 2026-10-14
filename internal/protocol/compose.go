// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.2：Compose 跨 Hub↔Agent 的线形状（与镜像/容器同款，定义在 protocol，
// dockercli/agentcli 留别名）。Compose 没有 daemon API，全部走宿主侧
// `docker compose` CLI（v2），所以这些形状描述的是 CLI 的解析产物，
// 而非 Engine 原生 JSON。

package protocol

// ComposeProject 是一个被发现的 compose 项目（白名单目录内扫描到 compose 文件）。
type ComposeProject struct {
	Name       string           `json:"name"`         // 目录名（白名单内唯一标识）
	Path       string           `json:"path"`         // 项目目录绝对路径
	ConfigFile string           `json:"config_file"`  // compose 文件绝对路径
	Status     string           `json:"status"`       // running | partial | stopped | unknown
	Services   []ComposeService `json:"services,omitempty"`
}

// ComposeService 是项目里的一个服务实例（docker compose ps 的一行）。
type ComposeService struct {
	Name  string `json:"name"`
	State string `json:"state"` // running | exited | paused | ...
	Image string `json:"image"`
}

// ComposeAction 是 POST /compose/{project}/{action} 允许的写动词。
// 注意：logs 是 GET 流端点（沿用容器日志通道），不在此白名单——
// 这里只收会改状态的四个动作，绝不把任意字符串透传给 shell。
type ComposeAction string

const (
	ComposeUp      ComposeAction = "up"
	ComposeDown    ComposeAction = "down"
	ComposeRestart ComposeAction = "restart"
	ComposePull    ComposeAction = "pull"
)

// ValidComposeAction 关门：只允许白名单里的动词，别的（如 rm、exec、
// restart-all）一律拒，绝不进 shell。单点定义在 protocol，Hub/Agent
// 两侧共用，不容许漂移。
func ValidComposeAction(a string) bool {
	switch ComposeAction(a) {
	case ComposeUp, ComposeDown, ComposeRestart, ComposePull:
		return true
	default:
		return false
	}
}
