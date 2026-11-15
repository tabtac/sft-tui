package monitor

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrShortWrite }

func TestLogsRingBufferQueryAndWriteErrorCapture(t *testing.T) {
	logs := &Logs{}
	var buf bytes.Buffer
	base := slog.New(logs.Handler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	logger := base.WithGroup("tx").With("attempt", 1)
	logger.Info("文件已接收", "user", "alice", "file", "report.pdf")

	// WithGroup 前设置的顶级属性应正确归属 user/file
	topLogger := base.With("user", "bob")
	for i := 0; i < 205; i++ {
		topLogger.Info("批量事件", "file", "data.bin")
	}
	page := logs.Query(LogQuery{After: 1, User: "bob", File: "data", Text: "批量"})
	if !page.Lost || page.Last != 206 || len(page.Events) != 200 {
		t.Fatalf("unexpected ring query result: last=%d lost=%v len=%d", page.Last, page.Lost, len(page.Events))
	}

	failLogger := slog.New(logs.Handler(slog.NewTextHandler(failingWriter{}, nil)))
	failLogger.Error("trigger fail")
	errPage := logs.Query(LogQuery{After: page.Last, Level: "ERROR"})
	if len(errPage.Events) < 2 {
		t.Fatalf("expected disk write failure event captured, got %+v", errPage.Events)
	}
}

func TestRuntimeLifecycleRatesAndDrain(t *testing.T) {
	rt := New()
	rt.Listening("127.0.0.1:7443")
	cid, ok := rt.Connect("alice", "127.0.0.1:50000")
	if !ok {
		t.Fatal("Connect rejected while running")
	}
	tid, ok := rt.Begin(cid, 1, "alice", "upload")
	if !ok {
		t.Fatal("Begin rejected while running")
	}
	rt.Describe(tid, "a.bin", 1024, 0)
	rt.Progress(tid, 512, "传输")
	snap := rt.Snapshot()
	if snap.Connections != 1 || snap.ActiveTasks != 1 || snap.Received != 512 {
		t.Fatalf("unexpected active snapshot: %+v", snap)
	}

	drained := rt.Drain(false)
	select {
	case <-drained:
		t.Fatal("drained closed while task active")
	default:
	}
	if _, ok := rt.Begin(cid, 3, "alice", "download"); ok {
		t.Fatal("expected Begin to reject while draining")
	}

	rt.End(tid, "failed", errors.New("broken"))
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("drained did not close after last task ended")
	}
	rt.Disconnect(cid)
	snap = rt.Snapshot()
	if snap.Failed != 1 || snap.Connections != 0 || len(snap.History) != 1 {
		t.Fatalf("unexpected final snapshot: %+v", snap)
	}
}
