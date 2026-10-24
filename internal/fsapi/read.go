// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package fsapi

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// sniffLen 与 net/http 的内容嗅探长度一致。
const sniffLen = 512

// errListFull 是 List 内部的截断信号，不外泄。
var errListFull = errors.New("fsapi: 列表已达上限")

// List 列出目录内容，按 Path 升序返回。
//
// depth 语义：<=1 只列直接子项，n>1 递归到第 n 层；大于配置上限时按上限收敛
// （不报错——列举是只读操作，收敛比拒绝更符合直觉，真越界仍由 maxDepth 拦）。
//
// 两条刻意的口径：
//  1. **列出的每一项都必须是可操作的**：每项都过一遍 Resolve，逃逸软链、
//     命中黑名单、悬空软链一律不出现在结果里——能看见就能点，点了不会 403。
//  2. 因此软链按其**真实落点**展示（Name 是链接名，Path 是解引用后的相对路径），
//     同一目标经不同链接出现两次是预期的（磁盘上确实有两处）。
//
// 单个子目录不可读不拖垮整次列举（与 compose discover 同口径）：跳过它继续。
// 但被请求的目录本身不可读时如实报错——那是用户明确要看的东西。
//
// 返回的 truncated 表示「因 maxEntries 截断」。
func (g *Guard) List(rootIndex int, rel string, depth int) ([]protocol.FileEntry, bool, error) {
	res, err := g.Resolve(rootIndex, rel)
	if err != nil {
		return nil, false, err
	}
	st, err := g.be.Stat(res.Abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, fmt.Errorf("%w: %s", ErrNotFound, rel)
		}
		return nil, false, err
	}
	if !st.IsDir() {
		return nil, false, fmt.Errorf("%w: %s", ErrNotDir, rel)
	}

	if depth <= 0 {
		depth = 1
	}
	if depth > g.maxDepth {
		depth = g.maxDepth
	}

	out := make([]protocol.FileEntry, 0, 16)
	truncated := false
	// seen 用解引用后的绝对路径去重，防软链成环把递归拖死。
	seen := map[string]bool{res.Abs: true}

	var walk func(dirRel string, level int) error
	walk = func(dirRel string, level int) error {
		dir, err := g.Resolve(rootIndex, dirRel)
		if err != nil {
			return nil
		}
		items, err := g.be.ReadDir(dir.Abs)
		if err != nil {
			// 子目录不可读：跳过，不中断整次列举。
			return nil
		}
		for _, it := range items {
			childRel := it.Name()
			if dirRel != "" {
				childRel = dirRel + "/" + it.Name()
			}
			cr, err := g.Resolve(rootIndex, childRel)
			if err != nil {
				continue
			}
			cst, err := g.be.Stat(cr.Abs)
			if err != nil {
				continue
			}
			out = append(out, protocol.FileEntry{
				Name:    it.Name(),
				Path:    cr.Rel,
				IsDir:   cst.IsDir(),
				Size:    entrySize(cst),
				Mode:    modeString(cst),
				ModTime: cst.ModTime(),
			})
			if len(out) >= g.maxEntries {
				truncated = true
				return errListFull
			}
			if cst.IsDir() && level < depth && !seen[cr.Abs] {
				seen[cr.Abs] = true
				if err := walk(cr.Rel, level+1); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := walk(res.Rel, 1); err != nil && !errors.Is(err, errListFull) {
		return nil, false, err
	}
	// os.ReadDir 每层有序，拼接后跨层仍需整体排序才是稳定顺序。
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, truncated, nil
}

// Stat 返回单个目标的元信息（不含正文）。
//
// 目标是目录时报 ErrIsDir：目录的信息由 List 给，接口不混用。
func (g *Guard) Stat(rootIndex int, rel string) (protocol.FileMeta, error) {
	res, err := g.Resolve(rootIndex, rel)
	if err != nil {
		return protocol.FileMeta{}, err
	}
	f, err := g.be.Open(res.Abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return protocol.FileMeta{}, fmt.Errorf("%w: %s", ErrNotFound, rel)
		}
		return protocol.FileMeta{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return protocol.FileMeta{}, err
	}
	if st.IsDir() {
		return protocol.FileMeta{}, fmt.Errorf("%w: %s", ErrIsDir, rel)
	}
	ct, err := contentType(res.Abs, f)
	if err != nil {
		return protocol.FileMeta{}, err
	}
	return protocol.FileMeta{
		Path:        res.Rel,
		Size:        st.Size(),
		ContentType: ct,
		ModTime:     st.ModTime(),
		Truncated:   st.Size() > g.maxFile,
	}, nil
}

// Reader 是一次流式读取：Meta 描述目标，Body 是正文。
type Reader struct {
	Meta protocol.FileMeta
	Body io.ReadCloser
}

// Close 释放底层文件句柄。
func (r *Reader) Close() error {
	if r == nil || r.Body == nil {
		return nil
	}
	return r.Body.Close()
}

// Open 打开文件用于流式读取。
//
// 正文被限制在 maxFile 字节内：Meta.Truncated 为 true 表示源文件更大、正文被截断。
// 截断是刻意的——把 10 GiB 的日志整个读进内存再序列化，比拒绝更糟。
func (g *Guard) Open(rootIndex int, rel string) (*Reader, error) {
	res, err := g.Resolve(rootIndex, rel)
	if err != nil {
		return nil, err
	}
	f, err := g.be.Open(res.Abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, rel)
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, fmt.Errorf("%w: %s", ErrIsDir, rel)
	}
	ct, err := contentType(res.Abs, f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Reader{
		Meta: protocol.FileMeta{
			Path:        res.Rel,
			Size:        st.Size(),
			ContentType: ct,
			ModTime:     st.ModTime(),
			Truncated:   st.Size() > g.maxFile,
		},
		Body: &limitedFile{r: io.LimitReader(f, g.maxFile), c: f},
	}, nil
}

// limitedFile 把「限流后的 Reader」与底层句柄的关闭动作绑在一起，
// 免得调用方拿到裸 *os.File 就绕过了限制。
type limitedFile struct {
	r io.Reader
	c io.Closer
}

func (l *limitedFile) Read(p []byte) (int, error) { return l.r.Read(p) }
func (l *limitedFile) Close() error               { return l.c.Close() }

// contentType 判定正文类型：扩展名优先（廉价且精确），未知才嗅探前 512 字节。
//
// 嗅探后必须 Seek 回文件头，否则正文会丢掉开头那 512 字节——这是流式读取
// 最容易踩的坑，所以把「嗅探 + 回退」封在一个函数里，调用方无从忘记。
func contentType(abs string, f *os.File) (string, error) {
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(abs))); ct != "" {
		return ct, nil
	}
	buf := make([]byte, sniffLen)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	if n == 0 {
		return "application/octet-stream", nil
	}
	return http.DetectContentType(buf[:n]), nil
}

// entrySize 对目录返回 0：目录体积无意义，返回 inode 大小只会误导前端。
func entrySize(st os.FileInfo) int64 {
	if st.IsDir() {
		return 0
	}
	return st.Size()
}

// modeString 输出形如 "0644" 的权限串，供前端直接展示。
func modeString(st os.FileInfo) string {
	return fmt.Sprintf("%04o", st.Mode().Perm())
}
