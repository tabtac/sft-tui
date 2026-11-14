package security

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"strings"

	"sft-tui/internal/config"
	"sft-tui/internal/pal"
)

func loadCA(path string) (*x509.CertPool, error) {
	p, err := pal.ReadSmall(path, MaxCertFileBytes)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(p) {
		return nil, fmt.Errorf("CA 文件中没有有效证书：%s", path)
	}
	return pool, nil
}

func parseIntermediates(chain [][]byte) (*x509.Certificate, *x509.CertPool, error) {
	if len(chain) == 0 {
		return nil, nil, fmt.Errorf("证书链为空")
	}
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, nil, err
	}
	intermediates := x509.NewCertPool()
	for _, der := range chain[1:] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, nil, err
		}
		intermediates.AddCert(cert)
	}
	return leaf, intermediates, nil
}

// ServerTLS 构建并绑定授权校验器的服务端 TLS 1.3 mTLS 配置。
func ServerTLS(c config.Server) (*tls.Config, *Authorizer, error) {
	cfg, ca, err := PrepareServerTLS(c)
	if err != nil {
		return nil, nil, err
	}
	auth, err := NewAuthorizer(c, ca)
	if err != nil {
		return nil, nil, err
	}
	BindAuthorizer(cfg, auth)
	return cfg, auth, nil
}

// PrepareServerTLS 在热重载或启动前完整验证服务端证书与 CA 信任链。
func PrepareServerTLS(c config.Server) (*tls.Config, *x509.CertPool, error) {
	ca, err := loadCA(c.TLS.CACertificate)
	if err != nil {
		return nil, nil, err
	}
	pair, err := tls.LoadX509KeyPair(c.TLS.Certificate, c.TLS.PrivateKey)
	if err != nil {
		return nil, nil, err
	}
	leaf, intermediates, err := parseIntermediates(pair.Certificate)
	if err != nil {
		return nil, nil, err
	}
	if _, err = leaf.Verify(x509.VerifyOptions{
		Roots:         ca,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return nil, nil, fmt.Errorf("服务端证书链校验失败：%w", err)
	}
	cfg := &tls.Config{
		MinVersion:             tls.VersionTLS13,
		MaxVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{pair},
		ClientCAs:              ca,
		ClientAuth:             tls.RequireAndVerifyClientCert,
		SessionTicketsDisabled: true,
	}
	return cfg, ca, nil
}

// BindAuthorizer 将证书指纹与策略校验绑定到 TLS 握手验证回调。
func BindAuthorizer(cfg *tls.Config, auth *Authorizer) {
	cfg.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return fmt.Errorf("缺少客户端证书")
		}
		_, err := auth.Certificate(state.PeerCertificates[0])
		return err
	}
}

// ClientTLS 构建客户端 TLS 1.3 mTLS 配置，校验服务端 CA 链、SAN（ServerName）及可选 SHA-256 指纹固定。
func ClientTLS(c config.Client) (*tls.Config, error) {
	ca, err := loadCA(c.TLS.CACertificate)
	if err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(c.TLS.Certificate, c.TLS.PrivateKey)
	if err != nil {
		return nil, err
	}
	leaf, intermediates, err := parseIntermediates(pair.Certificate)
	if err != nil {
		return nil, err
	}
	if leaf.IsCA {
		return nil, fmt.Errorf("客户端证书不能为 CA 证书")
	}
	if _, err = leaf.Verify(x509.VerifyOptions{
		Roots:         ca,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return nil, fmt.Errorf("客户端证书链校验失败：%w", err)
	}

	var expectedFP [32]byte
	hasExpectedFP := false
	if c.ServerFingerprint != "" {
		raw, err := hex.DecodeString(strings.TrimSpace(c.ServerFingerprint))
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("非法服务端 SHA-256 指纹")
		}
		copy(expectedFP[:], raw)
		hasExpectedFP = true
	}

	cfg := &tls.Config{
		MinVersion:             tls.VersionTLS13,
		MaxVersion:             tls.VersionTLS13,
		RootCAs:                ca,
		Certificates:           []tls.Certificate{pair},
		ServerName:             c.ServerName,
		SessionTicketsDisabled: true,
	}
	if hasExpectedFP {
		cfg.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("缺少服务端证书")
			}
			peer := state.PeerCertificates[0]
			if CertFingerprint(peer) != expectedFP && PubKeyFingerprint(peer) != expectedFP {
				return fmt.Errorf("服务端证书或公钥 SHA-256 指纹不匹配")
			}
			return nil
		}
	}
	return cfg, nil
}

// ClientIdentityKey 根据客户端证书、公钥指纹与目标服务端生成确定性的身份标识，用于队列与断点隔离。
func ClientIdentityKey(c config.Client) (string, error) {
	pair, err := tls.LoadX509KeyPair(c.TLS.Certificate, c.TLS.PrivateKey)
	if err != nil {
		return "", err
	}
	leaf, _, err := parseIntermediates(pair.Certificate)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	certFP := CertFingerprint(leaf)
	pubFP := PubKeyFingerprint(leaf)
	h.Write(certFP[:])
	h.Write(pubFP[:])
	h.Write([]byte(c.ServerAddress))
	h.Write([]byte{0})
	h.Write([]byte(c.ServerName))
	return hex.EncodeToString(h.Sum(nil)), nil
}
