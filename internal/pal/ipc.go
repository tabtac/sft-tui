package pal

import (
	"net"
	"time"
)

// IPC 接口定义
type IPC interface {
	GetEndpoint(baseDir string) string
	Listen(endpoint string) (net.Listener, error)
	Dial(endpoint string, timeout time.Duration) (net.Conn, error)
	IsNotFound(err error) bool
}
