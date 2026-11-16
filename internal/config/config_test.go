package config

import (
	"bytes"
	"math"
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

func TestStrictConfigurationObjectAndSizeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  []byte
		valid bool
	}{
		{"empty", nil, false}, {"null", []byte("null"), false}, {"array", []byte("[]"), false},
		{"defaults", []byte("{}"), true}, {"trailing_object", []byte("{} {}"), false},
		{"trailing_text", []byte("{} garbage"), false}, {"unknown_field", []byte(`{"surprise":true}`), false},
		{"exact_limit", append([]byte("{}"), bytes.Repeat([]byte(" "), int(MaxConfigBytes)-2)...), true},
		{"over_limit", append([]byte("{}"), bytes.Repeat([]byte(" "), int(MaxConfigBytes)-1)...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client.json")
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadClient(path)
			if (err == nil) != tc.valid {
				t.Errorf("configuration validity=%v want=%v err=%v", err == nil, tc.valid, err)
			}
		})
	}
}

func TestServerResourceValidationBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Server)
	}{
		{"connections_zero", func(c *Server) { c.Resources.MaxConnections = 0 }},
		{"handshakes_above_connections", func(c *Server) { c.Resources.MaxHandshakes = c.Resources.MaxConnections + 1 }},
		{"user_streams_zero", func(c *Server) { c.Resources.MaxStreamsPerUser = 0 }},
		{"pending_zero", func(c *Server) { c.Resources.MaxPendingTransfersPerUser = 0 }},
		{"retention_zero", func(c *Server) { c.Resources.TransferRetentionSeconds = 0 }},
		{"file_size_above_int64", func(c *Server) { c.Resources.MaxFileBytes = uint64(math.MaxInt64) + 1 }},
		{"quota_below_file", func(c *Server) { c.Resources.UserStorageBytes = c.Resources.MaxFileBytes - 1 }},
		{"zero_bandwidth", func(c *Server) { c.Bandwidth.UploadBytesPerSecond = 0 }},
		{"burst_below_chunk", func(c *Server) { c.Bandwidth.BurstBytes = uint64(c.Limits.ChunkSize) - 1 }},
		{"zero_timeout", func(c *Server) { c.Timeouts.FrameReadSeconds = 0 }},
		{"too_many_streams", func(c *Server) { c.Limits.MaxStreams = 65 }},
		{"chunk_below_min", func(c *Server) { c.Limits.ChunkSize = 4095 }},
		{"small_log", func(c *Server) { c.Logging.MaxFileBytes = 65535 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultServer()
			tc.mutate(&cfg)
			if err := cfg.ValidateDraft(); err == nil {
				t.Error("invalid resource configuration accepted")
			}
		})
	}
}

func TestHotMergePreservesRestartFieldsAndEmptyACL(t *testing.T) {
	base := DefaultServer()
	base.AuthorizedClients = []AuthorizedClient{{UserID: "alice", Certificate: "alice.pem", Permissions: nil}}
	updated := base
	updated.Listen = "127.0.0.1:9999"
	updated.StorageRoot = "other"
	updated.TLS.Certificate = "new.pem"
	updated.AuthorizedClients = []AuthorizedClient{{UserID: "alice", Certificate: "alice.pem", Permissions: []string{}}}
	merged := MergeHot(base, updated)
	if merged.Listen != base.Listen || merged.StorageRoot != base.StorageRoot || merged.TLS.Certificate != "new.pem" {
		t.Error("hot merge applied restart-only fields or lost certificate update")
	}
	if HotEqual(base, updated) {
		t.Error("nil legacy ACL and explicit deny-all ACL compared equal")
	}
	if merged.AuthorizedClients[0].Permissions == nil {
		t.Error("hot merge lost explicit deny-all ACL")
	}
}
