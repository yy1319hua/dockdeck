package config

import "testing"

func TestLoad_RejectsWildcardBind(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8443", ":8443", "[::]:8443"} {
		c := defaults()
		c.ListenAddr = addr
		if err := c.Validate(); err == nil {
			t.Errorf("应拒绝绑定 %q", addr)
		}
	}
}

func TestLoad_PLAINTEXTOnlyLoopback(t *testing.T) {
	c := defaults()
	c.TLSMode = TLSModeNone
	c.ListenAddr = "192.0.2.5:8443" // 非回环 + 明文 → 拒
	if err := c.Validate(); err == nil {
		t.Fatal("明文非回环应被拒")
	}
	c.ListenAddr = "127.0.0.1:8443"
	if err := c.Validate(); err != nil {
		t.Fatalf("明文回环应允许: %v", err)
	}
}

func TestLoad_SelfSignedInternalIPOK(t *testing.T) {
	c := defaults()
	c.TLSMode = TLSModeSelf
	c.ListenAddr = "192.0.2.5:8443"
	if err := c.Validate(); err != nil {
		t.Fatalf("自签 + 内网/具体 IP 应允许: %v", err)
	}
}
