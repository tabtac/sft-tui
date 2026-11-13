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
	"math"
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
	// 1. 检查根目录本身是否为符号链接
	if info, err := os.Lstat(rootDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("security violation: storage root %q is a symbolic link", rootDir)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat root failed: %w", err)
	}

	// 2. 打开并锚定根目录，执行符号链接防穿越校验与私有权限应用
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

	// 3. 在根目录句柄下安全初始化子目录，严禁符号链接混入并收紧为 0700 权限
	for _, sub := range []string{"staging", "objects", "metadata"} {
		if err := root.EnsureSecureSubdir(sub); err != nil {
			return nil, fmt.Errorf("failed to secure sub-directory %q: %w", sub, err)
		}
	}

	// 4. 独占进程锁：排查 .lock 是否为符号链接
	if info, err := root.Lstat(".lock"); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("security violation: lock file is a symlink or irregular")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	lFile, err := root.OpenFile(".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file failed: %w", err)
	}
	if err := pal.LockFile(lFile); err != nil {
		lFile.Close()
		return nil, ErrStoreLocked
	}

	// 5. 排查 SQLite 文件与其 WAL/SHM 附属文件，防止符号链接逃逸
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

	// 6. 初始化 SQLite 数据库（严格验证 WAL + synchronous=FULL）
	dbAbsPath := filepath.Join(root.Path(), "metadata", "metadata.db")
	db, err := initDB(dbAbsPath)
	if err != nil {
		pal.UnlockFile(lFile)
		lFile.Close()
		return nil, err
	}

	// 7. 启动自愈与崩溃恢复：对齐 tasks 检查点与 staging/.part 物理文件
	if err := reconcileStaging(root, db); err != nil {
		db.Close()
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

// reconcileStaging 检查并修复掉电或非正常退出留下的未确认尾部与数据缺失
func reconcileStaging(root *pal.Root, db *sql.DB) error {
	rows, err := db.Query("SELECT id, offset, file_size FROM tasks WHERE status NOT IN ('COMPLETED', 'FAILED', 'CANCELLED', 'EXPIRED')")
	if err != nil {
		return err
	}
	defer rows.Close()

	type taskInfo struct {
		idHex    string
		offset   uint64
		fileSize uint64
	}
	var list []taskInfo
	for rows.Next() {
		var item taskInfo
		if err := rows.Scan(&item.idHex, &item.offset, &item.fileSize); err != nil {
			return err
		}
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, item := range list {
		partRel := "staging/" + item.idHex + ".part"
		info, err := root.Lstat(partRel)
		if err != nil {
			if os.IsNotExist(err) {
				if item.offset > 0 {
					if _, err := db.Exec("UPDATE tasks SET status='FAILED', updated_at=? WHERE id=?", time.Now().Unix(), item.idHex); err != nil {
						return err
					}
				}
				continue
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			if _, err := db.Exec("UPDATE tasks SET status='FAILED', updated_at=? WHERE id=?", time.Now().Unix(), item.idHex); err != nil {
				return err
			}
			continue
		}
		size := uint64(info.Size())
		if size > item.offset {
			// 截断未确认的临时尾部
			f, err := root.OpenFile(partRel, os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			truncErr := f.Truncate(int64(item.offset))
			syncErr := f.Sync()
			closeErr := f.Close()
			if truncErr != nil {
				return truncErr
			}
			if syncErr != nil {
				return syncErr
			}
			if closeErr != nil {
				return closeErr
			}
		} else if size < item.offset {
			// 已确认的数据缺失，将任务置为 FAILED
			if _, err := db.Exec("UPDATE tasks SET status='FAILED', updated_at=? WHERE id=?", time.Now().Unix(), item.idHex); err != nil {
				return err
			}
		}
	}
	return nil
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

const taskColumns = "id, user_id, request_id, direction, file_name, file_size, file_sha256, object_id, offset, status, created_at, updated_at, expires_at"

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var idHex, reqHex, shaHex string
	var dir int
	var status string

	err := row.Scan(&idHex, &t.UserID, &reqHex, &dir, &t.FileName, &t.FileSize, &shaHex, &t.ObjectID, &t.Offset, &status, &t.CreatedAt, &t.UpdatedAt, &t.ExpiresAt)
	if err != nil {
		return t, err
	}

	id, err := HexToID(idHex)
	if err != nil {
		return t, fmt.Errorf("corrupt id: %w", err)
	}
	reqID, err := HexToID(reqHex)
	if err != nil {
		return t, fmt.Errorf("corrupt request_id: %w", err)
	}
	if !IsValidHex(t.ObjectID, 32) {
		return t, fmt.Errorf("corrupt object_id: %q", t.ObjectID)
	}
	if dir != int(protocol.DirectionUpload) && dir != int(protocol.DirectionDownload) {
		return t, fmt.Errorf("corrupt direction: %d", dir)
	}
	taskStatus := TaskStatus(status)
	if !taskStatus.IsValid() {
		return t, fmt.Errorf("corrupt status: %q", status)
	}
	if len(shaHex) != 64 || !IsValidHex(shaHex, 64) {
		return t, fmt.Errorf("corrupt sha256: %q", shaHex)
	}
	b, err := hex.DecodeString(shaHex)
	if err != nil || len(b) != 32 {
		return t, fmt.Errorf("corrupt sha256 bytes: %w", err)
	}

	t.ID = id
	t.RequestID = reqID
	t.Direction = protocol.Direction(dir)
	t.Status = taskStatus
	copy(t.FileSHA256[:], b)

	return t, nil
}

// CreateUploadTask 创建上传任务并初始化暂存文件（具备严格的名称校验与幂等性保障）
func (s *Store) CreateUploadTask(ctx context.Context, userID string, req protocol.PutRequest) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	if err := protocol.ValidateName(req.FileName); err != nil {
		return Task{}, fmt.Errorf("invalid logical filename %q: %w", req.FileName, err)
	}
	if req.FileSize > math.MaxInt64 {
		return Task{}, fmt.Errorf("unsupported file size %d", req.FileSize)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return Task{}, err
	}

	// 1. 检查幂等性：查询是否已存在相同 (user_id, request_id) 的任务
	existingRow := s.db.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE user_id=? AND request_id=?", userID, IDToHex(req.RequestID))
	existing, err := scanTask(existingRow)
	if err == nil {
		if existing.Direction != protocol.DirectionUpload ||
			existing.FileName != req.FileName ||
			existing.FileSize != req.FileSize ||
			existing.FileSHA256 != req.FileSHA256 {
			return Task{}, ErrTaskConflict
		}
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Task{}, err
	}

	// 2. 检查 published_files 中是否已存在重名文件
	var existsCount int
	err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM published_files WHERE user_id=? AND file_name=?", userID, req.FileName).Scan(&existsCount)
	if err != nil {
		return Task{}, err
	}
	if existsCount > 0 {
		return Task{}, ErrFileExists
	}

	// 3. 检查是否有同名的未完成活跃任务
	var liveCount int
	err = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE user_id=? AND file_name=? AND direction=1 AND status IN ('ACTIVE', 'PAUSED', 'VERIFYING')", userID, req.FileName).Scan(&liveCount)
	if err != nil {
		return Task{}, err
	}
	if liveCount > 0 {
		return Task{}, ErrTaskConflict
	}

	// 4. 创建新任务
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

	// 通过 root 句柄初始化 .part 文件
	partRel := stagingRelPath(task.ID)
	f, err := s.root.OpenFile(partRel, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return Task{}, err
	}
	_ = f.Close()

	// 写入数据库
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tasks (id, user_id, request_id, direction, file_name, file_size, file_sha256, object_id, offset, status, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, IDToHex(task.ID), task.UserID, IDToHex(task.RequestID), int(task.Direction),
		task.FileName, task.FileSize, hex.EncodeToString(task.FileSHA256[:]),
		task.ObjectID, task.Offset, string(task.Status),
		task.CreatedAt, task.UpdatedAt, task.ExpiresAt)
	if err != nil {
		_ = s.root.Remove(partRel)
		return Task{}, err
	}

	return task, nil
}

// WriteChunkAt 严格写入数据块，并在落盘后推进 SQLite 检查点
func (s *Store) WriteChunkAt(ctx context.Context, taskID protocol.TransferID, offset uint64, data []byte) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return 0, err
	}

	task, err := s.getTaskLocked(ctx, taskID)
	if err != nil {
		return 0, err
	}
	if task.Status != TaskActive {
		return 0, fmt.Errorf("task cannot be written: status is %s", task.Status)
	}
	if len(data) == 0 {
		return 0, errors.New("empty chunk is not permitted")
	}
	if offset != task.Offset {
		return 0, fmt.Errorf("offset mismatch: expected %d, got %d: %w", task.Offset, offset, ErrOffsetMismatch)
	}
	if offset > math.MaxInt64 || uint64(len(data)) > math.MaxInt64 || offset+uint64(len(data)) > task.FileSize {
		return 0, fmt.Errorf("chunk exceeds file size limit: offset=%d len=%d size=%d", offset, len(data), task.FileSize)
	}

	partRel := stagingRelPath(taskID)
	info, err := s.root.Lstat(partRel)
	if err != nil {
		return 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return 0, fmt.Errorf("security violation: staging part file %q is a symlink or irregular", partRel)
	}

	f, err := s.root.OpenFile(partRel, os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		f.Close()
		return 0, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}

	newOffset := offset + uint64(len(data))
	now := time.Now().Unix()

	// 原子推进数据库已提交检查点
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET offset=?, updated_at=? WHERE id=? AND status=? AND offset=?
	`, newOffset, now, IDToHex(taskID), string(TaskActive), offset)
	if err != nil {
		return 0, err
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return 0, errors.New("failed to advance checkpoint: state conflict")
	}

	return newOffset, nil
}

// CommitUpload 校验最终 SHA-256 并原子发布文件。
// 修复 Windows 句柄冲突与防 TOCTOU 替换：
// 1. 流式校验实际字节数与 SHA-256。
// 2. 校验完成后及时关闭读取句柄，再执行移除或备用原子重命名，彻底避免 Windows MoveFileEx 句柄自阻塞。
// 3. 优先通过硬链接 (Link) 原子连接到 objects/，保证校验文件与发布文件物理一致，且失败时保留暂存文件供恢复。
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

	info, err := s.root.Lstat(partRel)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("security violation: staging part file is a symlink or irregular")
	}

	// 检查目标不可变对象是否已存在，防止被意外覆盖
	if _, err := s.root.Lstat(objRel); err == nil {
		return fmt.Errorf("object %s already exists", task.ObjectID)
	}

	// 打开暂存文件流式计算 SHA-256 并严格校验实际读取字节数
	f, err := s.root.OpenFile(partRel, os.O_RDONLY, 0)
	if err != nil {
		return err
	}

	hasher := sha256.New()
	n, copyErr := io.Copy(hasher, f)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if uint64(n) != fileSize || uint64(n) != task.FileSize {
		return ErrOffsetMismatch
	}

	actualDigest := hasher.Sum(nil)
	if hex.EncodeToString(actualDigest) != hex.EncodeToString(declaredSHA[:]) ||
		hex.EncodeToString(actualDigest) != hex.EncodeToString(task.FileSHA256[:]) {
		_, _ = s.db.ExecContext(ctx, "UPDATE tasks SET status=?, updated_at=? WHERE id=?", string(TaskFailed), time.Now().Unix(), IDToHex(taskID))
		return ErrHashMismatch
	}

	// 优先尝试硬链接发布
	linkErr := s.root.Link(partRel, objRel)
	if linkErr != nil {
		if errors.Is(linkErr, os.ErrExist) || os.IsExist(linkErr) {
			return fmt.Errorf("object %s already exists: %w", task.ObjectID, linkErr)
		}
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
		if linkErr == nil {
			_ = s.root.Remove(objRel)
		}
		return err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO published_files (object_id, user_id, file_name, file_size, file_sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, task.ObjectID, task.UserID, task.FileName, task.FileSize, hex.EncodeToString(task.FileSHA256[:]), now)
	if err != nil {
		if linkErr == nil {
			_ = s.root.Remove(objRel)
		}
		return fmt.Errorf("publish metadata insertion failed: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE tasks SET status=?, updated_at=? WHERE id=?
	`, string(TaskCompleted), now, IDToHex(taskID))
	if err != nil {
		if linkErr == nil {
			_ = s.root.Remove(objRel)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		if linkErr == nil {
			_ = s.root.Remove(objRel)
		}
		return err
	}

	// 事务提交成功后，从 staging 移除入口
	if linkErr == nil {
		_ = s.root.Remove(partRel)
	}

	return nil
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
		if len(shaHex) != 64 || !IsValidHex(shaHex, 64) {
			return nil, false, fmt.Errorf("corrupt published file sha256: %q", shaHex)
		}
		b, err := hex.DecodeString(shaHex)
		if err != nil || len(b) != 32 {
			return nil, false, fmt.Errorf("corrupt published file sha256 bytes: %w", err)
		}
		var d protocol.Digest
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

	if len(shaHex) != 64 || !IsValidHex(shaHex, 64) {
		return nil, pub, fmt.Errorf("corrupt published file sha256: %q", shaHex)
	}
	b, err := hex.DecodeString(shaHex)
	if err != nil || len(b) != 32 {
		return nil, pub, fmt.Errorf("corrupt published file sha256 bytes: %w", err)
	}
	copy(pub.SHA256[:], b)

	objRel := objectRelPath(pub.ObjectID)
	info, err := s.root.Lstat(objRel)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, pub, ErrFileNotFound
		}
		return nil, pub, err
	}
	if !info.Mode().IsRegular() || uint64(info.Size()) != pub.FileSize {
		return nil, pub, fmt.Errorf("damaged published object %q: irregular or size mismatch (got %d, want %d)", pub.ObjectID, info.Size(), pub.FileSize)
	}

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
	row := s.db.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id=?", IDToHex(taskID))
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrTaskNotFound
	}
	return t, err
}
