package storage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"sft-tui/internal/protocol"
)

func TestReviewReopenReconcilesUnacknowledgedTail(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("ABCD"), 1))
	reviewWrite(t, s, task, 0, []byte("AB"))
	f, err := os.OpenFile(reviewPartPath(root, task.ID), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write([]byte("unacknowledged tail"))
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		t.Fatalf("fixture: %v %v %v", writeErr, syncErr, closeErr)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := reviewOpenStore(t, root)
	// BUG: OpenStore 没有检查并截断超过 SQLite 检查点的临时尾部。
	if got := reviewReadFile(t, reviewPartPath(root, task.ID)); string(got) != "AB" {
		t.Errorf("BUG: reopen kept unacknowledged tail: %q", got)
	}
	if got := reviewTask(t, reopened, task.ID); got.Offset != 2 {
		t.Errorf("reopen offset=%d", got.Offset)
	}
}

func TestReviewReopenDetectsMissingAcknowledgedData(t *testing.T) {
	for _, damage := range []string{"short", "missing"} {
		t.Run(damage, func(t *testing.T) {
			root := t.TempDir()
			s := reviewOpenStore(t, root)
			task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("ABCD"), 1))
			reviewWrite(t, s, task, 0, []byte("AB"))
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			path := reviewPartPath(root, task.ID)
			if damage == "short" {
				if err := os.Truncate(path, 1); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(root)
			if err != nil {
				return
			} // 拒绝启动也是有效的损坏报告策略。
			defer reopened.Close()
			got := reviewTask(t, reopened, task.ID)
			// BUG: 已确认数据缺失后仍报告 ACTIVE，恢复时会继续承诺不存在的数据。
			if !got.Status.IsTerminal() {
				t.Errorf("BUG: %s part remains resumable: offset=%d status=%s", damage, got.Offset, got.Status)
			}
		})
	}
}

func TestReviewFailedTaskCreationRollsBackMetadata(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	staging := filepath.Join(root, "staging")
	if err := os.Remove(staging); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staging, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateUploadTask(context.Background(), "alice", reviewRequest("a", nil, 1))
	if err == nil {
		t.Fatal("part creation fault must fail request")
	}
	// BUG: INSERT tasks 先提交，创建暂存文件失败时没有回滚/清理该任务。
	if count := reviewCount(t, s, "tasks"); count != 0 {
		t.Errorf("BUG: failed create left %d unusable tasks", count)
	}
}

func TestReviewProcessCrashHelper(t *testing.T) {
	if os.Getenv("SFT_STAGE1_CRASH_HELPER") != "1" {
		return
	}
	args := os.Args
	root := args[len(args)-1]
	s, err := OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("ABCD"), 1))
	reviewWrite(t, s, task, 0, []byte("AB"))
	if err := os.WriteFile(filepath.Join(root, "test-task-id"), []byte(IDToHex(task.ID)), 0600); err != nil {
		t.Fatal(err)
	}
	// 故意绕过 Store.Close 与 testing 清理，模拟进程在确认检查点后突然退出。
	// 这覆盖进程崩溃恢复，不模拟设备掉电或硬件写缓存丢失。
	os.Exit(0)
}

func TestReviewCheckpointSurvivesProcessExit(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReviewProcessCrashHelper$", "--", root)
	cmd.Env = append(os.Environ(), "SFT_STAGE1_CRASH_HELPER=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, output)
	}
	id, err := HexToID(string(reviewReadFile(t, filepath.Join(root, "test-task-id"))))
	if err != nil {
		t.Fatal(err)
	}
	s := reviewOpenStore(t, root)
	task := reviewTask(t, s, protocol.TransferID(id))
	if task.Offset != 2 {
		t.Errorf("checkpoint lost after process exit: %d", task.Offset)
	}
	if got := reviewReadFile(t, reviewPartPath(root, task.ID)); string(got) != "AB" {
		t.Errorf("durable bytes=%q", got)
	}
}

func TestReviewLockReleasedAfterCloseAndFailedOpen(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("repeated Close: %v", err)
	}
	again := reviewOpenStore(t, root)
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	// 数据库位置被目录占据，验证初始化失败后不会泄漏进程锁。
	dbPath := filepath.Join(root, "metadata", "metadata.db")
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dbPath, 0700); err != nil {
		t.Fatal(err)
	}
	bad, err := OpenStore(root)
	if bad != nil {
		bad.Close()
	}
	if err == nil {
		t.Fatal("directory database should fail")
	}
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	reviewOpenStore(t, root)
}

func TestReopenSweepsOrphanStagingAndObjects(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)

	// 1. 创建一个未完成上传任务（应保留 staging/.part）
	liveUpload := reviewCreateTask(t, s, "alice", reviewRequest("live-part.bin", []byte("ABCD"), 1))
	reviewWrite(t, s, liveUpload, 0, []byte("AB"))

	// 2. 创建一个已发布文件（应保留 objects/<objectID>）
	pubTask := reviewPublish(t, s, "alice", "published.bin", []byte("PAYLOAD"), 2)

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 3. 注入孤儿文件到 staging/ 与 objects/，模拟进程异常退出或删除中途崩溃的残留
	orphanPart := filepath.Join(root, "staging", "deadbeef000000000000000000000000.part")
	orphanObj := filepath.Join(root, "objects", "000000000000000000000000000000000000000000000000000000000000dead")
	if err := os.WriteFile(orphanPart, []byte("orphan-part"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanObj, []byte("orphan-obj"), 0600); err != nil {
		t.Fatal(err)
	}

	// 4. 重开 Store 触发启动自愈清理
	reopened := reviewOpenStore(t, root)
	_ = reopened

	if _, err := os.Lstat(orphanPart); !os.IsNotExist(err) {
		t.Errorf("expected orphan staging file to be removed, err=%v", err)
	}
	if _, err := os.Lstat(orphanObj); !os.IsNotExist(err) {
		t.Errorf("expected orphan object file to be removed, err=%v", err)
	}
	if got := string(reviewReadFile(t, reviewPartPath(root, liveUpload.ID))); got != "AB" {
		t.Errorf("live staging file corrupted or removed: %q", got)
	}
	liveObjPath := filepath.Join(root, "objects", pubTask.ObjectID)
	if got := string(reviewReadFile(t, liveObjPath)); got != "PAYLOAD" {
		t.Errorf("live published object corrupted or removed: %q", got)
	}
}
