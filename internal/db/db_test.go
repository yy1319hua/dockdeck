package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestOpen_Migrates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// schema_migrations 应记录已应用的迁移。
	var ver int
	if err := d.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&ver); err != nil {
		t.Fatalf("读版本: %v", err)
	}
	if ver < 1 {
		t.Fatalf("迁移版本 %d，期望 >=1", ver)
	}

	// 关键表应存在。
	for _, tbl := range []string{"users", "sessions", "servers", "audit_logs", "metrics_snapshots", "settings"} {
		var name string
		err := d.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
		if err != nil {
			t.Fatalf("表 %s 不存在: %v", tbl, err)
		}
	}
}

func TestOpen_Idempotent(t *testing.T) {
	// 对同一库二次 Open 不应报错，也不应重复应用迁移。
	path := filepath.Join(t.TempDir(), "test.db")
	d1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var v1 int
	_ = d1.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&v1)
	d1.Close()

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("二次 Open: %v", err)
	}
	defer d2.Close()
	var v2 int
	_ = d2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&v2)
	if v2 != v1 {
		t.Fatalf("迁移被重复应用: 首次 %d 行，二次 %d 行", v1, v2)
	}
}

// TestAuditLogsNoForeignKeys 锁死 0004 迁移的语义：审计表不依赖活表。
// 两个方向都要在：
//  1. 全新库（走完整迁移链）：打向不存在服务器的审计行必须插得进去——
//     探测/404 的行曾经被 server_id 外键静默吞掉（中间件 _ = 忽略错误）；
//  2. 升级库（预置 v3 状态+存量数据再 Open）：存量审计行原样保留，
//     AUTOINCREMENT 延续（sqlite_sequence 跟着表改名走），新行继续拿
//     比存量更大的 id。
func TestAuditLogsNoForeignKeys(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	// 全新库：服务器 99999 不存在，审计行必须落得进去。
	if _, err := d.Exec(`INSERT INTO audit_logs(user_id,server_id,action,target,result,error,ip,request_id)
		VALUES(1,99999,'metrics.latest','/api/v1/servers/99999/metrics','error','Not Found','127.0.0.1','req-1')`); err != nil {
		t.Fatalf("不存在服务器的审计行应插入成功（外键已去）: %v", err)
	}
	// user_id 同理：主体 424242 不存在也必须是历史记录而非违约。
	if _, err := d.Exec(`INSERT INTO audit_logs(user_id,action,result)
		VALUES(424242,'token.create','ok')`); err != nil {
		t.Fatalf("不存在用户的审计行应插入成功（外键已去）: %v", err)
	}
}

// TestAuditLogsMigrationPreservesRows 验证升级路径：v3 存量库应用 0004
// 后数据不丢、序号不断。预置手法=手工执行 0001-0003 的 SQL 并登记
// schema_migrations，再 Open 走 0004——与真实部署同一条代码路径。
func TestAuditLogsMigrationPreservesRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")

	// 裸驱动建 v3 状态（不走 Open，避免它把 0001 重放一遍撞 "table exists"）。
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("裸开库: %v", err)
	}
	for _, f := range []string{"migrations/0001_init.sql", "migrations/0002_api_tokens.sql", "migrations/0003_metrics.sql"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s: %v", f, err)
		}
		if _, err := raw.Exec(string(b)); err != nil {
			t.Fatalf("执行 %s: %v", f, err)
		}
	}
	// 一台真实存在的服务器 + 一个真实存在的用户 + 三行存量审计。
	// （旧 schema 的 user_id/server_id 外键此刻还在干活——种子行必须引用
	// 真实存在的主体，否则 v3 状态根本建不起来；这也反向印证 0004 之前
	// 「探测行落不了库」不是种子问题是真缺陷。）
	if _, err := raw.Exec(`INSERT INTO users(username,pw_hash) VALUES('admin','x')`); err != nil {
		t.Fatalf("种子用户: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO servers(name,base_url,token_cipher) VALUES('srv','https://a:9527','x')`); err != nil {
		t.Fatalf("种子服务器: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := raw.Exec(`INSERT INTO audit_logs(user_id,username,server_id,action,target,result,ip,request_id)
			VALUES(1,'admin',1,'container.list','/api/v1/servers/1/containers','ok','127.0.0.1',?)`, "req-"+strconv.Itoa(i)); err != nil {
			t.Fatalf("种子审计行 %d: %v", i, err)
		}
	}
	// 登记 1-3 已应用，Open 就只会跑 0004。
	// schema_migrations 不在 0001 里、由 migrate() 自建——真实 v3 库里
	// 它早已存在，这里照 migrate() 的 DDL 手工补一个。
	if _, err := raw.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name    TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
	)`); err != nil {
		t.Fatalf("建 schema_migrations: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_migrations(version,name) VALUES(1,'0001_init.sql'),(2,'0002_api_tokens.sql'),(3,'0003_metrics.sql')`); err != nil {
		t.Fatalf("登记迁移: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("关裸连接: %v", err)
	}

	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open（应用 0004）: %v", err)
	}
	defer d.Close()

	// 存量三行原样保留。
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM audit_logs`).Scan(&n); err != nil {
		t.Fatalf("数审计行: %v", err)
	}
	if n != 3 {
		t.Fatalf("存量审计行应保留 3 行，得 %d", n)
	}
	// AUTOINCREMENT 延续：新行 id 必须大于存量最大（3），不能重头再来。
	res, err := d.Exec(`INSERT INTO audit_logs(action,target,result) VALUES('metrics.latest','/api/v1/servers/99999/metrics','error')`)
	if err != nil {
		t.Fatalf("升级后插入审计行: %v", err)
	}
	newID, _ := res.LastInsertId()
	if newID <= 3 {
		t.Fatalf("AUTOINCREMENT 应延续（新 id %d，存量最大 3）——表改名把 sqlite_sequence 弄丢了", newID)
	}
	// 升级后同样不再有外键：直接打一个不存在的 server_id。
	if _, err := d.Exec(`INSERT INTO audit_logs(server_id,action,result) VALUES(777,'probe','error')`); err != nil {
		t.Fatalf("升级后审计行仍不该受外键约束: %v", err)
	}
}
