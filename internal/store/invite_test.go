// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/db"
)

func newInviteStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return &Store{DB: d, Key: bytes.Repeat([]byte("K"), 32)}
}

func TestEnrollServerLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newInviteStore(t)

	if ok, _ := st.KnownJoinSecret(ctx, "never-issued"); ok {
		t.Fatal("未签发的 secret 不应被认识")
	}

	plain, err := st.CreateInvite(ctx, "web-01", time.Minute)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if ok, _ := st.KnownJoinSecret(ctx, plain); !ok {
		t.Fatal("签发的 secret 应被认识")
	}

	// 明文绝不落库：以哈希存，直接查表不应含明文。
	var cnt int
	_ = st.DB.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM server_invites WHERE join_hash=? OR name_hint LIKE ?`,
		crypto.HashToken(plain), "%"+plain+"%").Scan(&cnt)
	if cnt == 0 {
		t.Fatal("邀请行应按哈希存在")
	}

	id, err := st.EnrollServer(ctx, plain, "web-01", "https://10.0.0.5:9527", "amd64", "29.1.3")
	if err != nil {
		t.Fatalf("EnrollServer: %v", err)
	}
	conn, err := st.ServerConn(ctx, id)
	if err != nil {
		t.Fatalf("ServerConn: %v", err)
	}
	if conn.Token != plain {
		t.Fatalf("解密出的 Agent 令牌应等于 join_secret，得 %q", conn.Token)
	}
	if conn.BaseURL != "https://10.0.0.5:9527" {
		t.Fatalf("base_url 不对：%q", conn.BaseURL)
	}
	var arch, dv string
	if err := st.DB.QueryRowContext(ctx,
		`SELECT arch, docker_ver FROM servers WHERE id=?`, id).Scan(&arch, &dv); err != nil {
		t.Fatalf("查 arch/docker_ver: %v", err)
	}
	if arch != "amd64" || dv != "29.1.3" {
		t.Fatalf("arch/docker_ver 应随 enroll 落库，得 %q / %q", arch, dv)
	}

	// 幂等重放：同 secret 再 enroll，返回同 id 且刷新 base_url。
	id2, err := st.EnrollServer(ctx, plain, "web-01", "https://10.0.0.9:9527", "arm64", "30.0.0")
	if err != nil {
		t.Fatalf("重放 EnrollServer: %v", err)
	}
	if id2 != id {
		t.Fatalf("重放应返回同 id，得 %d vs %d", id2, id)
	}
	if conn2, _ := st.ServerConn(ctx, id); conn2.BaseURL != "https://10.0.0.9:9527" {
		t.Fatalf("重放应刷新 base_url，得 %q", conn2.BaseURL)
	}
	if err := st.DB.QueryRowContext(ctx,
		`SELECT arch, docker_ver FROM servers WHERE id=?`, id).Scan(&arch, &dv); err != nil {
		t.Fatalf("查 arch/docker_ver: %v", err)
	}
	if arch != "arm64" || dv != "30.0.0" {
		t.Fatalf("重放应刷新 arch/docker_ver，得 %q / %q", arch, dv)
	}

	if _, err := st.EnrollServer(ctx, "totally-unknown", "x", "https://x:1", "", ""); err != ErrInviteNotFound {
		t.Fatalf("未知 secret 应 ErrInviteNotFound，得 %v", err)
	}
}

func TestEnrollServerNameTaken(t *testing.T) {
	ctx := context.Background()
	st := newInviteStore(t)
	a, _ := st.CreateInvite(ctx, "", time.Minute)
	b, _ := st.CreateInvite(ctx, "", time.Minute)
	if _, err := st.EnrollServer(ctx, a, "dup", "https://a:1", "", ""); err != nil {
		t.Fatalf("首次 enroll: %v", err)
	}
	if _, err := st.EnrollServer(ctx, b, "dup", "https://b:1", "", ""); err != ErrServerNameTaken {
		t.Fatalf("同名应 ErrServerNameTaken，得 %v", err)
	}
}

func TestEnrollServerExpired(t *testing.T) {
	ctx := context.Background()
	st := newInviteStore(t)
	plain := "expired-secret-value"
	exp := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := st.DB.ExecContext(ctx,
		`INSERT INTO server_invites(join_hash, expires_at) VALUES(?,?)`,
		crypto.HashToken(plain), exp); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.EnrollServer(ctx, plain, "e", "https://e:1", "", ""); err != ErrInviteExpired {
		t.Fatalf("过期邀请应 ErrInviteExpired，得 %v", err)
	}
}
