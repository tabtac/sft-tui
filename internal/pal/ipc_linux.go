//go:build !windows

package pal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const maxUnixSocketPathBytes = 96

func ipcRuntimeDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("sft-ipc-%d", os.Getuid()))
}

func ensureRuntimeIPCDir(dir string) error {
	if err := EnsurePrivateDir(dir); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("IPC 运行目录属主不匹配：%s", dir)
	}
	return nil
}

// GetIPCPath 返回 Linux 下基于 Unix Domain Socket 的 IPC 端点路径
func GetIPCPath(baseDir string) string {
	return GetNamedIPCPath(baseDir, "sftd")
}

// GetNamedIPCPath 返回指定名称的本地 IPC 端点路径。
// 若直接拼接后的路径超过 Unix socket 地址长度安全阈值，则在当前用户私有的短运行目录内按规范化路径生成确定性哈希端点。
func GetNamedIPCPath(baseDir, name string) string {
	if name == "" {
		name = "sftd"
	}
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		abs = filepath.Clean(baseDir)
	}
	direct := filepath.Join(abs, "."+name+".sock")
	if len(direct) <= maxUnixSocketPathBytes {
		return direct
	}
	h := sha256.Sum256([]byte(abs + "\x00" + name))
	shortName := name
	if len(shortName) > 20 {
		shortName = shortName[:20]
	}
	return filepath.Join(ipcRuntimeDir(), fmt.Sprintf("%s-%s.sock", shortName, hex.EncodeToString(h[:8])))
}

// ListenIPC 监听 Unix Domain Socket，设置安全权限 0600
func ListenIPC(endpoint string) (net.Listener, error) {
	if filepath.Dir(endpoint) == ipcRuntimeDir() {
		if err := ensureRuntimeIPCDir(ipcRuntimeDir()); err != nil {
			return nil, err
		}
	}
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
	if filepath.Dir(endpoint) == ipcRuntimeDir() {
		info, err := os.Lstat(ipcRuntimeDir())
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("IPC 运行目录无效：%s", ipcRuntimeDir())
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Uid != uint32(os.Getuid()) {
			return nil, fmt.Errorf("IPC 运行目录属主不匹配：%s", ipcRuntimeDir())
		}
	}
	return net.DialTimeout("unix", endpoint, timeout)
}

// IsIPCNotFound 判断错误是否为本地服务未运行
func IsIPCNotFound(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}
