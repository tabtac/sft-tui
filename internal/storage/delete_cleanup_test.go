package storage

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sft-tui/internal/protocol"
)

func captureCleanupLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func TestDeletePublishedFileSQLFailurePreservesPublicationAndPausedTask(t *testing.T) {
	for _, trigger := range []string{
		`CREATE TRIGGER fail_delete BEFORE UPDATE OF status ON tasks WHEN NEW.status='CANCELLED' BEGIN SELECT RAISE(FAIL,'cancel fault'); END`,
		`CREATE TRIGGER fail_delete BEFORE DELETE ON published_files BEGIN SELECT RAISE(FAIL,'delete fault'); END`,
	} {
		t.Run(trigger, func(t *testing.T) {
			s := reviewOpenStore(t, t.TempDir())
			reviewPublish(t, s, "alice", "keep.bin", []byte("keep payload"), 1)
			task, file, lease, err := s.BeginDownloadSession(context.Background(), "alice", protocol.GetRequest{
				RequestID: protocol.RequestID{2}, FileName: "keep.bin",
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = file.Close()
			if err := s.ReleaseTask(context.Background(), task.ID, lease, TaskPaused); err != nil {
				t.Fatal(err)
			}
			reviewExec(t, s, trigger)
			if _, err := s.DeletePublishedFile(context.Background(), "alice", "keep.bin"); err == nil {
				t.Fatal("database failure must still be returned")
			}
			if got := reviewTask(t, s, task.ID); got.Status != TaskPaused {
				t.Fatalf("failed deletion changed paused download to %s", got.Status)
			}
			file, _, err = s.OpenPublishedFile(context.Background(), "alice", "keep.bin")
			if err != nil {
				t.Fatalf("failed deletion made publication inaccessible: %v", err)
			}
			_ = file.Close()
			if files, _, err := s.ListPublishedFiles(context.Background(), "alice", "", 10); err != nil || len(files) != 1 {
				t.Fatalf("failed deletion lost publication: files=%v err=%v", files, err)
			}
		})
	}
}

func TestDeletePublishedFileDefersBlockedObjectCleanup(t *testing.T) {
	logs := captureCleanupLogs(t)
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	deleted := reviewPublish(t, s, "alice", "blocked.bin", []byte("deleted payload"), 1)
	keep := reviewPublish(t, s, "bob", "blocked.bin", []byte("other owner"), 2)
	object := filepath.Join(root, "objects", deleted.ObjectID)
	unblock := blockObjectRemoval(t, object)
	pub, err := s.DeletePublishedFile(context.Background(), "alice", "blocked.bin")
	if err != nil || pub.ObjectID != deleted.ObjectID {
		t.Fatalf("committed logical deletion must succeed: pub=%+v err=%v", pub, err)
	}
	if _, _, err := s.OpenPublishedFile(context.Background(), "alice", "blocked.bin"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("logically deleted publication remains accessible: %v", err)
	}
	if got := string(reviewReadFile(t, object)); got != "deleted payload" {
		t.Fatalf("blocked physical object changed: %q", got)
	}
	for _, want := range []string{`"level":"WARN"`, "磁盘回收待重试", `"user":"alice"`, `"file":"blocked.bin"`, deleted.ObjectID} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing cleanup warning field %q: %s", want, logs.String())
		}
	}
	unblock()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := reviewOpenStore(t, root)
	if _, err := os.Stat(object); !os.IsNotExist(err) {
		t.Fatalf("restart did not reclaim deleted object: %v", err)
	}
	if got := string(reviewReadFile(t, filepath.Join(root, "objects", keep.ObjectID))); got != "other owner" {
		t.Fatalf("restart damaged another owner's same-named object: %q", got)
	}
	file, _, err := reopened.OpenPublishedFile(context.Background(), "bob", "blocked.bin")
	if err != nil {
		t.Fatalf("restart lost valid publication: %v", err)
	}
	_ = file.Close()
}
