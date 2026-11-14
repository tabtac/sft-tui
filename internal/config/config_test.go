package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsAndValidation(t *testing.T) {
	srv := DefaultServer()
	if err := srv.Validate(); err == nil {
		t.Fatal("expected empty authorized_clients to fail Validate")
	}
	if err := srv.ValidateDraft(); err != nil {
		t.Fatalf("ValidateDraft failed: %v", err)
	}

	srv.AuthorizedClients = []AuthorizedClient{
		{UserID: "alice", Certificate: "certs/alice.pem", Permissions: []string{"upload", "download"}},
	}
	if err := srv.Validate(); err != nil {
		t.Fatalf("Validate failed with user: %v", err)
	}

	cli := DefaultClient()
	if err := cli.Validate(); err != nil {
		t.Fatalf("DefaultClient Validate failed: %v", err)
	}
	cli.ServerFingerprint = "not-hex"
	if err := cli.Validate(); err == nil {
		t.Fatal("expected invalid fingerprint to fail")
	}
}

func TestLoadStrictJSONAndHotReload(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "server.json")

	srv := DefaultServer()
	srv.AuthorizedClients = []AuthorizedClient{
		{UserID: "alice", Certificate: "alice.pem", Permissions: []string{"download", "upload"}},
	}
	if err := SaveJSON(cfgPath, srv); err != nil {
		t.Fatalf("SaveJSON: %v", err)
	}
	loaded, err := LoadServer(cfgPath)
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}

	reordered := loaded
	reordered.AuthorizedClients = []AuthorizedClient{
		{UserID: "alice", Certificate: "alice.pem", Permissions: []string{"upload", "download"}},
	}
	if !HotEqual(loaded, reordered) {
		t.Error("HotEqual should ignore permission ordering")
	}
	if RestartRequired(loaded, reordered) {
		t.Error("changing authorized_clients should not require restart")
	}

	reordered.Listen = "0.0.0.0:8443"
	if !RestartRequired(loaded, reordered) {
		t.Error("changing listen address must require restart")
	}

	// Reject unknown fields
	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte(`{"listen":"127.0.0.1:7443","unknown":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadServerDraft(badPath); err == nil {
		t.Fatal("expected unknown JSON field to be rejected")
	}
}

func TestClientAddressFromListen(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"0.0.0.0:7443", "127.0.0.1:7443", true},
		{"[::]:7443", "[::1]:7443", true},
		{"192.168.1.10:9000", "192.168.1.10:9000", true},
		{"bad", "", false},
	}
	for _, tc := range cases {
		got, err := ClientAddressFromListen(tc.in)
		if (err == nil) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("ClientAddressFromListen(%q) = %q, %v (want %q)", tc.in, got, err, tc.want)
		}
	}
}
