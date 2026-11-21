//go:build !windows

package pal

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// GetIPCPath 返回 Linux 下基于 Unix Domain Socket 的 IPC 端点路径
func GetIPCPath(baseDir string) string {
	return GetNamedIPCPath(baseDir, "sftd")
}

// GetNamedIPCPath 返回指定名称的本地 IPC 端点路径。
func GetNamedIPCPath(baseDir, name string) string {
	return filepath.Join(baseDir, "."+name+".sock")
}

// ListenIPC 监听 Unix Domain Socket，设置安全权限 0600
func ListenIPC(endpoint string) (net.Listener, error) {
	if info, err := os.Lstat(endpoint); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("IPC endpoint exists and is not a socket: %s", endpoint)
		}
		// 若 socket 已存在，尝试连接看是否已有服务运行
		conn, dialErr := net.DialTimeout("unix", endpoint, 200*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return nil, fmt.Errorf("another instance is already running on %s", endpoint)
		}
		// 遗留的无用 socket 文件，安全移除
		_ = os.Remove(endpoint)
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	l, err := net.Listen("unix", endpoint)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(endpoint, 0600); err != nil {
		l.Close()
		_ = os.Remove(endpoint)
		return nil, err
	}
	return l, nil
}

// DialIPC 连接本地 Unix Domain Socket
func DialIPC(endpoint string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", endpoint, timeout)
}

// IsIPCNotFound 判断错误是否为本地服务未运行
func IsIPCNotFound(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}
