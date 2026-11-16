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
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/testcert"
)

func testCertConfig(t *testing.T) (string, config.Server, config.Client) {
	t.Helper()
	dir := t.TempDir()
	if err := testcert.Generate(dir); err != nil {
		t.Fatalf("testcert.Generate: %v", err)
	}
	if err := testcert.IssueClient(dir, "alice"); err != nil {
		t.Fatalf("IssueClient alice: %v", err)
	}
	if err := testcert.IssueClient(dir, "bob"); err != nil {
		t.Fatalf("IssueClient bob: %v", err)
	}

	srv := config.DefaultServer()
	srv.StorageRoot = filepath.Join(dir, "storage")
	srv.TLS = config.TLSFiles{
		CACertificate: filepath.Join(dir, "ca.pem"),
		Certificate:   filepath.Join(dir, "server.pem"),
		PrivateKey:    filepath.Join(dir, "server-key.pem"),
	}
	srv.AuthorizedClients = []config.AuthorizedClient{
		{UserID: "alice", Certificate: filepath.Join(dir, "alice.pem"), Permissions: []string{"upload", "download"}},
	}

	cli := config.DefaultClient()
	cli.StateDir = filepath.Join(dir, "state")
	cli.ServerName = "localhost"
	cli.TLS = config.TLSFiles{
		CACertificate: filepath.Join(dir, "ca.pem"),
		Certificate:   filepath.Join(dir, "alice.pem"),
		PrivateKey:    filepath.Join(dir, "alice-key.pem"),
	}
	return dir, srv, cli
}

func dialTLSOverLoopback(t *testing.T, srvTLS, cliTLS *tls.Config) (srvErr error, cliErr error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srvCh := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			srvCh <- err
			return
		}
		defer c.Close()
		sc := tls.Server(c, srvTLS)
		_ = sc.SetDeadline(time.Now().Add(3 * time.Second))
		srvCh <- sc.Handshake()
	}()

	raw, err := net.DialTimeout("tcp", ln.Addr().String(), 3*time.Second)
	if err != nil {
		return <-srvCh, err
	}
	defer raw.Close()
	cc := tls.Client(raw, cliTLS)
	_ = cc.SetDeadline(time.Now().Add(3 * time.Second))
	cliErr = cc.Handshake()
	_ = cc.Close()
	srvErr = <-srvCh
	return srvErr, cliErr
}

func TestMTLSHandshakeSANAndFingerprintPinning(t *testing.T) {
	dir, srvCfg, cliCfg := testCertConfig(t)

	srvTLS, auth, err := ServerTLS(srvCfg)
	if err != nil {
		t.Fatalf("ServerTLS: %v", err)
	}

	// 读取服务端指纹并启用指纹固定
	srvCert, err := ReadCertificate(srvCfg.TLS.Certificate)
	if err != nil {
		t.Fatalf("ReadCertificate: %v", err)
	}
	cliCfg.ServerFingerprint = CertFingerprintHex(srvCert)

	cliTLS, err := ClientTLS(cliCfg)
	if err != nil {
		t.Fatalf("ClientTLS: %v", err)
	}

	// 1. 合法 mTLS 双向握手 + 身份指纹验证
	if srvErr, cliErr := dialTLSOverLoopback(t, srvTLS, cliTLS); srvErr != nil || cliErr != nil {
		t.Fatalf("valid mTLS handshake failed: srv=%v cli=%v", srvErr, cliErr)
	}

	// 校验主体的公钥指纹、Subject 与权限
	aliceCert, err := ReadCertificate(filepath.Join(dir, "alice.pem"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := auth.Certificate(aliceCert)
	if err != nil {
		t.Fatalf("auth.Certificate alice: %v", err)
	}
	if p.UserID != "alice" || p.PubKeyFingerprint != PubKeyFingerprint(aliceCert) || !p.Allows("upload") {
		t.Fatalf("unexpected principal: %+v", p)
	}

	// 2. 服务端 SAN 不匹配必须被拒绝
	badSAN := cliCfg
	badSAN.ServerName = "wrong.example.com"
	badSAN.ServerFingerprint = ""
	badSANTLS, err := ClientTLS(badSAN)
	if err != nil {
		t.Fatal(err)
	}
	if _, cliErr := dialTLSOverLoopback(t, srvTLS, badSANTLS); cliErr == nil {
		t.Fatal("expected SAN mismatch handshake to fail")
	}

	// 3. 错误服务端指纹必须被拒绝
	badPin := cliCfg
	badPin.ServerFingerprint = CertFingerprintHex(aliceCert)
	badPinTLS, err := ClientTLS(badPin)
	if err != nil {
		t.Fatal(err)
	}
	if _, cliErr := dialTLSOverLoopback(t, srvTLS, badPinTLS); cliErr == nil {
		t.Fatal("expected fingerprint mismatch handshake to fail")
	}

	// 4. 未授权用户 bob 即使由同一 CA 签发也必须被服务端拒绝
	bobCert, err := ReadCertificate(filepath.Join(dir, "bob.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Certificate(bobCert); err == nil {
		t.Fatal("expected unauthorized user bob to be rejected")
	}
}

func TestAuthorizerHotReloadRevokesActiveConnection(t *testing.T) {
	dir, srvCfg, _ := testCertConfig(t)
	_, auth, err := ServerTLS(srvCfg)
	if err != nil {
		t.Fatal(err)
	}

	aliceCert, err := ReadCertificate(filepath.Join(dir, "alice.pem"))
	if err != nil {
		t.Fatal(err)
	}

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	if _, err := auth.Register(aliceCert, c1); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 热重载禁用 alice，已注册连接应被立即关闭
	srvCfg.AuthorizedClients[0].Disabled = true
	if err := auth.Reload(srvCfg.AuthorizedClients); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	_ = c2.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if _, err := c2.Read(buf); err == nil {
		t.Fatal("expected revoked connection to be closed on hot reload")
	}
}

func writeCertificateMaterial(t *testing.T, m CertificateMaterial) (string, string) {
	t.Helper()
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, m.Certificate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, m.PrivateKey, 0600); err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func TestCertificateLifetimeAndIssuerBounds(t *testing.T) {
	ca, err := NewCA(1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, days := range []int{-1, 0, 3651} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			if _, err := NewCA(days, "", ""); err == nil {
				t.Error("invalid CA lifetime accepted")
			}
			if _, err := NewLeafFromCA(ca, "alice", "", days); err == nil {
				t.Error("invalid leaf lifetime accepted")
			}
		})
	}
	leaf, err := NewLeafFromCA(ca, "alice", "", 30)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Leaf.NotBefore.Before(ca.Leaf.NotBefore) || leaf.Leaf.NotAfter.After(ca.Leaf.NotAfter) {
		t.Error("leaf validity exceeds its issuer")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Leaf)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("issued client certificate is unusable: %v", err)
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Error("client certificate unexpectedly authorizes TLS server role")
	}
}

func TestCARenewalRetainsKeyAndRejectsShortening(t *testing.T) {
	ca, err := NewCA(2, "", "", "test CA")
	if err != nil {
		t.Fatal(err)
	}
	cert, key := writeCertificateMaterial(t, ca)
	if _, err := NewCA(1, cert, key); err == nil {
		t.Error("CA renewal shortened existing validity")
	}
	renewed, err := NewCA(3, cert, key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ca.Leaf.RawSubjectPublicKeyInfo, renewed.Leaf.RawSubjectPublicKeyInfo) || ca.Leaf.Subject.String() != renewed.Leaf.Subject.String() {
		t.Error("renewal changed CA identity")
	}
	if !renewed.Leaf.NotAfter.After(ca.Leaf.NotAfter) || renewed.Leaf.SerialNumber.Cmp(ca.Leaf.SerialNumber) == 0 {
		t.Error("renewal did not extend validity with a new serial")
	}
}

func TestServerNameParsingRejectsInvalidSANs(t *testing.T) {
	for _, name := range []string{"", ",localhost", "localhost,", "a..b", "-bad.example", "bad-.example", "https://example.com", "example.com:443", "a b", strings.Repeat("a", 64) + ".com"} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseServerNames(name); err == nil {
				t.Errorf("invalid SAN accepted: %q", name)
			}
		})
	}
	dns, ips, err := ParseServerNames("localhost,127.0.0.1,::1,localhost")
	if err != nil || len(dns) != 1 || len(ips) != 2 {
		t.Errorf("SAN deduplication/IP parsing: dns=%v ips=%v err=%v", dns, ips, err)
	}
}

func TestCSRSignatureFingerprintAndPrivateKeyIsolation(t *testing.T) {
	ca, err := NewCA(2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	cert, key := writeCertificateMaterial(t, ca)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "external"}}, private)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "user.csr")
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SignCSRExpected(cert, key, "alice", path, 1, strings.Repeat("0", 64)); err == nil {
		t.Error("stale CSR fingerprint accepted")
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(der))
	issued, err := SignCSRExpected(cert, key, "alice", path, 1, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if len(issued.PrivateKey) != 0 {
		t.Error("CSR signing returned a server-generated private key")
	}
	if !bytes.Equal(issued.Leaf.PublicKey.(ed25519.PublicKey), public) {
		t.Error("CSR public key was replaced")
	}
	corrupt := append([]byte(nil), der...)
	corrupt[len(corrupt)-1] ^= 1
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"bad_signature", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: corrupt})},
		{"extra_block", append(append([]byte(nil), data...), data...)},
		{"trailing_garbage", append(append([]byte(nil), data...), []byte("garbage")...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadCSR(path); err == nil {
				t.Error("invalid CSR accepted")
			}
		})
	}
}

func TestAuthorizerFailedReloadPreservesExistingAuthorization(t *testing.T) {
	dir, cfg, _ := testCertConfig(t)
	_, auth, err := ServerTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := ReadCertificate(filepath.Join(dir, "alice.pem"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		entries []config.AuthorizedClient
	}{
		{"missing_certificate", []config.AuthorizedClient{{UserID: "alice", Certificate: filepath.Join(dir, "missing.pem")}}},
		{"duplicate_user", append(append([]config.AuthorizedClient(nil), cfg.AuthorizedClients...), cfg.AuthorizedClients[0])},
		{"same_certificate_other_user", append(append([]config.AuthorizedClient(nil), cfg.AuthorizedClients...), config.AuthorizedClient{UserID: "bob", Certificate: cfg.AuthorizedClients[0].Certificate})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := auth.Reload(tc.entries); err == nil {
				t.Error("invalid ACL reload accepted")
			}
			if principal, err := auth.Certificate(alice); err != nil || !principal.Allows("upload") {
				t.Errorf("failed reload changed existing authorization: %v", err)
			}
		})
	}
}

func TestExplicitEmptyPermissionsDenyAll(t *testing.T) {
	for _, tc := range []struct {
		name        string
		permissions []string
		up, down    bool
	}{
		{"nil_legacy", nil, true, true}, {"empty", []string{}, false, false}, {"upload", []string{"upload"}, true, false}, {"download", []string{"download"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Principal{Permissions: tc.permissions, NotAfter: time.Now().Add(time.Hour)}
			if p.Allows("upload") != tc.up || p.Allows("download") != tc.down {
				t.Error("permission semantics changed")
			}
			p.Disabled = true
			if p.Allows("upload") || p.Allows("download") {
				t.Error("disabled principal retained permissions")
			}
			p.Disabled = false
			p.NotAfter = time.Now().Add(-time.Second)
			if p.Allows("upload") || p.Allows("download") {
				t.Error("expired principal retained permissions")
			}
		})
	}
}

func TestReturnedPrincipalCannotMutateAuthorization(t *testing.T) {
	dir, cfg, _ := testCertConfig(t)
	cfg.AuthorizedClients[0].Permissions = []string{"download"}
	_, auth, err := ServerTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := ReadCertificate(filepath.Join(dir, "alice.pem"))
	if err != nil {
		t.Fatal(err)
	}
	principal, err := auth.Certificate(cert)
	if err != nil {
		t.Fatal(err)
	}
	principal.Permissions[0] = "upload"
	// BUG: 返回的 Principal.Permissions 与内部授权表共享底层切片，调用者可绕过 Reload 改变权限。
	if _, err := auth.Check("alice", "upload"); err == nil {
		t.Error("BUG: mutating returned principal escalated stored permissions")
	}
	if _, err := auth.Check("alice", "download"); err != nil {
		t.Errorf("BUG: returned principal mutation removed original permission: %v", err)
	}
}
