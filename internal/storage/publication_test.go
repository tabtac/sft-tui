package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"sft-tui/internal/protocol"
)

func TestReviewPublicationRejectsActualSizeMismatch(t *testing.T) {
	for _, content := range [][]byte{nil, []byte("LONG")} {
		name := "longer"
		if len(content) == 0 {
			name = "shorter"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s := reviewOpenStore(t, root)
			req := reviewRequest("a", []byte("X"), 1)
			req.FileSHA256 = sha256.Sum256(content)
			task := reviewCreateTask(t, s, "alice", req)
			reviewWrite(t, s, task, 0, []byte("X"))
			if err := os.WriteFile(reviewPartPath(root, task.ID), content, 0600); err != nil {
				t.Fatal(err)
			}
			// BUG: io.Copy 的字节数未参与校验，哈希匹配即可发布与元数据大小不一致的文件。
			if err := s.CommitUpload(context.Background(), task.ID, 1, req.FileSHA256); err == nil {
				t.Error("BUG: actual file length disagrees with FileSize but publication succeeded")
			}
			if reviewCount(t, s, "published_files") != 0 {
				t.Error("invalid-sized object became visible")
			}
		})
	}
}

func TestReviewPublicationSQLFailuresAreRecoverable(t *testing.T) {
	for _, fault := range []struct{ name, trigger string }{
		{"insert_file", `CREATE TRIGGER fail_publish BEFORE INSERT ON published_files BEGIN SELECT RAISE(FAIL,'publish fault'); END`},
		{"complete_task", `CREATE TRIGGER fail_publish BEFORE UPDATE OF status ON tasks WHEN NEW.status='COMPLETED' BEGIN SELECT RAISE(FAIL,'complete fault'); END`},
	} {
		t.Run(fault.name, func(t *testing.T) {
			root := t.TempDir()
			s := reviewOpenStore(t, root)
			task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("A"), 1))
			reviewWrite(t, s, task, 0, []byte("A"))
			reviewExec(t, s, fault.trigger)
			if err := s.CommitUpload(context.Background(), task.ID, 1, task.FileSHA256); err == nil {
				t.Fatal("injected failure must be returned")
			}
			if reviewCount(t, s, "published_files") != 0 {
				t.Error("failed transaction exposed metadata")
			}
			reviewExec(t, s, "DROP TRIGGER fail_publish")
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := reviewOpenStore(t, root)
			recovered := reviewTask(t, reopened, task.ID)
			// 恢复可补完发布，也可保留可重试的暂存文件；两者都应最终完成同一任务。
			if recovered.Status != TaskCompleted {
				if err := reopened.CommitUpload(context.Background(), task.ID, 1, task.FileSHA256); err != nil {
					t.Fatalf("BUG: failed publication permanently stranded task after reopen: %v", err)
				}
			}
			if reviewCount(t, reopened, "published_files") != 1 || reviewTask(t, reopened, task.ID).Status != TaskCompleted {
				t.Error("publication recovery did not produce exactly one completed file")
			}
		})
	}
}

func TestReviewPublicationDoesNotOverwriteExistingObject(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("new"), 1))
	reviewWrite(t, s, task, 0, []byte("new"))
	objectPath := filepath.Join(root, "objects", task.ObjectID)
	if err := os.WriteFile(objectPath, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	// BUG: Link 的 EEXIST 被当作“不支持硬链接”，随后 Rename 会覆盖已有不可变对象。
	if err := s.CommitUpload(context.Background(), task.ID, 3, task.FileSHA256); err == nil {
		t.Error("BUG: publication overwrote a pre-existing object ID")
	}
	if got := reviewReadFile(t, objectPath); string(got) != "existing" {
		t.Errorf("existing object changed to %q", got)
	}
	if reviewCount(t, s, "published_files") != 0 {
		t.Error("object collision exposed new metadata")
	}
}

func TestReviewConcurrentNameConflictPreservesWinnerAndLoser(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	first := reviewCreateTask(t, s, "alice", reviewRequest("same", []byte("A"), 1))
	second, createErr := s.CreateUploadTask(context.Background(), "alice", reviewRequest("same", []byte("B"), 2))
	if createErr != nil && !errors.Is(createErr, ErrTaskConflict) && !errors.Is(createErr, ErrFileExists) {
		t.Fatalf("unexpected pending-name conflict error: %v", createErr)
	}
	reviewWrite(t, s, first, 0, []byte("A"))
	if createErr == nil {
		reviewWrite(t, s, second, 0, []byte("B"))
	}
	if err := s.CommitUpload(context.Background(), first.ID, 1, first.FileSHA256); err != nil {
		t.Fatal(err)
	}
	// 可在接纳时预留文件名，也可在最终发布时仲裁；两种策略均需保住获胜任务。
	if createErr != nil {
		if reviewCount(t, s, "tasks") != 1 || reviewCount(t, s, "published_files") != 1 {
			t.Error("rejected pending-name conflict left stray metadata")
		}
		return
	}
	if err := s.CommitUpload(context.Background(), second.ID, 1, second.FileSHA256); err == nil {
		t.Fatal("second publication must conflict")
	}
	f, _, err := s.OpenPublishedFile(context.Background(), "alice", "same")
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || string(data) != "A" {
		t.Fatalf("winner corrupted: %q %v %v", data, readErr, closeErr)
	}
	// BUG: 输掉同名竞争的任务被移走 .part 后仍为 ACTIVE，且留下未被引用的 object。
	loser := reviewTask(t, s, second.ID)
	if !loser.Status.IsTerminal() {
		if _, err := os.Stat(reviewPartPath(root, loser.ID)); err != nil {
			t.Errorf("BUG: live loser has no resumable part: %v", err)
		}
	}
}

func TestReviewCommitFailuresDoNotExposeFile(t *testing.T) {
	for _, tc := range []struct {
		name      string
		size      uint64
		badDigest bool
		write     bool
	}{
		{"incomplete", 2, false, false},
		{"declared_size", 3, false, true},
		{"declared_hash", 2, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := reviewOpenStore(t, t.TempDir())
			task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("AB"), 1))
			if tc.write {
				reviewWrite(t, s, task, 0, []byte("AB"))
			}
			digest := task.FileSHA256
			if tc.badDigest {
				digest[0] ^= 1
			}
			if err := s.CommitUpload(context.Background(), task.ID, tc.size, digest); err == nil {
				t.Error("invalid COMMIT accepted")
			}
			files, _, err := s.ListPublishedFiles(context.Background(), "alice", "", 12)
			if err != nil || len(files) != 0 {
				t.Errorf("failed commit visible: %+v err=%v", files, err)
			}
			f, _, err := s.OpenPublishedFile(context.Background(), "alice", "a")
			if f != nil {
				f.Close()
			}
			if !errors.Is(err, ErrFileNotFound) {
				t.Errorf("failed commit opened: %v", err)
			}
		})
	}
}

func TestReviewPublishedObjectMustRemainRegularAndCorrectSize(t *testing.T) {
	for _, damage := range []string{"truncate", "directory", "missing"} {
		t.Run(damage, func(t *testing.T) {
			root := t.TempDir()
			s := reviewOpenStore(t, root)
			task := reviewPublish(t, s, "alice", "a", []byte("AB"), 1)
			path := filepath.Join(root, "objects", task.ObjectID)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "truncate":
				if err := os.WriteFile(path, []byte("A"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			f, _, err := s.OpenPublishedFile(context.Background(), "alice", "a")
			if f != nil {
				f.Close()
			}
			// BUG: 下载打开对象时未验证普通文件类型及记录中的大小。
			if err == nil {
				t.Errorf("BUG: damaged object (%s) opened as a valid published file", damage)
			}
		})
	}
}

func TestReviewPublicationIsUserScoped(t *testing.T) {
	s := reviewOpenStore(t, t.TempDir())
	reviewPublish(t, s, "alice", "same", []byte("A"), 1)
	reviewPublish(t, s, "bob", "same", []byte("B"), 1)
	for _, user := range []string{"alice", "bob", "mallory"} {
		files, _, err := s.ListPublishedFiles(context.Background(), user, "", 12)
		if err != nil {
			t.Fatal(err)
		}
		if user == "mallory" {
			if len(files) != 0 {
				t.Error("other user's metadata leaked")
			}
			f, _, err := s.OpenPublishedFile(context.Background(), user, "same")
			if f != nil {
				f.Close()
			}
			if !errors.Is(err, ErrFileNotFound) {
				t.Errorf("foreign file opened: %v", err)
			}
		} else if len(files) != 1 {
			t.Errorf("user %s list=%+v", user, files)
		}
	}
}

func TestReviewMissingTaskCommit(t *testing.T) {
	s := reviewOpenStore(t, t.TempDir())
	err := s.CommitUpload(context.Background(), protocol.TransferID{99}, 0, protocol.Digest{})
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("missing task commit=%v", err)
	}
}

func TestCommitUploadReleasesStoreLockDuringHashingAndRespectsCancel(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	content := []byte("verify-concurrent-hashing-does-not-block-store")
	task, lease, err := s.BeginUploadSession(context.Background(), "alice", reviewRequest("big.bin", content, 10))
	if err != nil {
		t.Fatalf("BeginUploadSession: %v", err)
	}
	if _, err := s.StageUploadChunk(context.Background(), task.ID, lease, 0, content); err != nil {
		t.Fatalf("StageUploadChunk: %v", err)
	}
	if _, err := s.FlushUploadCheckpoint(context.Background(), task.ID, lease); err != nil {
		t.Fatalf("FlushUploadCheckpoint: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	s.onCommitHash = func() {
		close(entered)
		<-release
	}

	commitDone := make(chan error, 1)
	go func() {
		commitDone <- s.CommitUploadSession(context.Background(), task.ID, lease, task.FileSize, task.FileSHA256)
	}()

	<-entered

	// 在哈希阶段，其他用户的读取与写入不应被全局锁阻塞
	otherTask, otherLease, err := s.BeginUploadSession(context.Background(), "bob", reviewRequest("other.bin", []byte("hi"), 11))
	if err != nil {
		close(release)
		t.Fatalf("concurrent BeginUploadSession blocked or failed: %v", err)
	}
	if _, _, err := s.ListPublishedFiles(context.Background(), "bob", "", 12); err != nil {
		close(release)
		t.Fatalf("concurrent ListPublishedFiles failed: %v", err)
	}
	if _, _, err := s.PublishedUsage(context.Background()); err != nil {
		close(release)
		t.Fatalf("concurrent PublishedUsage failed: %v", err)
	}
	if err := s.ReleaseTask(context.Background(), otherTask.ID, otherLease, TaskCancelled); err != nil {
		close(release)
		t.Fatalf("concurrent ReleaseTask failed: %v", err)
	}

	close(release)
	if err := <-commitDone; err != nil {
		t.Fatalf("CommitUploadSession failed: %v", err)
	}
	if reviewCount(t, s, "published_files") != 1 {
		t.Fatalf("expected 1 published file after commit")
	}

	// 验证 ctx 取消时能中止无锁哈希阶段而不阻塞或误发布
	s.onCommitHash = nil
	cancelTask, cancelLease, err := s.BeginUploadSession(context.Background(), "alice", reviewRequest("cancel.bin", content, 12))
	if err != nil {
		t.Fatalf("BeginUploadSession(cancel): %v", err)
	}
	if _, err := s.StageUploadChunk(context.Background(), cancelTask.ID, cancelLease, 0, content); err != nil {
		t.Fatalf("StageUploadChunk(cancel): %v", err)
	}
	if _, err := s.FlushUploadCheckpoint(context.Background(), cancelTask.ID, cancelLease); err != nil {
		t.Fatalf("FlushUploadCheckpoint(cancel): %v", err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	s.onCommitHash = cancel
	if err := s.CommitUploadSession(cctx, cancelTask.ID, cancelLease, cancelTask.FileSize, cancelTask.FileSHA256); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if reviewCount(t, s, "published_files") != 1 {
		t.Fatalf("cancelled commit must not publish file")
	}
}
