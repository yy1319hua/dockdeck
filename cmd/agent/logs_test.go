// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 日志端点测试：假 Engine 造复用流/裸流/长连接三种形态。

package main

import (
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mu2Set 设置日志形态（测试串行执行，无并发写）。
func (f *fakeDocker) mu2Set(muxed []byte, follow bool) {
	f.logsMuxed, f.logsFollow = muxed, follow
}

// muxFrame 造一帧复用流数据（streamType: 0=stdout 1=stderr）。
func muxFrame(streamType byte, payload string) []byte {
	out := make([]byte, 8+len(payload))
	out[0] = streamType
	binary.BigEndian.PutUint32(out[4:8], uint32(len(payload)))
	copy(out[8:], payload)
	return out
}

// serveLogsRoute 在假 Engine 的 mux 上挂日志路由（读 f.logsMuxed/logsFollow）。
// follow 模式：写完内容 Flush 后挂住等客户端断开，模拟「跟随中」。
// serveLogs 处理 /containers/*/logs（由 containers_test 的 catch-all 分派进来）。
func (f *fakeDocker) serveLogs(w http.ResponseWriter, r *http.Request) {
	if f.logsMuxed == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(f.logsMuxed)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	if f.logsFollow {
		<-r.Context().Done()
	}
}

// agentWithLogs 起一个带日志形态的假 Engine + Agent 服务器。
func agentWithLogs(t *testing.T, muxed []byte, follow bool) (*httptest.Server, *fakeDocker) {
	t.Helper()
	f := newFakeDocker(t)
	f.mu2Set(muxed, follow)
	srv := httptest.NewServer(buildHandler(smokeToken, smokeCollector(t), nil, newContainersAPI(f.sock, "")))
	t.Cleanup(srv.Close)
	return srv, f
}

// TestLogs_Demuxed 验证默认输出是合流文本：stdout 与 stderr 帧按序拼接。
func TestLogs_Demuxed(t *testing.T) {
	muxed := append(muxFrame(0, "line1\n"), muxFrame(1, "err1\n")...)
	srv, f := agentWithLogs(t, muxed, false)
	resp, body := hDo(t, srv, http.MethodGet, "/api/v1/containers/abc123def456/logs?tail=100", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，got %d：%s", resp.StatusCode, body)
	}
	if string(body) != "line1\nerr1\n" {
		t.Errorf("合流文本不符：%q", body)
	}
	if got := f.nextPath(t); !strings.Contains(got, "tail=100") {
		t.Errorf("tail 应透传，got %q", got)
	}
}

// TestLogs_TTYRaw 验证 TTY 裸流（无帧头）被原样输出——Demux 的启发式分支。
func TestLogs_TTYRaw(t *testing.T) {
	raw := []byte("plain tty output\nline two\n")
	srv, _ := agentWithLogs(t, raw, false)
	_, body := hDo(t, srv, http.MethodGet, "/api/v1/containers/abc123def456/logs", "")
	if string(body) != string(raw) {
		t.Errorf("裸流应原样输出：%q", body)
	}
}

// TestLogs_Follow 验证 follow 流式：首帧先到（不等流结束），客户端主动断开。
func TestLogs_Follow(t *testing.T) {
	muxed := muxFrame(0, "first\n")
	srv, _ := agentWithLogs(t, muxed, true)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/containers/abc123def456/logs?follow=1", nil)
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Authorization", "Bearer "+smokeToken)
	req.Header.Set("X-DD-Timestamp", stamp)
	req.Header.Set("X-DD-Signature", signFor(http.MethodGet, "/api/v1/containers/abc123def456/logs?follow=1", nil, stamp))

	start := time.Now()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 期望的是 Demux 后的载荷，不是原始帧（帧头 8 字节已被剥掉）。
	want := "first\n"
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("读首帧失败: %v", err)
	}
	if string(buf) != want {
		t.Errorf("首帧内容不符: %q", buf)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("首帧等待过久（%v），不像流式", elapsed)
	}
}

// TestLogs_Export 验证 export=1 原样导出复用流字节（不 Demux）。
func TestLogs_Export(t *testing.T) {
	muxed := append(muxFrame(0, "line1\n"), muxFrame(1, "err1\n")...)
	srv, _ := agentWithLogs(t, muxed, false)
	resp, body := hDo(t, srv, http.MethodGet, "/api/v1/containers/abc123def456/logs?export=1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200，got %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("导出应为 octet-stream，got %q", resp.Header.Get("Content-Type"))
	}
	if string(body) != string(muxed) {
		t.Errorf("导出应与引擎字节流完全一致：%q", body)
	}
}

// TestLogs_BadTail 验证 tail 白名单（all 或数字）。
func TestLogs_BadTail(t *testing.T) {
	srv, _ := agentWithLogs(t, muxFrame(0, "x\n"), false)
	resp, _ := hDo(t, srv, http.MethodGet, "/api/v1/containers/abc123def456/logs?tail=-1;rm", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 tail 应 400，got %d", resp.StatusCode)
	}
}
