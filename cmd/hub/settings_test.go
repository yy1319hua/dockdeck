// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S7.8 设置页四枚端点（方案 Q1–Q5 全 A）的后端验证：
// 保护名单读写（含 JSON 新格式落库、坏正则 400、旧逗号兼容、空数组清除）、
// TLS 指纹视图、整库备份下载（快照可再开库 + 审计 + 零残留）、
// 以及「仅会话」鉴权矩阵。
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/guard"
)

// protectedResp 解保护名单响应（信封 data）。
type protectedResp struct {
	Data struct {
		Patterns     []string `json:"patterns"`
		UsingDefault bool     `json:"using_default"`
	} `json:"data"`
}

func getProtected(t *testing.T, h *hub, cookie string) (*http.Response, protectedResp) {
	t.Helper()
	resp, body := h.get(t, "/api/v1/settings/protected-containers", "", cookie)
	var v protectedResp
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatalf("解析保护名单响应: %v｜%s", err, body)
		}
	}
	return resp, v
}

func putProtected(t *testing.T, h *hub, body, cookie string) (*http.Response, []byte) {
	t.Helper()
	return h.do(t, http.MethodPut, "/api/v1/settings/protected-containers", body, "", cookie)
}

// rawSetting 直读 settings 行的底层文本——锁「落库格式」而不只是「读出口径」。
func rawSetting(t *testing.T, h *hub, key string) (string, bool) {
	t.Helper()
	var v string
	err := h.st.DB.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("读 settings: %v", err)
	}
	return v, true
}

// TestProtectedContainersRoundTrip 锁 Q3：PUT→GET 往返，含逗号正则
// （量词 {2,3}）完整保留；底层落库是 JSON 数组文本而非逗号分隔——
// 后者会把 ^db-[0-9]{2,3}$ 切碎成两段。
func TestProtectedContainersRoundTrip(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)

	resp, body := putProtected(t, h, `{"patterns":["^dockdeck-","^db-[0-9]{2,3}$"]}`, cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 应 200，得 %d｜%s", resp.StatusCode, body)
	}
	var put protectedResp
	if err := json.Unmarshal(body, &put); err != nil {
		t.Fatalf("解析 PUT 响应: %v｜%s", err, body)
	}
	if len(put.Data.Patterns) != 2 || put.Data.Patterns[1] != "^db-[0-9]{2,3}$" || put.Data.UsingDefault {
		t.Fatalf("PUT 回显不符：%+v", put.Data)
	}

	_, got := getProtected(t, h, cookie)
	if len(got.Data.Patterns) != 2 || got.Data.Patterns[0] != "^dockdeck-" || got.Data.Patterns[1] != "^db-[0-9]{2,3}$" {
		t.Fatalf("GET 往返不符：%+v", got.Data)
	}
	if got.Data.UsingDefault {
		t.Error("非空名单时 using_default 应为 false")
	}

	raw, ok := rawSetting(t, h, guard.SettingKey)
	if !ok {
		t.Fatal("settings 应已有行")
	}
	if want := `["^dockdeck-","^db-[0-9]{2,3}$"]`; raw != want {
		t.Errorf("底层应为 JSON 数组文本 %s，得 %s", want, raw)
	}

	// 写入口径：trim 首尾空白、丢弃空条目（与 ParseList 去空语义一致）。
	resp, body = putProtected(t, h, `{"patterns":[" ^x- ","","^y-"]}`, cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT 应 200，得 %d｜%s", resp.StatusCode, body)
	}
	if raw, _ := rawSetting(t, h, guard.SettingKey); raw != `["^x-","^y-"]` {
		t.Errorf("trim/去空后落库不符：%s", raw)
	}
}

// TestProtectedContainersBadPattern 锁 Q3 预检：坏正则 400 并指出
// 「第 N 条」，且库无写入（保存前的内容原样不动）。
func TestProtectedContainersBadPattern(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)

	if resp, _ := putProtected(t, h, `{"patterns":["^ok-"]}`, cookie); resp.StatusCode != http.StatusOK {
		t.Fatal("先存一份好名单")
	}

	resp, body := putProtected(t, h, `{"patterns":["^ok-","["]}`, cookie)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("坏正应 400，得 %d｜%s", resp.StatusCode, body)
	}
	if code := errorCode(t, body); code != "bad_pattern" {
		t.Errorf("code 应为 bad_pattern，得 %q", code)
	}
	if !strings.Contains(string(body), "第 2 条") {
		t.Errorf("文案应指出第几条：%s", body)
	}
	// 库无写入：还是原来的好名单。
	if raw, _ := rawSetting(t, h, guard.SettingKey); raw != `["^ok-"]` {
		t.Errorf("坏正则不应落库，得 %s", raw)
	}

	// 请求体不合法（未知字段，DecodeJSON 严格模式）→ 400 bad_request。
	resp, body = putProtected(t, h, `{"patterns":["^a-"],"extra":1}`, cookie)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "bad_request" {
		t.Errorf("未知字段应 400 bad_request，得 %d｜%s", resp.StatusCode, body)
	}
}

// TestProtectedContainersLegacyCommaRead 锁存量兼容：S7.8 前的逗号
// 分隔数据读取时按逗号切，不迁移、不清除。
func TestProtectedContainersLegacyCommaRead(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)
	if err := h.st.SetSetting(context.Background(), guard.SettingKey, "^a-, ^b-"); err != nil {
		t.Fatal(err)
	}
	_, got := getProtected(t, h, cookie)
	if len(got.Data.Patterns) != 2 || got.Data.Patterns[0] != "^a-" || got.Data.Patterns[1] != "^b-" {
		t.Fatalf("旧逗号格式应读成 2 条：%+v", got.Data)
	}
	// 旧数据读后不动：再 PUT 才换成 JSON 数组。
	if raw, _ := rawSetting(t, h, guard.SettingKey); raw != "^a-, ^b-" {
		t.Errorf("读取不应改写存量：%s", raw)
	}
}

// TestProtectedContainersEmptyClears 锁空数组清除：PUT [] → 空名单 +
// using_default=true（容器守卫此时回落默认名单）；"{}"（缺 patterns）
// 同效——PUT 是整名单覆盖语义。
func TestProtectedContainersEmptyClears(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)
	if resp, _ := putProtected(t, h, `{"patterns":["^keep-"]}`, cookie); resp.StatusCode != http.StatusOK {
		t.Fatal("先存一份")
	}
	for _, body := range []string{`{"patterns":[]}`, `{}`} {
		resp, b := putProtected(t, h, body, cookie)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 应 200，得 %d｜%s", body, resp.StatusCode, b)
		}
		_, got := getProtected(t, h, cookie)
		if len(got.Data.Patterns) != 0 || !got.Data.UsingDefault {
			t.Fatalf("%s 后应为空名单 + 默认提示：%+v", body, got.Data)
		}
		if raw, _ := rawSetting(t, h, guard.SettingKey); raw != "[]" {
			t.Errorf("空名单应落库 []，得 %s", raw)
		}
	}
}

// TestTLSFingerprintView 锁 Q5：自签模式回指纹 + self_signed=true；
// 其余模式空串 + false（UI 明说「未使用内置自签证书」）。
func TestTLSFingerprintView(t *testing.T) {
	h := newHub(t)
	cookie := h.session(t)

	resp, body := h.get(t, "/api/v1/settings/tls-fingerprint", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, body)
	}
	var v struct {
		Data struct {
			Fingerprint string `json:"fingerprint"`
			SelfSigned  bool   `json:"self_signed"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if v.Data.Fingerprint != "" || v.Data.SelfSigned {
		t.Errorf("未配指纹时应为空串 + false：%+v", v.Data)
	}

	h.srv.tlsFingerprint = "AB:CD:EF:01"
	resp, body = h.get(t, "/api/v1/settings/tls-fingerprint", "", cookie)
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if v.Data.Fingerprint != "AB:CD:EF:01" || !v.Data.SelfSigned {
		t.Errorf("配了指纹应回显 + true：%+v", v.Data)
	}
}

// TestBackupDownload 锁 Q4：快照 200 + 下载响应头 + 内容是可再开库的
// SQLite 文件（魔数与实查双重验证）+ 审计落 admin.backup + 中转目录
// 零残留。
func TestBackupDownload(t *testing.T) {
	h := newHub(t)
	h.srv.dataDir = t.TempDir() // 备份中转目录落这里
	cookie := h.session(t)
	before := len(auditRows(t, h))

	resp, body := h.get(t, "/api/v1/admin/backup", "", cookie)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，得 %d｜%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.sqlite3" {
		t.Errorf("Content-Type 应为 application/vnd.sqlite3，得 %q", ct)
	}
	cd := resp.Header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment; filename=\"dockdeck-") || !strings.HasSuffix(cd, ".db\"") {
		t.Errorf("Content-Disposition 形态不对：%q", cd)
	}
	if cl := resp.Header.Get("Content-Length"); cl != fmt.Sprintf("%d", len(body)) {
		t.Errorf("Content-Length %s 与实际 %d 不符", cl, len(body))
	}
	if len(body) < 16 || string(body[:16]) != "SQLite format 3\x00" {
		t.Fatal("响应体不是 SQLite 文件（魔数不符）")
	}

	// 快照能再开库、且内容与源库一致（admin 用户在册）。
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := os.WriteFile(snap, body, 0o600); err != nil {
		t.Fatal(err)
	}
	d2, err := sql.Open("sqlite", snap)
	if err != nil {
		t.Fatalf("快照开库失败: %v", err)
	}
	defer d2.Close()
	var users int
	if err := d2.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("快照查 users 失败: %v", err)
	}
	if users != 1 {
		t.Errorf("快照里应有 1 个用户，得 %d", users)
	}

	// 审计：恰多 1 行 admin.backup（ok）。
	rows := auditRows(t, h)
	if len(rows) != before+1 {
		t.Fatalf("备份应落 1 条审计：前 %d，后 %d", before, len(rows))
	}
	if rows[0].Action != "admin.backup" || rows[0].Result != "ok" {
		t.Errorf("审计行不符：%+v", rows[0])
	}

	// 中转目录零残留（defer os.Remove 生效）。
	ents, err := os.ReadDir(filepath.Join(h.srv.dataDir, "backups"))
	if err != nil {
		t.Fatalf("读 backups 目录: %v", err)
	}
	if len(ents) != 0 {
		t.Errorf("中转文件应已删除，剩 %d 个", len(ents))
	}
}

// TestSettingsEndpointsAuth 锁「仅会话」：未认证 401；全权限 Token 也
// 401（权限大小与身份类型是两回事）；会话 200。
func TestSettingsEndpointsAuth(t *testing.T) {
	h := newHub(t)
	h.srv.dataDir = t.TempDir() // backup 走会话时会真进 handler，得有中转目录
	cookie := h.session(t)
	omni := h.token(t, "omni", apitoken.AllScopes...)

	gets := []string{
		"/api/v1/settings/protected-containers",
		"/api/v1/settings/tls-fingerprint",
		"/api/v1/admin/backup",
	}
	for _, p := range gets {
		if resp, _ := h.get(t, p, "", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s 未认证应 401，得 %d", p, resp.StatusCode)
		}
		if resp, _ := h.get(t, p, omni, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s 带全权限 Token 应 401，得 %d", p, resp.StatusCode)
		}
		if resp, _ := h.get(t, p, "", cookie); resp.StatusCode != http.StatusOK {
			t.Errorf("%s 会话应 200，得 %d", p, resp.StatusCode)
		}
	}
	if resp, _ := putProtected(t, h, `{"patterns":["^a-"]}`, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("PUT 未认证应 401，得 %d", resp.StatusCode)
	}
	if resp, _ := putProtected(t, h, `{"patterns":["^a-"]}`, omni); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("PUT 带 Token 应 401，得 %d", resp.StatusCode)
	}
	if resp, _ := putProtected(t, h, `{"patterns":["^a-"]}`, cookie); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT 会话应 200，得 %d", resp.StatusCode)
	}
}
