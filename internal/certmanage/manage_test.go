package certmanage

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sft-tui/internal/config"
	"sft-tui/internal/security"
)

func testOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{
		Dir:             filepath.Join(dir, "certs"),
		ServerConfig:    filepath.Join(dir, "configs", "server.local.json"),
		ClientConfigDir: filepath.Join(dir, "configs"),
		Name:            "alice",
	}
}

func TestLifecycleInitRotateRevokeCSRExport(t *testing.T) {
	o := testOptions(t)
	res, err := Execute("init", o)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !res.Changed || len(res.Server.AuthorizedClients) != 1 || res.Server.AuthorizedClients[0].UserID != "alice" {
		t.Fatalf("unexpected init result: %+v", res)
	}
	if _, _, err := security.ServerTLS(res.Server); err != nil {
		t.Fatalf("ServerTLS after init: %v", err)
	}

	// 新建用户 + 吊销 + 策略修改 + 导出
	oBob := o
	oBob.Name = "bob"
	if _, err := Execute("user", oBob); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	bundleDir := filepath.Join(t.TempDir(), "bundle-bob")
	oExport := oBob
	oExport.Out = bundleDir
	if _, err := Execute("export", oExport); err != nil {
		t.Fatalf("export bob: %v", err)
	}
	if _, err := config.LoadClient(filepath.Join(bundleDir, "client.bob.local.json")); err != nil {
		t.Fatalf("LoadClient exported bundle: %v", err)
	}

	if _, err := Execute("revoke", oBob); err != nil {
		t.Fatalf("revoke bob: %v", err)
	}
	if _, err := Execute("export", oExport); err == nil {
		t.Fatal("expected revoked user export to fail")
	}

	// 外部私钥 CSR 签发 + 仅公钥导出
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "carol"}}, priv)
	if err != nil {
		t.Fatal(err)
	}
	csrPath := filepath.Join(t.TempDir(), "carol.csr")
	if err := os.WriteFile(csrPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}), 0600); err != nil {
		t.Fatal(err)
	}
	oCarol := o
	oCarol.Name = "carol"
	oCarol.CSR = csrPath
	if _, err := Execute("user", oCarol); err != nil {
		t.Fatalf("user via CSR: %v", err)
	}
	oCarolPub := oCarol
	oCarolPub.CSR = ""
	oCarolPub.Out = filepath.Join(t.TempDir(), "pub-carol")
	oCarolPub.CertificateOnly = true
	if _, err := Execute("export", oCarolPub); err != nil {
		t.Fatalf("public-only export: %v", err)
	}
	if _, err := os.Stat(filepath.Join(oCarolPub.Out, "README.txt")); err != nil {
		t.Fatalf("missing README in public export: %v", err)
	}

	oAll := o
	oAll.Name = ""
	stRes, err := Execute("status", oAll)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	report := strings.Join(stRes.Messages, "\n")
	if !strings.Contains(report, "alice") || !strings.Contains(report, "bob\t已吊销") || !strings.Contains(report, "carol\t已授权") {
		t.Fatalf("unexpected all-users status report:\n%s", report)
	}
}
