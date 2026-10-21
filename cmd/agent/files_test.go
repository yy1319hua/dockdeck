// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yy1319hua/dockdeck/internal/config"
	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

// filesTree 造一棵 HTTP 层测试用的树：
//
//	notes.txt        内容 "notes\n"
//	big.txt          内容 "0123456789"
//	empty/           空目录
//	sub/inner.txt
//	sub/.env         黑名单（段名）
//	key.pem          黑名单（*.pem）
//	escape -> 白名单外的目录
func filesTree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "data")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside, filepath.Join(root, "empty"), filepath.Join(root, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(root, "notes.txt"):        "notes\n",
		filepath.Join(root, "big.txt"):          "0123456789",
		filepath.Join(root, "key.pem"):          "pem\n",
		filepath.Join(root, "sub", "inner.txt"): "inner\n",
		filepath.Join(root, "sub", ".env"):      "SECRET=1\n",
		filepath.Join(outside, "secret.txt"):    "outside\n",
	}
	for p, c := range files {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	return root
}

// filesServer 起一个与生产同款中间件链的 Agent，只开文件管理。
func filesServer(t *testing.T, root string, maxFile int64) *httptest.Server {
	t.Helper()
	fapi, err := newFilesAPI(config.AgentConfig{
		FsRoots:      []string{root},
		FsDeny:       []string{"*.pem", ".env"},
		FsMaxFile:    maxFile,
		FsMaxDepth:   3,
		FsMaxEntries: 50,
	})
	if err != nil {
		t.Fatalf("newFilesAPI: %v", err)
	}
	srv := httptest.NewServer(buildHandler(smokeToken, smokeCollector(t), nil,
		newContainersAPI("", ""), newImagesAPI(""), newComposeAPI(nil, ""), fapi, newResourcesAPI("")))
	t.Cleanup(srv.Close)
	return srv
}

// fileReq 造一个带 Bearer + 时间戳 + 签名的请求。
// 签名覆盖 RequestURI（含查询串）与 body，必须与实际发出的请求逐字一致。
func fileReq(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Authorization", "Bearer "+smokeToken)
	req.Header.Set("X-DD-Timestamp", stamp)
	req.Header.Set("X-DD-Signature",
		crypto.SignRequest([]byte(smokeToken), stamp, method, req.URL.RequestURI(), []byte(body)))
	return req
}

// doFile 发一次请求并返回状态码、头与 body。
func doFile(t *testing.T, srv *httptest.Server, method, path, body string) (int, http.Header, []byte) {
	t.Helper()
	resp, err := srv.Client().Do(fileReq(t, method, srv.URL+path, body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, raw
}

// errCodeOf 从信封里取错误码。
func errCodeOf(t *testing.T, raw []byte) string {
	t.Helper()
	var env struct {
		Error *protocol.Error `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("响应不是信封：%v，body=%s", err, raw)
	}
	if env.Error == nil {
		return ""
	}
	return env.Error.Code
}

// 红线第 3 条：未配 --fs-root 时模块不注册任何路由——连 401 都不该有，
// 因为端点在网络层就不存在。
func TestFiles_DisabledWithoutRoots(t *testing.T) {
	fapi, err := newFilesAPI(config.AgentConfig{})
	if err != nil {
		t.Fatalf("未配根目录不该报错：%v", err)
	}
	if fapi != nil {
		t.Fatal("未配根目录时 newFilesAPI 应返回 nil")
	}
	srv := httptest.NewServer(buildHandler(smokeToken, smokeCollector(t), nil,
		newContainersAPI("", ""), newImagesAPI(""), newComposeAPI(nil, ""), fapi, newResourcesAPI("")))
	defer srv.Close()

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/files"},
		{http.MethodGet, "/api/v1/files/stat?path=a"},
		{http.MethodGet, "/api/v1/files/content?path=a"},
		{http.MethodPut, "/api/v1/files/content?path=a"},
		{http.MethodPost, "/api/v1/files/delete"},
	} {
		resp, err := srv.Client().Do(fileReq(t, tc.method, srv.URL+tc.path, ""))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s 应 404（模块未启用），got %d", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// 配了根目录却不可用必须让启动失败，不能静默降级成「模块关闭」。
func TestFiles_BadRootFailsStartup(t *testing.T) {
	if _, err := newFilesAPI(config.AgentConfig{
		FsRoots: []string{filepath.Join(t.TempDir(), "nope")}, FsMaxFile: 1024, FsMaxDepth: 3, FsMaxEntries: 10,
	}); err == nil {
		t.Fatal("根目录不存在时应报错")
	}
}

func TestFiles_List(t *testing.T) {
	root := filesTree(t)
	srv := filesServer(t, root, 1<<20)

	code, _, raw := doFile(t, srv, http.MethodGet, "/api/v1/files", "")
	if code != http.StatusOK {
		t.Fatalf("列表应 200，got %d body=%s", code, raw)
	}
	var env struct {
		Data protocol.FilesList `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("解析信封失败：%v", err)
	}
	names := map[string]bool{}
	for _, e := range env.Data.Entries {
		names[e.Name] = true
	}
	for _, want := range []string{"notes.txt", "big.txt", "sub", "empty"} {
		if !names[want] {
			t.Errorf("列表缺少 %q：%v", want, names)
		}
	}
	// 黑名单与逃逸软链都不该出现：看得见的必须都是点得动的。
	for _, bad := range []string{"key.pem", "escape"} {
		if names[bad] {
			t.Errorf("不可操作项 %q 不该出现在列表里", bad)
		}
	}
}

func TestFiles_ListEmptyIsArrayNotNull(t *testing.T) {
	root := filesTree(t)
	srv := filesServer(t, root, 1<<20)

	code, _, raw := doFile(t, srv, http.MethodGet, "/api/v1/files?path=empty", "")
	if code != http.StatusOK {
		t.Fatalf("空目录列表应 200，got %d body=%s", code, raw)
	}
	if !strings.Contains(string(raw), `"entries":[]`) {
		t.Errorf("空清单应是 []，got %s", raw)
	}
}

func TestFiles_StatAndContent(t *testing.T) {
	root := filesTree(t)
	srv := filesServer(t, root, 1<<20)

	code, _, raw := doFile(t, srv, http.MethodGet, "/api/v1/files/stat?path=notes.txt", "")
	if code != http.StatusOK {
		t.Fatalf("stat 应 200，got %d body=%s", code, raw)
	}
	var env struct {
		Data protocol.FileMeta `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Path != "notes.txt" || env.Data.Size != 6 || env.Data.Truncated {
		t.Errorf("FileMeta = %+v", env.Data)
	}
	if !strings.HasPrefix(env.Data.ContentType, "text/plain") {
		t.Errorf("ContentType = %q", env.Data.ContentType)
	}

	code, hdr, raw := doFile(t, srv, http.MethodGet, "/api/v1/files/content?path=notes.txt", "")
	if code != http.StatusOK {
		t.Fatalf("读正文应 200，got %d", code)
	}
	if string(raw) != "notes\n" {
		t.Errorf("正文 = %q，want notes\\n", raw)
	}
	if !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Errorf("响应 Content-Type = %q", hdr.Get("Content-Type"))
	}
	if hdr.Get("X-DD-File-Size") != "6" {
		t.Errorf("X-DD-File-Size = %q，want 6", hdr.Get("X-DD-File-Size"))
	}
	if hdr.Get("X-DD-Truncated") != "" {
		t.Errorf("小文件不该带截断头，got %q", hdr.Get("X-DD-Truncated"))
	}
	if hdr.Get("Content-Length") != "6" {
		t.Errorf("Content-Length = %q，want 6", hdr.Get("Content-Length"))
	}
}

func TestFiles_ContentTruncated(t *testing.T) {
	root := filesTree(t)
	srv := filesServer(t, root, 4)

	code, hdr, raw := doFile(t, srv, http.MethodGet, "/api/v1/files/content?path=big.txt", "")
	if code != http.StatusOK {
		t.Fatalf("读正文应 200，got %d", code)
	}
	if string(raw) != "0123" {
		t.Errorf("正文 = %q，want 0123（截在上限 4 字节）", raw)
	}
	if hdr.Get("X-DD-Truncated") != "1" {
		t.Errorf("应带截断头，got %q", hdr.Get("X-DD-Truncated"))
	}
	if hdr.Get("X-DD-File-Size") != "10" {
		t.Errorf("X-DD-File-Size 应给真实大小 10，got %q", hdr.Get("X-DD-File-Size"))
	}
}

func TestFiles_PutCreatesThenBacksUp(t *testing.T) {
	root := filesTree(t)
	srv := filesServer(t, root, 1<<20)

	code, _, raw := doFile(t, srv, http.MethodPut, "/api/v1/files/content?path=notes.txt", "first\n")
	if code != http.StatusOK {
		t.Fatalf("覆盖写应 200，got %d body=%s", code, raw)
	}
	// 每次都解进新变量：json.Unmarshal 是「合并」语义，复用变量会让上一条的
	// backup_file 残留下来，把「无备份」断言骗过去。
	var overwritten protocol.FileWritten
	if err := json.Unmarshal(dataOf(t, raw), &overwritten); err != nil {
		t.Fatal(err)
	}
	if overwritten.Created || overwritten.BackupFile == "" || overwritten.Size != 6 {
		t.Fatalf("FileWritten = %+v（覆盖已有文件应有备份、非 Created）", overwritten)
	}
	if got := readAll(t, filepath.Join(root, overwritten.BackupFile)); got != "notes\n" {
		t.Errorf("备份内容 = %q，want 旧内容", got)
	}
	if got := readAll(t, filepath.Join(root, "notes.txt")); got != "first\n" {
		t.Errorf("目标内容 = %q", got)
	}

	// 新文件：没有备份可回滚，Created 为 true。
	code, _, raw = doFile(t, srv, http.MethodPut, "/api/v1/files/content?path=brand.txt", "new")
	if code != http.StatusOK {
		t.Fatalf("新建应 200，got %d body=%s", code, raw)
	}
	var created protocol.FileWritten
	if err := json.Unmarshal(dataOf(t, raw), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Created || created.BackupFile != "" {
		t.Errorf("新建文件不该有备份：%+v", created)
	}
	if created.Path != "brand.txt" || created.Size != 3 {
		t.Errorf("FileWritten = %+v", created)
	}
}

func TestFiles_PutRejectsTooLarge(t *testing.T) {
	root := filesTree(t)
	srv := filesServer(t, root, 4)

	code, _, raw := doFile(t, srv, http.MethodPut, "/api/v1/files/content?path=notes.txt", "0123456789")
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应 413，got %d body=%s", code, raw)
	}
	if got := errCodeOf(t, raw); got != protocol.CodeTooLarge {
		t.Errorf("错误码 = %q，want %q", got, protocol.CodeTooLarge)
	}
	if got := readAll(t, filepath.Join(root, "notes.txt")); got != "notes\n" {
		t.Errorf("超限后原文件被改动：%q", got)
	}
}

// 错误码映射：越界 400、逃逸/黑名单 403、不存在 404、冲突 409。
func TestFiles_ErrorMapping(t *testing.T) {
	root := filesTree(t)
	srv := filesServer(t, root, 1<<20)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
		code   string
	}{
		{"上级穿越", http.MethodGet, "/api/v1/files/content?path=../notes.txt", "", http.StatusBadRequest, protocol.CodeBadRequest},
		{"绝对路径", http.MethodGet, "/api/v1/files/content?path=/etc/passwd", "", http.StatusBadRequest, protocol.CodeBadRequest},
		{"黑名单", http.MethodGet, "/api/v1/files/content?path=key.pem", "", http.StatusForbidden, protocol.CodePathDenied},
		{"子目录黑名单", http.MethodGet, "/api/v1/files/content?path=sub/.env", "", http.StatusForbidden, protocol.CodePathDenied},
		{"逃逸软链", http.MethodGet, "/api/v1/files/content?path=escape/secret.txt", "", http.StatusForbidden, protocol.CodePathDenied},
		{"不存在", http.MethodGet, "/api/v1/files/content?path=missing.txt", "", http.StatusNotFound, protocol.CodeNotFound},
		{"根序号越界", http.MethodGet, "/api/v1/files?root=9", "", http.StatusBadRequest, protocol.CodeBadRequest},
		{"depth 非法", http.MethodGet, "/api/v1/files?depth=abc", "", http.StatusBadRequest, protocol.CodeBadRequest},
		{"列一个文件", http.MethodGet, "/api/v1/files?path=notes.txt", "", http.StatusBadRequest, protocol.CodeBadRequest},
		{"删非空目录", http.MethodPost, "/api/v1/files/delete", `{"action":"delete","path":"sub"}`, http.StatusConflict, protocol.CodeConflict},
		{"建已存在目录", http.MethodPost, "/api/v1/files/mkdir", `{"action":"mkdir","path":"sub"}`, http.StatusConflict, protocol.CodeConflict},
		{"改名到已存在", http.MethodPost, "/api/v1/files/rename", `{"action":"rename","path":"notes.txt","dest":"big.txt"}`, http.StatusConflict, protocol.CodeConflict},
		{"改根名", http.MethodPost, "/api/v1/files/rename", `{"action":"rename","path":"","dest":"x"}`, http.StatusForbidden, protocol.CodePathDenied},
		{"动作名不一致", http.MethodPost, "/api/v1/files/delete", `{"action":"rename","path":"notes.txt","dest":"x"}`, http.StatusBadRequest, protocol.CodeBadRequest},
		{"未知动作", http.MethodPost, "/api/v1/files/frobnicate", `{"action":"frobnicate","path":"notes.txt"}`, http.StatusBadRequest, protocol.CodeBadRequest},
		{"rename 缺 dest", http.MethodPost, "/api/v1/files/rename", `{"action":"rename","path":"notes.txt"}`, http.StatusBadRequest, protocol.CodeBadRequest},
		{"请求体非法", http.MethodPost, "/api/v1/files/delete", `{"action":"delete","path":"x","surprise":1}`, http.StatusBadRequest, protocol.CodeBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, raw := doFile(t, srv, tc.method, tc.path, tc.body)
			if code != tc.want {
				t.Fatalf("状态码 = %d，want %d（body=%s）", code, tc.want, raw)
			}
			if got := errCodeOf(t, raw); got != tc.code {
				t.Errorf("错误码 = %q，want %q", got, tc.code)
			}
		})
	}
}

func TestFiles_ActionLifecycle(t *testing.T) {
	root := filesTree(t)
	srv := filesServer(t, root, 1<<20)

	code, _, raw := doFile(t, srv, http.MethodPost, "/api/v1/files/mkdir", `{"action":"mkdir","path":"newdir"}`)
	if code != http.StatusOK {
		t.Fatalf("mkdir 应 200，got %d body=%s", code, raw)
	}
	if st, err := os.Stat(filepath.Join(root, "newdir")); err != nil || !st.IsDir() {
		t.Fatalf("目录未建出：%v", err)
	}

	code, _, raw = doFile(t, srv, http.MethodPost, "/api/v1/files/rename",
		`{"action":"rename","path":"notes.txt","dest":"newdir/renamed.txt"}`)
	if code != http.StatusOK {
		t.Fatalf("rename 应 200，got %d body=%s", code, raw)
	}
	if got := readAll(t, filepath.Join(root, "newdir", "renamed.txt")); got != "notes\n" {
		t.Errorf("改名后内容 = %q", got)
	}

	code, _, raw = doFile(t, srv, http.MethodPost, "/api/v1/files/delete",
		`{"action":"delete","path":"newdir/renamed.txt"}`)
	if code != http.StatusOK {
		t.Fatalf("delete 应 200，got %d body=%s", code, raw)
	}
	if _, err := os.Stat(filepath.Join(root, "newdir", "renamed.txt")); !os.IsNotExist(err) {
		t.Error("文件应已删除")
	}
}

// 无凭证必须过不了鉴权：文件接口不因为是本地功能而放宽。
func TestFiles_RequiresAuth(t *testing.T) {
	root := filesTree(t)
	srv := filesServer(t, root, 1<<20)

	resp, err := srv.Client().Get(srv.URL + "/api/v1/files")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无凭证应 401，got %d", resp.StatusCode)
	}
}

// 上传大于默认签名体上限（1 MiB）的文件必须能通：文件路由用 AuthN 抬高了上限，
// 若退回默认 Auth，1 MiB 以上的请求会在鉴权层被 413——而调用方看到的
// 只是「too_large」，完全看不出是鉴权层拦的。
func TestFiles_PutBeyondDefaultAuthBodyLimit(t *testing.T) {
	root := filesTree(t)
	srv := filesServer(t, root, 4<<20)

	big := strings.Repeat("a", (1<<20)+512*1024) // 1.5 MiB
	code, _, raw := doFile(t, srv, http.MethodPut, "/api/v1/files/content?path=big-upload.bin", big)
	if code != http.StatusOK {
		t.Fatalf("1.5 MiB 上传应 200，got %d body=%s", code, raw)
	}
	st, err := os.Stat(filepath.Join(root, "big-upload.bin"))
	if err != nil {
		t.Fatalf("文件未落盘：%v", err)
	}
	if st.Size() != int64(len(big)) {
		t.Errorf("落盘大小 = %d，want %d", st.Size(), len(big))
	}
	// 超过单文件上限仍然要拒（AuthN 抬高的是鉴权层门槛，不是文件上限）。
	huge := strings.Repeat("a", (4<<20)+1)
	code, _, raw = doFile(t, srv, http.MethodPut, "/api/v1/files/content?path=huge.bin", huge)
	if code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超过单文件上限应 413，got %d body=%s", code, raw)
	}
}

func readAll(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}
