// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package store

import (
	"context"
	"testing"
)

// TestEnsureLocalServer_CreatesThenIdempotent 覆盖本机注册的核心契约：
// 首次创建、二次复用同一 id 与令牌、换地址只刷新 base_url，且列表始终只有一行。
func TestEnsureLocalServer_CreatesThenIdempotent(t *testing.T) {
	s := newServerStore(t)
	ctx := context.Background()

	id1, tok1, err := s.EnsureLocalServer(ctx, "http://127.0.0.1:9528")
	if err != nil {
		t.Fatalf("首次注册: %v", err)
	}
	if id1 <= 0 {
		t.Fatalf("id = %d，期望正整数", id1)
	}
	if tok1 == "" {
		t.Fatal("首次注册应生成非空令牌")
	}

	list, err := s.ListServers(ctx)
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len = %d，期望 1", len(list))
	}
	got := list[0]
	if got.Type != ServerTypeLocal {
		t.Errorf("type = %q，期望 %q", got.Type, ServerTypeLocal)
	}
	if got.Name != LocalServerName {
		t.Errorf("name = %q，期望 %q", got.Name, LocalServerName)
	}
	if got.BaseURL != "http://127.0.0.1:9528" {
		t.Errorf("base_url = %q", got.BaseURL)
	}
	if !got.Enabled {
		t.Error("本机应处于启用状态")
	}

	// 二次注册（同地址）：id 与令牌都不变，不新增行。
	id2, tok2, err := s.EnsureLocalServer(ctx, "http://127.0.0.1:9528")
	if err != nil {
		t.Fatalf("二次注册: %v", err)
	}
	if id2 != id1 {
		t.Errorf("二次 id = %d，期望复用 %d", id2, id1)
	}
	if tok2 != tok1 {
		t.Error("令牌应稳定复用，不应每次启动轮换")
	}
	if list, _ = s.ListServers(ctx); len(list) != 1 {
		t.Errorf("二次注册后 len = %d，期望 1", len(list))
	}

	// 三次注册（换地址）：仍是同一 id，只刷新 base_url。
	id3, _, err := s.EnsureLocalServer(ctx, "http://127.0.0.1:9599")
	if err != nil {
		t.Fatalf("三次注册: %v", err)
	}
	if id3 != id1 {
		t.Errorf("换地址后 id = %d，期望仍为 %d", id3, id1)
	}
	list, _ = s.ListServers(ctx)
	if len(list) != 1 || list[0].BaseURL != "http://127.0.0.1:9599" {
		t.Errorf("base_url 未刷新: %+v", list)
	}
}

// TestEnsureLocalServer_NameTakenFallsBack 验证默认名被远端占用时不抢名：
// 本机改用带后缀的名字，仍然以 type=local 区分，远端行不受影响。
func TestEnsureLocalServer_NameTakenFallsBack(t *testing.T) {
	s := newServerStore(t)
	ctx := context.Background()
	remoteID := seedServer(t, s, LocalServerName, "remote-tok", true)

	localID, _, err := s.EnsureLocalServer(ctx, "http://127.0.0.1:9528")
	if err != nil {
		t.Fatalf("EnsureLocalServer: %v", err)
	}
	if localID == remoteID {
		t.Fatal("本机不应复用远端服务器的 id")
	}
	list, err := s.ListServers(ctx)
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len = %d，期望 2（远端 + 本机）", len(list))
	}
	for _, ss := range list {
		switch ss.ID {
		case remoteID:
			if ss.Type != ServerTypeAgent || ss.Name != LocalServerName {
				t.Errorf("远端行被改动: %+v", ss)
			}
		case localID:
			if ss.Type != ServerTypeLocal {
				t.Errorf("本机 type = %q", ss.Type)
			}
			if ss.Name != localServerNameFallback {
				t.Errorf("本机 name = %q，期望回退名 %q", ss.Name, localServerNameFallback)
			}
		}
	}
}

// TestEnsureLocalServer_RebuildsTokenWhenCipherBroken 验证密文损坏时自愈：
// id 不变、令牌重建，且新令牌能被 ServerConn 正常解密（不会把本机永久锁死）。
func TestEnsureLocalServer_RebuildsTokenWhenCipherBroken(t *testing.T) {
	s := newServerStore(t)
	ctx := context.Background()

	id, oldTok, err := s.EnsureLocalServer(ctx, "http://127.0.0.1:9528")
	if err != nil {
		t.Fatalf("首次注册: %v", err)
	}
	if _, err := s.DB.Exec(`UPDATE servers SET token_cipher='broken' WHERE id=?`, id); err != nil {
		t.Fatalf("破坏密文: %v", err)
	}

	id2, newTok, err := s.EnsureLocalServer(ctx, "http://127.0.0.1:9528")
	if err != nil {
		t.Fatalf("密文损坏后注册: %v", err)
	}
	if id2 != id {
		t.Errorf("id = %d，期望不变 %d", id2, id)
	}
	if newTok == "" || newTok == oldTok {
		t.Fatal("密文损坏时应重建令牌")
	}
	conn, err := s.ServerConn(ctx, id)
	if err != nil {
		t.Fatalf("ServerConn: %v", err)
	}
	if conn.Token != newTok {
		t.Error("库内密文与返回令牌不一致")
	}
}

func TestEnsureLocalServer_RejectsEmptyBaseURL(t *testing.T) {
	s := newServerStore(t)
	if _, _, err := s.EnsureLocalServer(context.Background(), "  "); err == nil {
		t.Fatal("空 base_url 应报错")
	}
}
