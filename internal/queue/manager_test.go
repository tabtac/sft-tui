package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sft-tui/internal/pal"
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

func enqueueControlFixture(t *testing.T, m *Manager, dir string) Task {
	t.Helper()
	path := filepath.Join(dir, "controlled.bin")
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	tasks, err := m.Enqueue([]Spec{{Direction: protocol.DirectionUpload, Remote: "controlled.bin", Path: path}})
	if err != nil {
		t.Fatal(err)
	}
	return tasks[0]
}

func awaitQueueState(t *testing.T, m *Manager, id string, state State) Task {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		task := m.Task(id)
		if task != nil && task.State == state && !m.HasRunning() {
			return *task
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("task did not settle in %s: %+v", state, m.Task(id))
	return Task{}
}

func TestQueueWithoutRunnerKeepsTaskWaiting(t *testing.T) {
	m, dir := newTestManager(t)
	task := enqueueControlFixture(t, m, dir)
	m.dispatch()
	if got := m.Task(task.ID); got.State != Waiting || got.Done != 0 || m.HasRunning() {
		t.Errorf("unconfigured executor completed/dispatched task: %+v", got)
	}
}

func TestControlFailureRequiresExplicitAcknowledgement(t *testing.T) {
	for _, mode := range []protocol.CancelMode{protocol.CancelPause, protocol.CancelAbort} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			m, dir := newTestManager(t)
			task := enqueueControlFixture(t, m, dir)
			if err := m.UpdateTask(task.ID, func(cur *Task) { cur.TransferID = strings.Repeat("1", 32) }); err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			failure := errors.New("peer control acknowledgement lost")
			m.SetRunner(func(ctx context.Context, task Task, op *Operation, progress func(uint64, uint64, State)) error {
				close(started)
				<-ctx.Done()
				return failure
			})
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("runner was not dispatched")
			}
			var err error
			if mode == protocol.CancelPause {
				err = m.Pause(task.ID)
			} else {
				err = m.Cancel(task.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := awaitQueueState(t, m, task.ID, Pending)
			if !strings.Contains(got.Error, failure.Error()) || got.TransferID == "" {
				t.Errorf("control failure lost error/checkpoint: %+v", got)
			}
			if mode == protocol.CancelAbort && !got.WantCancel {
				t.Error("failed cancel lost persistent intent")
			}
		})
	}
}

func TestHasCheckpointIncludesUnansweredRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		task Task
		want bool
	}{
		{"empty", Task{}, false},
		{"request_only", Task{RequestID: strings.Repeat("1", 32)}, true},
		{"transfer", Task{TransferID: strings.Repeat("2", 32)}, true},
		{"temp_name", Task{TempName: "part"}, true},
		{"temp_inode", Task{TempInode: 1}, true},
		{"published", Task{Published: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// BUG: PUT/GET 已发送但响应丢失时仅有 RequestID，仍需幂等查询并取消服务端任务。
			if got := tc.task.HasCheckpoint(); got != tc.want {
				t.Errorf("BUG: HasCheckpoint=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestCancelUnansweredRequestKeepsRemoteCleanupIntent(t *testing.T) {
	m, dir := newTestManager(t)
	task := enqueueControlFixture(t, m, dir)
	if err := m.UpdateTask(task.ID, func(cur *Task) { cur.State = Paused; cur.RequestID = strings.Repeat("1", 32) }); err != nil {
		t.Fatal(err)
	}
	if err := m.Cancel(task.ID); err != nil {
		t.Fatal(err)
	}
	got := m.Task(task.ID)
	// BUG: RequestID-only 任务被当作从未发送，直接写入 Cancelled 并跳过远端清理。
	if got.Terminal() || !got.WantCancel || got.RequestID == "" {
		t.Errorf("BUG: unacknowledged request cancelled locally: state=%s wantCancel=%v", got.State, got.WantCancel)
	}
}

func TestQuiescePropagatesUnconfirmedCancellation(t *testing.T) {
	m, dir := newTestManager(t)
	task := enqueueControlFixture(t, m, dir)
	if err := m.UpdateTask(task.ID, func(cur *Task) { cur.State = Pending; cur.WantCancel = true; cur.TransferID = strings.Repeat("1", 32) }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// BUG: Quiesce 在 active 为空时吞掉 PauseAll 对未完成取消的错误。
	if err := m.Quiesce(ctx); err == nil {
		t.Error("BUG: quiesce reported success with unresolved cancellation")
	}
}

func TestHistoryCountLimitAlsoBoundsTerminalCurrentTasks(t *testing.T) {
	m, _ := newTestManager(t)
	m.mu.Lock()
	now := time.Now()
	for i := 0; i < HistoryLimit+5; i++ {
		id := fmt.Sprintf("terminal-%d", i)
		task := &Task{ID: id, State: Completed, Updated: now.Add(time.Duration(i) * time.Nanosecond)}
		m.tasks[id] = task
		m.history = append(m.history, *task)
	}
	m.pruneRecords(now)
	current, history := len(m.tasks), len(m.history)
	m.mu.Unlock()
	// BUG: 当前仅按时间删除终态，数量上限只作用于 history，长期运行会积累当前任务映射。
	if current > HistoryLimit || history > HistoryLimit {
		t.Errorf("BUG: terminal retention current=%d history=%d limit=%d", current, history, HistoryLimit)
	}
}

func TestUpdateCheckpointRollbackOnDatabaseFailure(t *testing.T) {
	m, dir := newTestManager(t)
	task := enqueueControlFixture(t, m, dir)
	if _, err := m.store.db.Exec(`CREATE TRIGGER reject_checkpoint BEFORE INSERT ON tasks BEGIN SELECT RAISE(FAIL,'checkpoint write rejected'); END`); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateTask(task.ID, func(cur *Task) { cur.RequestID = strings.Repeat("1", 32); cur.Done = 2 }); err == nil {
		t.Error("injected persistence failure not returned")
	}
	got := m.Task(task.ID)
	if got.RequestID != task.RequestID || got.Done != task.Done {
		t.Errorf("failed checkpoint update leaked into memory: %+v", got)
	}
	if _, err := m.store.db.Exec("DROP TRIGGER reject_checkpoint"); err != nil {
		t.Fatal(err)
	}
}

func TestIPCHardeningRejectsUnknownFieldsAndTrailingObjects(t *testing.T) {
	m, dir := newTestManager(t)
	_ = m

	// 1. 合法 RemoteStatus 正常返回
	if _, err := RemoteStatus(dir, "alice-key"); err != nil {
		t.Fatalf("RemoteStatus: %v", err)
	}

	ep := IPCPath(dir, "alice-key")
	sendRaw := func(payload string) ipcResponse {
		t.Helper()
		conn, err := pal.DialIPC(ep, 2*time.Second)
		if err != nil {
			t.Fatalf("DialIPC: %v", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte(payload)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		var resp ipcResponse
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			t.Fatalf("Decode resp for %q: %v", payload, err)
		}
		return resp
	}

	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"unknown_field", `{"op":"status","unknown":true}` + "\n"},
		{"trailing_object", `{"op":"status"}{"op":"status"}` + "\n"},
		{"non_object", `["status"]` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := sendRaw(tc.payload)
			if resp.OK || resp.Error == "" {
				t.Fatalf("expected error response for %s, got %+v", tc.name, resp)
			}
		})
	}
}
