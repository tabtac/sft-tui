//go:build !windows

package pal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// AtomicReplace 在 Linux 下使用系统调用原子重命名。
// 若跨设备 (EXDEV)，先复制到目标同目录临时文件并落盘，再原子重命名覆盖目标。
func AtomicReplace(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		_ = SyncParentDir(dst)
		return nil
	}

	var linkErr *os.LinkError
	if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
		if err := replaceCrossDevice(src, dst); err != nil {
			return err
		}
		_ = os.Remove(src)
		_ = SyncParentDir(dst)
		return nil
	}
	return err
}

func replaceCrossDevice(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	inInfo, err := in.Stat()
	if err != nil {
		return err
	}
	if !inInfo.Mode().IsRegular() {
		return fmt.Errorf("source must be a regular file: %s", src)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".replace-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := syncAndCloseCopy(tmp, in); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// LockFile 非阻塞独占锁
func LockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// UnlockFile 释放文件锁
func UnlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// OpenLockFile 安全打开互斥进程锁文件 (防御符号链接挟持)
func OpenLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
}

// OpenRegularNoFollow 使用 O_NOFOLLOW 原子打开普通文件并复核文件类型，消除 Lstat->Open 符号链接 TOCTOU 窗口。
func OpenRegularNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, perm)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("security violation: %q must be a regular file", path)
	}
	return f, nil
}

// FileIdentity 返回文件底层设备号、inode 与硬链接数
func FileIdentity(f *os.File) (dev uint64, ino uint64, nlink uint64, err error) {
	info, err := f.Stat()
	if err != nil {
		return 0, 0, 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0, fmt.Errorf("unexpected stat type %T", info.Sys())
	}
	return uint64(st.Dev), uint64(st.Ino), uint64(st.Nlink), nil
}

// SyncParentDir 刷新父目录元数据
func SyncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
