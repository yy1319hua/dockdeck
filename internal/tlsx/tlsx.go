// Package tlsx 生成 Hub 的自签证书，用于 tailnet 上的 TLS。
// 自签意味着浏览器会警告——指纹显示在部署输出与设置页，供用户核对首连。
package tlsx

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// CertFiles 是自签证书的落盘位置。
type CertFiles struct{ CertPath, KeyPath string }

// EnsureSelfSigned 若证书不存在或 IP SAN 不覆盖当前 host，则重新生成，返回 tls 证书与指纹。
func EnsureSelfSigned(dir, host string, port int) (tls.Certificate, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, "", err
	}
	cf := CertFiles{filepath.Join(dir, "hub.crt"), filepath.Join(dir, "hub.key")}

	// 已有且包含所需 SAN → 直接复用，保证指纹在多次重启间稳定。
	if c, fp, err := loadIfValid(cf, host, port); err == nil {
		return c, fp, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "dockdeck-hub", Organization: []string{"DockDeck"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:         true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	// 一并放入回环，便于本机健康检查。
	tmpl.IPAddresses = append(tmpl.IPAddresses, net.IPv4(127, 0, 0, 1))

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(cf.CertPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(cf.KeyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	c, err := tls.LoadX509KeyPair(cf.CertPath, cf.KeyPath)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	fp := fingerprint(c.Certificate[0])
	return c, fp, nil
}

func loadIfValid(cf CertFiles, host string, port int) (tls.Certificate, string, error) {
	c, err := tls.LoadX509KeyPair(cf.CertPath, cf.KeyPath)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if len(c.Certificate) == 0 {
		return tls.Certificate{}, "", fmt.Errorf("空证书")
	}
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if time.Now().After(x.NotAfter) {
		return tls.Certificate{}, "", fmt.Errorf("证书过期")
	}
	// 校验 host 是否被 SAN 覆盖。
	var ok bool
	if ip := net.ParseIP(host); ip != nil {
		for _, a := range x.IPAddresses {
			if a.Equal(ip) {
				ok = true
			}
		}
	} else {
		for _, d := range x.DNSNames {
			if d == host {
				ok = true
			}
		}
	}
	_ = port
	if !ok {
		return tls.Certificate{}, "", fmt.Errorf("证书 SAN 不含 %q", host)
	}
	return c, fingerprint(c.Certificate[0]), nil
}

// fingerprint 返回十六进制 SHA-256（冒号分隔），供用户核对。
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	out := make([]byte, 0, len(sum)*3)
	for i, b := range sum {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, fmt.Sprintf("%02x", b)...)
	}
	return string(out)
}
