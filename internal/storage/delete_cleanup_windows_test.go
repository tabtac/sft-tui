package storage

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

func blockObjectRemoval(t *testing.T, object string) func() {
	t.Helper()
	name, err := windows.UTF16PtrFromString(object)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unblock := func() { once.Do(func() { _ = windows.CloseHandle(handle) }) }
	t.Cleanup(unblock)
	return unblock
}

func TestReopenDefersLockedOrphansAndContinuesCleanup(t *testing.T) {
	for _, sub := range []string{"staging", "objects"} {
		t.Run(sub, func(t *testing.T) {
			logs := captureCleanupLogs(t)
			root := t.TempDir()
			s := reviewOpenStore(t, root)
			keep := reviewPublish(t, s, "alice", "keep.bin", []byte("valid payload"), 1)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			locked := filepath.Join(root, sub, "00000000000000000000000000000000")
			other := filepath.Join(root, sub, "ffffffffffffffffffffffffffffffff")
			for _, path := range []string{locked, other} {
				if err := os.WriteFile(path, []byte("orphan"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			unblock := blockObjectRemoval(t, locked)
			reopened := reviewOpenStore(t, root)
			if _, err := os.Stat(locked); err != nil {
				t.Fatalf("locked orphan must remain pending: %v", err)
			}
			if _, err := os.Stat(other); !os.IsNotExist(err) {
				t.Fatalf("locked orphan prevented cleanup of another orphan: %v", err)
			}
			if !strings.Contains(logs.String(), "孤儿文件回收待重试") || !strings.Contains(logs.String(), filepath.Base(locked)) {
				t.Fatalf("missing startup cleanup warning: %s", logs.String())
			}
			if got := string(reviewReadFile(t, filepath.Join(root, "objects", keep.ObjectID))); got != "valid payload" {
				t.Fatalf("valid object changed: %q", got)
			}
			unblock()
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			reviewOpenStore(t, root)
			if _, err := os.Stat(locked); !os.IsNotExist(err) {
				t.Fatalf("unlocked orphan was not reclaimed on restart: %v", err)
			}
		})
	}
}
