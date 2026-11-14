package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"sft-tui/internal/protocol"
)

func TestSessionLeasesQuotaAndResume(t *testing.T) {
	ctx := context.Background()
	s := reviewOpenStore(t, t.TempDir())
	s.Configure(Limits{
		MaxFileBytes:               10,
		UserStorageBytes:           15,
		MaxPendingTransfersPerUser: 2,
		MaxFilesPerUser:            2,
		TransferRetention:          time.Hour,
	})

	// 1. 超出单文件限额应返回 ErrQuotaExceeded (映射 CodeQuotaExceeded)
	oversize := protocol.PutRequest{RequestID: protocol.RequestID{1}, FileName: "big.bin", FileSize: 11}
	if _, _, err := s.BeginUploadSession(ctx, "alice", oversize); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected ErrQuotaExceeded, got %v", err)
	} else if AsRemote(err).Code != protocol.CodeQuotaExceeded {
		t.Fatalf("expected CodeQuotaExceeded, got %v", AsRemote(err).Code)
	}

	// 2. 正常开始上传会话并持有租约
	data := []byte("01234567")
	req := protocol.PutRequest{
		RequestID:  protocol.RequestID{2},
		FileName:   "part.bin",
		FileSize:   uint64(len(data)),
		FileSHA256: sha256.Sum256(data),
	}
	task, lease1, err := s.BeginUploadSession(ctx, "alice", req)
	if err != nil {
		t.Fatalf("BeginUploadSession: %v", err)
	}
	if _, err := s.WriteUploadChunk(ctx, task.ID, lease1, 0, data[:4]); err != nil {
		t.Fatalf("WriteUploadChunk: %v", err)
	}

	// 同一 TaskID 在租约未释放前被另一条流 RESUME 必须报 ErrTaskBusy
	resumeReq := protocol.ResumeRequest{
		TransferID:   task.ID,
		Direction:    protocol.DirectionUpload,
		ClientOffset: 0,
		FileSHA256:   req.FileSHA256,
	}
	if _, _, err := s.ResumeUploadSession(ctx, "alice", resumeReq); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("expected ErrTaskBusy while lease held, got %v", err)
	}

	// 暂停释放后，新流可恢复并从偏移 4 继续完成
	if err := s.ReleaseTask(ctx, task.ID, lease1, TaskPaused); err != nil {
		t.Fatalf("ReleaseTask pause: %v", err)
	}
	resumed, lease2, err := s.ResumeUploadSession(ctx, "alice", resumeReq)
	if err != nil {
		t.Fatalf("ResumeUploadSession: %v", err)
	}
	if resumed.Offset != 4 {
		t.Fatalf("expected resumed offset 4, got %d", resumed.Offset)
	}
	// 旧租约失效不得修改新租约状态
	if _, err := s.WriteUploadChunk(ctx, task.ID, lease1, 4, data[4:]); !errors.Is(err, ErrTaskNotActive) {
		t.Fatalf("expected stale lease to fail, got %v", err)
	}
	if _, err := s.WriteUploadChunk(ctx, task.ID, lease2, 4, data[4:]); err != nil {
		t.Fatalf("WriteUploadChunk lease2: %v", err)
	}
	if err := s.CommitUploadSession(ctx, task.ID, lease2, uint64(len(data)), req.FileSHA256); err != nil {
		t.Fatalf("CommitUploadSession: %v", err)
	}

	// 3. 存储空间配额校验（已用 8/15，再传 8 字节超限）
	req2 := protocol.PutRequest{RequestID: protocol.RequestID{3}, FileName: "second.bin", FileSize: 8, FileSHA256: sha256.Sum256(data)}
	if _, _, err := s.BeginUploadSession(ctx, "alice", req2); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected user storage quota exceeded, got %v", err)
	}

	// 4. 下载会话及恢复校验
	dlReq := protocol.GetRequest{RequestID: protocol.RequestID{4}, FileName: "part.bin"}
	dlTask, dlFile, dlLease1, err := s.BeginDownloadSession(ctx, "alice", dlReq)
	if err != nil {
		t.Fatalf("BeginDownloadSession: %v", err)
	}
	_ = dlFile.Close()

	if err := s.CheckpointDownloadSession(ctx, dlTask.ID, dlLease1, 4); err != nil {
		t.Fatalf("CheckpointDownloadSession: %v", err)
	}
	if err := s.ReleaseTask(ctx, dlTask.ID, dlLease1, TaskPaused); err != nil {
		t.Fatalf("ReleaseTask download: %v", err)
	}
	dlResumed, dlFile2, dlLease2, err := s.ResumeDownloadSession(ctx, "alice", protocol.ResumeRequest{
		TransferID:   dlTask.ID,
		Direction:    protocol.DirectionDownload,
		ClientOffset: 4,
		FileSHA256:   req.FileSHA256,
	})
	if err != nil {
		t.Fatalf("ResumeDownloadSession: %v", err)
	}
	_ = dlFile2.Close()
	if dlResumed.Offset != 4 {
		t.Fatalf("expected download resumed offset 4, got %d", dlResumed.Offset)
	}
	if err := s.CheckpointDownloadSession(ctx, dlTask.ID, dlLease2, 8); err != nil {
		t.Fatalf("CheckpointDownloadSession end: %v", err)
	}
	if err := s.FinishDownloadSession(ctx, dlTask.ID, dlLease2); err != nil {
		t.Fatalf("FinishDownloadSession: %v", err)
	}

	files, totalBytes, err := s.PublishedUsage(ctx)
	if err != nil || files != 1 || totalBytes != 8 {
		t.Fatalf("unexpected PublishedUsage: files=%d bytes=%d err=%v", files, totalBytes, err)
	}
}
