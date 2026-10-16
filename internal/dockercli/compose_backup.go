// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// 本文件是 Compose 写前备份的列举 / 读取 / 恢复（S7.11）。
//
// 备份是 WriteFile 在覆盖写之前生成的 <compose 文件>.bak.<RFC3339>，与 compose
// 文件同目录。此前它只有「写的时候顺手产生、路径在 toast 里闪一下」的可见性。

// backupPrefix 是备份文件名的前缀：<compose 文件名>.bak.
func backupPrefix(file string) string {
	return filepath.Base(file) + ".bak."
}

// resolveBackup 把备份文件名解析成绝对路径，并过三道关卡（Q5-A）：
//  1. 名字必须以 <compose 文件名>.bak. 开头——不是这个文件的备份就不认；
//  2. 名字不得含路径分隔符——只接受当前目录下的文件名；
//  3. realpath 必须落在 compose 文件所在目录内——挡住软链指向别处：
//     备份是文件副本，若被换成指向别处的软链，就能借「读备份 / 恢复」
//     读到或写进白名单之外的任意文件。
func resolveBackup(file, name string) (string, error) {
	prefix := backupPrefix(file)
	if name == "" || !strings.HasPrefix(name, prefix) {
		return "", fmt.Errorf("dockercli: 非法备份名 %q（须以 %s 开头）", name, prefix)
	}
	if strings.ContainsAny(name, `/\`) || name != filepath.Base(name) {
		return "", fmt.Errorf("dockercli: 非法备份名 %q（不得含路径分隔符）", name)
	}
	dir := filepath.Dir(file)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("dockercli: 解析 compose 目录失败: %w", err)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(dir, name))
	if err != nil {
		return "", fmt.Errorf("dockercli: 备份不存在或不可读: %w", err)
	}
	if real != filepath.Join(realDir, name) {
		return "", fmt.Errorf("dockercli: 备份路径越界: %q", name)
	}
	return real, nil
}

// ListBackups 列出 compose 文件的写前备份，按时间倒序（最新在前）。
//
// 排序直接用文件名字典序倒序：后缀是 RFC3339，同一时区偏移下定长，字典序
// 即时间序——不必解析时间，也就不会被解析失败搅乱顺序。
func (c *ComposeClient) ListBackups(ctx context.Context, file string) ([]protocol.ComposeBackup, error) {
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil {
		return nil, fmt.Errorf("dockercli: 列举备份目录失败: %w", err)
	}
	prefix := backupPrefix(file)
	out := []protocol.ComposeBackup{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			// 单条 stat 失败不拖垮整张清单（删掉就删掉，少列一条比整块报错好）。
			continue
		}
		out = append(out, protocol.ComposeBackup{
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// ReadBackup 读取备份正文（预览与 diff 用）。名字先过 resolveBackup 三道关卡。
func (c *ComposeClient) ReadBackup(ctx context.Context, file, name string) (string, error) {
	abs, err := resolveBackup(file, name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("dockercli: 读取备份失败: %w", err)
	}
	return string(data), nil
}

// RestoreBackup 用备份内容覆盖当前 compose 文件，返回恢复前新建的备份文件名。
//
// 恢复**本身就是一次覆盖写**，故直接走 WriteFile——它会先把「恢复前的当前
// 内容」备份成新的 .bak.<RFC3339> 再覆盖。不做这一步，回滚就是单向门：
// 恢复到旧版本之后，就再也回不到恢复前的状态了（Q2-A）。
func (c *ComposeClient) RestoreBackup(ctx context.Context, file, name string) (string, error) {
	content, err := c.ReadBackup(ctx, file, name)
	if err != nil {
		return "", err
	}
	backup, err := c.WriteFile(ctx, file, content)
	if err != nil {
		return "", err
	}
	// 只回文件名：新端点一律不回宿主机绝对路径（Q4-A）。
	return filepath.Base(backup), nil
}
