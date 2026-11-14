package pal

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestReviewStageAndReplaceContents(t *testing.T) {
	for _, size := range []int{0, 1, 4096, 65536} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			base := t.TempDir()
			path := filepath.Join(base, "config")
			if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			want := bytes.Repeat([]byte("a"), size)
			if err := StageAndReplace(path, want); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("contents: len=%d err=%v", len(got), err)
			}
			matches, err := filepath.Glob(filepath.Join(base, ".stage-*"))
			if err != nil || len(matches) != 0 {
				t.Errorf("temporary files leaked: %v %v", matches, err)
			}
		})
	}
}

func TestReviewStageFailureCleansTemporaryFile(t *testing.T) {
	base := t.TempDir()
	destination := filepath.Join(base, "directory")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "keep"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := StageAndReplace(destination, []byte("replacement")); err == nil {
		t.Error("directory replacement should fail")
	}
	matches, err := filepath.Glob(filepath.Join(base, ".stage-*"))
	if err != nil || len(matches) != 0 {
		t.Errorf("failed replace leaked temp files: %v %v", matches, err)
	}
	if got, err := os.ReadFile(filepath.Join(destination, "keep")); err != nil || string(got) != "safe" {
		t.Errorf("existing destination damaged: %q %v", got, err)
	}
}

func TestReviewCopyFileCannotDestroyItsOwnSource(t *testing.T) {
	for _, alias := range []string{"same_path", "hardlink"} {
		t.Run(alias, func(t *testing.T) {
			base := t.TempDir()
			src := filepath.Join(base, "source")
			if err := os.WriteFile(src, []byte("keep me"), 0600); err != nil {
				t.Fatal(err)
			}
			dst := src
			if alias == "hardlink" {
				dst = filepath.Join(base, "alias")
				if err := os.Link(src, dst); err != nil {
					t.Skipf("hardlink unavailable: %v", err)
				}
			}
			// BUG: 源目标指向同一 inode 时，O_TRUNC 在读取前清空源文件且返回成功。
			if err := CopyFile(src, dst); err == nil {
				t.Error("BUG: self-copy returned success")
			}
			if got, err := os.ReadFile(src); err != nil || string(got) != "keep me" {
				t.Errorf("BUG: self-copy lost source: %q %v", got, err)
			}
		})
	}
}

func TestReviewCopyAndReplaceFailuresPreserveExistingDestination(t *testing.T) {
	base := t.TempDir()
	dst := filepath.Join(base, "destination")
	if err := os.WriteFile(dst, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(base, "missing")
	for _, operation := range []struct {
		name string
		run  func(string, string) error
	}{
		{"copy", CopyFile}, {"replace", AtomicReplace},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(missing, dst); err == nil {
				t.Error("missing source accepted")
			}
			if got, err := os.ReadFile(dst); err != nil || string(got) != "safe" {
				t.Errorf("existing destination damaged: %q %v", got, err)
			}
		})
	}
}

func TestReviewLockReleasedAndReacquired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, err := OpenLockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenLockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := LockFile(first); err != nil {
		t.Fatal(err)
	}
	if err := LockFile(second); err == nil {
		t.Error("second holder acquired exclusive lock")
	}
	if err := UnlockFile(first); err != nil {
		t.Fatal(err)
	}
	if err := LockFile(second); err != nil {
		t.Fatalf("lock could not be reacquired: %v", err)
	}
	if err := UnlockFile(second); err != nil {
		t.Fatal(err)
	}
}
