// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package dockercli

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// frame 构造一个 Docker 复用流帧：1 字节类型 + 3 字节填充 + 4 字节大端长度 + 载荷。
func frame(stream byte, payload string) []byte {
	b := make([]byte, frameHeaderLen+len(payload))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:8], uint32(len(payload)))
	copy(b[8:], payload)
	return b
}

// TestDemuxMergesStdoutAndStderr 是 GAP-9 的回归测试。
//
// Docker 的帧类型是 0=stdin / 1=stdout / 2=stderr。早期实现把 2 当「system」
// 丢弃，导致所有写 stderr 的程序（Go log、logrus）日志为空。
func TestDemuxMergesStdoutAndStderr(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "out-1\n"))
	buf.Write(frame(2, "err-1\n"))
	buf.Write(frame(1, "out-2\n"))
	buf.Write(frame(2, "err-2\n"))

	var got bytes.Buffer
	if err := Demux(&buf, &got); err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if want := "out-1\nerr-1\nout-2\nerr-2\n"; got.String() != want {
		t.Errorf("合流结果 = %q，want %q", got.String(), want)
	}
}

// TestDemuxSkipsUndefinedStream 验证未定义类型（3+）被丢弃但不中断后续帧。
func TestDemuxSkipsUndefinedStream(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "keep-1\n"))
	buf.Write(frame(3, "drop-me\n"))
	buf.Write(frame(2, "keep-2\n"))

	var got bytes.Buffer
	if err := Demux(&buf, &got); err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if want := "keep-1\nkeep-2\n"; got.String() != want {
		t.Errorf("结果 = %q，want %q", got.String(), want)
	}
}

// TestDemuxRawTTYStream 验证 TTY 裸流（首字节不是 0/1/2）原样透传。
func TestDemuxRawTTYStream(t *testing.T) {
	raw := []byte("plain tty output without frame headers")
	var got bytes.Buffer
	if err := Demux(bytes.NewReader(raw), &got); err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if !bytes.Equal(got.Bytes(), raw) {
		t.Errorf("裸流应原样输出，得 %q", got.String())
	}
}
