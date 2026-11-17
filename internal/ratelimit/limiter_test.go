package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/protocol"
)

func TestSharedUserBucketCancellationAndIsolation(t *testing.T) {
	u := New(8, 100, 64)
	b := config.Bandwidth{UploadBytesPerSecond: 1000, DownloadBytesPerSecond: 1000, BurstBytes: 100}
	if err := u.Wait(context.Background(), "alice", b, protocol.DirectionUpload, 100); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := u.Wait(context.Background(), "alice", b, protocol.DirectionUpload, 100); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if elapsed := time.Since(started); elapsed < 350*time.Millisecond {
		t.Fatalf("user bucket not shared: %v", elapsed)
	}

	for _, dir := range []protocol.Direction{protocol.DirectionUpload, protocol.DirectionDownload} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		if err := u.Wait(ctx, "bob", b, dir, 100); err != nil {
			cancel()
			t.Fatalf("independent user/direction failed: %v", err)
		}
		cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- u.Wait(ctx, "bob", b, protocol.DirectionUpload, 100) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled token wait blocked")
	}
}

func TestGlobalBandwidthCeilingAcrossUsers(t *testing.T) {
	global := config.Bandwidth{UploadBytesPerSecond: 1000, DownloadBytesPerSecond: 1000, BurstBytes: 100}
	u := New(8, 100, 64, &global)
	fastUser := config.Bandwidth{UploadBytesPerSecond: 100000, DownloadBytesPerSecond: 100000, BurstBytes: 1000}

	// 消耗突发桶，随后两个独立用户共享全局 1000 B/s 速率，传输 400B 需至少 ~350ms
	if err := u.Wait(context.Background(), "alice", fastUser, protocol.DirectionUpload, 100); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	var wg sync.WaitGroup
	for _, user := range []string{"alice", "bob"} {
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(usr string) {
				defer wg.Done()
				if err := u.Wait(context.Background(), usr, fastUser, protocol.DirectionUpload, 100); err != nil {
					t.Error(err)
				}
			}(user)
		}
	}
	wg.Wait()
	if elapsed := time.Since(started); elapsed < 350*time.Millisecond {
		t.Fatalf("global bucket did not cap multi-user burst: %v", elapsed)
	}
}

func TestStreamAndRequestLimits(t *testing.T) {
	u := New(1, 2, 64)
	b := config.DefaultServer().Bandwidth
	rel, err := u.Acquire("alice", b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = u.Acquire("alice", b); err == nil {
		t.Fatal("stream cap bypassed")
	}
	rel()
	rel() // 验证幂等

	rel, err = u.Acquire("alice", b)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if _, err = u.Acquire("alice", b); err == nil {
		t.Fatal("initialization rate cap bypassed")
	}
	other, err := u.Acquire("bob", b)
	if err != nil {
		t.Fatal(err)
	}
	other()
}

func TestAcquireListDoesNotConsumeNewTaskQuota(t *testing.T) {
	u := New(1, 1, 64)
	b := config.DefaultServer().Bandwidth

	for i := 0; i < 10; i++ {
		rel, err := u.AcquireList("alice", b)
		if err != nil {
			t.Fatalf("AcquireList #%d failed: %v", i, err)
		}
		// 并发数仍受 maxStreams 限制
		if _, err := u.AcquireList("alice", b); err == nil {
			t.Fatal("AcquireList bypassed maxStreams")
		}
		rel()
	}

	// 即使连续 List 多次，新建传输配额仍完好
	rel, err := u.Acquire("alice", b)
	if err != nil {
		t.Fatalf("Acquire after lists failed: %v", err)
	}
	rel()

	// 新建任务配额耗尽后，AcquireList 仍不受新建任务配额阻断
	if _, err := u.Acquire("alice", b); err == nil {
		t.Fatal("expected second Acquire to hit tasksPerMinute limit")
	}
	listRel, err := u.AcquireList("alice", b)
	if err != nil {
		t.Fatalf("AcquireList blocked by tasksPerMinute exhaustion: %v", err)
	}
	listRel()
}

func TestUploadCheckpointBudgetSurvivesPolicyChangesAndCancellation(t *testing.T) {
	u := New(8, 100, 8)
	b := config.DefaultServer().Bandwidth
	if err := u.WaitUploadCheckpoint(context.Background(), "alice", b); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- u.WaitUploadCheckpoint(ctx, "alice", b) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("checkpoint wait blocked")
	}
	b.UploadBytesPerSecond *= 2
	deadline, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if err := u.WaitUploadCheckpoint(deadline, "alice", b); err == nil {
		t.Fatal("bandwidth change reset checkpoint budget")
	}
	if err := u.WaitUploadCheckpoint(deadline, "bob", b); err != nil {
		t.Fatalf("other user checkpoint consumed: %v", err)
	}
}
