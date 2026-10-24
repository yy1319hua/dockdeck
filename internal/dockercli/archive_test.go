// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeArchiveServer 按用例拼一个假 Engine：只认 archive 两个端点。
type fakeArchiveServer struct {
	getStatus  int
	getBody    string
	getStat    string // 原样放入响应头（已 base64）；空串则不带
	putStatus  int
	gotQuery   string
	gotMethod  string
	gotCT      string
	gotBody    string
	rejectBody bool
}

func (f *fakeArchiveServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/v") {
		r.URL.Path = r.URL.Path[strings.Index(r.URL.Path, "/containers"):]
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/containers/c1/archive":
		w.Header().Set("X-Docker-Container-Path-Stat", f.getStat)
		w.WriteHeader(f.getStatus)
		io.WriteString(w, f.getBody)
	case r.Method == http.MethodPut && r.URL.Path == "/containers/c1/archive":
		f.gotMethod = r.Method
		f.gotQuery = r.URL.RawQuery
		f.gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		f.gotBody = string(b)
		w.WriteHeader(f.putStatus)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newArchiveClient(t *testing.T, f *fakeArchiveServer) *Client {
	t.Helper()
	c := New("")
	c.HTTP = &http.Client{}
	// 把全部流量重定向到假 Engine（Client 原生直连 unix socket 的
	// "http://docker" 占位地址，此处借 RoundTripper 改写目标）。
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c.HTTP.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = strings.TrimPrefix(srv.URL, "http://")
		return http.DefaultTransport.RoundTrip(req)
	})
	return c
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func statHeader(t *testing.T, s ArchiveStat) string {
	t.Helper()
	raw, _ := json.Marshal(s)
	return base64.StdEncoding.EncodeToString(raw)
}

func TestCopyFromContainerOK(t *testing.T) {
	f := &fakeArchiveServer{
		getStatus: 200, getBody: "TARDATA",
		getStat: statHeader(t, ArchiveStat{Name: "app.conf", Size: 7, Mode: 0o644}),
	}
	c := newArchiveClient(t, f)
	rc, stat, err := c.CopyFromContainer(context.Background(), "c1", "/etc/app.conf")
	if err != nil {
		t.Fatalf("CopyFromContainer: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "TARDATA" {
		t.Fatalf("body = %q, want TARDATA", data)
	}
	if stat.Name != "app.conf" || stat.Size != 7 {
		t.Fatalf("stat = %+v", stat)
	}
}

func TestCopyFromContainerNotFound(t *testing.T) {
	c := newArchiveClient(t, &fakeArchiveServer{getStatus: 404})
	_, _, err := c.CopyFromContainer(context.Background(), "c1", "/nope")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want 404 提示", err)
	}
}

func TestCopyFromContainerMissingStatHeader(t *testing.T) {
	c := newArchiveClient(t, &fakeArchiveServer{getStatus: 200, getBody: "x"})
	_, _, err := c.CopyFromContainer(context.Background(), "c1", "/etc/x")
	if err == nil || !strings.Contains(err.Error(), "X-Docker-Container-Path-Stat") {
		t.Fatalf("err = %v, want 缺 stat 头报错", err)
	}
}

func TestCopyFromContainerBadID(t *testing.T) {
	c := newArchiveClient(t, &fakeArchiveServer{getStatus: 200})
	if _, _, err := c.CopyFromContainer(context.Background(), "c 1", "/x"); err == nil {
		t.Fatal("非法 id 应被拒")
	}
	if _, _, err := c.CopyFromContainer(context.Background(), "c1", ""); err == nil {
		t.Fatal("空路径应被拒")
	}
}

func TestCopyToContainerOK(t *testing.T) {
	f := &fakeArchiveServer{putStatus: 200}
	c := newArchiveClient(t, f)
	err := c.CopyToContainer(context.Background(), "c1", "/tmp", strings.NewReader("TARBALL"),
		CopyToContainerOptions{NoOverwriteDirNonDir: true, CopyUIDGID: true})
	if err != nil {
		t.Fatalf("CopyToContainer: %v", err)
	}
	if f.gotCT != "application/x-tar" || f.gotBody != "TARBALL" {
		t.Fatalf("CT=%q body=%q", f.gotCT, f.gotBody)
	}
	for _, want := range []string{"path=%2Ftmp", "noOverwriteDirNonDir=1", "copyUIDGID=1"} {
		if !strings.Contains(f.gotQuery, want) {
			t.Fatalf("query = %q, 缺 %s", f.gotQuery, want)
		}
	}
}

func TestCopyToContainerRO(t *testing.T) {
	// :ro 写不进：Engine 报 403，本层原文透传（P2.2 才做友好提示）。
	c := newArchiveClient(t, &fakeArchiveServer{putStatus: 403})
	err := c.CopyToContainer(context.Background(), "c1", "/ro", strings.NewReader("x"), CopyToContainerOptions{})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v, want 403 透传", err)
	}
}

func TestCopyToContainerBadArgs(t *testing.T) {
	c := newArchiveClient(t, &fakeArchiveServer{putStatus: 200})
	if err := c.CopyToContainer(context.Background(), "bad id", "/tmp", strings.NewReader("x"), CopyToContainerOptions{}); err == nil {
		t.Fatal("非法 id 应被拒")
	}
	if err := c.CopyToContainer(context.Background(), "c1", "", strings.NewReader("x"), CopyToContainerOptions{}); err == nil {
		t.Fatal("空目录应被拒")
	}
	if err := c.CopyToContainer(context.Background(), "c1", "/tmp", nil, CopyToContainerOptions{}); err == nil {
		t.Fatal("空 tar 流应被拒")
	}
}
