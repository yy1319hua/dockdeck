package main

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yy1319hua/dockdeck/internal/apitoken"
	"github.com/yy1319hua/dockdeck/internal/auth"
	"github.com/yy1319hua/dockdeck/internal/db"
	"github.com/yy1319hua/dockdeck/internal/middleware"
	"github.com/yy1319hua/dockdeck/internal/store"
)

// dumpOpenAPI 生成 OpenAPI 文档并写入 dir（openapi.json / openapi.yaml），随后退出。
//
// 关键点：复用与运行时完全相同的路由声明（server.routes → route.Table），
// 所以产物与实装同源，不会出现「文档一套、实装另一套」。
//
// 用临时目录建库 + 随机临时主密钥：生成 spec 不需要任何真实数据，
// 更不该创建/触碰线上的 data 与 secrets 目录（生成模式因此跳过 EnsureDirs）。
func dumpOpenAPI(dir string) error {
	tmp, err := os.MkdirTemp("", "dockdeck-openapi-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	sqlDB, err := db.Open(filepath.Join(tmp, "openapi.db"))
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	st := &store.Store{DB: sqlDB, Key: key}
	srv := &server{
		st:      st,
		am:      auth.NewManager(st, auth.DefaultOptions()),
		tokens:  apitoken.NewAPI(sqlDB),
		rl:      middleware.NewRateLimit(10, 30*time.Second),
		dataDir: tmp,
	}
	srv.routes() // 只装配登记表与 handler 闭包，不监听

	doc := srv.spec()
	jsonBytes, err := doc.JSON()
	if err != nil {
		return err
	}
	yamlBytes, err := doc.YAML()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	jsonPath := filepath.Join(dir, "openapi.json")
	yamlPath := filepath.Join(dir, "openapi.yaml")
	if err := os.WriteFile(jsonPath, jsonBytes, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(yamlPath, yamlBytes, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "openapi 已写出：%s、%s\n", jsonPath, yamlPath)
	return nil
}
