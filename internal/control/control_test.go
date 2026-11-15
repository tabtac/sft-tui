package control

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

func TestReloadEndpointIdentityPermissionsAndFailure(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "server.json")

	var calls atomic.Int32
	var reject atomic.Bool
	svc, err := Start(cfgPath, root, func() error {
		calls.Add(1)
		if reject.Load() {
			return errors.New("invalid replacement")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer svc.Close()

	if runtime.GOOS == "linux" {
		info, err := os.Stat(filepath.Join(root, ".sftd.sock"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("control socket permissions: %v", err)
		}
	}

	if ok, err := Reload(filepath.Join(root, "other.json"), root); err == nil || ok {
		t.Fatal("wrong config reached reload callback")
	}
	if calls.Load() != 0 {
		t.Fatal("wrong config mutated runtime")
	}
	if ok, err := Reload(cfgPath, root); err != nil || !ok {
		t.Fatalf("reload: %v", err)
	}
	reject.Store(true)
	if ok, err := Reload(cfgPath, root); err == nil || ok {
		t.Fatal("failed reload reported success")
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 calls, got %d", calls.Load())
	}
	reject.Store(false)
	if ok, err := Reload(cfgPath, filepath.Join(root, "offline")); err != nil || !ok {
		t.Fatalf("registered endpoint must override fallback: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", calls.Load())
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if ok, err := Reload(cfgPath, filepath.Join(root, "offline")); err != nil || ok {
		t.Fatal("expected absent offline service to return (false, nil)")
	}
}
