package db

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Open 打开（或创建）SQLite 库，设置运行期 pragma，并向前应用所有迁移。
// 单写模型：整个 Hub 共享一条连接池，busy_timeout 兜住偶发并发。
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)&_pragma=txlock(immediate)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db: 打开失败: %w", err)
	}
	// SQLite 单写者：限制池上限避免 SQLITE_BUSY 抖动。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("db: 连接失败: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		// 文件名形如 0001_init.sql
		parts := strings.SplitN(e.Name(), "_", 2)
		var v int
		if _, err := fmt.Sscanf(parts[0], "%d", &v); err != nil {
			return nil, fmt.Errorf("db: 迁移文件名非法: %s", e.Name())
		}
		b, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	if len(ms) == 0 {
		return nil, errors.New("db: 未发现任何迁移文件")
	}
	return ms, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name    TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
	)`); err != nil {
		return fmt.Errorf("db: 建 schema_migrations 失败: %w", err)
	}

	var cur int
	err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&cur)
	if err != nil {
		return fmt.Errorf("db: 读当前版本失败: %w", err)
	}

	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if m.version <= cur {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return fmt.Errorf("db: 迁移 %s 失败: %w", m.name, err)
		}
	}
	return nil
}

func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 迁移脚本可含多条语句；sqlite 驱动 Exec 支持多语句以分号分隔。
	if _, err := tx.Exec(m.sql); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations(version,name) VALUES(?,?)`, m.version, m.name); err != nil {
		return err
	}
	return tx.Commit()
}
