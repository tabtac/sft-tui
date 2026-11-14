//go:build linux

package pal

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewUnixIPCPrivatePermissions(t *testing.T) {
	endpoint := GetIPCPath(t.TempDir())
	listener, err := ListenIPC(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		t.Errorf("socket mode=%v", info.Mode())
	}
}

func TestReviewUnixIPCDoesNotRemoveRegularFile(t *testing.T) {
	endpoint := GetIPCPath(t.TempDir())
	if err := os.WriteFile(endpoint, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := ListenIPC(endpoint)
	if listener != nil {
		listener.Close()
	}
	if err == nil {
		t.Error("regular file accepted as socket")
	}
	if got, err := os.ReadFile(endpoint); err != nil || string(got) != "safe" {
		t.Errorf("endpoint file damaged: %q %v", got, err)
	}
}

func TestReviewUnixIPCRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.WriteFile(target, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint := GetIPCPath(base)
	if err := os.Symlink(target, endpoint); err != nil {
		t.Fatal(err)
	}
	listener, err := ListenIPC(endpoint)
	if listener != nil {
		listener.Close()
	}
	if err == nil {
		t.Error("symlink endpoint accepted")
	}
}

func TestReviewUnixIPCReclaimsStaleSocket(t *testing.T) {
	endpoint := GetIPCPath(t.TempDir())
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := ListenIPC(endpoint)
	if err != nil {
		t.Fatalf("stale socket not reclaimed: %v", err)
	}
	defer listener.Close()
}

func TestReviewUnixIPCSecondListenerPreservesFirst(t *testing.T) {
	endpoint := GetIPCPath(t.TempDir())
	first, err := ListenIPC(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := ListenIPC(endpoint)
	if second != nil {
		second.Close()
	}
	if err == nil {
		t.Error("second live endpoint listener accepted")
	}
	conn, err := DialIPC(endpoint, time.Second)
	if conn != nil {
		conn.Close()
	}
	if err != nil {
		t.Errorf("failed second listener damaged first endpoint: %v", err)
	}
}
