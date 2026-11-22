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

func TestValidationRejectsSingleWildcardAndInvalidConnectAddresses(t *testing.T) {
	if _, _, err := ParseServerNames("*"); err == nil {
		t.Fatal("expected bare '*' SAN to be rejected")
	}
	for _, addr := range []string{
		"a.example,b.example:7443",
		"*.example.com:7443",
		"*:7443",
		"[0:0:0:0:0:0:0:0]:7443",
		"0.0.0.0:7443",
		"[::]:7443",
	} {
		if _, _, err := ValidateConnectAddress(addr); err == nil {
			t.Errorf("ValidateConnectAddress(%q) unexpectedly succeeded", addr)
		}
	}

	dir := t.TempDir()
	caMat, err := NewCA(30, "", "", "test-ca")
	if err != nil {
		t.Fatal(err)
	}
	caCert := filepath.Join(dir, "ca.pem")
	caKey := filepath.Join(dir, "ca-key.pem")
	if err := os.WriteFile(caCert, caMat.Certificate, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caKey, caMat.PrivateKey, 0600); err != nil {
		t.Fatal(err)
	}
	c := config.DefaultServer()
	c.TLS = config.TLSFiles{CACertificate: caCert, Certificate: caCert, PrivateKey: caKey}
	if _, _, err := PrepareServerTLS(c); err == nil {
		t.Fatal("PrepareServerTLS accepted CA certificate as server certificate")
	}
}

func TestClientIdentityKeyStableAcrossRenewalAndIsolatedByUserCAAndServer(t *testing.T) {
	dir := t.TempDir()
	ca1, err := NewCA(30, "", "", "ca-1")
	if err != nil {
		t.Fatal(err)
	}
	ca1Cert, ca1Key := writeCertificateMaterial(t, ca1)

	leaf1, err := NewLeafFromCA(ca1, "alice", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	cert1, key1 := writeCertificateMaterial(t, leaf1)

	// 1. 服务端生成新密钥续期（新序列号 + 新公钥）
	leaf2, err := NewLeafFromCA(ca1, "alice", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	cert2, key2 := writeCertificateMaterial(t, leaf2)

	// 2. CSR 续期（不同外部密钥 + 相同用户 CN）
	_, extPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "alice"}}, extPriv)
	if err != nil {
		t.Fatal(err)
	}
	csrPath := filepath.Join(dir, "alice.csr")
	if err := os.WriteFile(csrPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}), 0600); err != nil {
		t.Fatal(err)
	}
	leaf3, err := SignCSRExpected(ca1Cert, ca1Key, "alice", csrPath, 25, "")
	if err != nil {
		t.Fatal(err)
	}
	extKeyDER, err := x509.MarshalPKCS8PrivateKey(extPriv)
	if err != nil {
		t.Fatal(err)
	}
	leaf3.PrivateKey = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: extKeyDER})
	cert3, key3 := writeCertificateMaterial(t, leaf3)

	// 3. CA 自身续期（保持 CA 公钥不变）
	ca1Renewed, err := NewCA(60, ca1Cert, ca1Key)
	if err != nil {
		t.Fatal(err)
	}
	ca1RenewedCert, _ := writeCertificateMaterial(t, ca1Renewed)

	// 4. 不同用户 bob 与不同 CA
	bobLeaf, err := NewLeafFromCA(ca1, "bob", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	bobCert, bobKey := writeCertificateMaterial(t, bobLeaf)

	ca2, err := NewCA(30, "", "", "ca-2")
	if err != nil {
		t.Fatal(err)
	}
	ca2Cert, _ := writeCertificateMaterial(t, ca2)
	aliceCA2Leaf, err := NewLeafFromCA(ca2, "alice", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	aliceCA2Cert, aliceCA2Key := writeCertificateMaterial(t, aliceCA2Leaf)

	makeCfg := func(caPath, certPath, keyPath, addr, serverName string) config.Client {
		c := config.DefaultClient()
		c.ServerAddress = addr
		c.ServerName = serverName
		c.TLS = config.TLSFiles{CACertificate: caPath, Certificate: certPath, PrivateKey: keyPath}
		return c
	}

	kBase, err := ClientIdentityKey(makeCfg(ca1Cert, cert1, key1, "127.0.0.1:7443", "localhost"))
	if err != nil {
		t.Fatalf("ClientIdentityKey base: %v", err)
	}
	kRenewManaged, err := ClientIdentityKey(makeCfg(ca1Cert, cert2, key2, "127.0.0.1:7443", "localhost"))
	if err != nil {
		t.Fatalf("ClientIdentityKey managed renew: %v", err)
	}
	kRenewCSR, err := ClientIdentityKey(makeCfg(ca1Cert, cert3, key3, "127.0.0.1:7443", "localhost"))
	if err != nil {
		t.Fatalf("ClientIdentityKey CSR renew: %v", err)
	}
	kRenewCA, err := ClientIdentityKey(makeCfg(ca1RenewedCert, cert2, key2, "127.0.0.1:7443", "localhost"))
	if err != nil {
		t.Fatalf("ClientIdentityKey CA renew: %v", err)
	}

	if kBase != kRenewManaged || kBase != kRenewCSR || kBase != kRenewCA {
		t.Fatalf("expected stable identity key across renewals: base=%s managed=%s csr=%s ca=%s", kBase, kRenewManaged, kRenewCSR, kRenewCA)
	}

	kBob, err := ClientIdentityKey(makeCfg(ca1Cert, bobCert, bobKey, "127.0.0.1:7443", "localhost"))
	if err != nil || kBob == kBase {
		t.Fatalf("expected different user bob to have distinct identity key (bob=%s, err=%v)", kBob, err)
	}
	kOtherCA, err := ClientIdentityKey(makeCfg(ca2Cert, aliceCA2Cert, aliceCA2Key, "127.0.0.1:7443", "localhost"))
	if err != nil || kOtherCA == kBase {
		t.Fatalf("expected different CA to have distinct identity key (otherCA=%s, err=%v)", kOtherCA, err)
	}
	kOtherAddr, err := ClientIdentityKey(makeCfg(ca1Cert, cert1, key1, "127.0.0.1:7444", "localhost"))
	if err != nil || kOtherAddr == kBase {
		t.Fatalf("expected different server address to have distinct identity key (otherAddr=%s, err=%v)", kOtherAddr, err)
	}
	kOtherSNI, err := ClientIdentityKey(makeCfg(ca1Cert, cert1, key1, "127.0.0.1:7443", "other.local"))
	if err != nil || kOtherSNI == kBase {
		t.Fatalf("expected different server name to have distinct identity key (otherSNI=%s, err=%v)", kOtherSNI, err)
	}
}

func TestAuthorizerCheckBandwidthZeroAllocAndEnforcement(t *testing.T) {
	dir := t.TempDir()
	ca, err := NewCA(30, "", "")
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, ca.Certificate, 0644); err != nil {
		t.Fatal(err)
	}

	alice, err := NewLeafFromCA(ca, "alice", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	aliceCert := filepath.Join(dir, "alice.pem")
	if err := os.WriteFile(aliceCert, alice.Certificate, 0644); err != nil {
		t.Fatal(err)
	}

	pool, err := loadCA(caPath)
	if err != nil {
		t.Fatal(err)
	}
	customBW := config.Bandwidth{
		UploadBytesPerSecond:   4 * 1024 * 1024,
		DownloadBytesPerSecond: 8 * 1024 * 1024,
		BurstBytes:             1024 * 1024,
	}
	srvCfg := config.DefaultServer()
	srvCfg.AuthorizedClients = []config.AuthorizedClient{{
		UserID:      "alice",
		Certificate: aliceCert,
		Permissions: []string{"upload"},
		Bandwidth:   &customBW,
	}}
	auth, err := NewAuthorizer(srvCfg, pool)
	if err != nil {
		t.Fatalf("NewAuthorizer: %v", err)
	}

	bw, err := auth.CheckBandwidth("alice", "upload")
	if err != nil || bw != customBW {
		t.Fatalf("CheckBandwidth(upload) = (%+v, %v), want (%+v, nil)", bw, err, customBW)
	}
	if _, err := auth.CheckBandwidth("alice", "download"); err == nil {
		t.Fatal("expected CheckBandwidth(download) to deny missing permission")
	}
	if _, err := auth.CheckBandwidth("unknown", "upload"); err == nil {
		t.Fatal("expected CheckBandwidth(unknown) to fail")
	}

	allocs := testing.AllocsPerRun(100, func() {
		if _, err := auth.CheckBandwidth("alice", "upload"); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocs/op on CheckBandwidth hot path, got %.1f", allocs)
	}
}
