//go:build !windows

package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func blockObjectRemoval(t *testing.T, object string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged user to exercise directory permission denial")
	}
	dir := filepath.Dir(object)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	unblock := func() {
		if err := os.Chmod(dir, info.Mode().Perm()); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(unblock)
	return unblock
}
