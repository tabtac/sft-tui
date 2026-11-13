//go:build !windows

package pal

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// AtomicReplace 在 Linux 下使用系统调用原子重命名。
// 若跨设备 (EXDEV)，则复制后落盘再删除源文件。
func AtomicReplace(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		_ = SyncParentDir(dst)
		return nil
	}

	var linkErr *os.LinkError
	if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
		// 跨文件系统备选路径
		if err := CopyFile(src, dst); err != nil {
			return err
		}
		_ = os.Remove(src)
		_ = SyncParentDir(dst)
		return nil
	}
	return err
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

// SyncParentDir 刷新父目录元数据
func SyncParentDir(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
