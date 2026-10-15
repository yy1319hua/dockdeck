// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// S5：文件写入/动作结果的线形状。字段名是对外契约，改它要同步 App 与 OpenAPI，
// 所以在这里钉住。
func TestFileWrittenJSON(t *testing.T) {
	w := FileWritten{
		Path:       "notes.txt",
		BackupFile: "notes.txt.bak.2026-10-15T12:00:00+08:00",
		Size:       6,
		Created:    false,
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal FileWritten: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"path":"notes.txt"`,
		`"backup_file":"notes.txt.bak.2026-10-15T12:00:00+08:00"`,
		`"size":6`,
		`"created":false`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("FileWritten 缺少 %s: %s", want, s)
		}
	}
	var got FileWritten
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal FileWritten: %v", err)
	}
	if got != w {
		t.Errorf("往返不一致：got %+v want %+v", got, w)
	}

	// 新文件没有备份可回滚：backup_file 应当整个不出现，而不是空串。
	nb, err := json.Marshal(FileWritten{Path: "new.txt", Size: 3, Created: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(nb), "backup_file") {
		t.Errorf("无备份时不该出现 backup_file: %s", nb)
	}
}

// 上传上限是 Hub/Agent 共享的硬数字，钉住它：改它必须是有意识的两侧同步发版。
func TestMaxFileUploadBytesIsPinned(t *testing.T) {
	if MaxFileUploadBytes != 16<<20 {
		t.Errorf("MaxFileUploadBytes = %d，want %d（改它要同步 Hub 转发侧的读 body 上限）",
			MaxFileUploadBytes, 16<<20)
	}
}

// 空清单必须是 []，不是 null——与容器/镜像清单同一口径。
func TestFilesListJSON(t *testing.T) {
	b, err := json.Marshal(FilesList{Path: "", Entries: []FileEntry{}, Truncated: false})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{`"path":""`, `"entries":[]`, `"truncated":false`} {
		if !strings.Contains(s, want) {
			t.Errorf("FilesList 缺少 %s: %s", want, s)
		}
	}
	if strings.Contains(s, `"entries":null`) {
		t.Errorf("空清单不该序列化成 null: %s", s)
	}

	full := FilesList{
		Path:      "sub",
		Entries:   []FileEntry{{Name: "a.txt", Path: "sub/a.txt", Size: 3, Mode: "0644"}},
		Truncated: true,
	}
	b2, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b2), `"truncated":true`) {
		t.Errorf("截断标记应出现在响应体里: %s", b2)
	}
	var got FilesList
	if err := json.Unmarshal(b2, &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != "sub" || len(got.Entries) != 1 || got.Entries[0].Name != "a.txt" || !got.Truncated {
		t.Errorf("往返不一致：%+v", got)
	}
}

func TestFileActionDoneJSON(t *testing.T) {
	rename := FileActionDone{Action: ActionRename, Path: "a.txt", Dest: "b.txt"}
	b, err := json.Marshal(rename)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{`"action":"rename"`, `"path":"a.txt"`, `"dest":"b.txt"`} {
		if !strings.Contains(s, want) {
			t.Errorf("FileActionDone 缺少 %s: %s", want, s)
		}
	}
	var got FileActionDone
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != rename {
		t.Errorf("往返不一致：got %+v want %+v", got, rename)
	}

	// delete 没有目标路径，dest 应缺席。
	del, err := json.Marshal(FileActionDone{Action: ActionDelete, Path: "gone.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(del), "dest") {
		t.Errorf("delete 不该带 dest: %s", del)
	}
}
