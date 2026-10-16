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
