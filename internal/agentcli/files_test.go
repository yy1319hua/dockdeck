// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S5.5：文件管理客户端链路（FileList/Stat/Content/Write/Action）。
// 关注点：方法与查询串是否与 Agent 侧路由一字不差（含百分号编码）、
// 信封内层是否正确解码、裸流是不是真的没被塞进信封，以及
// "该拒的不出门"——非法动作与超限写在本地拦下，一条请求都不产生。

package agentcli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// writeErrorEnvelope 按 Agent 侧 httpx.WriteError 的形态给错误信封。
// 包内既有的 writeEnvelope 只写 data 字段，用它还原不出「非 200 + error」这条分支。
func writeErrorEnvelope(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"code": code, "message": message},
	})
}

func TestClient_FileList(t *testing.T) {
	var gotMethod, gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotURI = r.Method, r.URL.RequestURI()
		writeEnvelope(w, http.StatusOK, protocol.FilesList{
			Path:      "d1",
			Entries:   []protocol.FileEntry{{Name: "a.txt", Path: "d1/a.txt", Size: 12, Mode: "0644"}},
			Truncated: false,
		})
	}))
	defer srv.Close()

	got, err := quickClient(srv).FileList(context.Background(), 0, "d1", 1)
	if err != nil {
		t.Fatalf("FileList: %v", err)
	}
	// url.Values.Encode 按键名排序，root=0 时省略不發：depth < path
	if gotMethod != http.MethodGet || gotURI != "/api/v1/files?depth=1&path=d1" {
		t.Errorf("方法与查询串不符 Agent 路由：%s %s", gotMethod, gotURI)
	}
	if got.Path != "d1" || len(got.Entries) != 1 || got.Entries[0].Name != "a.txt" {
		t.Errorf("信封内层解码不符：%+v", got)
	}
}

func TestClient_FileListRootSent(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		writeEnvelope(w, http.StatusOK, protocol.FilesList{Path: ""})
	}))
	defer srv.Close()

	if _, err := quickClient(srv).FileList(context.Background(), 2, "", 3); err != nil {
		t.Fatalf("FileList: %v", err)
	}
	// root 非 0 才出现在查询串里，depth 显式给出（不依赖 Agent 默认值）。

	want := "/api/v1/files?depth=3&path=&root=2"
	if gotURI != want {
		t.Errorf("查询串 = %q, want %q", gotURI, want)
	}
}

func TestClient_FileStat(t *testing.T) {
	var gotMethod, gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotURI = r.Method, r.URL.RequestURI()
		writeEnvelope(w, http.StatusOK, protocol.FileMeta{
			Path: "a.txt", Size: 12, ContentType: "text/plain; charset=utf-8", Truncated: true,
		})
	}))
	defer srv.Close()

	got, err := quickClient(srv).FileStat(context.Background(), 0, "a.txt")
	if err != nil {
		t.Fatalf("FileStat: %v", err)
	}
	if gotMethod != http.MethodGet || gotURI != "/api/v1/files/stat?path=a.txt" {
		t.Errorf("方法与路径不符 Agent 路由：%s %s", gotMethod, gotURI)
	}
	if got.Size != 12 || got.ContentType != "text/plain; charset=utf-8" || !got.Truncated {
		t.Errorf("信封内层解码不符：%+v", got)
	}
}

// 路径允许含空格、斜杠与非 ASCII：必须走 QueryEscape，否则请求会被打歪。
func TestClient_FileQueryEncoding(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.URL.RequestURI()
		writeEnvelope(w, http.StatusOK, protocol.FileMeta{Path: "a b/中文.txt"})
	}))
	defer srv.Close()

	if _, err := quickClient(srv).FileStat(context.Background(), 0, "a b/中文.txt"); err != nil {
		t.Fatalf("FileStat: %v", err)
	}
	// 空格→+，斜杠→%2F，中文按 UTF-8 百分号编码（大写十六进制）。
	want := "/api/v1/files/stat?path=a+b%2F%E4%B8%AD%E6%96%87.txt"
	if gotURI != want {
		t.Errorf("查询串 = %q, want %q", gotURI, want)
	}
}

func TestClient_FileContent(t *testing.T) {
	var gotMethod, gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotURI = r.Method, r.URL.RequestURI()
		h := w.Header()
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("X-DD-File-Path", "a.txt")
		h.Set("X-DD-File-Size", "11")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "hello world")
	}))
	defer srv.Close()

	st, err := quickClient(srv).FileContent(context.Background(), 0, "a.txt")
	if err != nil {
		t.Fatalf("FileContent: %v", err)
	}
	defer st.Body.Close()
	if gotMethod != http.MethodGet || gotURI != "/api/v1/files/content?path=a.txt" {
		t.Errorf("方法与路径不符 Agent 路由：%s %s", gotMethod, gotURI)
	}
	body, err := io.ReadAll(st.Body)
	if err != nil {
		t.Fatalf("读流：%v", err)
	}
	if string(body) != "hello world" {
		t.Errorf("正文应原样透传（不经信封），得 %q", body)
	}
	if st.ContentType != "text/plain; charset=utf-8" || st.Path != "a.txt" || st.Size != 11 || st.Truncated {
		t.Errorf("元信息解析不符：%+v", st)
	}
}

// 截断标记必须能从响应头还原：否则 Hub 会把「被截了」当完整文件发出去。
func TestClient_FileContentTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-DD-File-Size", "1048576")
		w.Header().Set("X-DD-Truncated", "1")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "partial")
	}))
	defer srv.Close()

	st, err := quickClient(srv).FileContent(context.Background(), 0, "big.bin")
	if err != nil {
		t.Fatalf("FileContent: %v", err)
	}
	defer st.Body.Close()
	if !st.Truncated || st.Size != 1048576 {
		t.Errorf("截断标记未还原：Truncated=%v Size=%d", st.Truncated, st.Size)
	}
}

// 非 200 时 Agent 给的是信封错误:必须把它的 code/message 还原成 protocol.Error,
// 而不是吞成一个不上不下的 upstream 错误。
func TestClient_FileContentErrorMapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeErrorEnvelope(w, http.StatusNotFound, protocol.CodeNotFound, "文件不存在")
	}))
	defer srv.Close()

	_, err := quickClient(srv).FileContent(context.Background(), 0, "nope.txt")
	if err == nil {
		t.Fatal("404 应返回错误")
	}
	// 必须是被还原的 protocol.Error 本体且错误码一致，而不是把响应原样塞进兜底文案。
	// 只断言「错误文本含 not_found」是蒙混过关：兜底分支会把响应正文拼进 message，
	// 于是「真还原」与「正文里恰好有这个词」看起来一模一样。
	var pe *protocol.Error
	if !errors.As(err, &pe) {
		t.Fatalf("应是还原后的 protocol.Error，得 %T: %v", err, err)
	}
	if pe.Code != protocol.CodeNotFound {
		t.Errorf("错误码 = %q, want %q", pe.Code, protocol.CodeNotFound)
	}
}

func TestClient_FileWrite(t *testing.T) {
	var gotMethod, gotURI, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotURI = r.Method, r.URL.RequestURI()
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		writeEnvelope(w, http.StatusOK, protocol.FileWritten{
			Path: "a.txt", BackupFile: "a.txt.bak.2026-10-15T00:00:00+08:00", Size: 11,
		})
	}))
	defer srv.Close()

	got, err := quickClient(srv).FileWrite(context.Background(), 0, "a.txt", []byte("hello world"))
	if err != nil {
		t.Fatalf("FileWrite: %v", err)
	}
	if gotMethod != http.MethodPut || gotURI != "/api/v1/files/content?path=a.txt" {
		t.Errorf("方法与路径不符 Agent 路由：%s %s", gotMethod, gotURI)
	}
	// 请求体就是文件内容本身（不套信封）。
	if gotBody != "hello world" {
		t.Errorf("上传正文应原样出门，得 %q", gotBody)
	}
	if got.Size != 11 || got.BackupFile == "" {
		t.Errorf("备份路径未回传：%+v", got)
	}
}

// 超限在客户端就拦下：用的是 Hub 与 Agent 共用的那个常量，多一跳没意义。
func TestClient_FileWriteOverCapRejected(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeEnvelope(w, http.StatusOK, protocol.FileWritten{Path: "big.bin"})
	}))
	defer srv.Close()

	big := make([]byte, protocol.MaxFileUploadBytes+1)
	_, err := quickClient(srv).FileWrite(context.Background(), 0, "big.bin", big)
	if err == nil {
		t.Fatalf("超过共享上限 %d 字节应被本地拦下", protocol.MaxFileUploadBytes)
	}
	if calls != 0 {
		t.Errorf("该拒的请求没出门：产生了 %d 次请求", calls)
	}
}

func TestClient_FileAction(t *testing.T) {
	var gotMethod, gotURI, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotURI = r.Method, r.URL.RequestURI()
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		writeEnvelope(w, http.StatusOK, protocol.FileActionDone{Action: protocol.ActionMkdir, Path: "d1"})
	}))
	defer srv.Close()

	got, err := quickClient(srv).FileAction(context.Background(), 1, protocol.ActionMkdir, "d1", "")
	if err != nil {
		t.Fatalf("FileAction: %v", err)
	}
	if gotMethod != http.MethodPost || gotURI != "/api/v1/files/mkdir?root=1" {
		t.Errorf("方法与路径不符 Agent 路由：%s %s", gotMethod, gotURI)
	}
	// 请求体是信封 JSON；dest 空时按 omitempty 省略。
	if !strings.Contains(gotBody, `"action":"mkdir"`) || !strings.Contains(gotBody, `"path":"d1"`) {
		t.Errorf("请求体不符 FileAction 契约：%s", gotBody)
	}
	if strings.Contains(gotBody, `"dest"`) {
		t.Errorf("非 rename 不应带 dest：%s", gotBody)
	}
	if got.Action != protocol.ActionMkdir || got.Path != "d1" {
		t.Errorf("信封内层解码不符：%+v", got)
	}
}

func TestClient_FileActionRenameSendsDest(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		writeEnvelope(w, http.StatusOK, protocol.FileActionDone{Action: protocol.ActionRename, Path: "a.txt", Dest: "b.txt"})
	}))
	defer srv.Close()

	if _, err := quickClient(srv).FileAction(context.Background(), 0, protocol.ActionRename, "a.txt", "b.txt"); err != nil {
		t.Fatalf("FileAction: %v", err)
	}
	if !strings.Contains(gotBody, `"dest":"b.txt"`) {
		t.Errorf("rename 应带上目标路径：%s", gotBody)
	}
}

// 非法动作在本地拦：它是 URL 的一段，拼请求前拦下比让 Agent 回 400 更省一跳。
func TestClient_FileActionInvalidRejected(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeEnvelope(w, http.StatusOK, protocol.FileActionDone{Action: "rm"})
	}))
	defer srv.Close()

	for _, bad := range []string{"rm", "", "MKDIR", "move"} {
		if _, err := quickClient(srv).FileAction(context.Background(), 0, bad, "d1", ""); err == nil {
			t.Errorf("动作 %q 应被本地拦下", bad)
		}
	}
	if calls != 0 {
		t.Errorf("非法动作没出门：产生了 %d 次请求", calls)
	}
}
