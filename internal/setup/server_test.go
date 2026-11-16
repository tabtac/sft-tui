package setup

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/security"
)

func serverFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	o := certmanage.Options{
		Dir:              filepath.Join(dir, "certs"),
		ServerConfig:     filepath.Join(dir, "server.local.json"),
		ClientConfigDir:  dir,
		Name:             "alice",
		Days:             30,
		CADays:           365,
		Hosts:            "localhost,127.0.0.1",
		CAName:           "Example CA",
		ServerCommonName: "Example transfer",
	}
	if _, err := certmanage.Execute("init", o); err != nil {
		t.Fatal(err)
	}
	return dir, o.ServerConfig
}

func TestServerInitFindsMatchingCertificatesWithoutExampleConfig(t *testing.T) {
	dir, existing := serverFixture(t)
	old, _ := config.LoadServer(existing)
	ca, _ := security.ReadCertificate(old.TLS.CACertificate)
	leaf, _ := security.ReadCertificate(old.TLS.Certificate)
	if ca.Subject.CommonName != "Example CA" || leaf.Subject.CommonName != "Example transfer" {
		t.Fatal("custom certificate names were lost")
	}
	matches, err := ScanServerTLS(dir)
	if err != nil || len(matches) != 1 || matches[0] != old.TLS {
		t.Fatalf("matched wrong server certificates: %+v %v", matches, err)
	}
	path := filepath.Join(dir, "new-config", "server.json")
	if err := Server(strings.NewReader(strings.Repeat("\n", 12)), io.Discard, path, dir); err != nil {
		t.Fatal(err)
	}
	c, err := config.LoadServerDraft(path)
	if err != nil || c.TLS != old.TLS || len(c.AuthorizedClients) != 0 || !filepath.IsAbs(c.StorageRoot) {
		t.Fatalf("incorrect initialized configuration: %+v %v", c, err)
	}
	if _, err := config.LoadServer(path); err == nil {
		t.Fatal("new server silently authorized discovered client certificates")
	}
	if _, _, err := security.PrepareServerTLS(c); err != nil {
		t.Fatal(err)
	}
}

func TestServerInitPreservesUsersAndDoesNotWriteOnEOF(t *testing.T) {
	dir, path := serverFixture(t)
	before, _ := os.ReadFile(path)
	if err := Server(strings.NewReader("\n\n"), io.Discard, path, dir); err == nil {
		t.Fatal("interrupted setup succeeded")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("interrupted setup changed existing configuration")
	}
	old, _ := config.LoadServer(path)
	if err := Server(strings.NewReader(strings.Repeat("\n", 12)), io.Discard, path, dir); err != nil {
		t.Fatal(err)
	}
	c, err := config.LoadServer(path)
	if err != nil || len(c.AuthorizedClients) != 1 || c.AuthorizedClients[0].Certificate != old.AuthorizedClients[0].Certificate || c.Resources != old.Resources {
		t.Fatalf("existing settings were lost: %+v %v", c, err)
	}
}

func TestParseBytesExactRationalBoundaries(t *testing.T) {
	if got, err := ParseBytes("10 MiB/s"); err != nil || got != 10<<20 {
		t.Fatalf("ParseBytes(10 MiB/s) = %d, %v", got, err)
	}
	if _, err := ParseBytes("0.5 B"); err == nil {
		t.Fatal("accepted fractional byte")
	}
}
