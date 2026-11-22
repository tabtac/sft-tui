package logfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
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

func TestWriterUTF8TruncationAndClosedLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	writer, err := Open(path, MinMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("中文🙂"), MinMaxBytes)
	written, err := writer.Write(payload)
	if err != nil || written != len(payload) {
		t.Fatalf("bounded writer returned wrong accepted length: %d %v", written, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Errorf("repeated Close=%v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(data) || int64(len(data)) > MinMaxBytes {
		t.Errorf("truncated log has invalid UTF-8/size: %d %v", len(data), err)
	}
	if n, err := writer.Write([]byte("late")); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Errorf("closed writer accepted data: %d %v", n, err)
	}
	reopened, err := Open(path, MinMaxBytes)
	if err != nil {
		t.Fatalf("Close leaked log lock: %v", err)
	}
	defer reopened.Close()
}

func TestConcurrentLogWritesRemainWholeRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	writer, err := Open(path, MinMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	const workers, records = 8, 40
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for record := 0; record < records; record++ {
				line := fmt.Sprintf("worker-%d-record-%02d\n", worker, record)
				if _, err := writer.Write([]byte(line)); err != nil {
					t.Errorf("concurrent log Write: %v", err)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	seen := make(map[string]bool)
	for _, line := range lines {
		if seen[line] {
			t.Errorf("duplicated log record: %q", line)
		}
		seen[line] = true
	}
	if len(seen) != workers*records {
		t.Errorf("lost/interleaved log records: got=%d want=%d", len(seen), workers*records)
	}
	for worker := 0; worker < workers; worker++ {
		for record := 0; record < records; record++ {
			if !seen[fmt.Sprintf("worker-%d-record-%02d", worker, record)] {
				t.Errorf("missing complete record: worker=%d record=%d", worker, record)
			}
		}
	}
}

func TestLogOpenRejectsSymbolicLinkWithoutTouchingTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "service.log")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	writer, err := Open(link, MinMaxBytes)
	if writer != nil {
		writer.Close()
	}
	if err == nil {
		t.Error("symlink service log accepted")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "preserve" {
		t.Errorf("log symlink target changed: %q %v", data, err)
	}
}

func TestWriterTwoSegmentRotationPreservesAuditHistoryWithinTotalBound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sftd.log")
	backup := path + ".1"

	w, err := Open(path, MinMaxBytes)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	// 写入首段审计事件（20 KiB）
	first := append([]byte("AUDIT_MARKER_SEG1\n"), bytes.Repeat([]byte("a"), 20*1024)...)
	if _, err := w.Write(first); err != nil {
		t.Fatal(err)
	}
	// 再写 20 KiB，超过单段 32 KiB 上限 -> 触发滚动，首段保留在 .1，新记录写入活动文件
	second := append([]byte("AUDIT_MARKER_SEG2\n"), bytes.Repeat([]byte("b"), 20*1024)...)
	if _, err := w.Write(second); err != nil {
		t.Fatal(err)
	}

	activeData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read active log: %v", err)
	}
	backupData, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("read rotated backup log: %v", err)
	}
	if !strings.Contains(string(backupData), "AUDIT_MARKER_SEG1") {
		t.Fatal("expected prior audit segment to be preserved in .1 after rotation")
	}
	if !strings.Contains(string(activeData), "AUDIT_MARKER_SEG2") {
		t.Fatal("expected new audit record in active log after rotation")
	}
	total := int64(len(activeData) + len(backupData))
	if total > MinMaxBytes {
		t.Fatalf("total rotated log size exceeded bound: %d > %d", total, MinMaxBytes)
	}
}
