// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

// 整库恢复上传端（#93，方案①暂存+重启生效）：
//	POST /api/v1/admin/restore（octet-stream 上传备份库；登录密码走
//	X-DD-Admin-Password 头——不进 URL、不进审计正文）。
//
// 流程：验密 → 落临时文件 → integrity_check + schema_migrations 版本比对 →
// VACUUM INTO 自动备份当前库 → 原子改名落 <dataDir>/restore-staged.db。
// 生效在下次重启：main.go 启动时调 db.ApplyStagedRestore。全程零副作用
// （任一步失败，当前库与运行状态不受影响）。

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/db"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

const (
	// restoreMaxBytes 是备份库上传上限：当前库量级数百 KB，64MiB 留足十年余量。
	restoreMaxBytes = 64 << 20
	// restorePasswordHeader 承载登录密码明文；中间件不记录请求头，审计只记动作。
	restorePasswordHeader = "X-DD-Admin-Password"
)

// serverRestore 实现 POST /api/v1/admin/restore。仅浏览器会话可调。
func (s *server) serverRestore(w http.ResponseWriter, r *http.Request) {
	if s.dataDir == "" {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "服务端未配置数据目录")
		return
	}
	principal, ok := httpx.CurrentUser(r)
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated", "需登录")
		return
	}
	// 双确认第一步：登录密码。跟登录同一把验尺（VerifyPassword），但不做
	// 失败计数——会话本身已过鉴权，这里是防「点着玩」的二次门槛。
	password := r.Header.Get(restorePasswordHeader)
	if password == "" {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "缺少 X-DD-Admin-Password 头（登录密码）")
		return
	}
	u, err := s.st.GetUserByName(r.Context(), principal.Username)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "读取账户失败")
		return
	}
	okp, err := verifyUserPassword(password, u.PwHash)
	if err != nil || !okp {
		httpx.WriteError(w, http.StatusForbidden, protocol.CodeForbidden, "密码不正确")
		return
	}

	// 上传体落临时文件（dataDir/backups/ 内，校验不过即删）。
	r.Body = http.MaxBytesReader(w, r.Body, restoreMaxBytes)
	dir := filepath.Join(s.dataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "创建备份目录失败")
		return
	}
	tmp, err := os.CreateTemp(dir, "restore-upload-*.db")
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "创建临时文件失败")
		return
	}
	tmpPath := tmp.Name()
	defer func() {
		if tmpPath != "" {
			os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, r.Body); err != nil {
		tmp.Close()
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "上传中断或超过 64MiB 上限")
		return
	}
	if err := tmp.Close(); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "落盘失败")
		return
	}

	// 校验 1：SQLite 完整性。
	if err := sqliteIntegrityCheck(tmpPath); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "备份文件校验失败: "+err.Error())
		return
	}
	// 校验 2：schema_migrations 版本不得超过当前代码（新 schema 旧代码读不了）。
	latest, err := db.LatestSchemaVersion()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "读取迁移版本失败")
		return
	}
	if err := checkStagedSchemaVersion(tmpPath, latest); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "备份文件校验失败: "+err.Error())
		return
	}

	// 恢复前自动备份当前库（VACUUM INTO 一致快照，永久保留不删）。
	preName := "pre-restore-" + time.Now().UTC().Format("20060102T150405Z") + ".db"
	prePath := filepath.Join(dir, preName)
	if _, err := s.st.DB.ExecContext(r.Context(), `VACUUM INTO '`+sqlQuote(prePath)+`'`); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "恢复前备份当前库失败")
		return
	}

	// 暂存：原子改名顶替旧的 staged（若有）。
	staged := filepath.Join(s.dataDir, "restore-staged.db")
	if err := os.Rename(tmpPath, staged); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "暂存恢复库失败")
		return
	}
	tmpPath = "" // 已移走，defer 不再删

	log.Printf("hub: 恢复库已暂存（上传 %d 字节，恢复前备份 %s），重启容器生效", sizeOf(staged), preName)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"staged":     true,
		"pre_backup": preName,
		"hint":       "已暂存并完成恢复前自动备份。重启 Hub 容器后生效（docker compose restart hub）。",
	})
}

// sqliteIntegrityCheck 打开上传库跑 integrity_check。
func sqliteIntegrityCheck(path string) error {
	d, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("无法作为 SQLite 打开")
	}
	defer d.Close()
	var result string
	if err := d.QueryRow(`PRAGMA integrity_check`).Scan(&result); err != nil {
		return fmt.Errorf("完整性检查执行失败")
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check 返回 %s", result)
	}
	return nil
}

// checkStagedSchemaVersion 比对备份库的迁移版本与当前代码。
func checkStagedSchemaVersion(path string, latest int) error {
	d, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("无法作为 SQLite 打开")
	}
	defer d.Close()
	var have int
	err = d.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&have)
	if err != nil {
		return fmt.Errorf("缺少 schema_migrations 表（不是 DockDeck 备份？）")
	}
	if have > latest {
		return fmt.Errorf("备份 schema 版本 %d 高于当前支持的 %d（备份来自更新的版本？）", have, latest)
	}
	return nil
}

// verifyUserPassword 复用登录同一把验尺（internal/crypto）。
func verifyUserPassword(password, hash string) (bool, error) {
	return crypto.VerifyPassword(password, hash)
}

func sizeOf(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

