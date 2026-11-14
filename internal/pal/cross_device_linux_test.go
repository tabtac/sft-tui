//go:build linux

package pal

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func reviewSecondFilesystem(t *testing.T) (string, string) {
	t.Helper()
	secondRoot := os.Getenv("SFT_TEST_SECOND_FS_ROOT")
	if secondRoot == "" {
		t.Skip("跨文件系统测试需设置 SFT_TEST_SECOND_FS_ROOT 到可写的另一文件系统，例如 /dev/shm")
	}
	first := t.TempDir()
	second, err := os.MkdirTemp(secondRoot, "sft-pal-cross-device-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(second); err != nil {
			t.Errorf("cleanup second filesystem: %v", err)
		}
	})
	firstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstInfo.Sys().(*syscall.Stat_t).Dev == secondInfo.Sys().(*syscall.Stat_t).Dev {
		t.Fatal("SFT_TEST_SECOND_FS_ROOT must reside on a different filesystem")
	}
	return first, second
}

func TestReviewCrossDeviceReplacePreservesOldInodeAliases(t *testing.T) {
	first, second := reviewSecondFilesystem(t)
	src := filepath.Join(first, "source")
	dst := filepath.Join(second, "destination")
	alias := filepath.Join(second, "old-alias")
	if err := os.WriteFile(src, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(dst, alias); err != nil {
		t.Fatal(err)
	}
	if err := AtomicReplace(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "new" {
		t.Errorf("new destination: %q %v", got, err)
	}
	// BUG: EXDEV 分支直接 O_TRUNC 旧 inode，会修改所有硬链接别名和已经打开的旧文件。
	if got, err := os.ReadFile(alias); err != nil || string(got) != "old" {
		t.Errorf("BUG: cross-device replacement mutated old inode: %q %v", got, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("successful move kept source: %v", err)
	}
}

func TestReviewCrossDeviceReplaceDoesNotFollowDestinationSymlink(t *testing.T) {
	first, second := reviewSecondFilesystem(t)
	src := filepath.Join(first, "source")
	dst := filepath.Join(second, "destination")
	victim := filepath.Join(second, "victim")
	if err := os.WriteFile(src, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, dst); err != nil {
		t.Fatal(err)
	}
	if err := AtomicReplace(src, dst); err != nil {
		t.Fatal(err)
	}
	// BUG: EXDEV 回退跟随目标符号链接，覆盖链接指向的文件。
	if got, err := os.ReadFile(victim); err != nil || string(got) != "safe" {
		t.Errorf("BUG: cross-device move followed symlink: %q %v", got, err)
	}
	info, err := os.Lstat(dst)
	if err != nil || !info.Mode().IsRegular() {
		t.Errorf("destination entry was not replaced: %v %v", info, err)
	}
}
