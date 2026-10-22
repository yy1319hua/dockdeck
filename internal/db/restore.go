// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 整库恢复（#93，方案①暂存+重启生效）：
//   - 上传端（cmd/hub/restore.go）负责校验（integrity_check / 迁移版本可比对）与
//     自动备份当前库，然后把合格备份落 <dataDir>/restore-staged.db；
//   - 本文件只做「重启生效」这一步：Hub 启动打开主库之前，若发现 staged 文件，
//     先把当前库挪进 backups/（带时间戳，不覆盖历史），再把 staged 换正。
//
// 为什么不在运行中在线换库：SQLite 连接池持有主库句柄，在线替换有并发窗口，
// 而重启容器只是一条 compose restart——稳比炫重要。

package db

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ApplyStagedRestore 检查并应用暂存恢复库。返回是否发生了替换。
//
// dbPath 是运行主库路径（如 /data/dockdeck.db）。staged 与其同目录的
// restore-staged.db；存在即视为一次待生效恢复。当前主库（连同 -wal/-shm
// 附属文件）整体挪进同目录 backups/pre-restore-boot-<UTC 时间戳>/，绝不
// 覆盖已有目录；staged 原子改名顶替主库。
//
// 只动文件、不打开库——迁移仍由随后的 Open 正常向前执行。
func ApplyStagedRestore(dbPath string) (bool, error) {
	dir := filepath.Dir(dbPath)
	staged := filepath.Join(dir, "restore-staged.db")
	if _, err := os.Stat(staged); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("db: 检查暂存恢复文件失败: %w", err)
	}

	backupDir := filepath.Join(dir, "backups", "pre-restore-boot-"+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return false, fmt.Errorf("db: 建恢复前备份目录失败: %w", err)
	}
	// 当前主库与 WAL 附属件整体进备份目录；主库可能不存在（首启即恢复）。
	if _, err := os.Stat(dbPath); err == nil {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			src := dbPath + suffix
			if _, err := os.Stat(src); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return false, fmt.Errorf("db: 检查主库附属文件失败: %w", err)
			}
			if err := os.Rename(src, filepath.Join(backupDir, filepath.Base(dbPath)+suffix)); err != nil {
				return false, fmt.Errorf("db: 备份当前主库失败: %w", err)
			}
		}
	}
	// WAL 附属件必须清掉：旧库的 -wal 若与新库混挂，帧回放会把旧数据带回来。
	// （上一段已把当前库的 -wal/-shm 挪走，这里只需换主文件。）
	if err := os.Rename(staged, dbPath); err != nil {
		return false, fmt.Errorf("db: 暂存库顶替主库失败: %w", err)
	}
	return true, nil
}

// LatestSchemaVersion 返回内嵌迁移的最高版本号。上传端用它拒绝
// 「来自更新版本 Hub」的备份（新 schema 旧代码不认识，向前迁移救不了）。
func LatestSchemaVersion() (int, error) {
	ms, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	max := 0
	for _, m := range ms {
		if m.version > max {
			max = m.version
		}
	}
	return max, nil
}
