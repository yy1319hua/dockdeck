// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.3：Compose YAML 文件读写的线形状（与镜像/容器同款，定义在 protocol，
// dockercli/agentcli 留别名）。这些形状描述 compose 文件内容本身，而非
// `docker compose` CLI 的输出——读写走 Agent 侧白名单目录内的真实文件系统，
// 不依赖 Engine 也不依赖 docker CLI。

package protocol

// ComposeFile 是 GET /compose/{project}/file 的响应：项目当前 compose 文件
// 的完整内容与它的绝对路径。
type ComposeFile struct {
	ConfigFile string `json:"config_file"` // compose 文件绝对路径
	Content    string `json:"content"`     // 文件原始内容（YAML 文本）
}

// ComposeFileUpdate 是写操作的请求体：新内容。读取当前内容由 Agent 侧负责，
// 调用方只提交想要写入的目标文本。
type ComposeFileUpdate struct {
	Content string `json:"content"`
}

// ComposeFileDiff 是 POST /compose/{project}/diff 的响应：新内容相对当前文件的
// unified diff（纯文本，供前端预览），尚未落盘、也未产生备份。
type ComposeFileDiff struct {
	Diff string `json:"diff"`
}

// ComposeFileWritten 是 PUT /compose/{project}/file 的响应：写入成功，并返回
// 写前自动生成的备份文件路径（*.bak.<RFC3339>），供用户回滚。
type ComposeFileWritten struct {
	BackupFile string `json:"backup_file"`
}
