// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agenthttp

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

	"github.com/yy1319hua/dockdeck/internal/crypto"
	"github.com/yy1319hua/dockdeck/internal/protocol"
)

const testToken = "test-agent-token"

var fixedNow = time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)

// signed 构造一个带 Bearer + 时间戳 + 签名的请求。
func signed(t *testing.T, method, target string, body []byte, ts int64) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+testToken)
	stamp := strconv.FormatInt(ts, 10)
	req.Header.Set(protocol.HeaderTimestamp, stamp)
	req.Header.Set(protocol.HeaderSignature,
		crypto.SignRequest([]byte(testToken), stamp, method, target, body))
	return req
}

// echoBody 是鉴权通过后的目标 handler：回显 body，用于验证 body 被正确回填。
func echoBody(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	_, _ = w.Write(b)
}

// runAuth 用固定时钟跑一次中间件，返回响应。
func runAuth(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	h := Auth(testToken, func() time.Time { return fixedNow })(http.HandlerFunc(echoBody))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// errCode 从响应体里取出错误码。
func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是预期信封：%v，body=%s", err, w.Body.String())
	}
	if env.Error == nil {
		return ""
	}
	return env.Error.Code
}

func TestAuth_RejectsMissingAuthorization(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/info", nil)
	req.Header.Del("Authorization")
	w := runAuth(t, req)
	if w.Code != http.StatusUnauthorized || errCode(t, w) != protocol.CodeUnauthorized {
		t.Errorf("got %d/%s", w.Code, errCode(t, w))
	}
}

func TestAuth_RejectsWrongToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/info", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	stamp := strconv.FormatInt(fixedNow.Unix(), 10)
	req.Header.Set(protocol.HeaderTimestamp, stamp)
	req.Header.Set(protocol.HeaderSignature,
		crypto.SignRequest([]byte(testToken), stamp, http.MethodGet, "/info", nil))
	w := runAuth(t, req)
	if w.Code != http.StatusUnauthorized || errCode(t, w) != protocol.CodeUnauthorized {
		t.Errorf("got %d/%s", w.Code, errCode(t, w))
	}
}

func TestAuth_RejectsMissingTimestamp(t *testing.T) {
	req := signed(t, http.MethodGet, "/info", nil, fixedNow.Unix())
	req.Header.Del(protocol.HeaderTimestamp)
	w := runAuth(t, req)
	if w.Code != http.StatusUnauthorized || errCode(t, w) != protocol.CodeAgentSignatureInvalid {
		t.Errorf("got %d/%s", w.Code, errCode(t, w))
	}
}

func TestAuth_RejectsStaleTimestamp(t *testing.T) {
	for _, d := range []time.Duration{121 * time.Second, -121 * time.Second} {
		req := signed(t, http.MethodGet, "/info", nil, fixedNow.Add(d).Unix())
		if w := runAuth(t, req); w.Code != http.StatusUnauthorized {
			t.Errorf("偏移 %v 应被拒，got %d", d, w.Code)
		}
	}
	// 窗口边界内放行。
	for _, d := range []time.Duration{119 * time.Second, -119 * time.Second} {
		req := signed(t, http.MethodGet, "/info", nil, fixedNow.Add(d).Unix())
		if w := runAuth(t, req); w.Code != http.StatusOK {
			t.Errorf("偏移 %v 应放行，got %d", d, w.Code)
		}
	}
}

func TestAuth_RejectsMissingSignature(t *testing.T) {
	req := signed(t, http.MethodGet, "/info", nil, fixedNow.Unix())
	req.Header.Del(protocol.HeaderSignature)
	w := runAuth(t, req)
	if w.Code != http.StatusUnauthorized || errCode(t, w) != protocol.CodeAgentSignatureInvalid {
		t.Errorf("got %d/%s", w.Code, errCode(t, w))
	}
}

func TestAuth_RejectsTamperedBody(t *testing.T) {
	body := []byte(`{"action":"stop"}`)
	req := signed(t, http.MethodPost, "/containers/x/stop", body, fixedNow.Unix())
	// 签名算完后偷换 body。
	req.Body = io.NopCloser(strings.NewReader(`{"action":"rm"}`))
	if w := runAuth(t, req); w.Code != http.StatusUnauthorized {
		t.Errorf("篡改 body 应被拒，got %d", w.Code)
	}
}

func TestAuth_RejectsTamperedQuery(t *testing.T) {
	req := signed(t, http.MethodGet, "/metrics?since=1", nil, fixedNow.Unix())
	req.URL.RawQuery = "since=9999"
	if w := runAuth(t, req); w.Code != http.StatusUnauthorized {
		t.Errorf("篡改查询串应被拒，got %d", w.Code)
	}
}

func TestAuth_RejectsOversizedBody(t *testing.T) {
	big := make([]byte, DefaultMaxBodyBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	req := signed(t, http.MethodPost, "/files/write", big, fixedNow.Unix())
	w := runAuth(t, req)
	if w.Code != http.StatusRequestEntityTooLarge || errCode(t, w) != protocol.CodeTooLarge {
		t.Errorf("got %d/%s", w.Code, errCode(t, w))
	}
}

// 文件上传要能带大于默认上限的 body：AuthN 抬高上限后，
// 同一个 1.5 MiB 请求应当放行（而不是像 Auth 那样 413）。
func TestAuthN_RaisesBodyLimitForUploads(t *testing.T) {
	big := make([]byte, DefaultMaxBodyBytes+DefaultMaxBodyBytes/2)
	for i := range big {
		big[i] = 'a'
	}
	req := signed(t, http.MethodPut, "/files/content?path=a.txt", big, fixedNow.Unix())

	h := AuthN(testToken, func() time.Time { return fixedNow }, 4<<20)(http.HandlerFunc(echoBody))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("抬高上限后 1.5 MiB 请求应放行，got %d body=%s", w.Code, w.Body.String())
	}
	if int64(w.Body.Len()) != int64(len(big)) {
		t.Errorf("body 长度 = %d，want %d", w.Body.Len(), len(big))
	}

	// 上限之外仍要拒：抬高不是取消。
	over := make([]byte, (4<<20)+1)
	req2 := signed(t, http.MethodPut, "/files/content?path=a.txt", over, fixedNow.Unix())
	h2 := AuthN(testToken, func() time.Time { return fixedNow }, 4<<20)(http.HandlerFunc(echoBody))
	w2 := httptest.NewRecorder()
	h2.ServeHTTP(w2, req2)
	if w2.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("超过自定义上限应 413，got %d", w2.Code)
	}
}

func TestAuth_AllowsValidRequestAndRestoresBody(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	req := signed(t, http.MethodPost, "/containers/x/stop", body, fixedNow.Unix())
	w := runAuth(t, req)
	if w.Code != http.StatusOK {
		t.Fatalf("合法请求应放行，got %d body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != string(body) {
		t.Errorf("handler 应能重新读到 body，got %q", w.Body.String())
	}
}

func TestAuth_AllowsEmptyBody(t *testing.T) {
	req := signed(t, http.MethodGet, "/info", nil, fixedNow.Unix())
	if w := runAuth(t, req); w.Code != http.StatusOK {
		t.Errorf("GET 空 body 应放行，got %d", w.Code)
	}
}

// withProc 把 /proc 读取指向临时目录，使三项解析可测。
func withProc(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig := procPath
	procPath = func(name string) string { return filepath.Join(dir, name) }
	t.Cleanup(func() { procPath = orig })
}

func TestCollectInfo_ParsesProcFiles(t *testing.T) {
	withProc(t, map[string]string{
		"meminfo": "MemTotal:        8192000 kB\nMemFree:         100000 kB\n",
		"uptime":  "3600.00 12345.00\n",
		"version": "Linux version 6.8.0-45-generic (buildd@host) #1 SMP PREEMPT\n",
	})
	info := CollectInfo(func() time.Time { return fixedNow })
	if info.MemoryTotal != 8192000*1024 {
		t.Errorf("MemoryTotal = %d，want %d", info.MemoryTotal, 8192000*1024)
	}
	want := fixedNow.Add(-time.Hour)
	if !info.BootTime.Equal(want) {
		t.Errorf("BootTime = %v，want %v", info.BootTime, want)
	}
	if info.Kernel != "6.8.0-45-generic" {
		t.Errorf("Kernel = %q", info.Kernel)
	}
	if info.OS == "" || info.Arch == "" || info.Cores <= 0 {
		t.Errorf("运行时字段不应为空：os=%q arch=%q cores=%d", info.OS, info.Arch, info.Cores)
	}
	if info.ProtocolVersion != protocol.ProtocolVersion {
		t.Errorf("ProtocolVersion = %q", info.ProtocolVersion)
	}
}

func TestCollectInfo_DegradesWhenProcUnreadable(t *testing.T) {
	// 指向空目录：三个文件都读不到，但 /info 仍应可用。
	withProc(t, map[string]string{})
	info := CollectInfo(func() time.Time { return fixedNow })
	if info.MemoryTotal != 0 || info.Kernel != "" || !info.BootTime.IsZero() {
		t.Errorf("读不到 /proc 时应降级为零值，got %+v", info)
	}
	if info.Cores <= 0 {
		t.Error("cores 来自运行时，不应受 /proc 影响")
	}
}

func TestInfoHandler_ReturnsJSON(t *testing.T) {
	withProc(t, map[string]string{
		"meminfo": "MemTotal:        1024000 kB\n",
		"uptime":  "100.00 1.00\n",
		"version": "Linux version 5.15.0-test (x) #1\n",
	})
	w := httptest.NewRecorder()
	Info().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/info", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d body=%s", w.Code, w.Body.String())
	}
	var env struct {
		Data protocol.AgentInfo `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析失败：%v，body=%s", err, w.Body.String())
	}
	if env.Data.MemoryTotal != 1024000*1024 {
		t.Errorf("MemoryTotal = %d", env.Data.MemoryTotal)
	}
	if env.Data.Kernel != "5.15.0-test" {
		t.Errorf("Kernel = %q", env.Data.Kernel)
	}
	if env.Data.AgentVersion == "" {
		t.Error("AgentVersion 应有默认值")
	}
}
