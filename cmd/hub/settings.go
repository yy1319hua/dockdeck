// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// settings.go：S7.8 设置页的四枚端点（方案 Q1–Q5 全 A）。
//
//   - GET/PUT /api/v1/settings/protected-containers  保护名单读写；
//   - GET     /api/v1/settings/tls-fingerprint       TLS 证书指纹；
//   - GET     /api/v1/admin/backup                   整库快照下载。
//
// 四枚一律 route.AuthSession：泄漏的 API Token 不得改安全配置、不得拖库
// （与 Token 管理同口径，见 internal/apitoken 包注释）。PUT 与 backup 包
// middleware.Audit——改安全配置与拖库都必须是审计表里的一等公民；两枚
// GET 不包：读配置与读指纹没有副作用，留痕只会淹掉真操作记录（同 S7.7
// 审计查询的口径，方案 Q4）。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yy1319hua/dockdeck/internal/guard"
	"github.com/yy1319hua/dockdeck/internal/httpx"
	"github.com/yy1319hua/dockdeck/internal/protocol"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// ---- 保护名单 ----

type protectedView struct {
	Patterns     []string `json:"patterns"`
	UsingDefault bool     `json:"using_default"`
}

type protectedReq struct {
	Patterns []string `json:"patterns"`
}

// getProtectedContainers 读保护名单。settings 行不存在（从未写过）或
// 空数组时名单为空、using_default=true——容器守卫 containerGuard 此时
// 回落 guard.DefaultPatterns（^dockdeck-），UI 据此提示「当前为默认
// 名单」，而不是把「空」误解成「什么都不保护」。
func (s *server) getProtectedContainers(w http.ResponseWriter, r *http.Request) {
	raw, err := s.st.GetSetting(r.Context(), guard.SettingKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "读取保护名单失败")
		return
	}
	list := guard.ParseList(raw)
	if list == nil {
		list = []string{} // JSON 侧给 [] 而不是 null，前端少一次判空
	}
	httpx.WriteJSON(w, http.StatusOK, protectedView{
		Patterns:     list,
		UsingDefault: len(list) == 0,
	})
}

// putProtectedContainers 整名单覆盖写。逐条 regexp.Compile 预检：坏正则
// 400 并指出「第 N 条」——名单是安全边界，坏正则宁可拒绝请求也不落库
// （与 guard.New 启动即拒同口径，只是这里拒的是单次请求）。
//
// 存 JSON 数组（S7.8 起的新格式）：正则含逗号（量词 {2,3}、字符类
// [a-z,0-9]）不被切碎——旧逗号分隔格式的切碎正是本轮换格式的原因。
// 空数组即清除：与 containerGuard 的口径一致，回落默认名单。
func (s *server) putProtectedContainers(w http.ResponseWriter, r *http.Request) {
	var body protectedReq
	if err := httpx.DecodeJSON(w, r, &body); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, protocol.CodeBadRequest, "请求体解析失败")
		return
	}
	kept := make([]string, 0, len(body.Patterns))
	for i, p := range body.Patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue // 空条目静默丢弃，与 ParseList 的去空语义一致
		}
		if _, err := regexp.Compile(p); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "bad_pattern",
				fmt.Sprintf("第 %d 条不是合法的正则：%v", i+1, err))
			return
		}
		kept = append(kept, p)
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "序列化保护名单失败")
		return
	}
	if err := s.st.SetSetting(r.Context(), guard.SettingKey, string(raw)); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "保存保护名单失败")
		return
	}
	// 回显新状态（200）：前端保存后直接拿到服务端确认过的名单，
	// 不必再发一次 GET。坏正则在写库前就 400 了，库无写入。
	s.getProtectedContainers(w, r)
}

// ---- TLS 指纹 ----

type tlsFingerprintView struct {
	Fingerprint string `json:"fingerprint"`
	SelfSigned  bool   `json:"self_signed"`
}

// getTLSFingerprint 回当前证书指纹（hex SHA-256，冒号分隔），供浏览器
// 证书固定（pinning）时人工比对。非自签模式（none / 外部终止 TLS）下
// 指纹为空串、self_signed=false——UI 明说「未使用内置自签证书」，
// 别拿空串让用户对空气比对。
//
// 刻意不展示过期时间：指纹是「当下这一枚」的标识，过期归运维在宿主机
// 侧处理（方案 Q5）。
func (s *server) getTLSFingerprint(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, tlsFingerprintView{
		Fingerprint: s.tlsFingerprint,
		SelfSigned:  s.tlsFingerprint != "",
	})
}

// ---- 整库备份 ----

// backupDB 整库快照下载：VACUUM INTO 临时文件再流回。
//
// 为什么不是直接读库文件：运行中的 SQLite 即便在 WAL 模式下直接 cp 也
// 可能漏掉未 checkpoint 的帧；VACUUM INTO 由 SQLite 自身保证导出一致
// 的完整快照。临时文件落在 <dataDir>/backups/，用完即删——目录只作
// 中转，不积累历史（历史备份归运维侧的离线策略，见 ARCHITECTURE
// 「备份与恢复」）。
//
// 响应是裸流（NoEnvelope）：快照可能几十上百 MiB，套 {data,error}
// 信封既别扭又破坏边下边存的流式语义。
func (s *server) backupDB(w http.ResponseWriter, r *http.Request) {
	if s.dataDir == "" {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "服务端未配置数据目录")
		return
	}
	dir := filepath.Join(s.dataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "创建备份目录失败")
		return
	}
	// UTC 秒级时间戳命名：同一秒内的重复下载互相覆盖（本就是中转文件），
	// 跨秒不重名。正则干净，可直接进 Content-Disposition。
	name := "dockdeck-" + time.Now().UTC().Format("20060102T150405Z") + ".db"
	tmp := filepath.Join(dir, name)
	defer os.Remove(tmp) // 中转文件绝不落地留存

	if _, err := s.st.DB.ExecContext(r.Context(), `VACUUM INTO '`+sqlQuote(tmp)+`'`); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "生成数据库快照失败")
		return
	}
	f, err := os.Open(tmp)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, protocol.CodeInternal, "读取快照失败")
		return
	}
	defer f.Close()

	h := w.Header()
	h.Set("Content-Type", "application/vnd.sqlite3")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if fi, err := f.Stat(); err == nil {
		h.Set("Content-Length", fmt.Sprintf("%d", fi.Size()))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// sqlQuote 把路径拼进 SQL 字符串字面量：单引号翻倍。VACUUM INTO 的
// 目标路径必须是 SQL 字面量（该语句没有参数绑定位），单引号翻倍是
// SQLite 文档给出的字符串字面量转义规则。
func sqlQuote(s string) string { return strings.ReplaceAll(s, "'", "''") }
