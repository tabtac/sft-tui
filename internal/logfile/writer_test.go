package logfile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriterBoundLockAndTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sftd.log")

	if _, err := Open(path, 1024); err == nil {
		t.Fatal("expected maxBytes < 64KiB to fail")
	}
	w, err := Open(path, MinMaxBytes)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	// 同路径第二把锁必须拒绝
	if _, err := Open(path, MinMaxBytes); err == nil {
		t.Fatal("expected concurrent Open on same logfile to fail lock check")
	}

	line := bytes.Repeat([]byte("a"), 32*1024)
	if _, err := w.Write(line); err != nil {
		t.Fatal(err)
	}
	// 再次写入超过 64KiB 时触发原地重置
	if _, err := w.Write(bytes.Repeat([]byte("b"), 40*1024)); err != nil {
		t.Fatal(err)
	}
	// 单条超大消息安全截断到 maxBytes
	if _, err := w.Write(bytes.Repeat([]byte("中"), MinMaxBytes)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > MinMaxBytes {
		t.Fatalf("log size exceeded bound: %d > %d", info.Size(), MinMaxBytes)
	}
}
