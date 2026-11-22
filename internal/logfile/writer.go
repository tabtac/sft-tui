// Package logfile 提供带独占文件锁、私有权限与总大小严格有界的双段滚动服务日志写入器。
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
	mu                       sync.Mutex
	path, backupPath         string
	file, lock               *os.File
	size, maxBytes, segLimit int64
	closed                   bool
}

// Open 打开或创建私有有界双段滚动日志文件，拒绝符号链接并防止多实例并发写入同一日志文件。
// 活动日志与单个历史滚动段（path + ".1"）各占 maxBytes/2，总磁盘占用严格不超过 maxBytes，
// 防止触达上限时一次性清空全部审计轨迹。
func Open(path string, maxBytes int64) (*Writer, error) {
	if maxBytes < MinMaxBytes || maxBytes > MaxMaxBytes {
		return nil, fmt.Errorf("日志文件大小须为 64 KiB–1 GiB")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = pal.EnsureDir(filepath.Dir(absolute)); err != nil {
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
	backupPath := absolute + ".1"
	if err := pal.CheckRegularOrAbsent(backupPath); err != nil {
		return fail(err)
	}
	f, err := openLogSegment(absolute, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fail(err)
	}
	info, err := f.Stat()
	if err == nil {
		_, err = f.Seek(0, io.SeekEnd)
	}
	if err != nil {
		_ = f.Close()
		return fail(err)
	}
	segLimit := maxBytes / 2
	return &Writer{
		path:       absolute,
		backupPath: backupPath,
		file:       f,
		lock:       lock,
		size:       info.Size(),
		maxBytes:   maxBytes,
		segLimit:   segLimit,
	}, nil
}

func openLogSegment(path string, flag int) (*os.File, error) {
	f, err := pal.OpenRegularNoFollow(path, flag, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (w *Writer) rotateLocked() error {
	if err := pal.CheckRegularOrAbsent(w.backupPath); err != nil {
		return err
	}
	if w.file != nil {
		_ = w.file.Sync()
		if err := w.file.Close(); err != nil {
			w.file = nil
			return err
		}
		w.file = nil
	}
	if err := pal.AtomicReplace(w.path, w.backupPath); err != nil {
		// 滚动失败时尽量恢复打开原文件，避免 Writer 陷入无句柄状态
		if reopened, rErr := openLogSegment(w.path, os.O_CREATE|os.O_RDWR); rErr == nil {
			if info, sErr := reopened.Stat(); sErr == nil {
				_, _ = reopened.Seek(0, io.SeekEnd)
				w.file = reopened
				w.size = info.Size()
			} else {
				_ = reopened.Close()
			}
		}
		return fmt.Errorf("日志滚动失败：%w", err)
	}
	f, err := openLogSegment(w.path, os.O_CREATE|os.O_RDWR|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("日志滚动后重建失败：%w", err)
	}
	w.file = f
	w.size = 0
	return nil
}

func (w *Writer) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.file == nil {
		return 0, os.ErrClosed
	}
	origLen := len(data)
	if origLen == 0 {
		return 0, nil
	}
	if int64(origLen) > w.segLimit {
		const suffix = "… [日志过长，已截断]\n"
		end := int(w.segLimit) - len(suffix)
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
	if w.size > 0 && w.size+int64(len(data)) > w.segLimit {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
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
	var err error
	if w.file != nil {
		err = w.file.Close()
		w.file = nil
	}
	_ = pal.UnlockFile(w.lock)
	if e := w.lock.Close(); err == nil {
		err = e
	}
	return err
}
