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

	// ComposeName 是 compose 自己认的项目名——即 compose 文件顶层的 name: 字段；
	// 文件没写 name: 时 compose 退回目录名，此时这里等于目录名。
	//
	// 为什么单独一个字段而不直接把 Name 换成它：Name 是前端路由段与列表 key，
	// 历史上一直是「目录名-角色后缀」，改了会让旧链接失效。ComposeName 只
	// 用于展示与冲突判定——compose 执行时以文件里的 name: 为准，两者不一致
	// 正是「面板上看到 A、实际操作到 B」这类误伤的根源，必须显式呈现。
	ComposeName string `json:"compose_name,omitempty"`

	// Sources 标注这个项目是怎么被发现的（可同时命中多个来源）。
	//
	// 单值字段表达不了「白名单与 label 同时指向它」这一交集态，而交集态恰恰是
	// 最可信的一种（人工确认过 + 引擎也认），值得单独呈现给运维。
	Sources []ComposeSource `json:"sources,omitempty"`

	// Conflict 为 true 表示该项目的 compose 名与其他路径下的 compose 文件重名。
	//
	// 重名意味着 compose 会把它们当成同一个项目（compose 以文件顶层 name: 为
	// 准），于是「操作备份副本」实际会作用到线上项目——这是自伤入口。
	// 冲突项目一律可见但禁止写操作。
	Conflict bool `json:"conflict,omitempty"`

	// ConflictPaths 列出与本项目重名的其它 compose 文件绝对路径。
	// 冲突时至少有一项，便于运维定位到「撞名的那份」。
	ConflictPaths []string `json:"conflict_paths,omitempty"`
}

// ComposeSource 标注 compose 项目的发现来源。
type ComposeSource string

const (
	// SourceWhitelist：由 --compose-dirs / DD_AGENT_COMPOSE_DIRS 白名单目录
	// 递归扫描发现——运维显式配置的目录。
	SourceWhitelist ComposeSource = "whitelist"
	// SourceLabel：由容器上的 compose label（com.docker.compose.project.*
	// 系列）反推目录后扫描发现——容器正在跑即是它真实存在的证据。
	SourceLabel ComposeSource = "label"
)

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
