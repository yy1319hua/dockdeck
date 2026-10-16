// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

// S7.11：Compose 写前备份的线形状。
//
// 备份由 dockercli.WriteFile 在覆盖写之前生成：`<compose 文件>.bak.<RFC3339>`，
// 与 compose 文件同目录。S4.3 只把它当作「写的时候顺手产生、路径在 toast 里
// 闪一下」的一次性信息；本文件补上列举、读取、恢复三件事的形状。
//
// 三条口径：
//  1. **只回文件名、不回绝对路径**（Q4-A）——与文件管理模块「对外不回宿主机
//     绝对路径」同口径。compose 模块既有的 ConfigFile / BackupFile 是绝对路径，
//     那是历史形状、不在本项改动范围；**新端点一律不跟着**。
//  2. **不提供删除**（Q3-A）——备份只增不减，清理走宿主机 CLI。自动或半自动
//     销毁回滚点，比备份占空间危险得多。
//  3. **恢复也是一次覆盖写**（Q2-A）——必须先备份「恢复前的当前内容」，
//     否则回滚是单向门：恢复到旧版本后就回不去了。

// ComposeBackup 是清单里的一条备份。
type ComposeBackup struct {
	// Name 是备份文件名（不含目录），形如 docker-compose.yml.bak.<RFC3339>。
	Name string `json:"name"`
	// Size 是字节数。
	Size int64 `json:"size"`
	// ModTime 是 RFC3339 文本（UTC），仅用于展示。清单的排序由文件名的
	// 字典序决定，不依赖这个字段（避免解析失败把顺序搅乱）。
	ModTime string `json:"mod_time"`
}

// ComposeBackupsList 是 GET /compose/{project}/backups 的响应。
type ComposeBackupsList struct {
	Backups []ComposeBackup `json:"backups"`
}

// ComposeBackupContent 是备份正文（预览与 diff 用）。
type ComposeBackupContent struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// ComposeBackupUpdate 是恢复操作的请求体：备份文件名。
//
// 备份名走请求体（读走查询串）而非 URL 路径段——名字里带 RFC3339 的 `+` 与
// `:`（如 `+08:00`），放进路径段就得处理编码差异（S7.5 已踩过项目名与镜像
// ref 的编码坑），放 body / 查询串最省心，也不给后来人留抄错的余地。
type ComposeBackupUpdate struct {
	File string `json:"file"`
}

// ComposeBackupRestored 是恢复操作的响应。
type ComposeBackupRestored struct {
	// Backup 是恢复前对「恢复前的当前内容」生成的新备份文件名（不含目录）。
	// 有了它，恢复之后还能再回到恢复前的状态——回滚不是单向门。
	Backup string `json:"backup"`
}
