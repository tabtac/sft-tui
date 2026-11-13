package queue

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sft-tui/internal/protocol"
)

func TestQueueStateProperties(t *testing.T) {
	actives := []State{Waiting, Preparing, Hashing, Transferring, Verifying, Reconnecting, Pausing, Cancelling}
	for _, s := range actives {
		if !s.Active() {
			t.Errorf("expected %s to be Active", s)
		}
	}

	terminals := []State{Completed, Cancelled, Skipped}
	for _, s := range terminals {
		if !s.Terminal() {
			t.Errorf("expected %s to be Terminal", s)
		}
		if s.Active() {
			t.Errorf("expected %s not to be Active", s)
		}
	}

	controllings := []State{Pausing, Cancelling}
	for _, s := range controllings {
		if !s.Controlling() {
			t.Errorf("expected %s to be Controlling", s)
		}
	}
}

func TestQueueTargetKeyAndStateKey(t *testing.T) {
	k1 := TargetKey(protocol.DirectionUpload, "remote.bin", "/tmp/local.bin")
	if k1 != "1:remote.bin" {
		t.Errorf("unexpected upload target key: %s", k1)
	}

	k2 := TargetKey(protocol.DirectionDownload, "remote.bin", "/tmp/local.bin")
	cleanPath := filepath.Clean("/tmp/local.bin")
	if !strings.HasPrefix(k2, "2:") || !strings.Contains(k2, cleanPath) {
		t.Errorf("unexpected download target key: %s", k2)
	}

	key, err := StateKeyForIdentity("alice-identity", protocol.DirectionUpload, "file.txt", "/tmp/file.txt")
	if err != nil || len(key) != 64 {
		t.Fatalf("StateKeyForIdentity failed: key=%s err=%v", key, err)
	}
}

func TestQueueStoreExclusivityAndPersistence(t *testing.T) {
	dir := t.TempDir()
	identity := "test-user-identity"

	// 1. 打开存储
	s1, err := OpenStore(dir, identity)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer s1.Close()

	// 2. 相同 identity 再次打开应被排他锁拦截
	if _, err := OpenStore(dir, identity); err == nil {
		t.Fatal("expected second OpenStore with same identity to fail due to lock")
	}

	// 3. 写入活动任务并验证加载
	task1 := Task{
		ID:        "task-1",
		Identity:  identity,
		Direction: protocol.DirectionUpload,
		Remote:    "file1.bin",
		Path:      filepath.Join(dir, "file1.bin"),
		State:     Waiting,
		Size:      1024,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s1.Write([]Task{task1}, false); err != nil {
		t.Fatalf("Write active task failed: %v", err)
	}

	loaded, err := s1.Load(false)
	if err != nil || len(loaded) != 1 || loaded[0].ID != "task-1" {
		t.Fatalf("Load active tasks: got %+v, err=%v", loaded, err)
	}

	// 4. 将任务转入终态并写入历史
	task1.State = Completed
	task1.Done = 1024
	task1.Updated = time.Now()
	if err := s1.Write([]Task{task1}, true); err != nil {
		t.Fatalf("Write completed task failed: %v", err)
	}

	// 未完成队列中任务应当已被删除
	loadedActive, err := s1.Load(false)
	if err != nil || len(loadedActive) != 0 {
		t.Fatalf("expected 0 active tasks, got %d", len(loadedActive))
	}

	// 历史记录中应当能查到
	loadedHistory, err := s1.Load(true)
	if err != nil || len(loadedHistory) != 1 || loadedHistory[0].ID != "task-1" {
		t.Fatalf("Load history tasks: got %+v, err=%v", loadedHistory, err)
	}
}

func TestQueueStoreHistoryPruning(t *testing.T) {
	dir := t.TempDir()
	identity := "pruning-identity"

	s, err := OpenStore(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 写入过期历史与超量历史
	var batch []Task
	for i := 0; i < 15; i++ {
		tItem := Task{
			ID:        fmt.Sprintf("t-%d", i),
			Identity:  identity,
			Direction: protocol.DirectionUpload,
			Remote:    fmt.Sprintf("f-%d", i),
			Path:      filepath.Join(dir, fmt.Sprintf("f-%d", i)),
			State:     Completed,
			Created:   time.Now(),
			Updated:   time.Now(),
		}
		if i == 0 {
			// 将第一个任务的更新时间置为 35 天前（超过 30 天）
			tItem.Updated = time.Now().Add(-35 * 24 * time.Hour)
		}
		batch = append(batch, tItem)
	}

	if err := s.Write(batch, true); err != nil {
		t.Fatalf("Write history batch failed: %v", err)
	}

	history, err := s.Load(true)
	if err != nil {
		t.Fatal(err)
	}
	// 第 0 个任务超过保留期应当已被裁剪
	for _, h := range history {
		if h.ID == "t-0" {
			t.Errorf("expected expired history record t-0 to be pruned")
		}
	}
}

func TestQueueStoreRejectNewerVersion(t *testing.T) {
	dir := t.TempDir()
	identity := "version-check"

	s, err := OpenStore(dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	// 人为拔高 user_version
	if _, err := s.db.Exec("PRAGMA user_version=99;"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := OpenStore(dir, identity); err == nil {
		t.Fatal("expected OpenStore to reject database with higher user_version")
	}
}
