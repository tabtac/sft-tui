package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewStorageRejectsDatabaseAuxiliarySymlinks(t *testing.T) {
	for _, name := range []string{"metadata.db", "metadata.db-wal", "metadata.db-shm"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			outside := filepath.Join(t.TempDir(), "keep")
			if err := os.WriteFile(outside, []byte("safe"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "metadata"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "metadata", name)); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			s, err := OpenStore(root)
			if s != nil {
				s.Close()
			}
			if err == nil {
				t.Errorf("database auxiliary symlink accepted: %s", name)
			}
			if got := reviewReadFile(t, outside); string(got) != "safe" {
				t.Errorf("outside file modified: %q", got)
			}
		})
	}
}

func TestReviewStorageRejectsRootSymlink(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(base, "root")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	s, err := OpenStore(link)
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Error("root symlink accepted")
	}
}

func TestReviewStorageRejectsInRootLockSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "victim"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("victim", filepath.Join(root, ".lock")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	s, err := OpenStore(root)
	if s != nil {
		s.Close()
	}
	// BUG: os.Root 限制逃出根目录，但仍会跟随指向根内的符号链接；锁文件需单独拒绝此情况。
	if err == nil {
		t.Error("BUG: in-root lock symlink accepted as exclusive instance identity")
	}
}

func TestReviewStagingSymlinkCannotModifyPublishedObject(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	published := reviewPublish(t, s, "alice", "original", []byte("safe"), 1)
	task := reviewCreateTask(t, s, "alice", reviewRequest("new", []byte("evil"), 2))
	part := reviewPartPath(root, task.ID)
	if err := os.Remove(part); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "objects", published.ObjectID), part); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	// BUG: 根内符号链接能通过 os.Root.OpenFile，把新上传写入既有的不可变对象。
	if _, err := s.WriteChunkAt(context.Background(), task.ID, 0, []byte("evil")); err == nil {
		t.Error("BUG: staging symlink accepted for writing")
	}
	if got := reviewReadFile(t, filepath.Join(root, "objects", published.ObjectID)); string(got) != "safe" {
		t.Errorf("BUG: published object corrupted through staging symlink: %q", got)
	}
}

func TestReviewExternalStagingSymlinkCannotModifyFile(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("evil"), 1))
	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(outside, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	part := reviewPartPath(root, task.ID)
	if err := os.Remove(part); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, part); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := s.WriteChunkAt(context.Background(), task.ID, 0, []byte("evil")); err == nil {
		t.Error("external staging symlink accepted")
	}
	if got := reviewReadFile(t, outside); string(got) != "safe" {
		t.Errorf("outside file corrupted: %q", got)
	}
}
