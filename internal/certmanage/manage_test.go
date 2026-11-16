package certmanage

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io/fs"
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

func TestApplyChangesPreflightFailurePreservesAllDestinations(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			root := t.TempDir()
			first := filepath.Join(root, "first.json")
			blocked := filepath.Join(root, "blocked")
			if existing {
				if err := os.WriteFile(first, []byte("original"), 0640); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(blocked, 0700); err != nil {
				t.Fatal(err)
			}
			err := applyChanges([]fileChange{{path: first, data: []byte("replacement"), mode: 0600}, {path: blocked, data: []byte("invalid"), mode: 0600}})
			if err == nil {
				t.Fatal("irregular destination accepted")
			}
			data, readErr := os.ReadFile(first)
			if existing {
				if readErr != nil || string(data) != "original" {
					t.Errorf("preflight failure changed original: %q %v", data, readErr)
				}
			} else if !os.IsNotExist(readErr) {
				t.Errorf("preflight failure created first destination: %v", readErr)
			}
			stages, err := filepath.Glob(filepath.Join(root, ".stage-*"))
			if err != nil || len(stages) != 0 {
				t.Errorf("temporary certificate files leaked: %q %v", stages, err)
			}
		})
	}
}

func TestApplyChangesRejectsCanonicalDuplicateAndExclusiveOverwrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, changes := range [][]fileChange{
		{{path: path, data: []byte("new"), mode: 0600}, {path: filepath.Join(root, ".", "config.json"), data: []byte("other"), mode: 0600}},
		{{path: path, data: []byte("new"), mode: 0600, exclusive: true}},
	} {
		if err := applyChanges(changes); err == nil {
			t.Error("duplicate/exclusive output accepted")
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "keep" {
			t.Errorf("rejected publication damaged original: %q %v", data, err)
		}
	}
}

func TestPolicyOptimisticConflictPreservesLatestConfiguration(t *testing.T) {
	o := testOptions(t)
	initial, err := Execute("init", o)
	if err != nil {
		t.Fatal(err)
	}
	old := initial.Server.AuthorizedClients[0]
	changed := o
	changed.Policy = &UserPolicy{Permissions: []string{"download"}}
	if _, err := Execute("policy", changed); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(o.ServerConfig)
	if err != nil {
		t.Fatal(err)
	}
	stale := o
	stale.ExpectedUser = &old
	stale.Policy = &UserPolicy{Disabled: true, Permissions: []string{"upload"}}
	if _, err := Execute("policy", stale); err == nil {
		t.Error("stale user policy overwrote newer edit")
	}
	after, err := os.ReadFile(o.ServerConfig)
	if err != nil || !bytes.Equal(before, after) {
		t.Error("rejected stale policy changed server configuration")
	}
}

func TestInitFailureDoesNotReplaceExistingConfiguration(t *testing.T) {
	o := testOptions(t)
	if _, err := Execute("init", o); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(o.ServerConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute("init", o); err == nil {
		t.Error("second init replaced existing CA/configuration")
	}
	after, err := os.ReadFile(o.ServerConfig)
	if err != nil || !bytes.Equal(before, after) {
		t.Error("failed initialization changed server configuration")
	}
}

func TestExportContainsOnlyClientIdentityAndRejectsReuse(t *testing.T) {
	o := testOptions(t)
	if _, err := Execute("init", o); err != nil {
		t.Fatal(err)
	}
	export := o
	export.Out = filepath.Join(t.TempDir(), "bundle")
	if _, err := Execute("export", export); err != nil {
		t.Fatal(err)
	}
	caKey, err := os.ReadFile(filepath.Join(o.Dir, "ca-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(export.Out, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, caKey) {
			t.Errorf("CA private key leaked into exported file %s", entry.Name())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(export.Out, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Execute("export", export); err == nil {
		t.Error("export overwrote existing bundle")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "keep" {
		// BUG: 导出目标已存在时，失败清理无条件 RemoveAll(o.Out)，删除调用前就存在的文件。
		t.Error("BUG: failed export removed the existing bundle and unrelated files")
	}
}
