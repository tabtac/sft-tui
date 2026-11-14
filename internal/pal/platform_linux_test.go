//go:build linux

package pal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReviewPrivateDirectoryPermissions(t *testing.T) {
	for _, ensure := range []struct {
		name string
		run  func(string) error
	}{
		{"private", EnsurePrivateDir}, {"secure", EnsureSecureDir},
	} {
		t.Run(ensure.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(path, 0777); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0777); err != nil {
				t.Fatal(err)
			}
			if err := ensure.run(path); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			// BUG: EnsureSecureDir 对已有宽松目录仅 MkdirAll，不会收紧权限。
			if info.Mode().Perm() != 0700 {
				t.Errorf("BUG: private directory permissions=%o want=700", info.Mode().Perm())
			}
		})
	}
}

func TestReviewPrivateDirectoryAndLockRejectSymlinks(t *testing.T) {
	for _, operation := range []string{"private_directory", "lock_file"} {
		t.Run(operation, func(t *testing.T) {
			base := t.TempDir()
			outside := t.TempDir()
			target := outside
			if operation == "lock_file" {
				target = filepath.Join(outside, "keep")
				if err := os.WriteFile(target, []byte("safe"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(base, "link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if operation == "private_directory" {
				if err := EnsurePrivateDir(link); err == nil {
					t.Error("symlink private directory accepted")
				}
			} else {
				f, err := OpenLockFile(link)
				if f != nil {
					f.Close()
				}
				if err == nil {
					t.Error("symlink lock accepted")
				}
			}
		})
	}
}

func TestReviewRootRemainsAnchoredAfterDirectoryRename(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "root")
	root, err := OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	moved := filepath.Join(base, "moved")
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	f, err := root.OpenFile("anchored", os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, "anchored")); err != nil {
		t.Errorf("write missed pinned root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "anchored")); !os.IsNotExist(err) {
		t.Errorf("write followed replaced pathname: %v", err)
	}
}

func TestReviewSecureSubdirTightensExistingPermissions(t *testing.T) {
	base := t.TempDir()
	root, err := OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	sub := filepath.Join(base, "sub")
	if err := os.Mkdir(sub, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0777); err != nil {
		t.Fatal(err)
	}
	if err := root.EnsureSecureSubdir("sub"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sub)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Errorf("subdirectory permissions: %v %v", info, err)
	}
}

func TestReviewNativeParentDirectoryForBackslashFilename(t *testing.T) {
	// Linux 的反斜杠是文件名字符，临时文件必须留在真正的父目录。
	path := filepath.Join(t.TempDir(), `config\name`)
	if got, want := filepathDir(path), filepath.Dir(path); got != want {
		t.Errorf("BUG: Linux backslash filename uses wrong staging directory: %q want=%q", got, want)
	}
}
