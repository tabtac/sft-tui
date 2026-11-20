package queue

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sft-tui/internal/protocol"
)

func TestQueueCrashRecoveryAndActiveStateReset(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		StateDir:     dir,
		Identity:     "recovery-identity",
		MaxFileBytes: 10 * 1024 * 1024,
		MaxStreams:   2,
	}

	// 1. 模拟崩溃前向数据库写入处于活跃状态（Transferring）的任务
	srcFile := filepath.Join(dir, "source.bin")
	if err := os.WriteFile(srcFile, []byte("data content"), 0600); err != nil {
		t.Fatal(err)
	}

	stateKey, err := StateKeyForIdentity(cfg.Identity, protocol.DirectionUpload, "remote.bin", srcFile)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	activeTask := Task{
		ID:        "crash-task-1",
		Batch:     "batch-1",
		Identity:  cfg.Identity,
		StateKey:  stateKey,
		Direction: protocol.DirectionUpload,
		Remote:    "remote.bin",
		Path:      srcFile,
		State:     Transferring,
		Done:      4,
		Size:      12,
		Created:   now,
		Updated:   now,
	}

	s, err := OpenStore(cfg.StateDir, cfg.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write([]Task{activeTask}, false); err != nil {
		s.Close()
		t.Fatal(err)
	}
	s.Close()

	// 2. 模拟重启：调用 Open 初始化管理器
	m, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer m.Close()

	// 3. 验证任务状态已被自动安全转为 Paused，并附带提示
	task := m.Task("crash-task-1")
	if task == nil {
		t.Fatal("expected task to exist after recovery")
	}
	if task.State != Paused {
		t.Errorf("expected state Paused, got %s", task.State)
	}
	if task.Error != "上次退出时任务未完成，请选择继续" {
		t.Errorf("expected error message, got %q", task.Error)
	}

	// 4. 验证底层 SQLite 已持久化该恢复状态
	s2, err := OpenStore(cfg.StateDir, cfg.Identity)
	// 因为 m 还持有 lock，直接 openStore 会被锁互斥
	// 所以我们关闭 m 再打开 store 校验
	if err == nil {
		s2.Close()
		t.Fatal("expected lock exclusivity conflict while manager is running")
	}

	m.Close()

	s3, err := OpenStore(cfg.StateDir, cfg.Identity)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()

	persisted, err := s3.Load(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 || persisted[0].State != Paused {
		t.Errorf("persisted state mismatch: %+v", persisted)
	}
}

func TestQueueRejectCorruptCheckpointIndex(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		StateDir: dir,
		Identity: "corrupt-identity",
	}

	srcFile := filepath.Join(dir, "f.bin")
	_ = os.WriteFile(srcFile, []byte("ok"), 0600)

	now := time.Now()
	badTask := Task{
		ID:        "bad-task",
		Batch:     "batch-1",
		Identity:  cfg.Identity,
		StateKey:  "corrupted-tampered-state-key",
		Direction: protocol.DirectionUpload,
		Remote:    "remote.bin",
		Path:      srcFile,
		State:     Pending,
		Size:      2,
		Created:   now,
		Updated:   now,
	}

	s, err := OpenStore(cfg.StateDir, cfg.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write([]Task{badTask}, false); err != nil {
		s.Close()
		t.Fatal(err)
	}
	s.Close()

	_, err = Open(cfg)
	if err == nil {
		t.Fatal("expected Open to fail due to corrupted state key")
	}
}

func TestConcurrentSnapshotAndStress(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		StateDir:     dir,
		Identity:     "stress-identity",
		MaxFileBytes: 10 * 1024 * 1024,
		MaxStreams:   4,
	}

	m, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	var runningCount atomic.Int32
	m.SetRunner(func(ctx context.Context, task Task, op *Operation, progress func(transferred, total uint64, phase State)) error {
		runningCount.Add(1)
		defer runningCount.Add(-1)
		for i := uint64(1); i <= 5; i++ {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				progress(i*2, 10, Transferring)
				time.Sleep(5 * time.Millisecond)
			}
		}
		return nil
	})

	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// 并发读取快照（模拟 60FPS TUI 高频读取）
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					_ = m.Snapshot(false)
					_ = m.Snapshot(true)
					time.Sleep(2 * time.Millisecond)
				}
			}
		}()
	}

	// 动态入队任务
	for i := 0; i < 10; i++ {
		fn := filepath.Join(dir, fmt.Sprintf("stress-%d.bin", i))
		_ = os.WriteFile(fn, []byte("sample-data"), 0600)
		_, err := m.Enqueue([]Spec{
			{Direction: protocol.DirectionUpload, Remote: fmt.Sprintf("stress-%d.bin", i), Path: fn, Size: 10},
		})
		if err != nil {
			t.Errorf("enqueue failed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	wg.Wait()
}

func TestControlPersistenceFailureKeepsIntent(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%v", active), func(t *testing.T) {
			dir := t.TempDir()
			identity := "persistence-test-identity"
			s, err := OpenStore(dir, identity)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			now := time.Now()
			task := &Task{
				ID:       "task-one",
				State:    Waiting,
				Identity: identity,
				Created:  now,
				Updated:  now,
			}

			m := &Manager{
				store:   s,
				tasks:   map[string]*Task{"task-one": task},
				active:  map[string]*running{},
				cfg:     Config{StateDir: dir, Identity: identity},
			}

			if active {
				op := NewOperation(context.Background())
				defer op.Close()
				m.active[task.ID] = &running{op: op}
			}

			// 关闭底层 SQLite 连接来人为制造持久化失败
			if err := s.db.Close(); err != nil {
				t.Fatal(err)
			}

			// 调用 Pause，预期失败
			if err := m.Pause(task.ID); err == nil {
				t.Fatal("expected Pause to fail when database is closed")
			}

			// 调度应当被暂停 (hold)，且记录了 storageErr
			if !m.hold || m.storageErr == nil {
				t.Fatal("dispatch not stopped on database failure")
			}

			if active {
				// 活跃任务被暂停时，已发出的 Pausing 请求不应被回退
				if task.State != Pausing || m.active[task.ID].op.Mode() != protocol.CancelPause {
					t.Fatalf("expected Pausing state and CancelPause mode, got state=%s mode=%v", task.State, m.active[task.ID].op.Mode())
				}
			} else {
				// 空闲任务持久化失败时，状态应回退为原状态 Waiting
				if task.State != Waiting {
					t.Fatalf("expected idle task state rolled back to Waiting, got %s", task.State)
				}
			}
		})
	}
}

