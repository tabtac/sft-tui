//go:build windows

package pal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
)

// GetIPCPath 返回 Windows 命名管道路径 \\.\pipe\sftd-<hash>
func GetIPCPath(baseDir string) string {
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		abs = baseDir
	}
	h := sha256.Sum256([]byte(strings.ToLower(abs)))
	return fmt.Sprintf(`\\.\pipe\sftd-%s`, hex.EncodeToString(h[:8]))
}

// ListenIPC 监听 Windows Named Pipe，配置仅当前用户与 SYSTEM 可访问的私有 DACL
func ListenIPC(endpoint string) (net.Listener, error) {
	sid, err := CurrentUserSID()
	if err != nil {
		return nil, err
	}
	sddl := "D:P(A;;GA;;;SY)(A;;GA;;;" + sid + ")"
	cfg := &winio.PipeConfig{
		SecurityDescriptor: sddl,
		InputBufferSize:    4096,
		OutputBufferSize:   4096,
	}
	return winio.ListenPipe(endpoint, cfg)
}

// DialIPC 连接 Windows 命名管道
func DialIPC(endpoint string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(endpoint, &timeout)
}

// IsIPCNotFound 判断错误是否为命名管道未找到
func IsIPCNotFound(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
