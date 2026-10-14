// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// 镜像端点测试：假 Engine 架在真 unix socket 上，Agent → dockercli →
// socket → 假 Engine 全链路真跑，只 mock Engine 本身（与容器测试同一思路）。

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// dataOf 解出信封的 data 层。
func dataOf(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解信封失败: %v (%s)", err, body)
	}
	return env.Data
}

func TestImages_List(t *testing.T) {
	srv, f := agentWith(t, "", "")
	f.imgList = []byte(`[{"Id":"sha256:aaaa","RepoTags":["alpine:3.20"],"Size":7890000,"Architecture":"arm64","Os":"linux"}]`)

	resp, body := hGET(t, srv, "/api/v1/images?all=true&dangling=1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d: %s", resp.StatusCode, body)
	}
	var list []struct {
		ID           string   `json:"Id"`
		RepoTags     []string `json:"RepoTags"`
		Architecture string   `json:"Architecture"`
	}
	if err := json.Unmarshal(dataOf(t, body), &list); err != nil {
		t.Fatalf("解 data 失败: %v", err)
	}
	if len(list) != 1 || list[0].RepoTags[0] != "alpine:3.20" || list[0].Architecture != "arm64" {
		t.Errorf("清单解码不对: %s", dataOf(t, body))
	}
	// all 原样抵达；dangling=1 在 Agent→Engine 一跳翻译成 Engine 的
	// filters 形态（固定值构造，见 dockercli.ListImages 头注）。
	if got := f.nextPath(t); got != "GET /images/json?all=true&filters=%7B%22dangling%22%3A%5B%22true%22%5D%7D" {
		t.Errorf("查询透传不对: %s", got)
	}
}

func TestImages_Pull(t *testing.T) {
	srv, f := agentWith(t, "", "")
	layer := strings.Repeat("a", 64)
	f.pullStream = []byte(`{"status":"Pulling from library/alpine","id":"3.20"}
{"status":"Pull complete","id":"` + layer + `"}
{"status":"Downloaded newer image for alpine:3.20"}
`)

	resp, body := hDo(t, srv, http.MethodPost, "/api/v1/images/pull", `{"ref":"alpine:3.20"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d: %s", resp.StatusCode, body)
	}
	var res struct {
		Status string   `json:"Status"`
		Layers []string `json:"Layers"`
	}
	if err := json.Unmarshal(dataOf(t, body), &res); err != nil {
		t.Fatalf("解 data 失败: %v", err)
	}
	if res.Status != "Downloaded newer image for alpine:3.20" || len(res.Layers) != 1 {
		t.Errorf("pull 汇总不对: %s", dataOf(t, body))
	}
	// ref 拆成 fromImage/tag 抵达 Engine。
	uri := f.nextPath(t)
	if !strings.Contains(uri, "fromImage=alpine") || !strings.Contains(uri, "tag=3.20") {
		t.Errorf("Engine 查询串不对: %s", uri)
	}
}

func TestImages_Pull_StreamErrorIs502(t *testing.T) {
	srv, f := agentWith(t, "", "")
	f.pullStream = []byte(`{"status":"Pulling from library/notexist"}
{"error":"pull access denied for notexist"}
`)

	resp, body := hDo(t, srv, http.MethodPost, "/api/v1/images/pull", `{"ref":"notexist"}`)
	// HTTP 200 但流内业务失败：Agent 层映射 502，文案带 Engine 原话。
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 %d, want 502: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "pull access denied") {
		t.Errorf("错误文案缺 Engine 原话: %s", body)
	}
}

func TestImages_Pull_EmptyRefRejectedLocally(t *testing.T) {
	srv, f := agentWith(t, "", "")

	resp, body := hDo(t, srv, http.MethodPost, "/api/v1/images/pull", `{}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 %d, want 400: %s", resp.StatusCode, body)
	}
	// Engine 不该收到任何请求——校验在出门前。
	select {
	case p := <-f.paths:
		t.Errorf("空 ref 不该打到 Engine: %s", p)
	default:
	}
}

func TestImages_Pull_BadRefRejectedLocally(t *testing.T) {
	srv, f := agentWith(t, "", "")

	resp, _ := hDo(t, srv, http.MethodPost, "/api/v1/images/pull", `{"ref":"../etc"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 %d, want 400", resp.StatusCode)
	}
	select {
	case p := <-f.paths:
		t.Errorf("坏 ref 不该打到 Engine: %s", p)
	default:
	}
}

func TestImages_Delete(t *testing.T) {
	srv, f := agentWith(t, "", "")

	resp, body := hDo(t, srv, http.MethodDelete, "/api/v1/images/alpine:3.20", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"ok"`) {
		t.Errorf("响应体不对: %s", body)
	}
	if got := f.nextPath(t); got != "DELETE /images/alpine:3.20" {
		t.Errorf("删除路径不对: %s", got)
	}
}

// TestImages_Delete_MultiSegmentRef 钉住尾通配符路由：含 "/" 的引用
// （library/nginx）单段 {ref} 匹配不到，{ref...} 才行。这是删官方库
// 镜像的日常路径，回归防线。
func TestImages_Delete_MultiSegmentRef(t *testing.T) {
	srv, f := agentWith(t, "", "")

	resp, _ := hDo(t, srv, http.MethodDelete, "/api/v1/images/library/nginx:latest", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d, want 200", resp.StatusCode)
	}
	if got := f.nextPath(t); got != "DELETE /images/library/nginx:latest" {
		t.Errorf("多段 ref 路径不对: %s", got)
	}
}

func TestImages_Delete_NotFound(t *testing.T) {
	srv, f := agentWith(t, "", "")
	f.deleteStatus = 404

	resp, body := hDo(t, srv, http.MethodDelete, "/api/v1/images/nothere", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码 %d, want 404: %s", resp.StatusCode, body)
	}
}

func TestImages_Prune(t *testing.T) {
	srv, f := agentWith(t, "", "")
	f.pruneBody = []byte(`{"ImagesDeleted":[{"Deleted":"sha256:cccc"}],"SpaceReclaimed":24000000}`)

	resp, body := hDo(t, srv, http.MethodPost, "/api/v1/images/prune", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 %d: %s", resp.StatusCode, body)
	}
	var res struct {
		ImagesDeleted []struct {
			Deleted string `json:"Deleted"`
		} `json:"ImagesDeleted"`
		SpaceReclaimed int64 `json:"SpaceReclaimed"`
	}
	if err := json.Unmarshal(dataOf(t, body), &res); err != nil {
		t.Fatalf("解 data 失败: %v", err)
	}
	if len(res.ImagesDeleted) != 1 || res.SpaceReclaimed != 24000000 {
		t.Errorf("prune 结果不对: %s", dataOf(t, body))
	}
	if got := f.nextPath(t); got != "POST /images/prune" {
		t.Errorf("prune 路径不对: %s", got)
	}
}
