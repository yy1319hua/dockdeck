// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// Package fsapi 是 Agent 侧的文件管理实现。
//
// 它只认「相对白名单根的路径」，把宿主机绝对路径当作内部细节：对外的 DTO 里
// 只有相对路径（见 protocol.FileEntry 的注释——绝对路径既是敏感信息，
// 也会让调用方拼出越界请求）。
//
// 本包最重要的不是那几个操作，而是 Resolve：全部校验发生在任何 IO 之前，
// 顺序是
//
//	词法检查（绝对路径 / .. 段 / 控制字符 / 超长 / 过深）
//	→ 白名单词法前缀
//	→ realpath 解引用
//	→ 再前缀（挡符号链接逃逸）
//	→ 黑名单（对请求路径与解引用后的真实路径各查一遍）
//
// 任何一步不过就直接返回，绝不先碰文件系统。
package fsapi

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// 哨兵错误。cmd/agent 负责把它们映射成 protocol 错误码与 HTTP 状态码——
// 本包不依赖 HTTP 语义，单测可以直接断言错误本身。
var (
	// ErrPathEscape 是词法层面的越界：绝对路径、.. 段、控制字符、超长、过深。
	// 这类是「请求本身就不合法」，对 400。
	ErrPathEscape = errors.New("fsapi: 路径越界")
	// ErrPathDenied 是解引用后出了白名单，或命中敏感黑名单。
	// 这类是「请求合法但被拒绝」，对 403。
	ErrPathDenied = errors.New("fsapi: 路径被拒绝")
	// ErrNotFound 是目标不存在（ResolveNew 时指父目录不存在）。
	ErrNotFound = errors.New("fsapi: 路径不存在")
	// ErrIsDir / ErrNotDir 是类型不匹配，供调用方决定 400 还是 409。
	ErrIsDir  = errors.New("fsapi: 目标是目录")
	ErrNotDir = errors.New("fsapi: 目标不是目录")
	// ErrExists / ErrNotEmpty 是冲突类：目标已存在、目录非空（对 409）。
	ErrExists   = errors.New("fsapi: 目标已存在")
	ErrNotEmpty = errors.New("fsapi: 目录非空")
	// 上限类错误。ErrTooLarge 对 413，另两个对 400。
	ErrTooLarge       = errors.New("fsapi: 超过单文件上限")
	ErrTooDeep        = errors.New("fsapi: 超过目录深度上限")
	ErrTooManyEntries = errors.New("fsapi: 目录条目超过上限")
	// 配置类错误，只在启动时出现。
	ErrNoRoots = errors.New("fsapi: 未配置文件管理根目录")
	ErrBadRoot = errors.New("fsapi: 根目录不可用")
)

// maxRelLen 是相对路径的字节上限。
//
// 取值与常见 PATH_MAX 同量级，目的是挡住「几 MB 的路径」这种把内存和日志打满的输入，
// 不是文件系统的真实限制（真实限制由内核在 ENAMETOOLONG 时给出）。
const maxRelLen = 4096

// Guard 持有白名单根与全部限制，是文件操作的唯一入口。
//
// 用值语义会被复制，故按指针使用；它自身只读，可并发复用。
type Guard struct {
	roots []string // 已 realpath 的根绝对路径，索引即对外的「根序号」
	deny  []string // 敏感文件黑名单 pattern（path.Match 语法）

	maxFile    int64
	maxDepth   int
	maxEntries int
}

// NewGuard 构造 Guard：根目录必须存在、是目录、且能被 realpath 解析。
//
// 启动时就把不合法的根打回，而不是等到第一个请求——
// 配错根目录属于配置事故，越早炸越好（红线「校验发生在任何 IO 之前」的延伸）。
func NewGuard(roots, deny []string, maxFile int64, maxDepth, maxEntries int) (*Guard, error) {
	if len(roots) == 0 {
		return nil, ErrNoRoots
	}
	if maxFile <= 0 || maxDepth <= 0 || maxEntries <= 0 {
		return nil, fmt.Errorf("fsapi: 上限必须为正数（file=%d depth=%d entries=%d）",
			maxFile, maxDepth, maxEntries)
	}

	realRoots := make([]string, 0, len(roots))
	for _, raw := range roots {
		r := strings.TrimSpace(raw)
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) {
			return nil, fmt.Errorf("%w: %q 不是绝对路径", ErrBadRoot, raw)
		}
		real, err := filepath.EvalSymlinks(filepath.Clean(r))
		if err != nil {
			return nil, fmt.Errorf("%w: %q 无法解析: %v", ErrBadRoot, raw, err)
		}
		st, err := os.Stat(real)
		if err != nil {
			return nil, fmt.Errorf("%w: %q 无法访问: %v", ErrBadRoot, raw, err)
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("%w: %q 不是目录", ErrBadRoot, raw)
		}
		realRoots = append(realRoots, real)
	}
	if len(realRoots) == 0 {
		return nil, ErrNoRoots
	}

	// 黑名单 pattern 在启动时就验一遍：坏 pattern 若留到请求期，
	// path.Match 会返回错误，那时「忽略错误」就等于把保护静默关掉。
	for _, p := range deny {
		if p == "" {
			continue
		}
		if _, err := path.Match(p, "probe"); err != nil {
			return nil, fmt.Errorf("fsapi: 黑名单 pattern %q 非法: %w", p, err)
		}
	}

	return &Guard{
		roots:      realRoots,
		deny:       append([]string(nil), deny...),
		maxFile:    maxFile,
		maxDepth:   maxDepth,
		maxEntries: maxEntries,
	}, nil
}

// Roots 返回根的数量。
func (g *Guard) Roots() int { return len(g.roots) }

// RootInfos 返回各根的序号、宿主路径与当前是否存在。
//
// 只做 os.Stat，不解析符号链接：这里回答的是「这个根现在还在不在」，
// 不是「某个请求能不能通过校验」。Exists=false 时面板应置灰并给出移除入口，
// 而不是让用户点进去看到一片空白。
func (g *Guard) RootInfos() []protocol.FileRoot {
	out := make([]protocol.FileRoot, 0, len(g.roots))
	for i, p := range g.roots {
		_, err := os.Stat(p)
		out = append(out, protocol.FileRoot{Index: i, Path: p, Exists: err == nil})
	}
	return out
}

// MaxFile 返回单文件上限（字节）。
func (g *Guard) MaxFile() int64 { return g.maxFile }

// MaxDepth 返回目录深度上限。
func (g *Guard) MaxDepth() int { return g.maxDepth }

// MaxEntries 返回单次目录列表的条目上限。
func (g *Guard) MaxEntries() int { return g.maxEntries }

// ensureNotSymlink 拒绝「最后一段是符号链接」的目标——写、删、改名都要先过它。
//
// 理由：这些操作作用在解引用后的真实路径上，而用户看到的是链接名。
// 若放行，`delete inlink`（链接）会删掉 `sub`（真目录），
// 写 `cert.txt`（链接到受保护文件）会把别处的文件内容搬进白名单里的新文件。
// 两种都不是用户按下去时以为会发生的事，所以一律拒绝，让用户直接指名真实路径。
//
// 读操作不设这道门：读穿软链是明确的意图，且 Resolve 已保证落点在白名单内。
func (g *Guard) ensureNotSymlink(res Resolved) error {
	st, err := os.Lstat(res.Lexical)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s 是符号链接", ErrPathDenied, res.Rel)
	}
	return nil
}

// Resolved 是一次成功的路径解析结果。
type Resolved struct {
	// RootIndex 是命中的根序号。
	RootIndex int
	// Abs 是通过全部校验的绝对路径（已 realpath 解引用）。
	// 它是内部细节：不要把它写进任何对外响应。
	Abs string
	// Lexical 是 realpath 之前的白名单内绝对路径。
	//
	// 写/删/改名要靠它判「最后一段是不是符号链接」：os.Lstat(Lexical) 会穿过
	// 父级软链但不跟最后一段，正好是我们要的语义。少了它就只能拿解引用后的
	// Abs 去判断，而那时链接信息已经丢了。
	Lexical string
	// Rel 是相对根的规范路径（"/" 分隔，根自身为空串）。
	// 解引用过符号链接后它可能与请求路径不同——这是刻意的：调用方拿到的是
	// 「真实落在哪里」，后面所有操作都基于它。
	Rel string
}

// Resolve 解析一个必须已存在的目标：读、stat、删除、rename 的源。
func (g *Guard) Resolve(rootIndex int, rel string) (Resolved, error) {
	return g.resolve(rootIndex, rel, true)
}

// ResolveNew 解析一个可以尚不存在的目标：写新文件、建目录、rename 的目标。
//
// 它解引用的是**父目录**——最后一段还不存在，无法直接 EvalSymlinks。
// 父目录本身必须存在（不支持一次建多级目录）。
func (g *Guard) ResolveNew(rootIndex int, rel string) (Resolved, error) {
	return g.resolve(rootIndex, rel, false)
}

func (g *Guard) resolve(rootIndex int, rel string, mustExist bool) (Resolved, error) {
	if rootIndex < 0 || rootIndex >= len(g.roots) {
		return Resolved{}, fmt.Errorf("%w: 根序号 %d 不存在", ErrPathEscape, rootIndex)
	}
	root := g.roots[rootIndex]

	cleanRel, err := g.checkRel(rel)
	if err != nil {
		return Resolved{}, err
	}

	// 第一道前缀：realpath 之前的词法比对。它挡不住符号链接（那要靠下面第二道），
	// 但能在不触碰文件系统的前提下先把明显越界的请求打掉。
	lexical := root
	if cleanRel != "" {
		lexical = filepath.Join(root, filepath.FromSlash(cleanRel))
	}
	if !withinRoot(root, lexical) {
		return Resolved{}, fmt.Errorf("%w: %s", ErrPathEscape, rel)
	}

	var real string
	if mustExist {
		real, err = filepath.EvalSymlinks(lexical)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return Resolved{}, fmt.Errorf("%w: %s", ErrNotFound, rel)
			}
			return Resolved{}, err
		}
	} else {
		parent := filepath.Dir(lexical)
		realParent, perr := filepath.EvalSymlinks(parent)
		if perr != nil {
			if errors.Is(perr, fs.ErrNotExist) {
				return Resolved{}, fmt.Errorf("%w: 父目录不存在: %s", ErrNotFound, rel)
			}
			return Resolved{}, perr
		}
		real = filepath.Join(realParent, filepath.Base(lexical))
	}

	// 第二道前缀：解引用之后再比一次。符号链接指到白名单外，就在这里被拦下。
	if !withinRoot(root, real) {
		return Resolved{}, fmt.Errorf("%w: 符号链接逃逸 %s", ErrPathDenied, rel)
	}

	realRel := relOf(root, real)
	if err := g.checkDeny(cleanRel); err != nil {
		return Resolved{}, err
	}
	// 请求路径与真实路径都要过黑名单：白名单内的软链可以指向白名单内
	// 另一个被保护的目录（例如 cert.txt → keys/server.pem），只查请求路径会漏。
	if realRel != cleanRel {
		if err := g.checkDeny(realRel); err != nil {
			return Resolved{}, err
		}
	}

	return Resolved{RootIndex: rootIndex, Abs: real, Lexical: lexical, Rel: realRel}, nil
}

// checkRel 做词法检查并返回规范相对路径。
//
// 它**不**用 filepath.Clean 把 ".." 静默吃掉：Clean("/"+rel) 会把 "../x" 变成 "/x"，
// 越界输入就变成了合法请求。这里选择显式拒绝——请求里出现 ".." 就是错的。
func (g *Guard) checkRel(rel string) (string, error) {
	if len(rel) > maxRelLen {
		return "", fmt.Errorf("%w: 路径超过 %d 字节", ErrPathEscape, maxRelLen)
	}
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: 路径含 NUL", ErrPathEscape)
	}
	for _, r := range rel {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: 路径含控制字符", ErrPathEscape)
		}
	}
	// 空串与 "/" 都表示「根本身」。
	if rel == "" || rel == "/" {
		return "", nil
	}
	if strings.HasPrefix(rel, "/") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: 不接受绝对路径 %q", ErrPathEscape, rel)
	}

	segs := make([]string, 0, 8)
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("%w: 不接受 .. 段 %q", ErrPathEscape, rel)
		}
		segs = append(segs, seg)
	}
	if len(segs) > g.maxDepth {
		return "", fmt.Errorf("%w: 深度 %d 超过上限 %d", ErrTooDeep, len(segs), g.maxDepth)
	}
	return strings.Join(segs, "/"), nil
}

// checkDeny 命中黑名单即拒（读与写一视同仁）。
//
// 匹配两遍：逐段匹配（".env" 在任何层级都失效）与整条相对路径匹配
// （"etc/shadow" 这类带 "/" 的 pattern 由此生效）。宁严不宽。
func (g *Guard) checkDeny(rel string) error {
	if rel == "" {
		return nil
	}
	segs := strings.Split(rel, "/")
	for _, pattern := range g.deny {
		if pattern == "" {
			continue
		}
		hit, err := path.Match(pattern, rel)
		if err != nil {
			// pattern 已在 NewGuard 验过；真走到这里说明有并发改写之类的问题，
			// 那就当命中处理——拒绝永远比放行安全。
			return fmt.Errorf("%w: 黑名单 pattern %q 非法", ErrPathDenied, pattern)
		}
		if hit {
			return fmt.Errorf("%w: 命中黑名单 %q", ErrPathDenied, pattern)
		}
		for _, seg := range segs {
			hit, err := path.Match(pattern, seg)
			if err != nil {
				return fmt.Errorf("%w: 黑名单 pattern %q 非法", ErrPathDenied, pattern)
			}
			if hit {
				return fmt.Errorf("%w: 命中黑名单 %q", ErrPathDenied, pattern)
			}
		}
	}
	return nil
}

// withinRoot 判断 p 是否就是 root 或位于 root 之内。
//
// 比较时带上分隔符边界：否则 "/data-evil" 会被 "/data" 当成自家孩子，
// 这类前缀混淆是白名单实现最经典的洞。
func withinRoot(root, p string) bool {
	sep := string(filepath.Separator)
	if root == sep {
		// 根就是 "/"：任何绝对路径都在它之内。
		return strings.HasPrefix(p, sep)
	}
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+sep)
}

// relOf 求 p 相对 root 的规范路径（root 自身返回空串）。
func relOf(root, p string) string {
	sep := string(filepath.Separator)
	if root == sep {
		return strings.TrimPrefix(p, sep)
	}
	if p == root {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(p, root), sep)
}

// PathToRel 把一个绝对路径映射到「根序号 + 相对路径」，返回是否命中白名单根。
//
// 用途：把外部来源给出的绝对路径（例如容器 label 里的 compose 工作目录
// com.docker.compose.project.working_dir）接入白名单体系，供 DeleteRecursive 等
// 操作使用。
//
// 只做词法前缀判定（复用 withinRoot/relOf），不解析符号链接：返回的
// (rootIndex, rel) 会交给 Resolve/DeleteRecursive 重新做 realpath 校验，
// 所以这里即使拿到的是软链形态也安全——真正落文件系统的操作会二次校验，
// 不存在「前缀过了就放行」的窗口。
//
// 无根命中时 ok=false（rootIndex=0, rel="")，调用方据业务决定是否拒绝。
func (g *Guard) PathToRel(abs string) (rootIndex int, rel string, ok bool) {
	for i, root := range g.roots {
		if withinRoot(root, abs) {
			return i, relOf(root, abs), true
		}
	}
	return 0, "", false
}
