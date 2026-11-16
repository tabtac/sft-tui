package security

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/pal"
)

// CertificateMaterial 保存签发生成的 PEM 编码证书、可选 PKCS#8 私钥及解析后的 X.509 结构体。
type CertificateMaterial struct {
	Certificate []byte
	PrivateKey  []byte
	Leaf        *x509.Certificate
}

func certificateLifetime(days int) (time.Time, time.Time, error) {
	if days < 1 || days > 3650 {
		return time.Time{}, time.Time{}, fmt.Errorf("有效期必须为 1–3650 天")
	}
	now := time.Now()
	return now.Add(-time.Minute), now.Add(time.Duration(days) * 24 * time.Hour), nil
}

func certificateSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1)))
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}

func encodeCertificate(template, parent *x509.Certificate, public any, signer any, private any) (CertificateMaterial, error) {
	der, err := x509.CreateCertificate(rand.Reader, template, parent, public, signer)
	if err != nil {
		return CertificateMaterial{}, err
	}
	var keyPEM []byte
	if private != nil {
		key, err := x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			return CertificateMaterial{}, err
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return CertificateMaterial{}, err
	}
	return CertificateMaterial{
		Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		PrivateKey:  keyPEM,
		Leaf:        leaf,
	}, nil
}

// NewCA 生成或续期自签名 Ed25519 CA 信任锚；续期时沿用原私钥与 Subject。
func NewCA(days int, renewCertificate, renewKey string, commonName ...string) (CertificateMaterial, error) {
	before, after, err := certificateLifetime(days)
	if err != nil {
		return CertificateMaterial{}, err
	}
	serial, err := certificateSerial()
	if err != nil {
		return CertificateMaterial{}, err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return CertificateMaterial{}, err
	}
	ca := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "SFT Local CA"},
		NotBefore:             before,
		NotAfter:              after,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	if len(commonName) > 0 && commonName[0] != "" {
		ca.Subject.CommonName = commonName[0]
	}
	var public, private any = pub, key
	if renewCertificate != "" {
		pair, err := tls.LoadX509KeyPair(renewCertificate, renewKey)
		if err != nil {
			return CertificateMaterial{}, err
		}
		old, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return CertificateMaterial{}, err
		}
		if !old.IsCA || old.KeyUsage&x509.KeyUsageCertSign == 0 || old.CheckSignatureFrom(old) != nil {
			return CertificateMaterial{}, fmt.Errorf("仅支持续期自签名 CA")
		}
		if !after.After(old.NotAfter) {
			return CertificateMaterial{}, fmt.Errorf("CA 续期必须延长现有到期时间")
		}
		ca.Subject = old.Subject
		if old.NotBefore.Before(before) {
			ca.NotBefore = old.NotBefore
		}
		public, private = old.PublicKey, pair.PrivateKey
	}
	return encodeCertificate(ca, ca, public, private, private)
}

// ParseServerNames 解析并校验逗号分隔的服务端 SAN（DNS 与 IP）。
func ParseServerNames(names string) ([]string, []net.IP, error) {
	var dns []string
	var ips []net.IP
	seen := make(map[string]bool)
	for _, name := range strings.Split(names, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, nil, fmt.Errorf("服务端 SAN 不得为空")
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		if ip := net.ParseIP(name); ip != nil {
			ips = append(ips, ip)
			continue
		}
		if len(name) > 253 {
			return nil, nil, fmt.Errorf("非法 DNS 名称：%q", name)
		}
		for i, label := range strings.Split(name, ".") {
			if i == 0 && label == "*" {
				continue
			}
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, nil, fmt.Errorf("非法 DNS 名称：%q", name)
			}
			for _, r := range label {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
					return nil, nil, fmt.Errorf("非法 DNS 名称：%q", name)
				}
			}
		}
		dns = append(dns, name)
	}
	return dns, ips, nil
}

// ValidateConnectAddress 校验客户端连接地址（host:port，禁止通配地址并校验域名/IP 格式）。
func ValidateConnectAddress(address string) (string, string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return "", "", fmt.Errorf("请输入主机:端口，IPv6 使用 [地址]:端口：%w", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("端口须为 1–65535")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "", "", fmt.Errorf("客户端连接地址必须为实际 IP 或 DNS，不能使用通配监听地址")
	}
	if _, _, err = ParseServerNames(host); err != nil {
		return "", "", err
	}
	return host, port, nil
}

// NewLeaf 使用磁盘上的 CA 证书和私钥签发带新 Ed25519 密钥对的叶证书（user 为空表示服务端证书）。
func NewLeaf(caCertificate, caKey, user, hosts string, days int, commonName ...string) (CertificateMaterial, error) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return CertificateMaterial{}, err
	}
	pair, err := tls.LoadX509KeyPair(caCertificate, caKey)
	if err != nil {
		return CertificateMaterial{}, err
	}
	return issueLeaf(pair, user, hosts, days, pub, key, commonName...)
}

// NewLeafFromCA 使用内存中的已生成 CA 材料签发叶证书，支持多文件原子初始化发布。
func NewLeafFromCA(ca CertificateMaterial, user, hosts string, days int, commonName ...string) (CertificateMaterial, error) {
	pair, err := tls.X509KeyPair(ca.Certificate, ca.PrivateKey)
	if err != nil {
		return CertificateMaterial{}, err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return CertificateMaterial{}, err
	}
	return issueLeaf(pair, user, hosts, days, pub, key, commonName...)
}

// ReadCSR 安全读取并校验证书签名请求文件及签名完整性。
func ReadCSR(path string) (*x509.CertificateRequest, error) {
	data, err := pal.ReadSmall(path, MaxCertFileBytes)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("CSR 必须仅包含一个 CERTIFICATE REQUEST")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}
	return csr, nil
}

// SignCSRExpected 签发客户端提交的 CSR（校验可选指纹以防 TOCTOU，私钥始终留在客户端）。
func SignCSRExpected(caCertificate, caKey, user, path string, days int, expectedFingerprint string) (CertificateMaterial, error) {
	if user == "" {
		return CertificateMaterial{}, fmt.Errorf("CSR 签发必须指定用户")
	}
	csr, err := ReadCSR(path)
	if err != nil {
		return CertificateMaterial{}, err
	}
	if expectedFingerprint != "" && expectedFingerprint != fmt.Sprintf("%x", sha256.Sum256(csr.Raw)) {
		return CertificateMaterial{}, fmt.Errorf("CSR 在校验后发生变化，请重新确认")
	}
	pair, err := tls.LoadX509KeyPair(caCertificate, caKey)
	if err != nil {
		return CertificateMaterial{}, err
	}
	return issueLeaf(pair, user, "", days, csr.PublicKey, nil)
}

func issueLeaf(pair tls.Certificate, user, hosts string, days int, public, private any, commonName ...string) (CertificateMaterial, error) {
	before, after, err := certificateLifetime(days)
	if err != nil {
		return CertificateMaterial{}, err
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return CertificateMaterial{}, err
	}
	now := time.Now()
	if !ca.IsCA || ca.KeyUsage&x509.KeyUsageCertSign == 0 || now.Before(ca.NotBefore) || !now.Before(ca.NotAfter) {
		return CertificateMaterial{}, fmt.Errorf("CA 不具备签发能力、尚未生效或已经过期")
	}
	serial, err := certificateSerial()
	if err != nil {
		return CertificateMaterial{}, err
	}
	notBefore := before
	if ca.NotBefore.After(notBefore) {
		notBefore = ca.NotBefore
	}
	notAfter := after
	if ca.NotAfter.Before(notAfter) {
		notAfter = ca.NotAfter
	}
	leaf := &x509.Certificate{
		SerialNumber: serial,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if user == "" {
		leaf.Subject = pkix.Name{CommonName: "SFT server"}
		if len(commonName) > 0 && commonName[0] != "" {
			leaf.Subject.CommonName = commonName[0]
		}
		leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		leaf.DNSNames, leaf.IPAddresses, err = ParseServerNames(hosts)
		if err != nil {
			return CertificateMaterial{}, err
		}
	} else {
		if !config.ValidUserID(user) || user == "ca" || user == "server" {
			return CertificateMaterial{}, fmt.Errorf("非法或保留的用户 ID")
		}
		leaf.Subject = pkix.Name{CommonName: "SFT " + user}
		leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	return encodeCertificate(leaf, ca, public, pair.PrivateKey, private)
}
