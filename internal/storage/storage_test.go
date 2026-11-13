package storage

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sft-tui/internal/protocol"
)

func TestTaskTypesAndHex(t *testing.T) {
	// 终态检查
	if !TaskCompleted.IsTerminal() || !TaskFailed.IsTerminal() || !TaskCancelled.IsTerminal() || !TaskExpired.IsTerminal() {
		t.Error("expected terminal states to return true")
	}
	if TaskActive.IsTerminal() || TaskPaused.IsTerminal() {
		t.Error("expected active/paused states to return false")
	}

	// Hex 转换与错误处理
	id := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	hexStr := IDToHex(id)
	restored, err := HexToID(hexStr)
	if err != nil || restored != id {
		t.Fatalf("IDToHex/HexToID roundtrip failed: %v", err)
	}

	// 非法 Hex 还原
	if _, err := HexToID("bad"); err == nil {
		t.Error("expected error for invalid hex length")
	}
	if _, err := HexToID("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"); err == nil {
		t.Error("expected error for non-hex characters")
	}
}

func TestStorageWorkflow(t *testing.T) {
	ctx := context.Background()
	rootDir := t.TempDir()

	store, err := OpenStore(rootDir)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	// 验证 PRAGMA synchronous 为 FULL (2) 以及 journal_mode 为 WAL
	var syncMode int
	if err := store.db.QueryRow("PRAGMA synchronous;").Scan(&syncMode); err != nil {
		t.Fatalf("failed to query synchronous: %v", err)
	}
	if syncMode != 2 && syncMode != 3 {
		t.Fatalf("expected PRAGMA synchronous=FULL (2), got: %d", syncMode)
	}

	var journalMode string
	if err := store.db.QueryRow("PRAGMA journal_mode;").Scan(&journalMode); err != nil {
		t.Fatalf("failed to query journal_mode: %v", err)
	}
	if strings.ToUpper(journalMode) != "WAL" {
		t.Fatalf("expected PRAGMA journal_mode=WAL, got: %s", journalMode)
	}

	// 1. 模拟上传一个测试文件
	testContent := []byte("hello world! this is a secure file transfer test.")
	contentHash := sha256.Sum256(testContent)

	req := protocol.PutRequest{
		RequestID:  protocol.RequestID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		FileName:   "test.txt",
		FileSize:   uint64(len(testContent)),
		FileSHA256: contentHash,
	}

	task, err := store.CreateUploadTask(ctx, "alice", req)
	if err != nil {
		t.Fatalf("CreateUploadTask failed: %v", err)
	}
	if task.Status != TaskActive || task.Offset != 0 {
		t.Fatalf("unexpected task initial state: %+v", task)
	}

	// 2. 分块写入数据并推进检查点
	part1 := testContent[:20]
	part2 := testContent[20:]

	newOffset, err := store.WriteChunkAt(ctx, task.ID, 0, part1)
	if err != nil {
		t.Fatalf("WriteChunkAt part 1 failed: %v", err)
	}
	if newOffset != 20 {
		t.Fatalf("expected offset 20, got: %d", newOffset)
	}

	newOffset, err = store.WriteChunkAt(ctx, task.ID, 20, part2)
	if err != nil {
		t.Fatalf("WriteChunkAt part 2 failed: %v", err)
	}
	if newOffset != uint64(len(testContent)) {
		t.Fatalf("expected final offset %d, got: %d", len(testContent), newOffset)
	}

	// 验证 SQLite 中检查点是否已推进
	recoveredTask, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if recoveredTask.Offset != uint64(len(testContent)) {
		t.Fatalf("expected recovered offset %d, got: %d", len(testContent), recoveredTask.Offset)
	}

	// 3. 提交并校验 SHA-256
	if err := store.CommitUpload(ctx, task.ID, uint64(len(testContent)), contentHash); err != nil {
		t.Fatalf("CommitUpload failed: %v", err)
	}

	// 提交后任务状态应为 COMPLETED
	completedTask, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask after commit failed: %v", err)
	}
	if completedTask.Status != TaskCompleted {
		t.Fatalf("expected task status COMPLETED, got: %s", completedTask.Status)
	}

	// 再次向已 COMPLETED 的任务写块应被拒绝
	if _, err := store.WriteChunkAt(ctx, task.ID, 0, []byte("extra")); err == nil {
		t.Error("expected WriteChunkAt to fail on completed task")
	}

	// 4. 重名上传冲突拦截
	_, err = store.CreateUploadTask(ctx, "alice", req)
	if err != ErrFileExists {
		t.Fatalf("expected ErrFileExists for duplicate file, got: %v", err)
	}

	// 5. 列表查询与分页
	for _, fn := range []string{"file_a.txt", "file_b.txt"} {
		fReq := protocol.PutRequest{
			RequestID:  protocol.RequestID{8, 8, 8},
			FileName:   fn,
			FileSize:   4,
			FileSHA256: sha256.Sum256([]byte("data")),
		}
		tItem, err := store.CreateUploadTask(ctx, "alice", fReq)
		if err != nil {
			t.Fatalf("CreateUploadTask %s failed: %v", fn, err)
		}
		_, _ = store.WriteChunkAt(ctx, tItem.ID, 0, []byte("data"))
		_ = store.CommitUpload(ctx, tItem.ID, 4, sha256.Sum256([]byte("data")))
	}

	p1, hasMore, err := store.ListPublishedFiles(ctx, "alice", "", 2)
	if err != nil || len(p1) != 2 || !hasMore {
		t.Fatalf("expected 2 files with hasMore=true, got: %v, more=%v, err=%v", len(p1), hasMore, err)
	}
	if p1[0].FileName != "file_a.txt" || p1[1].FileName != "file_b.txt" {
		t.Errorf("p1 sort order unexpected: %v, %v", p1[0].FileName, p1[1].FileName)
	}

	p2, hasMore, err := store.ListPublishedFiles(ctx, "alice", "file_b.txt", 2)
	if err != nil || len(p2) != 1 || hasMore || p2[0].FileName != "test.txt" {
		t.Fatalf("expected 1 file with hasMore=false, got: %v, more=%v, err=%v", len(p2), hasMore, err)
	}

	// 6. 打开已发布文件并验证内容
	f, pub, err := store.OpenPublishedFile(ctx, "alice", "test.txt")
	if err != nil {
		t.Fatalf("OpenPublishedFile failed: %v", err)
	}
	defer f.Close()

	if pub.FileSize != uint64(len(testContent)) {
		t.Errorf("published file size mismatch: %d != %d", pub.FileSize, len(testContent))
	}
	readBytes, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("failed to read published file: %v", err)
	}
	if string(readBytes) != string(testContent) {
		t.Errorf("file content mismatch: expected %q, got %q", string(testContent), string(readBytes))
	}

	// 打开不存在文件
	if _, _, err := store.OpenPublishedFile(ctx, "alice", "non_existent.txt"); err != ErrFileNotFound {
		t.Errorf("expected ErrFileNotFound, got: %v", err)
	}
}

func TestSymlinkEscapeRejected(t *testing.T) {
	outsideDir := t.TempDir()
	rootDir := t.TempDir()

	// 1. 预先把 root/staging 软链接到外部目录
	stagingSymlink := filepath.Join(rootDir, "staging")
	if err := os.Symlink(outsideDir, stagingSymlink); err != nil {
		t.Skip("symlinks not supported on this platform/environment")
	}

	// OpenStore 必须被安全拦截，禁止成功初始化
	_, err := OpenStore(rootDir)
	if err == nil {
		t.Fatal("expected OpenStore to fail when staging is a symlink, but it succeeded!")
	}
	_ = os.Remove(stagingSymlink)

	// 2. 预先把 root/objects 软链接到外部目录
	objectsSymlink := filepath.Join(rootDir, "objects")
	_ = os.Symlink(outsideDir, objectsSymlink)
	_, err = OpenStore(rootDir)
	if err == nil {
		t.Fatal("expected OpenStore to fail when objects is a symlink, but it succeeded!")
	}
	_ = os.Remove(objectsSymlink)

	// 3. 预先把 root/metadata 软链接到外部目录
	metaSymlink := filepath.Join(rootDir, "metadata")
	_ = os.Symlink(outsideDir, metaSymlink)
	_, err = OpenStore(rootDir)
	if err == nil {
		t.Fatal("expected OpenStore to fail when metadata is a symlink, but it succeeded!")
	}
	_ = os.Remove(metaSymlink)

	// 4. 正常初始化后，如果外部注入 metadata.db-wal 符号链接
	store, err := OpenStore(rootDir)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	store.Close()

	evilWal := filepath.Join(rootDir, "metadata", "metadata.db-wal")
	_ = os.Symlink(filepath.Join(outsideDir, "evil.wal"), evilWal)
	_, err = OpenStore(rootDir)
	if err == nil {
		t.Fatal("expected OpenStore to fail when metadata.db-wal is a symlink, but it succeeded!")
	}
}

func TestCommitHashMismatchAndSizeMismatch(t *testing.T) {
	ctx := context.Background()
	rootDir := t.TempDir()

	store, err := OpenStore(rootDir)
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer store.Close()

	content := []byte("correct content")
	declaredHash := sha256.Sum256([]byte("tampered content")) // 声明的哈希不匹配

	req := protocol.PutRequest{
		RequestID:  protocol.RequestID{9, 9, 9},
		FileName:   "bad.txt",
		FileSize:   uint64(len(content)),
		FileSHA256: declaredHash,
	}

	task, err := store.CreateUploadTask(ctx, "bob", req)
	if err != nil {
		t.Fatalf("CreateUploadTask failed: %v", err)
	}

	// 尚未写完时尝试 commit（大小不一致）
	_, _ = store.WriteChunkAt(ctx, task.ID, 0, content[:5])
	err = store.CommitUpload(ctx, task.ID, uint64(len(content)), declaredHash)
	if err != ErrOffsetMismatch {
		t.Fatalf("expected ErrOffsetMismatch, got: %v", err)
	}

	// 补全写入，但哈希不匹配
	_, _ = store.WriteChunkAt(ctx, task.ID, 5, content[5:])
	err = store.CommitUpload(ctx, task.ID, uint64(len(content)), declaredHash)
	if err != ErrHashMismatch {
		t.Fatalf("expected ErrHashMismatch, got: %v", err)
	}

	tFailed, _ := store.GetTask(ctx, task.ID)
	if tFailed.Status != TaskFailed {
		t.Errorf("expected status FAILED, got: %s", tFailed.Status)
	}
}

func TestStoreExclusiveLock(t *testing.T) {
	rootDir := t.TempDir()

	store1, err := OpenStore(rootDir)
	if err != nil {
		t.Fatalf("OpenStore 1 failed: %v", err)
	}
	defer store1.Close()

	// 第二个实例尝试打开同一个 rootDir，必须报错 ErrStoreLocked
	_, err = OpenStore(rootDir)
	if err != ErrStoreLocked {
		t.Fatalf("expected ErrStoreLocked for concurrent store open, got: %v", err)
	}

	// 查询不存在任务
	var nonExistentID protocol.TransferID
	if _, err := store1.GetTask(context.Background(), nonExistentID); err != ErrTaskNotFound {
		t.Errorf("expected ErrTaskNotFound, got: %v", err)
	}
}
