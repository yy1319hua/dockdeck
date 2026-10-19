// Package crypto 汇集 DockDeck 的密码哈希、令牌加密与请求签名原语。
// 全部基于标准库 + golang.org/x/crypto，无 CGO 依赖。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ---- 随机字节 ----

// RandomBytes 返回 n 字节密码学随机数。
func RandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, fmt.Errorf("crypto: 随机数生成失败: %w", err)
	}
	return b, nil
}

// RandomToken 生成 n 字节随机数并以 base64url（无填充）编码，用作各类令牌。
func RandomToken(n int) (string, error) {
	b, err := RandomBytes(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ---- Argon2id 密码哈希 ----

// ArgonParams 记录一次哈希所用参数，校验时原样从编码串取回，避免参数漂移。
type ArgonParams struct {
	Time    uint32
	Memory  uint32
	Threads uint32
	SaltLen uint32
}

// DefaultArgon 面向「登录偶发、需抗 GPU」场景，内存约 64MiB。
var DefaultArgon = ArgonParams{Time: 3, Memory: 64 * 1024, Threads: 4, SaltLen: 16}

// HashPassword 返回编码串：argon2id$v=19$m=,t=,p=$salt$hash（salt/hash 为 base64 RawStd）。
func HashPassword(password string, p ArgonParams) (string, error) {
	salt, err := RandomBytes(int(p.SaltLen))
	if err != nil {
		return "", err
	}
	dk := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, uint8(p.Threads), 32)
	return fmt.Sprintf("argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk)), nil
}

// VerifyPassword 解析编码串、按原参数重算并常量时间比对。
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false, errors.New("crypto: 密码编码格式非法")
	}
	if parts[1] != "v=19" {
		return false, fmt.Errorf("crypto: 不支持的 argon2 版本 %q", parts[1])
	}
	var m, t, pr uint32
	if _, err := fmt.Sscanf(parts[2], "m=%d,t=%d,p=%d", &m, &t, &pr); err != nil {
		return false, fmt.Errorf("crypto: 参数解析失败: %w", err)
	}
	if m == 0 || t == 0 || pr == 0 {
		return false, errors.New("crypto: argon2 参数非法（为 0）")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, fmt.Errorf("crypto: salt 解码失败: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("crypto: hash 解码失败: %w", err)
	}
	if pr > 255 {
		return false, errors.New("crypto: 并行度超限")
	}
	dk := argon2.IDKey([]byte(password), salt, t, m, uint8(pr), uint32(len(want)))
	return hmac.Equal(dk, want), nil
}

// ---- 会话令牌存储：仅保存其 SHA-256 ----

// HashToken 返回令牌的 SHA-256 十六进制，用于库里存会话/校验，明文不落库。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

// ---- 机器主密钥：AES-256-GCM ----

// SealSecret 用主密钥对明文做 AES-256-GCM，返回 base64(nonce||ciphertext)。
func SealSecret(key, plaintext []byte) (string, error) {
	if len(key) != 32 {
		return "", fmt.Errorf("crypto: 主密钥长度须为 32 字节，得到 %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// OpenSecret 解密 SealSecret 产物。
func OpenSecret(key []byte, encoded string) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: 主密钥长度须为 32 字节，得到 %d", len(key))
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("crypto: 密文 base64 解码失败: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return nil, errors.New("crypto: 密文过短")
	}
	nonce, ct := raw[:ns], raw[ns:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: 解密失败（主密钥不符或数据被篡改）")
	}
	return pt, nil
}

// LoadOrCreateMasterKey 读取密钥文件；不存在则生成 32 字节随机密钥并以 0600 落盘（base64 存储）。
func LoadOrCreateMasterKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		raw, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if derr != nil || len(raw) != 32 {
			return nil, errors.New("crypto: 主密钥文件内容非法")
		}
		return raw, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raw, err := RandomBytes(32)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(raw)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return raw, nil
}

// ---- 请求签名：HMAC-SHA256 ----

// SignRequest 计算 Hub→Agent 写操作签名，覆盖 timestamp、method、path、body。
// 各字段以换行分隔，避免拼接歧义。
func SignRequest(secret []byte, timestamp, method, path string, body []byte) string {
	nl := []byte{'\n'}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp))
	mac.Write(nl)
	mac.Write([]byte(method))
	mac.Write(nl)
	mac.Write([]byte(path))
	mac.Write(nl)
	mac.Write(body)
	return fmt.Sprintf("%x", mac.Sum(nil))
}

// VerifyRequest 常量时间校验签名。
func VerifyRequest(secret []byte, timestamp, method, path string, body []byte, sig string) bool {
	want := SignRequest(secret, timestamp, method, path, body)
	return hmac.Equal([]byte(want), []byte(strings.ToLower(sig)))
}
