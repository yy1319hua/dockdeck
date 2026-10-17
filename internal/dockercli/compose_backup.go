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

// backupTimeLayout 是备份文件名里的时间戳格式：定宽 9 位奈秒（不足补零）。
//
// 为什么不用 time.RFC3339（秒级）：同一秒内两次写会算出同一个备份名，后一次
// 覆盖前一份，用户静默丢一个备份（粗时钟主机上奈秒也可能撞，由 writeBackupExclusive
// 的 O_EXCL 兜底）。为什么定宽而不用 RFC3339Nano：后者小数位变长（末尾零被
// 抹），会打破 backupNames「文件名倒字典序＝时间倒序」的假设——那里靠定长
// 才不必解析时间，也就不会被解析失败搅乱顺序。
const backupTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// maxComposeBackups 是每个 compose 文件保留的写前备份份数上限，超出删最旧的。
//
// 轮转＝自动删旧备份，与 S7.11 Q3-A「不提供删除」有张力，故取一个远超任何
// 回滚场景需要的数：正常回滚只看最近一两份，要连写满 20 份才会碰到第一个
// 被轮转掉的。删除只针对「超出上限的旧备份」，最新的 maxComposeBackups 份
// 永不受影响；宿主机 CLI 手动清理的通道不变（Q3-A 的手工口径仍在）。
const maxComposeBackups = 20

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

// backupNames 返回 file 的写前备份文件名，按时间倒序（最新在前）。
//
// 倒序即文件名倒字典序：备份名后缀是定宽时间戳（backupTimeLayout），同一时区
// 偏移下定长，字典序＝时间序——不必解析时间，也就不会被解析失败搅乱顺序。
// ListBackups 与 rotateComposeBackups 共用本函数，保证「谁算最新」两处一致，
// 不会出现「清单说这份最新、轮转却把它删了」的分裂。
//
// 向后兼容：S7.11 之前生成的旧格式备份（秒级 RFC3339、无小数位）与新的定宽
// 奈秒格式混存时，秒级字段先分出胜负；仅整秒相同时才轮到 "."(0x2e) 与
// "+"(0x2b) 比——而那种情况新格式（更精确的那一次写）本就该更新→倒序在前，
// 方向正确。故混存不会乱序。
func backupNames(file string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Dir(file))
	if err != nil {
		return nil, fmt.Errorf("dockercli: 列举备份目录失败: %w", err)
	}
	prefix := backupPrefix(file)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// ListBackups 列出 compose 文件的写前备份，按时间倒序（最新在前）。
func (c *ComposeClient) ListBackups(ctx context.Context, file string) ([]protocol.ComposeBackup, error) {
	names, err := backupNames(file)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(file)
	out := []protocol.ComposeBackup{}
	for _, name := range names {
		// 列举与 stat 之间文件可能被删，单条失败就少列一条，不拖垮整张清单。
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		out = append(out, protocol.ComposeBackup{
			Name:    name,
			Size:    info.Size(),
			ModTime: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// rotateComposeBackups 轮转 file 的写前备份：保留最近 maxComposeBackups 份，
// 删除更旧的。**尽力而为**——调用点（WriteFile）此时已写入成功，不该因一条
// 旧备份删不掉（占用/权限）就让整次写入报错，故忽略删除错误，下次写再试。
//
// 只删普通文件：跳过目录，也跳过软链——软链用 os.Remove 摘的是链本身（不会
// 伤及指向目标）本就安全，但保守起见轮转不碰非普通文件，与 resolveBackup
// 「备份须是落目录内的真文件」的口径一致。
func rotateComposeBackups(file string) {
	names, err := backupNames(file)
	if err != nil || len(names) <= maxComposeBackups {
		return
	}
	dir := filepath.Dir(file)
	for _, name := range names[maxComposeBackups:] {
		p := filepath.Join(dir, name)
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		_ = os.Remove(p)
	}
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
