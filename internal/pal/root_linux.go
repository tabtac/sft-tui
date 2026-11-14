//go:build !windows

package pal

import (
	"fmt"
	"os"
)

// EnsurePrivateDir 在 Linux 下确保私有目录安全：
// 1. 若已存在，强制校验不是符号链接，并立即收紧已有权限为 0700（防止已预置 0777 宽松权限）。
// 2. 若不存在，安全创建并赋权 0700。
func EnsurePrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("security violation: private path %q is a symbolic link", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("security violation: private path %q is not a directory", path)
		}
		// 收紧已有权限
		return os.Chmod(path, 0700)
	}

	if !os.IsNotExist(err) {
		return err
	}

	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

// secureSubdir 收紧根内子目录权限为 0700
func (r *Root) secureSubdir(sub string) error {
	return r.root.Chmod(sub, 0700)
}

// SyncRootSubdir 刷新子目录元数据落盘
func (r *Root) SyncRootSubdir(sub string) error {
	f, err := r.root.Open(sub)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
