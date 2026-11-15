// Package logfile 提供带独占文件锁、私有权限与大小上限的单文件有界服务日志写入器。
package logfile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"

	"sft-tui/internal/pal"
)

const (
	MinMaxBytes = 64 * 1024
	MaxMaxBytes = 1 << 30
)

type Writer struct {
	mu             sync.Mutex
	file, lock     *os.File
	size, maxBytes int64
	closed         bool
}

// Open 打开或创建私有有界日志文件，拒绝符号链接并防止多实例并发写入同一日志文件。
func Open(path string, maxBytes int64) (*Writer, error) {
	if maxBytes < MinMaxBytes || maxBytes > MaxMaxBytes {
		return nil, fmt.Errorf("日志文件大小须为 64 KiB–1 GiB")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = pal.EnsurePrivateDir(filepath.Dir(absolute)); err != nil {
		return nil, err
	}
	lock, err := pal.OpenLockFile(absolute + ".lock")
	if err != nil {
		return nil, err
	}
	if err = pal.LockFile(lock); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("日志正被另一个服务使用：%w", err)
	}
	fail := func(err error) (*Writer, error) {
		_ = pal.UnlockFile(lock)
		_ = lock.Close()
		return nil, err
	}
	if err := pal.CheckRegularOrAbsent(absolute); err != nil {
		return fail(err)
	}
	f, err := os.OpenFile(absolute, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fail(err)
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("服务日志必须是普通文件")
	}
	if err == nil {
		err = f.Chmod(0600)
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekEnd)
	}
	if err != nil {
		_ = f.Close()
		return fail(err)
	}
	return &Writer{file: f, lock: lock, size: info.Size(), maxBytes: maxBytes}, nil
}

func (w *Writer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	origLen := len(data)
	if origLen == 0 {
		return 0, nil
	}
	if int64(origLen) > w.maxBytes {
		const suffix = "… [日志过长，已截断]\n"
		end := int(w.maxBytes) - len(suffix)
		if end < 0 {
			end = 0
		}
		for end > 0 && !utf8.RuneStart(data[end]) {
			end--
		}
		buf := make([]byte, 0, end+len(suffix))
		buf = append(buf, data[:end]...)
		buf = append(buf, suffix...)
		data = buf
	}
	if w.size > 0 && w.size+int64(len(data)) > w.maxBytes {
		// 在同文件内重置覆盖，不留归档副本，防止无限膨胀
		if err := w.file.Truncate(0); err != nil {
			return 0, fmt.Errorf("日志达到上限，清空失败：%w", err)
		}
		if _, err := w.file.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		w.size = 0
	}
	n, err := w.file.Write(data)
	w.size += int64(n)
	if err != nil {
		return n, err
	}
	if n != len(data) {
		return n, io.ErrShortWrite
	}
	return origLen, nil
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	err := w.file.Close()
	_ = pal.UnlockFile(w.lock)
	if e := w.lock.Close(); err == nil {
		err = e
	}
	return err
}
