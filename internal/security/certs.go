package security

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"time"

	"sft-tui/internal/pal"
)

const MaxCertFileBytes = 64 * 1024

// CertFingerprint 返回 X.509 证书 DER 原文的 SHA-256 指纹。
func CertFingerprint(cert *x509.Certificate) [32]byte {
	return sha256.Sum256(cert.Raw)
}

// CertFingerprintHex 返回十六进制证书指纹。
func CertFingerprintHex(cert *x509.Certificate) string {
	fp := CertFingerprint(cert)
	return hex.EncodeToString(fp[:])
}

// PubKeyFingerprint 返回 X.509 证书 SubjectPublicKeyInfo (SPKI) 的 SHA-256 公钥指纹。
func PubKeyFingerprint(cert *x509.Certificate) [32]byte {
	return sha256.Sum256(cert.RawSubjectPublicKeyInfo)
}

// PubKeyFingerprintHex 返回十六进制公钥指纹。
func PubKeyFingerprintHex(cert *x509.Certificate) string {
	fp := PubKeyFingerprint(cert)
	return hex.EncodeToString(fp[:])
}

// ReadCertificate 读取并解析单个 PEM X.509 证书文件。
func ReadCertificate(path string) (*x509.Certificate, error) {
	data, err := pal.ReadSmall(path, MaxCertFileBytes)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("文件不是有效的 PEM 证书：%s", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

// CertificateInspection 为展示用的证书检查快照，不作授权判定依据。
type CertificateInspection struct {
	Certificate       *x509.Certificate
	Fingerprint       string
	PubKeyFingerprint string
	State             string
	Error             error
}

// InspectCertificate 检查证书状态与指纹摘要。
func InspectCertificate(path string, now time.Time) CertificateInspection {
	cert, err := ReadCertificate(path)
	if err != nil {
		return CertificateInspection{State: "无法读取", Error: err}
	}
	state := "有效"
	switch {
	case now.Before(cert.NotBefore):
		state = "尚未生效"
	case !now.Before(cert.NotAfter):
		state = "已过期"
	case cert.NotAfter.Sub(now) < 7*24*time.Hour:
		state = "7 天内到期"
	}
	return CertificateInspection{
		Certificate:       cert,
		Fingerprint:       CertFingerprintHex(cert),
		PubKeyFingerprint: PubKeyFingerprintHex(cert),
		State:             state,
	}
}
