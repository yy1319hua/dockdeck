// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

import "strings"

// ImageSummary 是镜像清单的一条，对应 Agent 侧 Docker Engine API
// GET /images/json 的原始投影。
//
// 定义放在 protocol 而非 dockercli，与 ContainerSummary 同一理由：
// 这个形状出现在 Hub↔Agent 线路上（Agent 原样转发 Engine 字段，Hub 解码），
// 两侧必须共用同一份定义。dockercli 保留同名别名。
//
// 字段按「镜像页展示刚需」选，遵循 ContainerSummary 先例——不为
// 「将来也许用到」预留（那是 inspect 原样透传的职责，list 不干）：
//   - RepoTags 为空（nil 或 []）即 dangling 镜像：无 tag 的中间层/孤儿镜像，
//     prune 的清理对象。len==0 判定对 nil 与 [] 同效，UI 不需区分二者；
//   - Architecture/Os 是多架构镜像页的必需列（buildx 推双架构后的现实）；
//   - Size 是引擎上报的总大小（byte），VirtualSize 已废弃不取。
type ImageSummary struct {
	ID           string            `json:"Id"`
	RepoTags     []string          `json:"RepoTags"`
	RepoDigests  []string          `json:"RepoDigests"`
	Created      int64             `json:"Created"`
	Size         int64             `json:"Size"`
	Architecture string            `json:"Architecture"`
	Os           string            `json:"Os"`
	Labels       map[string]string `json:"Labels"`
}

// ValidImageRef 判断镜像引用（pull 的 fromImage / delete 的目标）是否合法。
//
// 与 ValidContainerID 同一哲学：**字符集与结构防注入，不猜语义**。
// 镜像引用比容器 ID 复杂——合法形态横跨
// name[:tag]、host[:port]/path/name[:tag]、name@sha256:<64hex>、裸 ID
// （sha256:<64hex> 或 12+ 位短 ID）。做完整的 Docker reference 语法解析
// （官方正则上百行）会把合法 ref 误拒，那不是我们的职责；我们要的是
// 拒掉「能拼出路径遍历/CRLF/奇怪字符」的输入，语义错误交给 Engine，
// 它的 404/400 原样透传给调用方。
//
// 规则：
//   - 非空，≤255 字节（Docker reference 上限）；
//   - 首字符必须字母数字（Engine 命名规则，顺带拒 "." / ".." dot-segment）；
//   - 字符集 [a-zA-Z0-9_.:/@-]，其余一律拒（空格、分号、CR/LF、% 等）；
//   - 拒 ".."（路径遍历）与 "//"（空路径段）；
//   - 结构残缺拒：以 ":" "/" "@" 结尾；
//   - 含 "@" 时强制 digest 形态：@sha256:<64 位小写 hex>，且 "@" 只出现一次。
//
// Hub（拼 Agent 转发路径前）与 Agent（拼 Engine 路径前）两侧都必须在
// 本地过它，单点定义防止两侧规则漂移。
func ValidImageRef(ref string) bool {
	if ref == "" || len(ref) > 255 {
		return false
	}
	first := ref[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')) {
		return false
	}
	for i := 0; i < len(ref); i++ {
		ch := ref[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		case ch == '_' || ch == '.' || ch == ':' || ch == '/' || ch == '@' || ch == '-':
		default:
			return false
		}
	}
	// 结构断言：路径遍历、空路径段、残缺结尾。
	if strings.Contains(ref, "..") || strings.Contains(ref, "//") {
		return false
	}
	switch ref[len(ref)-1] {
	case ':', '/', '@':
		return false
	}
	// digest 形态强校验：含 "@" 就必须是 @sha256:<64hex>，且只能有一个 "@"。
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		if strings.IndexByte(ref[at+1:], '@') >= 0 {
			return false
		}
		const prefix = "sha256:"
		d := ref[at+1:]
		if !strings.HasPrefix(d, prefix) || len(d) != len(prefix)+64 {
			return false
		}
		for i := len(prefix); i < len(d); i++ {
			c := d[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
	}
	return true
}
