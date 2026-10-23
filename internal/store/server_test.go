// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/db"
)

// newServerStore 建临时库 + 确定性主密钥，返回 Store。
func newServerStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return &Store{DB: d, Key: key}
}

// seedServer 插一台服务器，token 用主密钥真加密——ServerConn 要验解密往返，
// 占位符密文（如 "x"）在这个文件里不够用。
func seedServer(t *testing.T, s *Store, name, token string, enabled bool) int64 {
	t.Helper()
	ct, err := crypto.SealSecret(s.Key, []byte(token))
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	on := 0
	if enabled {
		on = 1
	}
	res, err := s.DB.Exec(
		`INSERT INTO servers(name,base_url,token_cipher,enabled) VALUES(?,?,?,?)`,
		name, "https://192.0.2.1:9527", ct, on,
	)
	if err != nil {
		t.Fatalf("insert server: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId: %v", err)
	}
	return id
}

func TestServerConn_RoundTrip(t *testing.T) {
	s := newServerStore(t)
	id := seedServer(t, s, "srv-a", "agent-token-xyz", true)

	c, err := s.ServerConn(context.Background(), id)
	if err != nil {
		t.Fatalf("ServerConn: %v", err)
	}
	if c.ID != id || c.Name != "srv-a" {
		t.Errorf("id/name = %d/%q，期望 %d/%q", c.ID, c.Name, id, "srv-a")
	}
	if c.BaseURL != "https://192.0.2.1:9527" {
		t.Errorf("base_url = %q", c.BaseURL)
	}
	if c.Token != "agent-token-xyz" {
		t.Errorf("token 解密结果 = %q，期望 %q", c.Token, "agent-token-xyz")
	}
}

func TestServerConn_NotFound(t *testing.T) {
	s := newServerStore(t)
	for _, id := range []int64{0, -1, 999} {
		if _, err := s.ServerConn(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Errorf("id=%d：err = %v，期望 ErrNotFound", id, err)
		}
	}
}

func TestServerConn_Disabled(t *testing.T) {
	s := newServerStore(t)
	id := seedServer(t, s, "srv-off", "tok", false)
	if _, err := s.ServerConn(context.Background(), id); !errors.Is(err, ErrServerDisabled) {
		t.Fatalf("err = %v，期望 ErrServerDisabled", err)
	}
}

func TestServerConn_CorruptTokenCipher(t *testing.T) {
	s := newServerStore(t)
	// 直接塞非密文：解密必须报错，不得静默放行（会把认证失败伪装成别的病）。
	if _, err := s.DB.Exec(
		`INSERT INTO servers(name,base_url,token_cipher,enabled) VALUES("bad","https://x:9527","不是密文",1)`,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var id int64
	if err := s.DB.QueryRow(`SELECT id FROM servers WHERE name="bad"`).Scan(&id); err != nil {
		t.Fatalf("select: %v", err)
	}
	if _, err := s.ServerConn(context.Background(), id); err == nil {
		t.Fatal("损坏密文应报错，不应静默放行")
	}
}

func TestListServers_EmptyReturnsSlice(t *testing.T) {
	s := newServerStore(t)
	list, err := s.ListServers(context.Background())
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if list == nil {
		t.Fatal("空清单应返回空切片而非 nil")
	}
	if len(list) != 0 {
		t.Errorf("len = %d，期望 0", len(list))
	}
}

func TestListServers_ReturnsAllIncludingDisabled(t *testing.T) {
	s := newServerStore(t)
	a := seedServer(t, s, "srv-a", "agent-tok-a", true)
	b := seedServer(t, s, "srv-b", "agent-tok-b", false)

	list, err := s.ListServers(context.Background())
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d，期望 2", len(list))
	}
	byID := map[int64]ServerSummary{}
	for _, ss := range list {
		byID[ss.ID] = ss
	}
	if !byID[a].Enabled {
		t.Errorf("srv-a 应为启用")
	}
	if byID[b].Enabled {
		t.Errorf("srv-b 应为禁用")
	}
	// 存量行（seedServer 未写 type）必须落在默认值 agent 上，
	// 否则迁移会把历史远端 Agent 误标成本机。
	if byID[a].Type != ServerTypeAgent {
		t.Errorf("srv-a 类型 = %q，期望默认 %q", byID[a].Type, ServerTypeAgent)
	}
	// 列表不得携带秘密：ServerSummary 类型本身不含 Token，
	// 这里额外断言序列化结果不含明文 Agent 令牌。
	buf := new(bytes.Buffer)
	if err := json.NewEncoder(buf).Encode(list); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(buf.String(), "agent-tok-a") || strings.Contains(buf.String(), "agent-tok-b") {
		t.Errorf("列表泄露了 Agent 令牌：%s", buf.String())
	}
}

// TestListServers_TypeLocal 验证 type 列可写入并读出 local：
// Hub 本机由启动时幂等注册（固定 server_id），列表必须能把它与远端 Agent
// 区分开，前端据此渲染「本机」徽标并禁止删除。
func TestListServers_TypeLocal(t *testing.T) {
	s := newServerStore(t)
	id := seedServer(t, s, "hub-local", "local-tok", true)
	if _, err := s.DB.Exec(`UPDATE servers SET type=? WHERE id=?`, ServerTypeLocal, id); err != nil {
		t.Fatalf("update type: %v", err)
	}
	list, err := s.ListServers(context.Background())
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d，期望 1", len(list))
	}
	if list[0].Type != ServerTypeLocal {
		t.Errorf("类型 = %q，期望 %q", list[0].Type, ServerTypeLocal)
	}
}

// TestDeleteServer 覆盖物理删除的级联语义、旧邀请失效与「删后同名可重新纳管」。
func TestDeleteServer(t *testing.T) {
	ctx := context.Background()
	st := newServerStore(t)

	// 经真实 invite→enroll 建一台。
	plain, err := st.CreateInvite(ctx, "doomed", time.Minute)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	id, err := st.EnrollServer(ctx, plain, "doomed", "reverse://doomed", "amd64", "29.1.3")
	if err != nil {
		t.Fatalf("EnrollServer: %v", err)
	}

	// 塞 metrics 历史（两表各一行），验证级联清空。
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO metrics_snapshots(server_id, ts) VALUES(?,?)`,
		id, "2026-10-20T00:00:00Z"); err != nil {
		t.Fatalf("seed metrics_snapshots: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO metrics_latest(server_id, ts, metrics) VALUES(?,?,?)`,
		id, "2026-10-20T00:00:00Z", "{}"); err != nil {
		t.Fatalf("seed metrics_latest: %v", err)
	}

	// 删除 → 返回名字、行消失、metrics 两表清空。
	name, err := st.DeleteServer(ctx, id)
	if err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	if name != "doomed" {
		t.Fatalf("DeleteServer 返回名 %q，want doomed", name)
	}
	if ok, _ := st.ServerExists(ctx, id); ok {
		t.Fatal("删除后服务器不应还存在")
	}
	for _, tbl := range []string{"metrics_snapshots", "metrics_latest"} {
		var cnt int
		if err := st.DB.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM `+tbl+` WHERE server_id=?`, id).Scan(&cnt); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if cnt != 0 {
			t.Fatalf("%s 应级联清空，剩 %d 行", tbl, cnt)
		}
	}

	// 旧 secret 重放 → 邀请已不可用（server_id 置空后的 410 分支）。
	if _, err := st.EnrollServer(ctx, plain, "doomed", "reverse://doomed", "", ""); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("旧 secret 重放应 ErrInviteExpired，得 %v", err)
	}

	// 名字已释放：同名新邀请可立即 enroll（解 409 撞名卡死）。
	plain2, err := st.CreateInvite(ctx, "doomed-again", time.Minute)
	if err != nil {
		t.Fatalf("CreateInvite #2: %v", err)
	}
	if _, err := st.EnrollServer(ctx, plain2, "doomed", "reverse://doomed", "", ""); err != nil {
		t.Fatalf("删后同名应可重新 enroll: %v", err)
	}

	// 删不存在的 → ErrNotFound（handler 映射 404）。
	if _, err := st.DeleteServer(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删不存在的应 ErrNotFound，得 %v", err)
	}
}

// TestServerRename 回归 #86：生效/trim/空名/超长/重名/不存在。
func TestServerRename(t *testing.T) {
	s := newServerStore(t)
	idA := seedServer(t, s, "srv-a", "tok-a", true)
	_ = seedServer(t, s, "srv-b", "tok-b", true)

	// 正常改名 + 首尾空白裁剪。
	got, err := s.RenameServer(context.Background(), idA, "  新名字  ")
	if err != nil || got != "新名字" {
		t.Fatalf("RenameServer = %q, %v", got, err)
	}
	if c, err := s.ServerConn(context.Background(), idA); err != nil || c.Name != "新名字" {
		t.Errorf("改名后读回 = %q, %v", c.Name, err)
	}

	// 空名 / 纯空白 / 超长。
	if _, err := s.RenameServer(context.Background(), idA, "   "); err == nil {
		t.Error("空名应报错")
	}
	if _, err := s.RenameServer(context.Background(), idA, strings.Repeat("长", 65)); err == nil {
		t.Error("超长应报错")
	}

	// 重名 → ErrServerNameTaken。
	if _, err := s.RenameServer(context.Background(), idA, "srv-b"); !errors.Is(err, ErrServerNameTaken) {
		t.Errorf("重名应 ErrServerNameTaken, 得 %v", err)
	}

	// 不存在 → ErrNotFound。
	if _, err := s.RenameServer(context.Background(), 99999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("不存在应 ErrNotFound, 得 %v", err)
	}

	// 改名不改连接 token（反向链路凭据不受影响）。
	if c, err := s.ServerConn(context.Background(), idA); err != nil || c.Token != "tok-a" {
		t.Errorf("token 应不变, 得 %q, %v", c.Token, err)
	}
}
