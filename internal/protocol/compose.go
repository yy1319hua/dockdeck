// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.2：Compose 跨 Hub↔Agent 的线形状（与镜像/容器同款，定义在 protocol，
// dockercli/agentcli 留别名）。Compose 没有 daemon API，全部走宿主侧
// `docker compose` CLI（v2），所以这些形状描述的是 CLI 的解析产物，
// 而非 Engine 原生 JSON。

package protocol

// ComposeProject 是一个被发现的 compose 项目（白名单目录内扫描到 compose 文件）。
type ComposeProject struct {
	// Name 是项目在白名单内的唯一标识（前端拿它做路由段与列表 key）：
	// 目录名；同目录多份 compose 时为「目录名-角色后缀」（如 dockdeck-agent）。
	// 同名再出现则追加 -2、-3 序号，保证唯一可解析。
	Name       string           `json:"name"`
	Path       string           `json:"path"`        // 项目目录绝对路径
	ConfigFile string           `json:"config_file"` // compose 文件绝对路径
	Status     string           `json:"status"`      // running | partial | stopped | unknown
	Services   []ComposeService `json:"services,omitempty"`
}

// ComposeService 是项目里的一个服务实例（docker compose ps 的一行）。
type ComposeService struct {
	Name  string `json:"name"`
	State string `json:"state"` // running | exited | paused | ...
	Image string `json:"image"`
	// Container 是该服务对应的容器名（docker compose ps 输出的 Name/Names 首段）。
	// 空 = compose 版本没给容器名（只有服务名可用）。
	Container string `json:"container,omitempty"`
	// ContainerID 是容器短 ID，与 Container 一起用于「本项目是否含 Agent 自身
	// 容器」的判定——Agent 自身容器在 compose 视角下只是一个普通服务实例，
	// 不比对容器身份就拦不住「把承载自己的项目 down 掉」这类自伤动作。
	ContainerID string `json:"container_id,omitempty"`
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

// ComposeActionResult 是写动作（up/down/restart/pull）的响应。Output 直接
// 透传 compose 的原生输出（stdout+stderr 合流），Hub 层原样给前端展示，
// 不另造文案——compose 的业务成败写在 Output 里。
type ComposeActionResult struct {
	Output string `json:"output"`
}
