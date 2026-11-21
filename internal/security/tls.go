package security

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

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
	if leaf.IsCA {
		return nil, nil, fmt.Errorf("服务端证书不能为 CA 证书")
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

func issuerRootKeyFingerprint(caRaw []byte, leaf *x509.Certificate, intermediates *x509.CertPool) ([32]byte, error) {
	caPool := x509.NewCertPool()
	var caCerts []*x509.Certificate
	rest := caRaw
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			continue
		}
		caPool.AddCert(cert)
		caCerts = append(caCerts, cert)
	}
	if len(caCerts) == 0 {
		return [32]byte{}, fmt.Errorf("CA 文件中没有有效证书")
	}

	verifyAt := func(at time.Time) ([][]*x509.Certificate, error) {
		return leaf.Verify(x509.VerifyOptions{
			Roots:         caPool,
			Intermediates: intermediates,
			CurrentTime:   at,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		})
	}
	chains, err := verifyAt(leaf.NotBefore)
	if err != nil {
		chains, err = verifyAt(leaf.NotBefore.Add(time.Second))
	}
	if err != nil {
		chains, err = verifyAt(time.Now())
	}
	if err != nil || len(chains) == 0 {
		return [32]byte{}, fmt.Errorf("无法确定客户端证书的信任根 CA：%w", err)
	}

	var best [32]byte
	for i, ch := range chains {
		if len(ch) == 0 {
			continue
		}
		fp := PubKeyFingerprint(ch[len(ch)-1])
		if i == 0 || bytes.Compare(fp[:], best[:]) < 0 {
			best = fp
		}
	}
	return best, nil
}

// ClientIdentityKey 根据受信任 CA 公钥、客户端证书主体与目标服务端生成确定性的身份标识，
// 用于队列与断点隔离，并在同一用户续期证书（服务端重签或 CSR 续期）后保持断点连续性。
func ClientIdentityKey(c config.Client) (string, error) {
	caRaw, err := pal.ReadSmall(c.TLS.CACertificate, MaxCertFileBytes)
	if err != nil {
		return "", err
	}
	pair, err := tls.LoadX509KeyPair(c.TLS.Certificate, c.TLS.PrivateKey)
	if err != nil {
		return "", err
	}
	leaf, intermediates, err := parseIntermediates(pair.Certificate)
	if err != nil {
		return "", err
	}
	rootKeyFP, err := issuerRootKeyFingerprint(caRaw, leaf, intermediates)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte("sft-identity-v2\x00"))
	h.Write(rootKeyFP[:])
	h.Write(leaf.RawSubject)
	h.Write([]byte{0})
	h.Write([]byte(c.ServerAddress))
	h.Write([]byte{0})
	h.Write([]byte(c.ServerName))
	return hex.EncodeToString(h.Sum(nil)), nil
}

// LegacyClientIdentityKey 返回旧版基于完整叶证书与公钥指纹的身份标识，仅用于迁移历史队列检查点。
func LegacyClientIdentityKey(c config.Client) (string, error) {
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
