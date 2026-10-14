// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

// S4.3：ComposeFile 系列 DTO 的 JSON 线形状校验。

package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestComposeFileJSON(t *testing.T) {
	// GET 响应：config_file + content 两个字段都要在线上出现。
	f := ComposeFile{ConfigFile: "/opt/web/docker-compose.yml", Content: "services:\n  web:\n"}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal ComposeFile: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"config_file"`) {
		t.Errorf("ComposeFile 缺少 config_file 字段: %s", s)
	}
	if !strings.Contains(s, `"content"`) {
		t.Errorf("ComposeFile 缺少 content 字段: %s", s)
	}
	var got ComposeFile
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal ComposeFile: %v", err)
	}
	if got.ConfigFile != f.ConfigFile || got.Content != f.Content {
		t.Errorf("round-trip 不一致: got %+v want %+v", got, f)
	}
}

func TestComposeFileUpdateJSON(t *testing.T) {
	// 写请求体只有 content 一个字段（路径由 URL 项目名决定）。
	u := ComposeFileUpdate{Content: "version: '3'\n"}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal ComposeFileUpdate: %v", err)
	}
	if !strings.Contains(string(b), `"content"`) {
		t.Errorf("ComposeFileUpdate 缺少 content 字段: %s", b)
	}
	// 多余字段不应被编码进线（后端用 DisallowUnknownFields 解析，调用方也只发 content）。
	if strings.Contains(string(b), "config_file") {
		t.Errorf("ComposeFileUpdate 不应含 config_file: %s", b)
	}
	var got ComposeFileUpdate
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal ComposeFileUpdate: %v", err)
	}
	if got.Content != u.Content {
		t.Errorf("round-trip 不一致: got %q want %q", got.Content, u.Content)
	}
}

func TestComposeFileDiffJSON(t *testing.T) {
	d := ComposeFileDiff{Diff: "@@ -1,2 +1,2 @@\n- a\n+ b\n"}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal ComposeFileDiff: %v", err)
	}
	if !strings.Contains(string(b), `"diff"`) {
		t.Errorf("ComposeFileDiff 缺少 diff 字段: %s", b)
	}
	var got ComposeFileDiff
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal ComposeFileDiff: %v", err)
	}
	if got.Diff != d.Diff {
		t.Errorf("round-trip 不一致: got %q want %q", got.Diff, d.Diff)
	}
}

func TestComposeFileWrittenJSON(t *testing.T) {
	w := ComposeFileWritten{BackupFile: "/opt/web/docker-compose.yml.bak.2026-10-14T22:30:00+08:00"}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal ComposeFileWritten: %v", err)
	}
	if !strings.Contains(string(b), `"backup_file"`) {
		t.Errorf("ComposeFileWritten 缺少 backup_file 字段: %s", b)
	}
	var got ComposeFileWritten
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal ComposeFileWritten: %v", err)
	}
	if got.BackupFile != w.BackupFile {
		t.Errorf("round-trip 不一致: got %q want %q", got.BackupFile, w.BackupFile)
	}
}
