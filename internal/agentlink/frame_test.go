// SPDX-License-Identifier: PolyForm-Noncommercial-1.0.0

package agentlink

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	in := Frame{
		T: TypeReq, ID: 42, V: 1, M: "GET", P: "/api/v1/containers?all=1",
		H: map[string]string{"Authorization": "Bearer x", "X-DD-Timestamp": "1790000000"},
		B: []byte("payload"),
	}
	raw, err := Encode(in)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	out, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if out.T != in.T || out.ID != in.ID || out.M != in.M || out.P != in.P {
		t.Fatalf("往返字段不一致: %+v", out)
	}
	if !bytes.Equal(out.B, in.B) {
		t.Fatalf("body 往返不一致: %q", out.B)
	}
	if out.H["Authorization"] != "Bearer x" {
		t.Fatalf("header 往返不一致: %+v", out.H)
	}
}

func TestFrameBodyLimits(t *testing.T) {
	// 请求上限 1 MiB
	ok := bytes.Repeat([]byte("a"), MaxReqBody)
	if _, err := Encode(Frame{T: TypeReq, M: "POST", P: "/x", B: ok}); err != nil {
		t.Errorf("恰好 1 MiB 应通过: %v", err)
	}
	if _, err := Encode(Frame{T: TypeReq, M: "POST", P: "/x", B: append(ok, 'b')}); err == nil {
		t.Error("超过 1 MiB 应报错")
	}
	// 响应上限 8 MiB
	if _, err := Encode(Frame{T: TypeResp, St: 200, B: bytes.Repeat([]byte("a"), MaxRespBody+1)}); err == nil {
		t.Error("响应超过 8 MiB 应报错")
	}
}

func TestFrameValidation(t *testing.T) {
	cases := []struct {
		name string
		f    Frame
		want bool // true = 应报错
	}{
		{"缺类型", Frame{}, true},
		{"未知类型", Frame{T: "wat"}, true},
		{"req 缺方法", Frame{T: TypeReq, P: "/x"}, true},
		{"req 缺路径", Frame{T: TypeReq, M: "GET"}, true},
		{"req 正常", Frame{T: TypeReq, M: "GET", P: "/x"}, false},
		{"resp 正常", Frame{T: TypeResp, St: 200}, false},
		{"ping", Frame{T: TypePing}, false},
		{"hello", Frame{T: TypeHello, V: 1, Caps: []string{CapBuffered}}, false},
		{"M2 帧结构合法", Frame{T: TypeData, ID: 1, Seq: 1, B: []byte("x")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Encode(c.f)
			if c.want && err == nil {
				t.Errorf("期望报错，实际通过")
			}
			if !c.want && err != nil {
				t.Errorf("期望通过，实际报错: %v", err)
			}
		})
	}
}

func TestFrameHasCap(t *testing.T) {
	f := Frame{Caps: []string{CapBuffered, CapPing}}
	if !f.HasCap(CapBuffered) || !f.HasCap(CapPing) {
		t.Error("应命中已声明能力")
	}
	if f.HasCap(CapStream) {
		t.Error("未声明的能力不应命中")
	}
}
