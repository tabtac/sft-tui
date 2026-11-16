package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sft-tui/internal/config"
	"sft-tui/internal/testcert"
	"sft-tui/internal/transfer"
)

func setupCLIFixture(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	certsDir := filepath.Join(dir, "certs")
	if err := testcert.Generate(certsDir); err != nil {
		t.Fatal(err)
	}
	if err := testcert.IssueClient(certsDir, "alice"); err != nil {
		t.Fatal(err)
	}

	srvCfg := config.DefaultServer()
	srvCfg.Listen = "127.0.0.1:0"
	srvCfg.StorageRoot = filepath.Join(dir, "storage")
	srvCfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "server.pem"),
		PrivateKey:    filepath.Join(certsDir, "server-key.pem"),
	}
	srvCfg.AuthorizedClients = []config.AuthorizedClient{
		{UserID: "alice", Certificate: filepath.Join(certsDir, "alice.pem"), Permissions: []string{"upload", "download"}},
	}

	d, err := transfer.NewDaemon(srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Serve(ctx, ln)
	}()

	cliCfg := config.DefaultClient()
	cliCfg.StateDir = filepath.Join(dir, "cli-state")
	cliCfg.ServerAddress = ln.Addr().String()
	cliCfg.ServerName = "localhost"
	cliCfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "alice.pem"),
		PrivateKey:    filepath.Join(certsDir, "alice-key.pem"),
	}
	cfgPath := filepath.Join(dir, "client.local.json")
	if err := config.SaveJSON(cfgPath, cliCfg); err != nil {
		d.Stop(true)
		cancel()
		<-done
		d.Close()
		t.Fatal(err)
	}

	cleanup := func() {
		d.Stop(true)
		cancel()
		<-done
		_ = d.Close()
	}
	return cfgPath, cleanup
}

func TestHeadlessCLIUploadListDownloadAndStatus(t *testing.T) {
	cfgPath, cleanup := setupCLIFixture(t)
	defer cleanup()

	srcDir := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "dl")
	aPath := filepath.Join(srcDir, "alpha.txt")
	bPath := filepath.Join(srcDir, "beta.bin")
	if err := os.WriteFile(aPath, []byte("alpha-content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, bytes.Repeat([]byte{0x5A}, 64*1024), 0600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := runCommand([]string{"-config", cfgPath, "-progress", "never", "upload", aPath, bPath}, &out, &errOut); code != 0 {
		t.Fatalf("cli upload failed (%d): %s %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "alpha.txt") || !strings.Contains(out.String(), "beta.bin") {
		t.Fatalf("unexpected upload output: %s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"-config", cfgPath, "list"}, &out, &errOut); code != 0 {
		t.Fatalf("cli list failed (%d): %s %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "alpha.txt") || !strings.Contains(out.String(), "beta.bin") {
		t.Fatalf("unexpected list output: %s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"-config", cfgPath, "-progress", "never", "-out", outDir, "download", "alpha.txt", "beta.bin"}, &out, &errOut); code != 0 {
		t.Fatalf("cli download failed (%d): %s %s", code, out.String(), errOut.String())
	}
	gotB, err := os.ReadFile(filepath.Join(outDir, "beta.bin"))
	if err != nil || !bytes.Equal(gotB, bytes.Repeat([]byte{0x5A}, 64*1024)) {
		t.Fatalf("downloaded beta mismatch: %v len=%d", err, len(gotB))
	}

	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"-config", cfgPath, "status"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "没有待恢复") {
		t.Fatalf("unexpected status output (%d): %s %s", code, out.String(), errOut.String())
	}
}
