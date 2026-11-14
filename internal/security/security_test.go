package security

import (
	"crypto/tls"
	"net"
	"path/filepath"
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
