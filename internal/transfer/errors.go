// Package transfer 实现基于 SFT/1 与 mTLS 1.3 的客户端/服务端传输引擎与队列 Runner。
package transfer

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"

	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
)

var (
	// ErrPaused 表示任务已按控制指令确认暂停，复用 queue.ErrPaused 哨兵值。
	ErrPaused = queue.ErrPaused
	// ErrCancelled 表示任务已按控制指令确认取消并清理，复用 queue.ErrCancelled 哨兵值。
	ErrCancelled = queue.ErrCancelled
	// ErrAlreadyCompleted 表示服务端上对应任务已经处于终态 COMPLETED。
	ErrAlreadyCompleted = errors.New("任务已完成，保留已发布文件")
)

// IsTransient 判断错误是否为可自动重连的瞬态网络错误。
// 证书、权限、协议状态、路径或元数据冲突等确定性错误绝不重试。
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "tls:") || strings.Contains(msg, "x509:") {
		return false
	}
	var remote *protocol.RemoteError
	var pathErr *os.PathError
	if errors.As(err, &remote) || errors.As(err, &pathErr) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed)
}

func isFatalTransfer(err error) bool {
	var remote *protocol.RemoteError
	if !errors.As(err, &remote) {
		return false
	}
	switch remote.Code {
	case protocol.CodeHashMismatch,
		protocol.CodeMetadataMismatch,
		protocol.CodeInvalidName,
		protocol.CodeNotFound,
		protocol.CodeAlreadyExists,
		protocol.CodeTransferExpired,
		protocol.CodeTransferCancelled:
		return true
	default:
		return false
	}
}

func isHardServerFailure(err error) bool {
	var remote *protocol.RemoteError
	if !errors.As(err, &remote) {
		return false
	}
	switch remote.Code {
	case protocol.CodeHashMismatch,
		protocol.CodeMetadataMismatch,
		protocol.CodeBadFrame,
		protocol.CodeBadState,
		protocol.CodeOffsetMismatch,
		protocol.CodeWindowViolation,
		protocol.CodeUnauthorized,
		protocol.CodeAlreadyExists:
		return true
	default:
		return false
	}
}
