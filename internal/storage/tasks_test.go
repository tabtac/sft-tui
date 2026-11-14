package storage

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"sft-tui/internal/protocol"
)

func TestDownloadTaskLifecycle(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	ctx := context.Background()

	// 1. 先发布一个文件
	content := []byte("hello secure file transfer")
	pubTask := reviewPublish(t, s, "alice", "download_me.txt", content, 1)

	// 2. 创建下载任务
	getReq := protocol.GetRequest{
		RequestID: protocol.RequestID{10},
		FileName:  "download_me.txt",
	}
	dlTask, f, err := s.CreateDownloadTask(ctx, "alice", getReq)
	if err != nil {
		t.Fatalf("CreateDownloadTask: %v", err)
	}
	defer f.Close()

	if dlTask.Direction != protocol.DirectionDownload || dlTask.FileSize != uint64(len(content)) || dlTask.ObjectID != pubTask.ObjectID {
		t.Fatalf("unexpected dlTask: %+v", dlTask)
	}

	// 3. 幂等性：同一 RequestID 再次请求应返回相同任务
	dlAgain, fAgain, err := s.CreateDownloadTask(ctx, "alice", getReq)
	if err != nil {
		t.Fatalf("idempotent CreateDownloadTask failed: %v", err)
	}
	defer fAgain.Close()
	if dlAgain.ID != dlTask.ID {
		t.Fatalf("idempotent retry produced different task: %v != %v", dlAgain.ID, dlTask.ID)
	}

	// 4. 读取数据块
	chunk1, err := s.ReadChunkAt(ctx, dlTask.ID, 0, 5)
	if err != nil || !bytes.Equal(chunk1, []byte("hello")) {
		t.Fatalf("ReadChunkAt offset 0: got %q, err %v", chunk1, err)
	}

	// 4.5 模拟进度落盘后服务端重启，下载任务不得被错误标记为 FAILED
	if err := s.CheckpointDownload(ctx, dlTask.ID, 5); err != nil {
		t.Fatalf("CheckpointDownload before reopen: %v", err)
	}
	_ = f.Close()
	_ = fAgain.Close()
	if err := s.Close(); err != nil {
		t.Fatalf("Close before reopen: %v", err)
	}
	reopened, err := OpenStore(root)
	if err != nil {
		t.Fatalf("OpenStore reopen: %v", err)
	}
	s = reopened
	recoveredDL, err := s.GetTask(ctx, dlTask.ID)
	if err != nil {
		t.Fatalf("GetTask after reopen: %v", err)
	}
	if recoveredDL.Status != TaskActive || recoveredDL.Offset != 5 {
		t.Fatalf("reopen corrupted active download task: %+v", recoveredDL)
	}

	chunk2, err := s.ReadChunkAt(ctx, dlTask.ID, 5, 200)
	if err != nil || !bytes.Equal(chunk2, []byte(" secure file transfer")) {
		t.Fatalf("ReadChunkAt offset 5: got %q, err %v", chunk2, err)
	}

	// 5. 检查点推进
	if err := s.CheckpointDownload(ctx, dlTask.ID, 5); err != nil {
		t.Fatalf("CheckpointDownload: %v", err)
	}
	if err := s.CheckpointDownload(ctx, dlTask.ID, uint64(len(content))); err != nil {
		t.Fatalf("CheckpointDownload to end: %v", err)
	}

	// 6. 完成下载
	if err := s.FinishDownload(ctx, dlTask.ID); err != nil {
		t.Fatalf("FinishDownload: %v", err)
	}
	finalTask, err := s.GetTask(ctx, dlTask.ID)
	if err != nil || finalTask.Status != TaskCompleted {
		t.Fatalf("unexpected final task state: %+v, err: %v", finalTask, err)
	}

	// 7. 异常分支验证：已完成任务读取/推进检查点
	if _, err := s.ReadChunkAt(ctx, dlTask.ID, 0, 10); err != ErrTaskNotActive {
		t.Errorf("expected ErrTaskNotActive, got: %v", err)
	}
	if err := s.CheckpointDownload(ctx, dlTask.ID, 0); err != ErrTaskNotActive {
		t.Errorf("expected ErrTaskNotActive on checkpoint, got: %v", err)
	}
	if err := s.FinishDownload(ctx, dlTask.ID); err != ErrTaskNotActive {
		t.Errorf("expected ErrTaskNotActive on finish, got: %v", err)
	}
}

func TestDownloadTaskEdgeCases(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	ctx := context.Background()

	// 1. 下载不存在的文件
	getReq := protocol.GetRequest{RequestID: protocol.RequestID{99}, FileName: "nonexistent"}
	if _, _, err := s.CreateDownloadTask(ctx, "alice", getReq); err != ErrFileNotFound {
		t.Errorf("expected ErrFileNotFound, got: %v", err)
	}

	// 2. 无效文件名
	badReq := protocol.GetRequest{RequestID: protocol.RequestID{99}, FileName: "../escape"}
	if _, _, err := s.CreateDownloadTask(ctx, "alice", badReq); err == nil {
		t.Error("expected error for bad filename")
	}

	// 3. 上传任务不能调用 ReadChunkAt
	upTask := reviewCreateTask(t, s, "alice", reviewRequest("up.txt", []byte("123"), 1))
	if _, err := s.ReadChunkAt(ctx, upTask.ID, 0, 2); err == nil {
		t.Error("expected error when ReadChunkAt is called on upload task")
	}

	// 4. ReadChunkAt 与 CheckpointDownload 越界检查
	pubTask := reviewPublish(t, s, "alice", "edge_dl.txt", []byte("data"), 2)
	dlTask, f, err := s.CreateDownloadTask(ctx, "alice", protocol.GetRequest{RequestID: protocol.RequestID{22}, FileName: "edge_dl.txt"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if b, err := s.ReadChunkAt(ctx, dlTask.ID, 0, 0); err != nil || b != nil {
		t.Errorf("expected nil for length 0, got %v %v", b, err)
	}
	if _, err := s.ReadChunkAt(ctx, dlTask.ID, 100, 5); err != ErrOffsetMismatch {
		t.Errorf("expected ErrOffsetMismatch, got %v", err)
	}
	if err := s.CheckpointDownload(ctx, dlTask.ID, 100); err != ErrOffsetMismatch {
		t.Errorf("expected ErrOffsetMismatch, got %v", err)
	}
	if err := s.CheckpointDownload(ctx, dlTask.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckpointDownload(ctx, dlTask.ID, 1); err != ErrOffsetMismatch {
		t.Errorf("expected rewind ErrOffsetMismatch, got %v", err)
	}
	_ = pubTask
}

func TestTaskPauseResumeCancelEdgeCases(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	ctx := context.Background()

	task := reviewCreateTask(t, s, "alice", reviewRequest("edge.txt", []byte("123"), 1))

	// 1. Resume 一个非 PAUSED 状态的任务应返回 ErrTaskNotPaused
	if _, err := s.ResumeTask(ctx, task.ID); err != ErrTaskNotPaused {
		t.Errorf("expected ErrTaskNotPaused, got: %v", err)
	}

	// 2. 取消任务
	if err := s.CancelTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}

	// 3. 再次取消已是终态的任务应返回 ErrTaskTerminal
	if err := s.CancelTask(ctx, task.ID); err != ErrTaskTerminal {
		t.Errorf("expected ErrTaskTerminal, got: %v", err)
	}
}

func TestTaskPauseResumeCancel(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	ctx := context.Background()

	task := reviewCreateTask(t, s, "alice", reviewRequest("pause_test.txt", []byte("12345"), 1))

	// 1. 写入一部分数据
	reviewWrite(t, s, task, 0, []byte("12"))

	// 2. 暂停任务
	if err := s.PauseTask(ctx, task.ID); err != nil {
		t.Fatalf("PauseTask: %v", err)
	}
	pausedTask, _ := s.GetTask(ctx, task.ID)
	if pausedTask.Status != TaskPaused {
		t.Fatalf("expected PAUSED, got %s", pausedTask.Status)
	}

	// 处于 PAUSED 状态时写入应被拒绝
	if _, err := s.WriteChunkAt(ctx, task.ID, 2, []byte("3")); err == nil {
		t.Fatal("expected write to fail while paused")
	}

	// 3. 恢复任务
	resumedTask, err := s.ResumeTask(ctx, task.ID)
	if err != nil || resumedTask.Status != TaskActive {
		t.Fatalf("ResumeTask: got %+v, err: %v", resumedTask, err)
	}

	// 恢复后写入成功
	reviewWrite(t, s, task, 2, []byte("345"))

	// 4. 取消任务
	if err := s.CancelTask(ctx, task.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	cancelledTask, _ := s.GetTask(ctx, task.ID)
	if cancelledTask.Status != TaskCancelled {
		t.Fatalf("expected CANCELLED, got %s", cancelledTask.Status)
	}

	// staging 中的 .part 应当已被清理
	partPath := reviewPartPath(root, task.ID)
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Fatalf("cancelled task part file was not cleaned up: %v", err)
	}
}

func TestTaskExpirationAndCleanup(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	ctx := context.Background()

	task := reviewCreateTask(t, s, "alice", reviewRequest("expiring.txt", []byte("data"), 1))
	reviewWrite(t, s, task, 0, []byte("da"))

	// 1. 将其过期时间设为过去的某个时间
	past := time.Now().Add(-1 * time.Hour)
	reviewExec(t, s, "UPDATE tasks SET expires_at=? WHERE id=?", past.Unix(), IDToHex(task.ID))

	// 2. 执行过期检查
	expiredCount, err := s.ExpireTasks(ctx, time.Now())
	if err != nil || expiredCount != 1 {
		t.Fatalf("ExpireTasks: count=%d, err=%v", expiredCount, err)
	}

	tExp, err := s.GetTask(ctx, task.ID)
	if err != nil || tExp.Status != TaskExpired {
		t.Fatalf("expected EXPIRED, got %+v, err=%v", tExp, err)
	}

	// 暂存文件应当已被清理
	if _, err := os.Stat(reviewPartPath(root, task.ID)); !os.IsNotExist(err) {
		t.Fatal("expired task staging file was not removed")
	}

	// 3. 执行超过 retention 的终态任务清理
	reviewExec(t, s, "UPDATE tasks SET updated_at=? WHERE id=?", time.Now().Add(-48*time.Hour).Unix(), IDToHex(task.ID))
	cleaned, err := s.CleanupExpired(ctx, 24*time.Hour)
	if err != nil || cleaned != 1 {
		t.Fatalf("CleanupExpired: count=%d, err=%v", cleaned, err)
	}

	if _, err := s.GetTask(ctx, task.ID); err != ErrTaskNotFound {
		t.Fatalf("expected task to be deleted, got err=%v", err)
	}
}
