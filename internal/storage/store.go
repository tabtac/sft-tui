package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"sft-tui/internal/pal"
	"sft-tui/internal/protocol"
)

// Store 服务端存储引擎，管理文件系统与 SQLite 状态库
type Store struct {
	root      *pal.Root
	db        *sql.DB
	lockFile  *os.File
	mu        sync.RWMutex
	retention time.Duration
}

// OpenStore 打开并初始化存储引擎，强制执行目录句柄锚定与子目录排他权限收紧。
func OpenStore(rootDir string) (*Store, error) {
	// 1. 打开并锚定根目录，执行符号链接防穿越校验与私有权限应用
	root, err := pal.OpenRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("open root failed: %w", err)
	}

	ok := false
	defer func() {
		if !ok {
			_ = root.Close()
		}
	}()

	// 2. 在根目录句柄下安全初始化子目录，严禁符号链接混入并收紧为 0700 权限
	for _, sub := range []string{"staging", "objects", "metadata"} {
		if err := root.EnsureSecureSubdir(sub); err != nil {
			return nil, fmt.Errorf("failed to secure sub-directory %q: %w", sub, err)
		}
	}

	// 3. 独占进程锁：通过 root 句柄打开 .lock 文件，防止符号链接挟持
	lFile, err := root.OpenFile(".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file failed: %w", err)
	}
	if err := pal.LockFile(lFile); err != nil {
		lFile.Close()
		return nil, ErrStoreLocked
	}

	// 4. 排查 SQLite 文件与其 WAL/SHM 附属文件，防止符号链接逃逸
	for _, fName := range []string{"metadata/metadata.db", "metadata/metadata.db-wal", "metadata/metadata.db-shm"} {
		if info, err := root.Lstat(fName); err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				pal.UnlockFile(lFile)
				lFile.Close()
				return nil, fmt.Errorf("security violation: database auxiliary file %q is a symlink or irregular", fName)
			}
		} else if !os.IsNotExist(err) {
			pal.UnlockFile(lFile)
			lFile.Close()
			return nil, err
		}
	}

	// 5. 初始化 SQLite 数据库（严格验证 WAL + synchronous=FULL）
	dbAbsPath := filepath.Join(root.Path(), "metadata", "metadata.db")
	db, err := initDB(dbAbsPath)
	if err != nil {
		pal.UnlockFile(lFile)
		lFile.Close()
		return nil, err
	}

	ok = true
	return &Store{
		root:      root,
		db:        db,
		lockFile:  lFile,
		retention: 24 * time.Hour,
	}, nil
}

// Close 关闭存储引擎、数据库与根目录句柄
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var errs []error
	if s.db != nil {
		errs = append(errs, s.db.Close())
		s.db = nil
	}
	if s.lockFile != nil {
		_ = pal.UnlockFile(s.lockFile)
		errs = append(errs, s.lockFile.Close())
		s.lockFile = nil
	}
	if s.root != nil {
		errs = append(errs, s.root.Close())
		s.root = nil
	}
	return errors.Join(errs...)
}

func stagingRelPath(taskID protocol.TransferID) string {
	return fmt.Sprintf("staging/%s.part", IDToHex(taskID))
}

func objectRelPath(objectID string) string {
	return fmt.Sprintf("objects/%s", objectID)
}

// CreateUploadTask 创建上传任务并初始化暂存文件
func (s *Store) CreateUploadTask(ctx context.Context, userID string, req protocol.PutRequest) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	var transferID protocol.TransferID
	if _, err := io.ReadFull(rand.Reader, transferID[:]); err != nil {
		return Task{}, err
	}

	var objBytes [16]byte
	if _, err := io.ReadFull(rand.Reader, objBytes[:]); err != nil {
		return Task{}, err
	}
	objectID := hex.EncodeToString(objBytes[:])

	task := Task{
		ID:         transferID,
		RequestID:  req.RequestID,
		UserID:     userID,
		Direction:  protocol.DirectionUpload,
		FileName:   req.FileName,
		FileSize:   req.FileSize,
		FileSHA256: req.FileSHA256,
		ObjectID:   objectID,
		Offset:     0,
		Status:     TaskActive,
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  now + int64(s.retention.Seconds()),
	}

	// 检查 published_files 中是否已存在重名文件
	var existsCount int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM published_files WHERE user_id=? AND file_name=?", userID, req.FileName).Scan(&existsCount)
	if err != nil {
		return Task{}, err
	}
	if existsCount > 0 {
		return Task{}, ErrFileExists
	}

	// 写入数据库
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tasks (id, user_id, request_id, direction, file_name, file_size, file_sha256, object_id, offset, status, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, IDToHex(task.ID), task.UserID, IDToHex(task.RequestID), int(task.Direction),
		task.FileName, task.FileSize, hex.EncodeToString(task.FileSHA256[:]),
		task.ObjectID, task.Offset, string(task.Status),
		task.CreatedAt, task.UpdatedAt, task.ExpiresAt)
	if err != nil {
		return Task{}, err
	}

	// 通过 root 句柄初始化 .part 文件
	partRel := stagingRelPath(task.ID)
	f, err := s.root.OpenFile(partRel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return Task{}, err
	}
	_ = f.Close()

	return task, nil
}

// WriteChunkAt 严格写入数据块，并在落盘后推进 SQLite 检查点
func (s *Store) WriteChunkAt(ctx context.Context, taskID protocol.TransferID, offset uint64, data []byte) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	partRel := stagingRelPath(taskID)
	f, err := s.root.OpenFile(partRel, os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		return 0, err
	}

	if _, err := f.Write(data); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}

	newOffset := offset + uint64(len(data))
	now := time.Now().Unix()

	// 原子推进数据库已提交检查点
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET offset=?, updated_at=? WHERE id=? AND status=?
	`, newOffset, now, IDToHex(taskID), string(TaskActive))
	if err != nil {
		return 0, err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return 0, errors.New("failed to advance checkpoint: task is not active")
	}

	return newOffset, nil
}

// CommitUpload 校验最终 SHA-256 并原子发布文件。
// 修复 Windows 句柄冲突与防 TOCTOU 替换：
// 1. 流式校验 SHA-256。
// 2. 优先通过硬链接 (Link) 原子连接到 objects/，保证校验文件与发布文件物理一致。
// 3. 校验完成后及时关闭读取句柄，再执行移除或备用原子重命名，彻底避免 Windows MoveFileEx 句柄自阻塞。
func (s *Store) CommitUpload(ctx context.Context, taskID protocol.TransferID, fileSize uint64, declaredSHA protocol.Digest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, err := s.getTaskLocked(ctx, taskID)
	if err != nil {
		return err
	}
	if task.Status != TaskActive {
		return fmt.Errorf("task cannot be committed: current status is %s", task.Status)
	}
	if task.Offset != fileSize || fileSize != task.FileSize {
		return ErrOffsetMismatch
	}

	partRel := stagingRelPath(taskID)
	objRel := objectRelPath(task.ObjectID)

	// 打开暂存文件流式计算 SHA-256
	f, err := s.root.OpenFile(partRel, os.O_RDONLY, 0)
	if err != nil {
		return err
	}

	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, f)
	// 计算完毕立即显式关闭句柄，防止后续 Rename 在 Windows 下发生句柄冲突
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}

	actualDigest := hasher.Sum(nil)
	if hex.EncodeToString(actualDigest) != hex.EncodeToString(declaredSHA[:]) ||
		hex.EncodeToString(actualDigest) != hex.EncodeToString(task.FileSHA256[:]) {
		_, _ = s.db.ExecContext(ctx, "UPDATE tasks SET status=?, updated_at=? WHERE id=?", string(TaskFailed), time.Now().Unix(), IDToHex(taskID))
		return ErrHashMismatch
	}

	// 优先尝试硬链接发布
	linkErr := s.root.Link(partRel, objRel)
	if linkErr == nil {
		// 链接成功，移除 staging 入口
		_ = s.root.Remove(partRel)
	} else {
		// 硬链接不支持时回退到根目录内原子重命名
		if renameErr := s.root.Rename(partRel, objRel); renameErr != nil {
			return fmt.Errorf("publish object failed (link: %v, rename: %w)", linkErr, renameErr)
		}
	}

	// 刷新 objects 目录落盘
	_ = s.root.SyncRootSubdir("objects")

	// 开启事务将元数据入库并标记任务完成
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO published_files (object_id, user_id, file_name, file_size, file_sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, task.ObjectID, task.UserID, task.FileName, task.FileSize, hex.EncodeToString(task.FileSHA256[:]), now)
	if err != nil {
		return fmt.Errorf("publish metadata insertion failed: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE tasks SET status=?, updated_at=? WHERE id=?
	`, string(TaskCompleted), now, IDToHex(taskID))
	if err != nil {
		return err
	}

	return tx.Commit()
}

// ListPublishedFiles 分页查询用户已发布的文件列表（按文件名升序排列）
func (s *Store) ListPublishedFiles(ctx context.Context, userID, after string, limit int) ([]protocol.FileEntry, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > protocol.ListPageSize {
		limit = protocol.ListPageSize
	}

	query := `
		SELECT file_name, file_size, file_sha256 
		FROM published_files 
		WHERE user_id = ? AND file_name > ? 
		ORDER BY file_name ASC 
		LIMIT ?
	`
	rows, err := s.db.QueryContext(ctx, query, userID, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var entries []protocol.FileEntry
	for rows.Next() {
		var name string
		var size uint64
		var shaHex string
		if err := rows.Scan(&name, &size, &shaHex); err != nil {
			return nil, false, err
		}
		var d protocol.Digest
		b, _ := hex.DecodeString(shaHex)
		copy(d[:], b)
		entries = append(entries, protocol.FileEntry{
			FileName: name,
			Size:     size,
			SHA256:   d,
		})
	}

	hasMore := false
	if len(entries) > limit {
		hasMore = true
		entries = entries[:limit]
	}

	return entries, hasMore, rows.Err()
}

// OpenPublishedFile 打开已发布的文件以供下载读取
func (s *Store) OpenPublishedFile(ctx context.Context, userID, fileName string) (*os.File, PublishedFile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var pub PublishedFile
	var shaHex string
	err := s.db.QueryRowContext(ctx, `
		SELECT object_id, user_id, file_name, file_size, file_sha256, created_at
		FROM published_files
		WHERE user_id=? AND file_name=?
	`, userID, fileName).Scan(&pub.ObjectID, &pub.UserID, &pub.FileName, &pub.FileSize, &shaHex, &pub.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, pub, ErrFileNotFound
	}
	if err != nil {
		return nil, pub, err
	}

	b, _ := hex.DecodeString(shaHex)
	copy(pub.SHA256[:], b)

	objRel := objectRelPath(pub.ObjectID)
	f, err := s.root.OpenFile(objRel, os.O_RDONLY, 0)
	if err != nil {
		return nil, pub, err
	}

	return f, pub, nil
}

// GetTask 获取任务
func (s *Store) GetTask(ctx context.Context, taskID protocol.TransferID) (Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getTaskLocked(ctx, taskID)
}

func (s *Store) getTaskLocked(ctx context.Context, taskID protocol.TransferID) (Task, error) {
	var t Task
	var idHex, reqHex, shaHex string
	var dir int
	var status string

	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, request_id, direction, file_name, file_size, file_sha256, object_id, offset, status, created_at, updated_at, expires_at
		FROM tasks WHERE id=?
	`, IDToHex(taskID)).Scan(&idHex, &t.UserID, &reqHex, &dir, &t.FileName, &t.FileSize, &shaHex, &t.ObjectID, &t.Offset, &status, &t.CreatedAt, &t.UpdatedAt, &t.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrTaskNotFound
	}
	if err != nil {
		return t, err
	}

	t.ID, _ = HexToID(idHex)
	t.RequestID, _ = HexToID(reqHex)
	t.Direction = protocol.Direction(dir)
	t.Status = TaskStatus(status)
	b, _ := hex.DecodeString(shaHex)
	copy(t.FileSHA256[:], b)

	return t, nil
}
