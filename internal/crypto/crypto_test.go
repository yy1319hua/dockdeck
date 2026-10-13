package crypto

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestArgon2_HashVerify(t *testing.T) {
	pw := "correct horse battery staple"
	enc, err := HashPassword(pw, DefaultArgon)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if enc == pw {
		t.Fatal("哈希结果等于明文")
	}
	ok, err := VerifyPassword(pw, enc)
	if err != nil || !ok {
		t.Fatalf("正确口令校验失败 ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword("wrong", enc)
	if err != nil {
		t.Fatalf("VerifyPassword(wrong): %v", err)
	}
	if ok {
		t.Fatal("错误口令却校验通过")
	}
}

func TestArgon2_MalformedEncoding(t *testing.T) {
	for _, bad := range []string{"", "plaintext", "argon2id$v=19$m=1,t=1,p=1$xx", "md5$1$2$3$4$5"} {
		if _, err := VerifyPassword("x", bad); err == nil {
			t.Errorf("应报错但通过: %q", bad)
		}
	}
}

func TestSealOpen(t *testing.T) {
	key, err := RandomBytes(32)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("agent-secret-token-xyz")
	ct, err := SealSecret(key, plain)
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	if bytes.Contains([]byte(ct), plain) {
		t.Fatal("密文疑似包含明文")
	}
	got, err := OpenSecret(key, ct)
	if err != nil {
		t.Fatalf("OpenSecret: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("解密不符: got=%q want=%q", got, plain)
	}

	// 错误密钥应解密失败。
	other, _ := RandomBytes(32)
	if _, err := OpenSecret(other, ct); err == nil {
		t.Fatal("错误密钥竟解密成功")
	}
}

func TestSignVerify(t *testing.T) {
	secret := []byte("machine-master-key")
	ts := "2026-10-13T00:00:00Z"
	body := []byte(`{"action":"stop"}`)
	sig := SignRequest(secret, ts, "POST", "/api/v1/containers/1/stop", body)
	if !VerifyRequest(secret, ts, "POST", "/api/v1/containers/1/stop", body, sig) {
		t.Fatal("签名校验失败")
	}
	// 篡改 path / body 都应失败。
	if VerifyRequest(secret, ts, "POST", "/api/v1/containers/2/stop", body, sig) {
		t.Fatal("篡改 path 竟通过")
	}
	if VerifyRequest(secret, ts, "POST", "/api/v1/containers/1/stop", []byte("x"), sig) {
		t.Fatal("篡改 body 竟通过")
	}
}

func TestMasterKey_LoadOrCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")

	k1, err := LoadOrCreateMasterKey(path)
	if err != nil {
		t.Fatalf("首次加载: %v", err)
	}
	if len(k1) != 32 {
		t.Fatalf("密钥长度 %d != 32", len(k1))
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("密钥文件权限 %o != 600", perm)
	}
	// 二次加载应得到同样的密钥。
	k2, err := LoadOrCreateMasterKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("二次加载密钥不一致")
	}
}
