// Package testcert 生成供单元/集成测试使用的独立 Ed25519 mTLS 证书夹具。
package testcert

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
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

	"sft-tui/internal/config"
)

type output struct {
	name string
	data []byte
	mode os.FileMode
}

func material(name string, template, parent *x509.Certificate, signer crypto.Signer) ([]output, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template.SerialNumber = serial.Add(serial, big.NewInt(1))
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		parent, signer = template, private
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, public, signer)
	if err != nil {
		return nil, err
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, err
	}
	return []output{
		{name + ".pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644},
		{name + "-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600},
	}, nil
}

func writeFiles(dir string, files []output) (err error) {
	var created []string
	defer func() {
		if err != nil {
			for _, p := range created {
				_ = os.Remove(p)
			}
		}
	}()
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		handle, e := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, f.mode)
		if e != nil {
			return e
		}
		created = append(created, p)
		_, err = handle.Write(f.data)
		if err == nil {
			err = handle.Sync()
		}
		if closeErr := handle.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Generate 生成测试用 CA、localhost 服务端证书和默认客户端证书。
func Generate(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	now := time.Now()
	ca := &x509.Certificate{
		Subject:               pkix.Name{CommonName: "SFT Test CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(30 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	files, err := material("ca", ca, nil, nil)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(files[0].data, files[1].data)
	if err != nil {
		return err
	}
	for _, role := range []string{"server", "client"} {
		leaf := &x509.Certificate{
			Subject:     pkix.Name{CommonName: "SFT " + role},
			NotBefore:   ca.NotBefore,
			NotAfter:    ca.NotAfter,
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		if role == "server" {
			leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			leaf.DNSNames = []string{"localhost"}
			leaf.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
		}
		issued, err := material(role, leaf, ca, pair.PrivateKey.(crypto.Signer))
		if err != nil {
			return err
		}
		files = append(files, issued...)
	}
	return writeFiles(dir, files)
}

// IssueClient 使用已有 CA 为指定用户签发独立的客户端证书。
func IssueClient(dir, user string) error {
	if !config.ValidUserID(user) || user == "ca" || user == "server" {
		return fmt.Errorf("非法或保留的用户 ID：%q", user)
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	now := time.Now()
	if !ca.IsCA || now.Before(ca.NotBefore) || !now.Before(ca.NotAfter) {
		return fmt.Errorf("CA 无效或已过期")
	}
	leaf := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "SFT " + user},
		NotBefore:   now.Add(-time.Minute),
		NotAfter:    ca.NotAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	files, err := material(user, leaf, ca, pair.PrivateKey.(crypto.Signer))
	if err != nil {
		return err
	}
	return writeFiles(dir, files)
}
