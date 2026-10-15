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
