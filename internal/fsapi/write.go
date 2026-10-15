// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package fsapi

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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

// backup 把当前内容另存为 <abs>.bak.<RFC3339>，返回备份的绝对路径。
//
// 同一秒内的两次写入会撞名，此时追加 .1/.2…：备份的价值是「还能回去」，
// 覆盖掉上一份备份就失去意义了。
func (g *Guard) backup(abs string) (string, error) {
	src, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer src.Close()

	base := abs + ".bak." + time.Now().Format(time.RFC3339)
	name := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			break
		} else if err != nil {
			return "", err
		}
		name = fmt.Sprintf("%s.%d", base, i)
		if i > 1000 {
			return "", fmt.Errorf("fsapi: 备份文件名重名过多: %s", base)
		}
	}

	dst, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
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
