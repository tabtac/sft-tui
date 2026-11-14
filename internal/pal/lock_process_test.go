package pal

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewLockProcessHelper(t *testing.T) {
	mode := os.Getenv("SFT_STAGE1_LOCK_HELPER")
	if mode == "" {
		return
	}
	path := os.Args[len(os.Args)-1]
	f, err := OpenLockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = LockFile(f)
	if mode == "busy" {
		if err == nil {
			UnlockFile(f)
			t.Fatal("child acquired parent's exclusive lock")
		}
	} else {
		if err != nil {
			t.Fatalf("child could not acquire released lock: %v", err)
		}
		if err := UnlockFile(f); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReviewLockExclusionAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	f, err := OpenLockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := LockFile(f); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"busy", "free"} {
		if mode == "free" {
			if err := UnlockFile(f); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReviewLockProcessHelper$", "--", path)
		cmd.Env = append(os.Environ(), "SFT_STAGE1_LOCK_HELPER="+mode)
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("child (%s): %v\n%s", mode, err, output)
		}
	}
}
