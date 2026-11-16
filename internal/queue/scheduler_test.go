package queue

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"sft-tui/internal/protocol"
)

func TestSnapshotReuseInvalidationAndIsolation(t *testing.T) {
	m, dir := newTestManager(t)

	srcFile := filepath.Join(dir, "snap.bin")
	_ = os.WriteFile(srcFile, []byte("data"), 0600)

	tasks, err := m.Enqueue([]Spec{
		{Direction: protocol.DirectionUpload, Remote: "snap.bin", Path: srcFile, Size: 4},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. 获取活动视图快照并验证内存隔离
	s1 := m.Snapshot(false)
	if len(s1) != 1 {
		t.Fatalf("expected 1 task in snapshot, got %d", len(s1))
	}
	s1[0].Remote = "mutated.bin"

	s2 := m.Snapshot(false)
	if s2[0].Remote != "snap.bin" {
		t.Errorf("Snapshot mutation leaked into internal cache: %s", s2[0].Remote)
	}

	// 2. 完成任务并获取历史快照
	_ = m.Cancel(tasks[0].ID)

	h1 := m.Snapshot(true)
	if len(h1) != 1 {
		t.Fatalf("expected 1 history record, got %d", len(h1))
	}
	h1[0].Remote = "mutated_history.bin"

	h2 := m.Snapshot(true)
	if h2[0].Remote == "mutated_history.bin" {
		t.Error("history Snapshot mutation leaked into internal cache")
	}
}

func TestActiveSnapshotSpeedAndETA(t *testing.T) {
	m, dir := newTestManager(t)

	srcFile := filepath.Join(dir, "speed.bin")
	_ = os.WriteFile(srcFile, []byte("large file content"), 0600)

	tasks, err := m.Enqueue([]Spec{
		{Direction: protocol.DirectionUpload, Remote: "speed.bin", Path: srcFile, Size: 10000},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := tasks[0].ID

	// 模拟将任务置为 Transferring 并添加采样点
	m.mu.Lock()
	r := &running{op: NewOperation(m.ctx)}
	m.active[id] = r
	m.tasks[id].State = Transferring
	t0 := time.Now().Add(-1 * time.Second)
	t1 := time.Now()
	r.samples = []sample{
		{at: t0, bytes: 1000},
		{at: t1, bytes: 6000},
	}
	m.mu.Unlock()

	snap := m.Snapshot(false)
	if len(snap) != 1 {
		t.Fatalf("expected 1 task, got %d", len(snap))
	}

	task := snap[0]
	if task.Speed <= 0 {
		t.Errorf("expected positive Speed, got %f", task.Speed)
	}
	if task.ETA <= 0 {
		t.Errorf("expected positive ETA, got %v", task.ETA)
	}
}

func TestSchedulerConcurrencyLimit(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		StateDir:     dir,
		Identity:     "sched-identity",
		MaxFileBytes: 10 * 1024 * 1024,
		MaxStreams:   2,
	}
	m, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	var runningCount atomic.Int32
	var maxConcurrent atomic.Int32
	done := make(chan struct{})
	started := make(chan struct{}, 4)

	m.SetRunner(func(ctx context.Context, task Task, op *Operation, progress func(transferred, total uint64, phase State)) error {
		cur := runningCount.Add(1)
		for {
			old := maxConcurrent.Load()
			if cur <= old || maxConcurrent.CompareAndSwap(old, cur) {
				break
			}
		}
		defer runningCount.Add(-1)
		started <- struct{}{}

		progress(1, task.Size, Transferring)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return nil
		}
	})

	var specs []Spec
	for i := 0; i < 4; i++ {
		fn := filepath.Join(dir, fmt.Sprintf("f%d.bin", i))
		_ = os.WriteFile(fn, []byte("x"), 0600)
		specs = append(specs, Spec{
			Direction: protocol.DirectionUpload,
			Remote:    fmt.Sprintf("r%d.bin", i),
			Path:      fn,
			Size:      1,
		})
	}

	tasks, err := m.Enqueue(specs)
	if err != nil {
		t.Fatal(err)
	}

	// 使用执行器握手确认两个槽位确实被使用，避免“完全未派发也通过上限测试”。
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("scheduler failed to fill both available slots")
		}
	}

	if mc := maxConcurrent.Load(); mc != 2 {
		t.Errorf("concurrency did not use exactly two slots: %d", mc)
	}

	close(done)
	for _, task := range tasks {
		awaitQueueState(t, m, task.ID, Completed)
	}
	if mc := maxConcurrent.Load(); mc > 2 {
		t.Errorf("concurrency exceeded limit while draining remaining tasks: %d > 2", mc)
	}
}
