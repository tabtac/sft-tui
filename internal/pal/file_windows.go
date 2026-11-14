//go:build windows

package pal

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// AtomicReplace 在 Windows 下使用 MoveFileEx 原生原子替换，并支持轻微重试抗文件锁竞争
func AtomicReplace(src, dst string) error {
	srcPtr, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	dstPtr, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}

	flags := uint32(windows.MOVEFILE_REPLACE_EXISTING | windows.MOVEFILE_WRITE_THROUGH)

	var lastErr error
	for i := 0; i < 5; i++ {
		err = windows.MoveFileEx(srcPtr, dstPtr, flags)
		if err == nil {
			return nil
		}
		lastErr = err
		// 若因杀毒软件或短暂只读锁导致拒绝访问，稍作退避重试
		if errorsIsAccessDenied(err) {
			time.Sleep(time.Duration(10*(i+1)) * time.Millisecond)
			continue
		}
		break
	}
	return fmt.Errorf("MoveFileEx failed: %w", lastErr)
}

func errorsIsAccessDenied(err error) bool {
	return err == windows.ERROR_ACCESS_DENIED || err == windows.ERROR_SHARING_VIOLATION
}

// LockFile 在 Windows 下调用 LockFileEx 实现非阻塞独占锁
func LockFile(f *os.File) error {
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &windows.Overlapped{},
	)
}

// UnlockFile 释放 Windows 文件锁
func UnlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}

// OpenLockFile 在 Windows 下安全打开锁文件
func OpenLockFile(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		p,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_ALWAYS,
		windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open lock", Path: path, Err: err}
	}

	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil ||
		info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		windows.CloseHandle(h)
		if err == nil {
			err = fmt.Errorf("lock file must be a regular file")
		}
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// FileIdentity 返回 Windows 卷序列号、文件索引与硬链接数
func FileIdentity(f *os.File) (dev uint64, ino uint64, nlink uint64, err error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0, 0, 0, err
	}
	ino = (uint64(info.FileIndexHigh) << 32) | uint64(info.FileIndexLow)
	return uint64(info.VolumeSerialNumber), ino, uint64(info.NumberOfLinks), nil
}

// SyncParentDir Windows 目录句柄不支持 fsync，依赖 NTFS 日志保证一致性
func SyncParentDir(path string) error {
	return nil
}
