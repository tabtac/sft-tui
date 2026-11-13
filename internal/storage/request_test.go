package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"sft-tui/internal/protocol"
)

func TestReviewRequestIdempotency(t *testing.T) {
	for _, completed := range []bool{false, true} {
		name := "active"
		if completed {
			name = "completed"
		}
		t.Run(name, func(t *testing.T) {
			s := reviewOpenStore(t, t.TempDir())
			req := reviewRequest("a", []byte("A"), 1)
			first := reviewCreateTask(t, s, "alice", req)
			reviewWrite(t, s, first, 0, []byte("A"))
			if completed {
				if err := s.CommitUpload(context.Background(), first.ID, 1, req.FileSHA256); err != nil {
					t.Fatal(err)
				}
			}
			// BUG: 同一 (UserID, RequestID) 重试必须返回同一任务和检查点；
			// 当前 ACTIVE 重试产生新任务，COMPLETED 重试被当作重名冲突。
			again, err := s.CreateUploadTask(context.Background(), "alice", req)
			if err != nil {
				t.Fatalf("BUG: identical request retry failed: %v", err)
			}
			if again.ID != first.ID || again.Offset != 1 || again.Status != reviewTask(t, s, first.ID).Status {
				t.Errorf("BUG: retry lost task identity/checkpoint: ID %s -> %s, offset=%d status=%s", IDToHex(first.ID), IDToHex(again.ID), again.Offset, again.Status)
			}
			if count := reviewCount(t, s, "tasks"); count != 1 {
				t.Errorf("retry created %d tasks", count)
			}
		})
	}
}

func TestReviewRequestIDMetadataConflict(t *testing.T) {
	changes := []struct {
		name   string
		change func(*protocol.PutRequest)
	}{
		{"name", func(r *protocol.PutRequest) { r.FileName = "other" }},
		{"size", func(r *protocol.PutRequest) { r.FileSize++ }},
		{"digest", func(r *protocol.PutRequest) { r.FileSHA256[0] ^= 1 }},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			s := reviewOpenStore(t, t.TempDir())
			req := reviewRequest("a", []byte("A"), 1)
			reviewCreateTask(t, s, "alice", req)
			tc.change(&req)
			if _, err := s.CreateUploadTask(context.Background(), "alice", req); err == nil {
				t.Error("BUG: reused RequestID with different metadata was accepted")
			}
			if count := reviewCount(t, s, "tasks"); count != 1 {
				t.Errorf("conflicting request left %d tasks", count)
			}
		})
	}
}

func TestReviewConcurrentRequestRetriesCreateOneTask(t *testing.T) {
	s := reviewOpenStore(t, t.TempDir())
	req := reviewRequest("a", []byte("A"), 1)
	const workers = 8
	results := make(chan Task, workers)
	errorsCh := make(chan error, workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			task, err := s.CreateUploadTask(context.Background(), "alice", req)
			results <- task
			errorsCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Errorf("retry: %v", err)
		}
	}
	ids := make(map[protocol.TransferID]bool)
	for task := range results {
		ids[task.ID] = true
	}
	if len(ids) != 1 || reviewCount(t, s, "tasks") != 1 {
		t.Errorf("BUG: concurrent retries created %d distinct tasks", len(ids))
	}
}

func TestReviewRequestNamespacesArePerUser(t *testing.T) {
	s := reviewOpenStore(t, t.TempDir())
	req := reviewRequest("same.txt", []byte("A"), 1)
	alice := reviewCreateTask(t, s, "alice", req)
	bob := reviewCreateTask(t, s, "bob", req)
	if alice.ID == bob.ID || alice.ObjectID == bob.ObjectID {
		t.Error("different users share task/object identity")
	}
}

func TestReviewCreateRejectsInvalidNames(t *testing.T) {
	names := []string{"", ".", "..", "../a", "a/b", `a\b`, "tail.", "tail ", "a\x00b", "a\nb", "e\u0301.txt", string([]byte{0xff}), strings.Repeat("x", 256)}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			s := reviewOpenStore(t, t.TempDir())
			// BUG: 创建 API 未调用协议名校验，非法记录随后会使 LIST 编码失败。
			if _, err := s.CreateUploadTask(context.Background(), "alice", reviewRequest(name, nil, 1)); err == nil {
				t.Errorf("BUG: invalid logical filename %q accepted", name)
			}
			if count := reviewCount(t, s, "tasks"); count != 0 {
				t.Errorf("invalid request persisted %d tasks", count)
			}
		})
	}
}

func TestReviewCreateRejectsUnsupportedSizes(t *testing.T) {
	for _, size := range []uint64{uint64(math.MaxInt64) + 1, math.MaxUint64} {
		s := reviewOpenStore(t, t.TempDir())
		req := reviewRequest("large", nil, 1)
		req.FileSize = size
		if _, err := s.CreateUploadTask(context.Background(), "alice", req); err == nil {
			t.Errorf("unsupported size %d accepted", size)
		}
		if count := reviewCount(t, s, "tasks"); count != 0 {
			t.Errorf("unsupported size left %d tasks", count)
		}
	}
}

func TestReviewCreateCancelledContextHasNoSideEffects(t *testing.T) {
	s := reviewOpenStore(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.CreateUploadTask(ctx, "alice", reviewRequest("a", nil, 1)); !errors.Is(err, context.Canceled) {
		t.Errorf("expected context cancellation, got %v", err)
	}
	if reviewCount(t, s, "tasks") != 0 {
		t.Error("cancelled create persisted a task")
	}
}

func TestReviewConcurrentSameOffsetCannotOverwriteAcknowledgedData(t *testing.T) {
	root := t.TempDir()
	s := reviewOpenStore(t, root)
	task := reviewCreateTask(t, s, "alice", reviewRequest("a", []byte("A"), 1))
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, value := range []byte{'A', 'B'} {
		go func(value byte) {
			<-start
			_, err := s.WriteChunkAt(context.Background(), task.ID, 0, []byte{value})
			results <- err
		}(value)
	}
	close(start)
	successes := 0
	for i := 0; i < 2; i++ {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Errorf("BUG: %d conflicting writes succeeded at the same offset", successes)
	}
}

func TestReviewDistinctRequestSamePublishedNameConflicts(t *testing.T) {
	s := reviewOpenStore(t, t.TempDir())
	reviewPublish(t, s, "alice", "a", []byte("A"), 1)
	_, err := s.CreateUploadTask(context.Background(), "alice", protocol.PutRequest{
		RequestID: protocol.RequestID{2}, FileName: "a", FileSize: 1, FileSHA256: sha256.Sum256([]byte("B")),
	})
	if !errors.Is(err, ErrFileExists) {
		t.Errorf("expected name conflict, got %v", err)
	}
}
