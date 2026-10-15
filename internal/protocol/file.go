// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

import "time"

// 文件写操作的动作取值。
const (
	// ActionMkdir 创建目录。
	ActionMkdir = "mkdir"
	// ActionDelete 删除文件或空目录。
	ActionDelete = "delete"
	// ActionRename 改名或移动，目标路径放在 Dest。
	ActionRename = "rename"
)

// FileEntry 是目录列表里的一项。
//
// Path 是相对于文件管理根白名单的路径，不暴露宿主机绝对路径——
// 绝对路径属于敏感信息，且会让前端有能力拼出越界请求。
type FileEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// IsDir 决定是否渲染为可展开目录。
	IsDir bool `json:"is_dir"`
	// Size 对目录无意义，约定为 0。
	Size int64 `json:"size"`
	// Mode 是形如 "0644" 的八进制字符串，便于前端直接展示。
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
}

// FileMeta 是读取文件内容前返回的元信息。
//
// 文件内容本身不进 JSON：正文走流式响应，避免把可能很大的文件整个读进内存
// 再序列化一遍。Truncated 表示因超过单文件上限而被截断。
type FileMeta struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// ContentType 由探测得出，未知时为 application/octet-stream。
	ContentType string    `json:"content_type"`
	ModTime     time.Time `json:"mod_time"`
	Truncated   bool      `json:"truncated"`
}

// FileAction 是一次文件写操作的请求体。
//
// Path 与 Dest 都必须先在根白名单内经 realpath 解引用后做前缀比对，
// 命中 ..、符号链接逃逸或敏感黑名单时分别返回 CodePathEscape / CodePathDenied，
// 该校验发生在任何 IO 之前。
type FileAction struct {
	Action string `json:"action"`
	Path   string `json:"path"`
	// Dest 仅 ActionRename 使用，其余动作为空。
	Dest string `json:"dest,omitempty"`
}

// FileWritten 是写入文件内容的结果。
//
// BackupFile 是写前备份的相对路径（新文件没有备份时为空）——备份也落在白名单内，
// 所以同样只给相对路径。Created 表示目标原本不存在，此时没有可回滚的备份。
type FileWritten struct {
	Path       string `json:"path"`
	BackupFile string `json:"backup_file,omitempty"`
	Size       int64  `json:"size"`
	Created    bool   `json:"created"`
}

// FileActionDone 是 mkdir / delete / rename 的结果。
//
// Dest 仅 rename 使用：改名后的相对路径（源在 Path）。
type FileActionDone struct {
	Action string `json:"action"`
	Path   string `json:"path"`
	Dest   string `json:"dest,omitempty"`
}

// FilesList 是目录列举的结果。
//
// Path 是请求路径的原样回显（便于调用方对账）；Truncated 表示因条目上限被截断——
// 它是数据的一部分，所以放响应体而不是 HTTP 头，App 与 UI 都看得见。
type FilesList struct {
	Path      string      `json:"path"`
	Entries   []FileEntry `json:"entries"`
	Truncated bool        `json:"truncated"`
}

// MaxFileUploadBytes 是文件上传的共享上限（16 MiB）。
//
// 放在 protocol 而不是两侧各自定义：Hub 与 Agent 必须用同一个数字。
// Agent 鉴权层要先读完整个 body 才能算 HMAC，Hub 转发时同样要把 body
// 读进内存签名，两边上限一旦漂移，就会冒出「Hub 放行、Agent 413」这类
// 只在大文件上才暴露的错位。
//
// 它同时是「单请求最大内存占用」的上界，因此不允许无限大：要传更大的文件，
// 改这个常量并两侧一起发版，而不是让运维随手调参。
const MaxFileUploadBytes = 16 << 20

// ValidFileAction 判断动作取值是否合法。
//
// 写操作入口必须先过它：未知动作一律拒绝，避免出现「动作名拼错却执行了默认分支」的情况。
func ValidFileAction(action string) bool {
	switch action {
	case ActionMkdir, ActionDelete, ActionRename:
		return true
	default:
		return false
	}
}
