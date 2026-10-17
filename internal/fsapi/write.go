// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package fsapi

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// fileMode 是写入文件的权限：固定 0644，**不带可执行位**。
//
// 覆盖已有文件时也不保留原权限：红线要求「上传物不可被执行」，
// 而保留 0755 就等于把刚上传的内容变成可执行文件。需要执行的脚本请在宿主机上自行 chmod。
const fileMode = 0o644

// dirMode 是新建目录的权限。
const dirMode = 0o755

// uploadTempPattern 是流式写入用的临时文件名（与目标同目录，便于 rename）。
const uploadTempPattern = ".dd-upload-*"

// backupTimeLayout 是备份文件名里的时间戳：定宽到纳秒（9 位小数）。
//
// 定宽是关键：轮转依赖「文件名字典序 = 备份时间先后序」，
// RFC3339Nano 小数位变长会把这个次序打乱。纳秒精度把同秒撞名
// 压到几乎为零，剩下的竞态由 O_EXCL + .N 后缀兜底。
const backupTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// maxFileBackups 是单个文件的备份保留份数，与 dockercli compose 备份
// 轮转口径一致（20 份）：远超手工回滚所需，又不至于让「每次保存
// 留一份」无上限地堆积。用户侧仍无删除入口（S5.4 口径）——
// 系统侧自动轮转是对「只增不减」的唯一兜底。
const maxFileBackups = 20

// Write 流式写入文件内容。
//
// 步骤顺序是刻意的（红线「备份优先于变更」）：
//  1. 请求体先流式写进**同目录的临时文件**，上限检查也在这一步做完——
//     超限时原文件一个字节都没动过；
//  2. 目标已存在时才做备份，**备份落盘成功才继续**；
//  3. 最后 rename 覆盖；这一步失败，原文件仍完好。
//
// 任何一步失败都会清掉临时文件：不留半成品、不留孤儿文件。
func (g *Guard) Write(rootIndex int, rel string, content io.Reader) (protocol.FileWritten, error) {
	res, err := g.ResolveNew(rootIndex, rel)
	if err != nil {
		return protocol.FileWritten{}, err
	}
	if res.Rel == "" {
		return protocol.FileWritten{}, fmt.Errorf("%w: 不能把根目录当文件写", ErrIsDir)
	}
	if err := g.ensureNotSymlink(res); err != nil {
		return protocol.FileWritten{}, err
	}

	exists := false
	switch st, err := os.Stat(res.Abs); {
	case err == nil:
		if st.IsDir() {
			return protocol.FileWritten{}, fmt.Errorf("%w: %s", ErrIsDir, res.Rel)
		}
		exists = true
	case errors.Is(err, fs.ErrNotExist):
	default:
		return protocol.FileWritten{}, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(res.Abs), uploadTempPattern)
	if err != nil {
		return protocol.FileWritten{}, err
	}
	tmpName := tmp.Name()
	// rename 成功后 tmpName 已不存在，这里的 Remove 只会拿到 ENOENT，忽略即可。
	defer func() { _ = os.Remove(tmpName) }()

	n, err := io.Copy(tmp, io.LimitReader(content, g.maxFile+1))
	if err != nil {
		tmp.Close()
		return protocol.FileWritten{}, err
	}
	if n > g.maxFile {
		tmp.Close()
		return protocol.FileWritten{}, fmt.Errorf("%w: 请求体超过 %d 字节", ErrTooLarge, g.maxFile)
	}
	if err := tmp.Chmod(fileMode); err != nil {
		tmp.Close()
		return protocol.FileWritten{}, err
	}
	if err := tmp.Close(); err != nil {
		return protocol.FileWritten{}, err
	}

	backupRel := ""
	if exists {
		backupAbs, err := g.backup(res.Abs)
		if err != nil {
			return protocol.FileWritten{}, err
		}
		backupRel = relOf(g.roots[res.RootIndex], backupAbs)
	}
	if err := os.Rename(tmpName, res.Abs); err != nil {
		return protocol.FileWritten{}, err
	}

	return protocol.FileWritten{
		Path:       res.Rel,
		BackupFile: backupRel,
		Size:       n,
		Created:    !exists,
	}, nil
}

// backup 把当前内容另存为 <abs>.bak.<定宽奈秒时间戳>，返回备份的绝对路径。
//
// 两个刻意的设计：
//  1. 选名与占名是同一个 O_CREATE|O_EXCL 调用——「绝不覆盖既有备份」
//     由内核保证，同一纳秒的竞态撞名自动换 .1/.2…，
//     不存在「Lstat 检查后又被抢占」的窗口（旧实现的漏洞：
//     两个并发备份会都通过检查，然后后一个直接 EEXIST 失败，
//     整次 Write 报错，而不是换名重来）；
//  2. 写成功后轮转：只保留最近 maxFileBackups 份，更旧的自动删。
func (g *Guard) backup(abs string) (string, error) {
	src, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer src.Close()

	base := abs + ".bak." + time.Now().Format(backupTimeLayout)
	name := base
	for n := 1; n <= 1000; n++ {
		dst, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
		if err == nil {
			if _, cerr := io.Copy(dst, src); cerr != nil {
				dst.Close()
				_ = os.Remove(name)
				return "", cerr
			}
			if cerr := dst.Close(); cerr != nil {
				_ = os.Remove(name)
				return "", cerr
			}
			rotateFileBackups(abs)
			return name, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		name = fmt.Sprintf("%s.%d", base, n)
	}
	return "", fmt.Errorf("fsapi: 备份文件名重名过多: %s", base)
}

// rotateFileBackups 轮转 abs 的备份：按名字排序后只保留最近
// maxFileBackups 份，更旧的删掉。
//
// 只删普通文件：撞进 <base>.bak. 前缀的目录/软链形态项跳过不删，
// 但名额照占——等价于「regular 备份最终保留 maxFileBackups 份」。
// 删错了没有后悔药，宁肯跳过。
func rotateFileBackups(abs string) {
	dir := filepath.Dir(abs)
	items, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := filepath.Base(abs) + ".bak."
	names := make([]string, 0, len(items))
	for _, it := range items {
		if strings.HasPrefix(it.Name(), prefix) {
			names = append(names, it.Name())
		}
	}
	if len(names) <= maxFileBackups {
		return
	}
	// 倒序：最新在前，[maxFileBackups:] 就是待删的旧账。
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, name := range names[maxFileBackups:] {
		p := filepath.Join(dir, name)
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		_ = os.Remove(p)
	}
}

// Mkdir 新建目录，只建一级（父目录必须已存在）。
//
// 不用 MkdirAll：一次请求顺手建出多层目录，属于「静默扩大影响范围」，
// 与 ResolveNew「父目录必须存在」的口径也不一致。
func (g *Guard) Mkdir(rootIndex int, rel string) (protocol.FileActionDone, error) {
	res, err := g.ResolveNew(rootIndex, rel)
	if err != nil {
		return protocol.FileActionDone{}, err
	}
	if res.Rel == "" {
		return protocol.FileActionDone{}, fmt.Errorf("%w: 根目录", ErrExists)
	}
	if err := g.ensureNotSymlink(res); err != nil {
		return protocol.FileActionDone{}, err
	}
	switch _, err := os.Lstat(res.Abs); {
	case err == nil:
		return protocol.FileActionDone{}, fmt.Errorf("%w: %s", ErrExists, res.Rel)
	case !errors.Is(err, fs.ErrNotExist):
		return protocol.FileActionDone{}, err
	}
	if err := os.Mkdir(res.Abs, dirMode); err != nil {
		return protocol.FileActionDone{}, err
	}
	return protocol.FileActionDone{Action: protocol.ActionMkdir, Path: res.Rel}, nil
}

// Delete 删除文件或空目录。
//
// 目录非空一律拒绝：递归删除是「一次操作毁掉一片」的典型入口，
// 白名单内也不给（要清目录就逐个删，看得见每一步）。
func (g *Guard) Delete(rootIndex int, rel string) (protocol.FileActionDone, error) {
	res, err := g.Resolve(rootIndex, rel)
	if err != nil {
		return protocol.FileActionDone{}, err
	}
	if res.Rel == "" {
		return protocol.FileActionDone{}, fmt.Errorf("%w: 不允许删除根目录", ErrPathDenied)
	}
	if err := g.ensureNotSymlink(res); err != nil {
		return protocol.FileActionDone{}, err
	}
	st, err := os.Stat(res.Abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return protocol.FileActionDone{}, fmt.Errorf("%w: %s", ErrNotFound, rel)
		}
		return protocol.FileActionDone{}, err
	}
	if st.IsDir() {
		items, err := os.ReadDir(res.Abs)
		if err != nil {
			return protocol.FileActionDone{}, err
		}
		if len(items) > 0 {
			return protocol.FileActionDone{}, fmt.Errorf("%w: %s", ErrNotEmpty, res.Rel)
		}
	}
	if err := os.Remove(res.Abs); err != nil {
		return protocol.FileActionDone{}, err
	}
	return protocol.FileActionDone{Action: protocol.ActionDelete, Path: res.Rel}, nil
}

// Rename 改名或移动，限定在同一个根白名单内。
//
// 目标已存在时拒绝，不覆盖：静默覆盖等于一次没打招呼的删除，
// 而红线对删除的要求是「输入名字确认」。
func (g *Guard) Rename(rootIndex int, rel, dest string) (protocol.FileActionDone, error) {
	src, err := g.Resolve(rootIndex, rel)
	if err != nil {
		return protocol.FileActionDone{}, err
	}
	if src.Rel == "" {
		return protocol.FileActionDone{}, fmt.Errorf("%w: 不允许改名根目录", ErrPathDenied)
	}
	if err := g.ensureNotSymlink(src); err != nil {
		return protocol.FileActionDone{}, err
	}

	dst, err := g.ResolveNew(rootIndex, dest)
	if err != nil {
		return protocol.FileActionDone{}, err
	}
	if dst.Rel == "" {
		return protocol.FileActionDone{}, fmt.Errorf("%w: 目标不能是根目录", ErrExists)
	}
	if err := g.ensureNotSymlink(dst); err != nil {
		return protocol.FileActionDone{}, err
	}
	if src.Abs == dst.Abs {
		return protocol.FileActionDone{}, fmt.Errorf("%w: 目标与源相同: %s", ErrExists, src.Rel)
	}
	switch _, err := os.Lstat(dst.Abs); {
	case err == nil:
		return protocol.FileActionDone{}, fmt.Errorf("%w: %s", ErrExists, dst.Rel)
	case !errors.Is(err, fs.ErrNotExist):
		return protocol.FileActionDone{}, err
	}
	if err := os.Rename(src.Abs, dst.Abs); err != nil {
		return protocol.FileActionDone{}, err
	}
	return protocol.FileActionDone{Action: protocol.ActionRename, Path: src.Rel, Dest: dst.Rel}, nil
}
