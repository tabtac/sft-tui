package storage

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"time"

	"sft-tui/internal/protocol"
)

// Configure 设置用户配额、文件大小上限与断点保留期。
func (s *Store) Configure(limits Limits) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limits = limits
	if limits.TransferRetention > 0 {
		s.retention = limits.TransferRetention
	}
}

func (s *Store) nextLeaseLocked(idHex string) uint64 {
	s.nextLease++
	if s.nextLease == 0 {
		s.nextLease = 1
	}
	s.leases[idHex] = s.nextLease
	return s.nextLease
}

func (s *Store) checkLeaseLocked(idHex string, lease uint64) error {
	if lease == 0 || s.leases[idHex] != lease {
		return ErrTaskNotActive
	}
	return nil
}

func (s *Store) checkQuotaLocked(ctx context.Context, userID string, uploadBytes uint64, isUpload bool) error {
	if isUpload && s.limits.MaxFileBytes > 0 && uploadBytes > s.limits.MaxFileBytes {
		return ErrQuotaExceeded
	}
	if s.limits.MaxPendingTransfersPerUser > 0 {
		var pending int
		if err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM tasks WHERE user_id=? AND status IN ('ACTIVE', 'PAUSED', 'VERIFYING')
		`, userID).Scan(&pending); err != nil {
			return err
		}
		if pending >= int(s.limits.MaxPendingTransfersPerUser) {
			return ErrQuotaExceeded
		}
	}
	if !isUpload {
		return nil
	}
	if s.limits.MaxFilesPerUser > 0 {
		var totalFiles uint64
		if err := s.db.QueryRowContext(ctx, `
			SELECT (SELECT COUNT(*) FROM published_files WHERE user_id=?) +
			       (SELECT COUNT(*) FROM tasks WHERE user_id=? AND direction=1 AND status IN ('ACTIVE', 'PAUSED', 'VERIFYING'))
		`, userID, userID).Scan(&totalFiles); err != nil {
			return err
		}
		if totalFiles >= uint64(s.limits.MaxFilesPerUser) {
			return ErrQuotaExceeded
		}
	}
	if s.limits.UserStorageBytes > 0 {
		var used, reserved uint64
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(file_size), 0) FROM published_files WHERE user_id=?`, userID).Scan(&used); err != nil {
			return err
		}
		if err := s.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(file_size), 0) FROM tasks WHERE user_id=? AND direction=1 AND status IN ('ACTIVE', 'PAUSED', 'VERIFYING')
		`, userID).Scan(&reserved); err != nil {
			return err
		}
		if used > s.limits.UserStorageBytes || reserved > s.limits.UserStorageBytes-used || uploadBytes > s.limits.UserStorageBytes-used-reserved {
			return ErrQuotaExceeded
		}
	}
	return nil
}

func (s *Store) expireUserLocked(ctx context.Context, userID string, now time.Time) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, direction FROM tasks
		WHERE user_id=? AND status IN ('ACTIVE', 'PAUSED', 'VERIFYING') AND expires_at <= ?
	`, userID, now.Unix())
	if err != nil {
		return err
	}
	defer rows.Close()

	type item struct {
		idHex string
		dir   int
	}
	var list []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.idHex, &it.dir); err != nil {
			return err
		}
		if s.leases[it.idHex] == 0 {
			list = append(list, it)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(list) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	nowUnix := now.Unix()
	for _, it := range list {
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status=?, updated_at=? WHERE id=?", string(TaskExpired), nowUnix, it.idHex); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, it := range list {
		if it.dir == int(protocol.DirectionUpload) {
			id, _ := HexToID(it.idHex)
			_ = s.root.Remove(stagingRelPath(id))
		}
	}
	return nil
}

func (s *Store) claimUploadLocked(ctx context.Context, t Task) (Task, uint64, error) {
	now := time.Now().Unix()
	if t.Status == TaskExpired || (t.ExpiresAt <= now && !t.Status.IsTerminal()) {
		return Task{}, 0, ErrTaskExpired
	}
	if t.Status == TaskCancelled {
		return Task{}, 0, ErrTaskCancelled
	}
	if t.Status == TaskFailed {
		return Task{}, 0, ErrHashMismatch
	}
	if t.Status == TaskCompleted {
		return t, 0, nil
	}

	idHex := IDToHex(t.ID)
	if s.leases[idHex] != 0 {
		return Task{}, 0, ErrTaskBusy
	}

	partRel := stagingRelPath(t.ID)
	info, err := s.root.Lstat(partRel)
	if err != nil {
		return Task{}, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || uint64(info.Size()) < t.Offset {
		return Task{}, 0, ErrTaskConflict
	}
	if uint64(info.Size()) > t.Offset {
		f, err := s.root.OpenFile(partRel, os.O_WRONLY, 0600)
		if err != nil {
			return Task{}, 0, err
		}
		truncErr := f.Truncate(int64(t.Offset))
		syncErr := f.Sync()
		closeErr := f.Close()
		if truncErr != nil {
			return Task{}, 0, truncErr
		}
		if syncErr != nil {
			return Task{}, 0, syncErr
		}
		if closeErr != nil {
			return Task{}, 0, closeErr
		}
	}

	expires := now + int64(s.retention.Seconds())
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status=?, updated_at=?, expires_at=? WHERE id=?", string(TaskActive), now, expires, idHex); err != nil {
		return Task{}, 0, err
	}
	t.Status = TaskActive
	t.UpdatedAt = now
	t.ExpiresAt = expires
	lease := s.nextLeaseLocked(idHex)
	return t, lease, nil
}

// BeginUploadSession 创建或幂等认领上传会话，返回任务信息与独占租约令牌。
func (s *Store) BeginUploadSession(ctx context.Context, userID string, req protocol.PutRequest) (Task, uint64, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, 0, err
	}
	if err := protocol.ValidateName(req.FileName); err != nil {
		return Task{}, 0, err
	}
	if req.FileSize > math.MaxInt64 {
		return Task{}, 0, ErrQuotaExceeded
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.expireUserLocked(ctx, userID, time.Now()); err != nil {
		return Task{}, 0, err
	}
	task, existed, err := s.createUploadLocked(ctx, userID, req, true)
	if err != nil {
		return Task{}, 0, err
	}
	if existed {
		return s.claimUploadLocked(ctx, task)
	}
	lease := s.nextLeaseLocked(IDToHex(task.ID))
	return task, lease, nil
}

// ResumeUploadSession 验证并恢复上传断点，返回任务与独占租约令牌。
func (s *Store) ResumeUploadSession(ctx context.Context, userID string, req protocol.ResumeRequest) (Task, uint64, error) {
	if req.Direction != protocol.DirectionUpload || req.ClientOffset != 0 {
		return Task{}, 0, ErrTaskConflict
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.expireUserLocked(ctx, userID, time.Now()); err != nil {
		return Task{}, 0, err
	}

	row := s.db.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id=? AND user_id=?", IDToHex(req.TransferID), userID)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, 0, ErrTaskNotFound
	}
	if err != nil {
		return Task{}, 0, err
	}
	if t.Direction != protocol.DirectionUpload || t.FileSHA256 != req.FileSHA256 {
		return Task{}, 0, ErrTaskConflict
	}
	return s.claimUploadLocked(ctx, t)
}

// WriteUploadChunk 在持有有效租约的前提下写入分片并推进检查点。
func (s *Store) WriteUploadChunk(ctx context.Context, taskID protocol.TransferID, lease uint64, offset uint64, data []byte) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.checkLeaseLocked(IDToHex(taskID), lease); err != nil {
		return 0, err
	}
	return s.writeChunkLocked(ctx, taskID, offset, data)
}

// CommitUploadSession 校验租约并完成最终 SHA-256 校验与原子发布，发布成功后释放租约。
func (s *Store) CommitUploadSession(ctx context.Context, taskID protocol.TransferID, lease uint64, fileSize uint64, declaredSHA protocol.Digest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idHex := IDToHex(taskID)
	if err := s.checkLeaseLocked(idHex, lease); err != nil {
		return err
	}
	if err := s.commitUploadLocked(ctx, taskID, fileSize, declaredSHA); err != nil {
		return err
	}
	delete(s.leases, idHex)
	return nil
}

func (s *Store) claimDownloadLocked(ctx context.Context, t Task, offset uint64) (Task, *os.File, uint64, error) {
	now := time.Now().Unix()
	if t.Status == TaskExpired || (t.ExpiresAt <= now && !t.Status.IsTerminal()) {
		return Task{}, nil, 0, ErrTaskExpired
	}
	if t.Status == TaskCancelled {
		return Task{}, nil, 0, ErrTaskCancelled
	}
	if t.Status == TaskFailed {
		return Task{}, nil, 0, ErrHashMismatch
	}
	if offset > t.FileSize {
		return Task{}, nil, 0, ErrOffsetMismatch
	}
	if t.Status == TaskCompleted {
		return t, nil, 0, nil
	}

	idHex := IDToHex(t.ID)
	if s.leases[idHex] != 0 {
		return Task{}, nil, 0, ErrTaskBusy
	}

	f, pub, err := s.openPublishedLocked(ctx, t.UserID, t.FileName)
	if err != nil {
		return Task{}, nil, 0, err
	}
	if pub.ObjectID != t.ObjectID || pub.FileSize != t.FileSize || pub.SHA256 != t.FileSHA256 {
		f.Close()
		return Task{}, nil, 0, ErrTaskConflict
	}

	expires := now + int64(s.retention.Seconds())
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET offset=?, status=?, updated_at=?, expires_at=? WHERE id=?", offset, string(TaskActive), now, expires, idHex); err != nil {
		f.Close()
		return Task{}, nil, 0, err
	}
	t.Offset = offset
	t.Status = TaskActive
	t.UpdatedAt = now
	t.ExpiresAt = expires
	lease := s.nextLeaseLocked(idHex)
	return t, f, lease, nil
}

// BeginDownloadSession 创建或幂等认领下载会话。
func (s *Store) BeginDownloadSession(ctx context.Context, userID string, req protocol.GetRequest) (Task, *os.File, uint64, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, nil, 0, err
	}
	if err := protocol.ValidateName(req.FileName); err != nil {
		return Task{}, nil, 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.expireUserLocked(ctx, userID, time.Now()); err != nil {
		return Task{}, nil, 0, err
	}
	task, f, existed, err := s.createDownloadLocked(ctx, userID, req, true)
	if err != nil {
		return Task{}, nil, 0, err
	}
	if existed {
		if f != nil {
			f.Close()
		}
		return s.claimDownloadLocked(ctx, task, task.Offset)
	}
	lease := s.nextLeaseLocked(IDToHex(task.ID))
	return task, f, lease, nil
}

// ResumeDownloadSession 验证并从客户端可靠偏移恢复下载会话。
func (s *Store) ResumeDownloadSession(ctx context.Context, userID string, req protocol.ResumeRequest) (Task, *os.File, uint64, error) {
	if req.Direction != protocol.DirectionDownload {
		return Task{}, nil, 0, ErrTaskConflict
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.expireUserLocked(ctx, userID, time.Now()); err != nil {
		return Task{}, nil, 0, err
	}

	row := s.db.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id=? AND user_id=?", IDToHex(req.TransferID), userID)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, nil, 0, ErrTaskNotFound
	}
	if err != nil {
		return Task{}, nil, 0, err
	}
	if t.Direction != protocol.DirectionDownload || t.FileSHA256 != req.FileSHA256 {
		return Task{}, nil, 0, ErrTaskConflict
	}
	return s.claimDownloadLocked(ctx, t, req.ClientOffset)
}

// CheckpointDownloadSession 在持有有效租约时推进下载检查点。
func (s *Store) CheckpointDownloadSession(ctx context.Context, taskID protocol.TransferID, lease uint64, offset uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idHex := IDToHex(taskID)
	if err := s.checkLeaseLocked(idHex, lease); err != nil {
		return err
	}
	task, err := s.getTaskLocked(ctx, taskID)
	if err != nil {
		return err
	}
	if task.Status != TaskActive {
		return ErrTaskNotActive
	}
	if offset > task.FileSize || offset < task.Offset {
		return ErrOffsetMismatch
	}
	now := time.Now().Unix()
	expires := now + int64(s.retention.Seconds())
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET offset=?, updated_at=?, expires_at=? WHERE id=? AND status=? AND offset<=?
	`, offset, now, expires, idHex, string(TaskActive), offset)
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrTaskNotActive
	}
	return nil
}

// FinishDownloadSession 校验租约并将下载任务标记为 COMPLETED。
func (s *Store) FinishDownloadSession(ctx context.Context, taskID protocol.TransferID, lease uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idHex := IDToHex(taskID)
	if err := s.checkLeaseLocked(idHex, lease); err != nil {
		return err
	}
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status=?, updated_at=? WHERE id=? AND status=? AND offset=file_size
	`, string(TaskCompleted), now, idHex, string(TaskActive))
	if err != nil {
		return err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrTaskNotActive
	}
	delete(s.leases, idHex)
	return nil
}

// ReleaseTask 释放租约并按给定目标状态更新未终结任务（如 PAUSED / CANCELLED / FAILED）。
func (s *Store) ReleaseTask(ctx context.Context, taskID protocol.TransferID, lease uint64, finalStatus TaskStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idHex := IDToHex(taskID)
	if lease == 0 || s.leases[idHex] != lease {
		return nil
	}
	delete(s.leases, idHex)

	task, err := s.getTaskLocked(ctx, taskID)
	if err != nil {
		if errors.Is(err, ErrTaskNotFound) {
			return nil
		}
		return err
	}
	if task.Status.IsTerminal() || finalStatus == "" || !finalStatus.IsValid() {
		return nil
	}

	now := time.Now().Unix()
	expires := now + int64(s.retention.Seconds())
	if _, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status=?, updated_at=?, expires_at=? WHERE id=? AND status NOT IN ('COMPLETED','FAILED','CANCELLED','EXPIRED')
	`, string(finalStatus), now, expires, idHex); err != nil {
		return err
	}

	if task.Direction == protocol.DirectionUpload && (finalStatus == TaskCancelled || finalStatus == TaskFailed) {
		_ = s.root.Remove(stagingRelPath(taskID))
	}
	return nil
}

// PublishedUsage 统计所有用户已发布对象数量与总字节数。
func (s *Store) PublishedUsage(ctx context.Context) (files uint64, totalBytes uint64, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	err = s.db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(file_size), 0) FROM published_files").Scan(&files, &totalBytes)
	return files, totalBytes, err
}

// AsRemote 将存储层错误映射为规范定义的 SFT/1 RemoteError。
func AsRemote(err error) *protocol.RemoteError {
	if err == nil {
		return nil
	}
	var remote *protocol.RemoteError
	if errors.As(err, &remote) {
		return remote
	}
	switch {
	case errors.Is(err, ErrFileNotFound), errors.Is(err, ErrTaskNotFound):
		return &protocol.RemoteError{Code: protocol.CodeNotFound, Message: "文件或任务不存在"}
	case errors.Is(err, ErrFileExists):
		return &protocol.RemoteError{Code: protocol.CodeAlreadyExists, Message: "同名文件已存在"}
	case errors.Is(err, ErrQuotaExceeded):
		return &protocol.RemoteError{Code: protocol.CodeQuotaExceeded, Message: "超出用户文件或存储限额"}
	case errors.Is(err, ErrTaskBusy):
		return &protocol.RemoteError{Code: protocol.CodeBusy, Message: "任务正被另一条传输流占用"}
	case errors.Is(err, ErrOffsetMismatch):
		return &protocol.RemoteError{Code: protocol.CodeOffsetMismatch, Message: "偏移量不匹配"}
	case errors.Is(err, ErrHashMismatch):
		return &protocol.RemoteError{Code: protocol.CodeHashMismatch, Message: "文件 SHA-256 校验失败"}
	case errors.Is(err, ErrTaskExpired):
		return &protocol.RemoteError{Code: protocol.CodeTransferExpired, Message: "传输任务已过期"}
	case errors.Is(err, ErrTaskCancelled):
		return &protocol.RemoteError{Code: protocol.CodeTransferCancelled, Message: "传输任务已取消"}
	case errors.Is(err, ErrTaskConflict):
		return &protocol.RemoteError{Code: protocol.CodeMetadataMismatch, Message: "任务参数或元数据冲突"}
	case errors.Is(err, ErrTaskNotActive), errors.Is(err, ErrTaskNotPaused), errors.Is(err, ErrTaskTerminal):
		return &protocol.RemoteError{Code: protocol.CodeBadState, Message: "任务当前状态不允许此操作"}
	default:
		return &protocol.RemoteError{Code: protocol.CodeInternalError, Message: "服务端无法完成文件操作"}
	}
}
