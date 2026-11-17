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

func TestLogQueriesAreIsolatedAndWriteErrorsDeduplicated(t *testing.T) {
	logs := &Logs{}
	fail := slog.New(logs.Handler(slog.NewTextHandler(failingWriter{}, nil)))
	fail.Error("first")
	fail.Error("second")
	page := logs.Query(LogQuery{Text: "文件日志写入失败"})
	if len(page.Events) != 1 {
		t.Fatalf("duplicate disk failure events: %d", len(page.Events))
	}
	var sink bytes.Buffer
	working := slog.New(logs.Handler(slog.NewTextHandler(&sink, nil)))
	working.Info("recovered", "user", "alice", "file", "Report.PDF")
	fail.Error("failed again")
	page = logs.Query(LogQuery{Text: "文件日志写入失败"})
	if len(page.Events) != 2 {
		t.Errorf("new failure after recovery was suppressed: %d", len(page.Events))
	}
	page = logs.Query(LogQuery{User: "alice", File: "report", Text: "RECOVERED"})
	if len(page.Events) != 1 {
		t.Fatalf("case-insensitive text/file query failed: %+v", page)
	}
	page.Events[0].Message = "mutated"
	again := logs.Query(LogQuery{User: "alice"})
	if len(again.Events) != 1 || again.Events[0].Message != "recovered" {
		t.Error("caller mutated stored log ring through query result")
	}
}

func TestRuntimeSnapshotsAreIndependentAndHistoryIsBounded(t *testing.T) {
	runtime := New()
	connection, ok := runtime.Connect("alice", "127.0.0.1:1")
	if !ok {
		t.Fatal("connect failed")
	}
	task, ok := runtime.Begin(connection, 1, "alice", "upload")
	if !ok {
		t.Fatal("begin failed")
	}
	runtime.Describe(task, "file", 10, 0)
	snapshot := runtime.Snapshot()
	snapshot.Peers[0].User = "intruder"
	snapshot.Tasks[0].File = "changed"
	again := runtime.Snapshot()
	if again.Peers[0].User != "alice" || again.Tasks[0].File != "file" {
		t.Error("runtime snapshot aliases mutable state")
	}
	runtime.End(task, "completed", nil)
	for i := 0; i < 300; i++ {
		id, ok := runtime.Begin(connection, uint32(3+i*2), "alice", "upload")
		if !ok {
			t.Fatal("begin failed")
		}
		runtime.End(id, "completed", nil)
	}
	if len(runtime.Snapshot().History) > 100 {
		t.Error("runtime history grew beyond its bound")
	}
}

func TestLogsQueryMinimumLevelIncludesHigherSeverities(t *testing.T) {
	logs := &Logs{}
	var buf bytes.Buffer
	logger := slog.New(logs.Handler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	logger.Debug("dbg-msg")
	logger.Info("info-msg")
	logger.Warn("warn-msg")
	logger.Error("err-msg")

	warnPage := logs.Query(LogQuery{Level: "WARN"})
	if len(warnPage.Events) != 2 {
		t.Fatalf("WARN minimum level returned %d events, want 2 (WARN + ERROR): %+v", len(warnPage.Events), warnPage.Events)
	}
	if warnPage.Events[0].Level != "WARN" || warnPage.Events[1].Level != "ERROR" {
		t.Fatalf("unexpected WARN query levels: %+v", warnPage.Events)
	}

	infoPage := logs.Query(LogQuery{Level: "INFO"})
	if len(infoPage.Events) != 3 {
		t.Fatalf("INFO minimum level returned %d events, want 3: %+v", len(infoPage.Events), infoPage.Events)
	}

	errPage := logs.Query(LogQuery{Level: "ERROR"})
	if len(errPage.Events) != 1 || errPage.Events[0].Level != "ERROR" {
		t.Fatalf("ERROR minimum level returned unexpected events: %+v", errPage.Events)
	}

	allPage := logs.Query(LogQuery{})
	if len(allPage.Events) != 4 {
		t.Fatalf("empty level filter returned %d events, want 4", len(allPage.Events))
	}
}
