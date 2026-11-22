//go:build windows

package pal

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const pipeBufferBytes = 4096

var (
	modKernel32IPC    = windows.NewLazySystemDLL("kernel32.dll")
	procWaitNamedPipe = modKernel32IPC.NewProc("WaitNamedPipeW")
)

func waitNamedPipe(name *uint16, timeoutMS uint32) error {
	r1, _, e1 := procWaitNamedPipe.Call(uintptr(unsafe.Pointer(name)), uintptr(timeoutMS))
	if r1 == 0 {
		if e1 != nil && e1 != windows.ERROR_SUCCESS {
			return e1
		}
		return windows.ERROR_SEM_TIMEOUT
	}
	return nil
}

// GetIPCPath 返回 Windows 命名管道路径 \\.\pipe\sftd-<hash>
func GetIPCPath(baseDir string) string {
	return GetNamedIPCPath(baseDir, "sftd")
}

// GetNamedIPCPath 返回指定名称的 Windows 命名管道路径。
func GetNamedIPCPath(baseDir, name string) string {
	abs, err := filepath.Abs(baseDir)
	if err != nil {
		abs = baseDir
	}
	if name == "" || name == "sftd" {
		h := sha256.Sum256([]byte(strings.ToLower(abs)))
		return fmt.Sprintf(`\\.\pipe\sftd-%s`, hex.EncodeToString(h[:8]))
	}
	h := sha256.Sum256([]byte(strings.ToLower(abs) + "\x00" + name))
	return fmt.Sprintf(`\\.\pipe\sft-%s`, hex.EncodeToString(h[:12]))
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

type windowsPipeListener struct {
	path          string
	pathUTF16     *uint16
	sd            *windows.SECURITY_DESCRIPTOR
	mu            sync.Mutex
	closed        bool
	pendingPipe   windows.Handle
	closeEvent    windows.Handle // 手动重置事件：一旦 Close 置位永不丢失，根除关闭信号竞态死锁
	acceptWG      sync.WaitGroup
	closeDoneOnce sync.Once
}

func createServerPipeInstance(pathUTF16 *uint16, sd *windows.SECURITY_DESCRIPTOR, first bool) (windows.Handle, error) {
	openMode := uint32(windows.PIPE_ACCESS_DUPLEX | windows.FILE_FLAG_OVERLAPPED)
	if first {
		openMode |= windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	}
	pipeMode := uint32(windows.PIPE_TYPE_BYTE | windows.PIPE_READMODE_BYTE | windows.PIPE_WAIT | windows.PIPE_REJECT_REMOTE_CLIENTS)
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	h, err := windows.CreateNamedPipe(
		pathUTF16,
		openMode,
		pipeMode,
		windows.PIPE_UNLIMITED_INSTANCES,
		pipeBufferBytes,
		pipeBufferBytes,
		0,
		sa,
	)
	runtime.KeepAlive(pathUTF16)
	runtime.KeepAlive(sd)
	if err != nil {
		return 0, err
	}
	return h, nil
}

// ListenIPC 监听 Windows Named Pipe，配置仅当前用户与 SYSTEM 可访问的私有 DACL。
// 使用 Win32 重叠 I/O 与手动重置关闭事件，确保任意时刻调用 Close() 均能唤醒 Accept() 并在有限时间内退出。
func ListenIPC(endpoint string) (net.Listener, error) {
	sid, err := CurrentUserSID()
	if err != nil {
		return nil, err
	}
	sddl := "D:P(A;;GA;;;SY)(A;;GA;;;" + sid + ")"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, fmt.Errorf("build pipe security descriptor: %w", err)
	}
	pathUTF16, err := windows.UTF16PtrFromString(endpoint)
	if err != nil {
		return nil, err
	}
	firstPipe, err := createServerPipeInstance(pathUTF16, sd, true)
	if err != nil {
		return nil, &os.PathError{Op: "listen", Path: endpoint, Err: err}
	}
	closeEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(firstPipe)
		return nil, err
	}
	return &windowsPipeListener{
		path:        endpoint,
		pathUTF16:   pathUTF16,
		sd:          sd,
		pendingPipe: firstPipe,
		closeEvent:  closeEvent,
	}, nil
}

func (l *windowsPipeListener) isClosed() bool {
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	return closed
}

func (l *windowsPipeListener) replenishPendingLockedSafe() {
	l.mu.Lock()
	if !l.closed && l.pendingPipe == 0 {
		if nextPipe, err := createServerPipeInstance(l.pathUTF16, l.sd, false); err == nil {
			l.pendingPipe = nextPipe
		}
	}
	l.mu.Unlock()
}

func (l *windowsPipeListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	closeEv := l.closeEvent
	l.acceptWG.Add(1)
	l.mu.Unlock()
	defer l.acceptWG.Done()

	ioEv, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(ioEv)

	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return nil, net.ErrClosed
		}
		h := l.pendingPipe
		l.pendingPipe = 0
		l.mu.Unlock()

		if h == 0 {
			h, err = createServerPipeInstance(l.pathUTF16, l.sd, false)
			if err != nil {
				if l.isClosed() {
					return nil, net.ErrClosed
				}
				return nil, &os.PathError{Op: "accept", Path: l.path, Err: err}
			}
		}

		if err := windows.ResetEvent(ioEv); err != nil {
			_ = windows.CloseHandle(h)
			return nil, err
		}
		ov := &windows.Overlapped{HEvent: ioEv}
		connErr := windows.ConnectNamedPipe(h, ov)

		switch {
		case connErr == nil || errors.Is(connErr, windows.ERROR_PIPE_CONNECTED):
			// 客户端已同步连入
		case errors.Is(connErr, windows.ERROR_IO_PENDING):
			ev, waitErr := windows.WaitForMultipleObjects([]windows.Handle{ioEv, closeEv}, false, windows.INFINITE)
			if waitErr != nil || ev != windows.WAIT_OBJECT_0 {
				_ = windows.CancelIoEx(h, ov)
				var transferred uint32
				ovErr := windows.GetOverlappedResult(h, ov, &transferred, true)
				if ovErr == nil {
					_ = windows.DisconnectNamedPipe(h)
				}
				_ = windows.CloseHandle(h)
				if l.isClosed() {
					return nil, net.ErrClosed
				}
				if waitErr != nil {
					return nil, waitErr
				}
				return nil, net.ErrClosed
			}
			var transferred uint32
			connErr = windows.GetOverlappedResult(h, ov, &transferred, true)
			if connErr != nil {
				if l.isClosed() {
					_ = windows.CloseHandle(h)
					return nil, net.ErrClosed
				}
				if errors.Is(connErr, windows.ERROR_NO_DATA) {
					l.replenishPendingLockedSafe()
					_ = windows.CloseHandle(h)
					continue
				}
				_ = windows.CloseHandle(h)
				return nil, &os.PathError{Op: "accept", Path: l.path, Err: connErr}
			}
		case errors.Is(connErr, windows.ERROR_NO_DATA):
			// 客户端连入后立刻断开，先补充新实例再关闭旧实例，避免 0 实例空窗
			if l.isClosed() {
				_ = windows.CloseHandle(h)
				return nil, net.ErrClosed
			}
			l.replenishPendingLockedSafe()
			_ = windows.CloseHandle(h)
			continue
		default:
			_ = windows.CloseHandle(h)
			if l.isClosed() {
				return nil, net.ErrClosed
			}
			return nil, &os.PathError{Op: "accept", Path: l.path, Err: connErr}
		}

		// 在返回连接前预先补充下一个监听实例，确保运行期不存在 0 实例空窗导致 Dial 误报不存在。
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			_ = windows.DisconnectNamedPipe(h)
			_ = windows.CloseHandle(h)
			return nil, net.ErrClosed
		}
		if l.pendingPipe == 0 {
			if nextPipe, nextErr := createServerPipeInstance(l.pathUTF16, l.sd, false); nextErr == nil {
				l.pendingPipe = nextPipe
			}
		}
		l.mu.Unlock()

		conn, err := newWindowsPipeConn(h, l.path)
		if err != nil {
			_ = windows.CloseHandle(h)
			return nil, err
		}
		return conn, nil
	}
}

func (l *windowsPipeListener) Close() error {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		if l.pendingPipe != 0 {
			_ = windows.CloseHandle(l.pendingPipe)
			l.pendingPipe = 0
		}
		if l.closeEvent != 0 {
			_ = windows.SetEvent(l.closeEvent)
		}
	}
	l.mu.Unlock()

	l.acceptWG.Wait()

	l.closeDoneOnce.Do(func() {
		l.mu.Lock()
		ev := l.closeEvent
		l.closeEvent = 0
		l.mu.Unlock()
		if ev != 0 {
			_ = windows.CloseHandle(ev)
		}
	})
	return nil
}

func (l *windowsPipeListener) Addr() net.Addr {
	return pipeAddr(l.path)
}

type pipeIOState struct {
	mu        sync.Mutex
	deadline  time.Time
	wakeEvent windows.Handle
}

type windowsPipeConn struct {
	h          windows.Handle
	path       string
	closed     atomic.Bool
	closeOnce  sync.Once
	ioMu       sync.RWMutex
	readState  pipeIOState
	writeState pipeIOState
}

func newWindowsPipeConn(h windows.Handle, path string) (*windowsPipeConn, error) {
	readEv, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return nil, err
	}
	writeEv, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(readEv)
		return nil, err
	}
	return &windowsPipeConn{
		h:          h,
		path:       path,
		readState:  pipeIOState{wakeEvent: readEv},
		writeState: pipeIOState{wakeEvent: writeEv},
	}, nil
}

func deadlineWaitMS(dl time.Time) (uint32, bool) {
	if dl.IsZero() {
		return windows.INFINITE, false
	}
	rem := time.Until(dl)
	if rem <= 0 {
		return 0, true
	}
	ms := rem.Milliseconds()
	if rem%time.Millisecond != 0 {
		ms++
	}
	if ms >= int64(windows.INFINITE) {
		return windows.INFINITE - 1, false
	}
	return uint32(ms), false
}

func (c *windowsPipeConn) doIO(isWrite bool, b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.ioMu.RLock()
	defer c.ioMu.RUnlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}

	st := &c.readState
	if isWrite {
		st = &c.writeState
	}

	st.mu.Lock()
	if c.closed.Load() {
		st.mu.Unlock()
		return 0, net.ErrClosed
	}
	dl := st.deadline
	_ = windows.ResetEvent(st.wakeEvent)
	wakeEv := st.wakeEvent
	st.mu.Unlock()

	waitMS, expired := deadlineWaitMS(dl)
	if expired {
		return 0, os.ErrDeadlineExceeded
	}

	ioEv, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(ioEv)

	ov := &windows.Overlapped{HEvent: ioEv}
	var done uint32
	if isWrite {
		err = windows.WriteFile(c.h, b, &done, ov)
	} else {
		err = windows.ReadFile(c.h, b, &done, ov)
	}

	if errors.Is(err, windows.ERROR_IO_PENDING) {
		for {
			ev, waitErr := windows.WaitForMultipleObjects([]windows.Handle{ioEv, wakeEv}, false, waitMS)
			if waitErr != nil {
				_ = windows.CancelIoEx(c.h, ov)
				_ = windows.GetOverlappedResult(c.h, ov, &done, true)
				runtime.KeepAlive(b)
				return int(done), waitErr
			}
			switch ev {
			case windows.WAIT_OBJECT_0:
				err = windows.GetOverlappedResult(c.h, ov, &done, true)
				goto doneIO
			case uint32(windows.WAIT_TIMEOUT):
				_ = windows.CancelIoEx(c.h, ov)
				ovErr := windows.GetOverlappedResult(c.h, ov, &done, true)
				runtime.KeepAlive(b)
				if ovErr == nil && done > 0 {
					return int(done), nil
				}
				return int(done), os.ErrDeadlineExceeded
			case windows.WAIT_OBJECT_0 + 1:
				st.mu.Lock()
				closed := c.closed.Load()
				dl = st.deadline
				if !closed {
					_ = windows.ResetEvent(st.wakeEvent)
				}
				st.mu.Unlock()
				if closed {
					_ = windows.CancelIoEx(c.h, ov)
					_ = windows.GetOverlappedResult(c.h, ov, &done, true)
					runtime.KeepAlive(b)
					return int(done), net.ErrClosed
				}
				waitMS, expired = deadlineWaitMS(dl)
				if expired {
					_ = windows.CancelIoEx(c.h, ov)
					ovErr := windows.GetOverlappedResult(c.h, ov, &done, true)
					runtime.KeepAlive(b)
					if ovErr == nil && done > 0 {
						return int(done), nil
					}
					return int(done), os.ErrDeadlineExceeded
				}
			default:
				_ = windows.CancelIoEx(c.h, ov)
				_ = windows.GetOverlappedResult(c.h, ov, &done, true)
				runtime.KeepAlive(b)
				return int(done), fmt.Errorf("unexpected pipe wait status: %d", ev)
			}
		}
	}

doneIO:
	runtime.KeepAlive(b)
	if err == nil {
		return int(done), nil
	}
	if !isWrite && (errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED)) {
		if done > 0 {
			return int(done), nil
		}
		return 0, io.EOF
	}
	if c.closed.Load() {
		return int(done), net.ErrClosed
	}
	return int(done), err
}

func (c *windowsPipeConn) Read(b []byte) (int, error)  { return c.doIO(false, b) }
func (c *windowsPipeConn) Write(b []byte) (int, error) { return c.doIO(true, b) }

func (c *windowsPipeConn) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		for _, st := range []*pipeIOState{&c.readState, &c.writeState} {
			st.mu.Lock()
			if st.wakeEvent != 0 {
				_ = windows.SetEvent(st.wakeEvent)
			}
			st.mu.Unlock()
		}
		// 等待所有进行中的重叠 I/O 完成取消并释放缓冲区后再关闭句柄
		c.ioMu.Lock()
		if c.h != 0 {
			closeErr = windows.CloseHandle(c.h)
			c.h = 0
		}
		for _, st := range []*pipeIOState{&c.readState, &c.writeState} {
			st.mu.Lock()
			if st.wakeEvent != 0 {
				_ = windows.CloseHandle(st.wakeEvent)
				st.wakeEvent = 0
			}
			st.mu.Unlock()
		}
		c.ioMu.Unlock()
	})
	return closeErr
}

func (c *windowsPipeConn) LocalAddr() net.Addr  { return pipeAddr(c.path) }
func (c *windowsPipeConn) RemoteAddr() net.Addr { return pipeAddr(c.path) }

func (c *windowsPipeConn) SetDeadline(t time.Time) error {
	_ = c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

func (c *windowsPipeConn) SetReadDeadline(t time.Time) error {
	c.readState.mu.Lock()
	c.readState.deadline = t
	if c.readState.wakeEvent != 0 {
		_ = windows.SetEvent(c.readState.wakeEvent)
	}
	c.readState.mu.Unlock()
	return nil
}

func (c *windowsPipeConn) SetWriteDeadline(t time.Time) error {
	c.writeState.mu.Lock()
	c.writeState.deadline = t
	if c.writeState.wakeEvent != 0 {
		_ = windows.SetEvent(c.writeState.wakeEvent)
	}
	c.writeState.mu.Unlock()
	return nil
}

// DialIPC 连接 Windows 命名管道
func DialIPC(endpoint string, timeout time.Duration) (net.Conn, error) {
	pathUTF16, err := windows.UTF16PtrFromString(endpoint)
	if err != nil {
		return nil, err
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	flags := uint32(windows.FILE_FLAG_OVERLAPPED | windows.SECURITY_SQOS_PRESENT | windows.SECURITY_IDENTIFICATION)
	for {
		h, err := windows.CreateFile(
			pathUTF16,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			nil,
			windows.OPEN_EXISTING,
			flags,
			0,
		)
		runtime.KeepAlive(pathUTF16)
		if err == nil {
			conn, cErr := newWindowsPipeConn(h, endpoint)
			if cErr != nil {
				_ = windows.CloseHandle(h)
				return nil, cErr
			}
			return conn, nil
		}
		if errors.Is(err, windows.ERROR_PIPE_BUSY) {
			var waitMS uint32 = 50
			if !deadline.IsZero() {
				rem := time.Until(deadline)
				if rem <= 0 {
					return nil, &os.PathError{Op: "dial", Path: endpoint, Err: os.ErrDeadlineExceeded}
				}
				if ms := rem.Milliseconds(); ms < int64(waitMS) {
					waitMS = max(1, uint32(ms))
				}
			}
			if wErr := waitNamedPipe(pathUTF16, waitMS); wErr != nil {
				if errors.Is(wErr, windows.ERROR_FILE_NOT_FOUND) {
					return nil, &os.PathError{Op: "open", Path: endpoint, Err: windows.ERROR_FILE_NOT_FOUND}
				}
				if !deadline.IsZero() && !time.Now().Before(deadline) {
					return nil, &os.PathError{Op: "dial", Path: endpoint, Err: os.ErrDeadlineExceeded}
				}
			}
			continue
		}
		return nil, &os.PathError{Op: "open", Path: endpoint, Err: err}
	}
}

// IsIPCNotFound 判断错误是否为命名管道未找到
func IsIPCNotFound(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND)
}
