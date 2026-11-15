package queue

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sft-tui/internal/protocol"
)

func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{
		StateDir:     dir,
		Identity:     "alice-key",
		MaxFileBytes: 10 * 1024 * 1024,
		MaxStreams:   3,
	}
	m, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() {
		m.Close()
	})
	return m, dir
}

func TestManagerEnqueueAndPreflight(t *testing.T) {
	m, dir := newTestManager(t)

	// 准备本地文件
	srcFile := filepath.Join(dir, "upload.bin")
	if err := os.WriteFile(srcFile, []byte("hello world"), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. 成功入队上传任务
	specs := []Spec{
		{
			Direction: protocol.DirectionUpload,
			Remote:    "upload.bin",
			Path:      srcFile,
			Size:      11,
		},
	}
	tasks, err := m.Enqueue(specs)
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}
	if len(tasks) != 1 || tasks[0].State != Waiting {
		t.Fatalf("unexpected tasks: %+v", tasks)
	}

	// 2. 目标冲突：同名上传任务未完成时再次入队应失败
	if _, err := m.Enqueue(specs); err == nil {
		t.Fatal("expected duplicate destination to be rejected")
	}

	// 3. 上传不存在的文件会进入 Pending 状态并记录错误
	badUpload := []Spec{
		{
			Direction: protocol.DirectionUpload,
			Remote:    "missing.bin",
			Path:      filepath.Join(dir, "missing.bin"),
		},
	}
	badTasks, err := m.Enqueue(badUpload)
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}
	if len(badTasks) != 1 || badTasks[0].State != Pending || badTasks[0].Error == "" {
		t.Fatalf("expected Pending state with error, got: %+v", badTasks[0])
	}

	// 4. 下载目标已存在会进入 Pending 状态并记录错误
	existingTarget := filepath.Join(dir, "exists.bin")
	if err := os.WriteFile(existingTarget, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	badDownload := []Spec{
		{
			Direction: protocol.DirectionDownload,
			Remote:    "remote_bad.bin",
			Path:      existingTarget,
		},
	}
	badDlTasks, err := m.Enqueue(badDownload)
	if err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}
	if len(badDlTasks) != 1 || badDlTasks[0].State != Pending || badDlTasks[0].Error == "" {
		t.Fatalf("expected Pending state with error, got: %+v", badDlTasks[0])
	}

	// 5. 成功入队下载任务
	validDownload := []Spec{
		{
			Direction: protocol.DirectionDownload,
			Remote:    "remote_ok.bin",
			Path:      filepath.Join(dir, "new_dl.bin"),
		},
	}
	dlTasks, err := m.Enqueue(validDownload)
	if err != nil {
		t.Fatalf("valid download enqueue failed: %v", err)
	}
	if len(dlTasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(dlTasks))
	}
}

func TestManagerPauseResumeCancel(t *testing.T) {
	m, dir := newTestManager(t)

	srcFile := filepath.Join(dir, "control.bin")
	if err := os.WriteFile(srcFile, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}

	tasks, err := m.Enqueue([]Spec{
		{
			Direction: protocol.DirectionUpload,
			Remote:    "control.bin",
			Path:      srcFile,
			Size:      4,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := tasks[0].ID

	// 1. 暂停
	if err := m.Pause(id); err != nil {
		t.Fatalf("Pause failed: %v", err)
	}
	t1 := m.Task(id)
	if t1.State != Paused {
		t.Fatalf("expected Paused, got %s", t1.State)
	}

	// 2. 恢复
	if err := m.Resume(id); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	t2 := m.Task(id)
	if t2.State != Waiting {
		t.Fatalf("expected Waiting, got %s", t2.State)
	}

	// 3. 取消（无检查点文件时直接成为 Cancelled 终态）
	if err := m.Cancel(id); err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}
	t3 := m.Task(id)
	if t3.State != Cancelled {
		t.Fatalf("expected Cancelled, got %s", t3.State)
	}

	// 4. 对终态任务调用 Pause 应报错
	if err := m.Pause(id); err == nil {
		t.Fatal("expected Pause on cancelled task to fail")
	}
}

func TestManagerPauseAllAndClearHistory(t *testing.T) {
	m, dir := newTestManager(t)

	srcFile := filepath.Join(dir, "f1.bin")
	_ = os.WriteFile(srcFile, []byte("1"), 0600)

	tasks, err := m.Enqueue([]Spec{
		{
			Direction: protocol.DirectionUpload,
			Remote:    "f1.bin",
			Path:      srcFile,
			Size:      1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// PauseAll
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.PauseAll(ctx); err != nil {
		t.Fatalf("PauseAll failed: %v", err)
	}

	tItem := m.Task(tasks[0].ID)
	if tItem.State != Paused {
		t.Fatalf("expected Paused after PauseAll, got %s", tItem.State)
	}

	// Cancel -> 进入历史
	if err := m.Cancel(tasks[0].ID); err != nil {
		t.Fatal(err)
	}

	// 清空历史
	if err := m.ClearHistory(); err != nil {
		t.Fatalf("ClearHistory failed: %v", err)
	}
	if len(m.history) != 0 {
		t.Errorf("expected history length 0, got %d", len(m.history))
	}
}

func TestManagerQuiesce(t *testing.T) {
	m, _ := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Quiesce(ctx); err != nil {
		t.Fatalf("Quiesce failed: %v", err)
	}
}

func TestManagerConflictInSameBatch(t *testing.T) {
	m, dir := newTestManager(t)

	f1 := filepath.Join(dir, "f1.bin")
	_ = os.WriteFile(f1, []byte("1"), 0600)

	// 同批包含相同远程目标
	specs := []Spec{
		{Direction: protocol.DirectionUpload, Remote: "same.bin", Path: f1},
		{Direction: protocol.DirectionUpload, Remote: "same.bin", Path: f1},
	}
	if _, err := m.Enqueue(specs); err == nil {
		t.Fatal("expected duplicate destination in same batch to fail")
	}
}

func TestManagerPauseAllAndResumeAll(t *testing.T) {
	m, dir := newTestManager(t)
	f1 := filepath.Join(dir, "r1.bin")
	f2 := filepath.Join(dir, "r2.bin")
	_ = os.WriteFile(f1, []byte("one"), 0600)
	_ = os.WriteFile(f2, []byte("two"), 0600)

	tasks, err := m.Enqueue([]Spec{
		{Direction: protocol.DirectionUpload, Remote: "r1.bin", Path: f1},
		{Direction: protocol.DirectionUpload, Remote: "r2.bin", Path: f2},
	})
	if err != nil || len(tasks) != 2 {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.PauseAll(context.Background()); err != nil {
		t.Fatalf("PauseAll: %v", err)
	}
	for _, tk := range m.Snapshot(false) {
		if tk.State != Paused {
			t.Fatalf("expected Paused after PauseAll, got %s", tk.State)
		}
	}
	if err := m.ResumeAll(); err != nil {
		t.Fatalf("ResumeAll: %v", err)
	}
	for _, tk := range m.Snapshot(false) {
		if tk.State != Waiting {
			t.Fatalf("expected Waiting after ResumeAll, got %s", tk.State)
		}
	}
}
