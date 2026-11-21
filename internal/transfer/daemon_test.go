package transfer

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/monitor"
	"sft-tui/internal/protocol"
	"sft-tui/internal/testcert"
)

func daemonFixture(t *testing.T) (config.Server, config.Client, config.Client) {
	t.Helper()
	dir := t.TempDir()
	certsDir := filepath.Join(dir, "certs")
	if err := testcert.Generate(certsDir); err != nil {
		t.Fatalf("testcert.Generate: %v", err)
	}
	if err := testcert.IssueClient(certsDir, "alice"); err != nil {
		t.Fatalf("IssueClient alice: %v", err)
	}
	if err := testcert.IssueClient(certsDir, "bob"); err != nil {
		t.Fatalf("IssueClient bob: %v", err)
	}

	srvCfg := config.DefaultServer()
	srvCfg.Listen = "127.0.0.1:0"
	srvCfg.StorageRoot = filepath.Join(dir, "storage")
	srvCfg.Resources.MaxConnectionsPerUser = 1
	srvCfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "server.pem"),
		PrivateKey:    filepath.Join(certsDir, "server-key.pem"),
	}
	srvCfg.AuthorizedClients = []config.AuthorizedClient{
		{UserID: "alice", Certificate: filepath.Join(certsDir, "alice.pem"), Permissions: []string{"upload", "download"}},
		{UserID: "bob", Certificate: filepath.Join(certsDir, "bob.pem"), Permissions: []string{"download"}},
	}

	makeCli := func(user string) config.Client {
		c := config.DefaultClient()
		c.StateDir = filepath.Join(dir, "cli-"+user)
		c.ServerName = "localhost"
		c.TLS = config.TLSFiles{
			CACertificate: filepath.Join(certsDir, "ca.pem"),
			Certificate:   filepath.Join(certsDir, user+".pem"),
			PrivateKey:    filepath.Join(certsDir, user+"-key.pem"),
		}
		return c
	}
	return srvCfg, makeCli("alice"), makeCli("bob")
}

func TestDaemonACLRatesConnLimitHotReloadAndGracefulStop(t *testing.T) {
	srvCfg, aliceCfg, bobCfg := daemonFixture(t)
	d, err := NewDaemon(srvCfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	defer d.Close()

	ln, err := net.Listen("tcp", srvCfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	aliceCfg.ServerAddress = ln.Addr().String()
	bobCfg.ServerAddress = ln.Addr().String()

	serveDone := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { serveDone <- d.Serve(ctx, ln) }()

	src := filepath.Join(t.TempDir(), "payload.bin")
	payload := make([]byte, 32*1024)
	for i := range payload {
		payload[i] = byte(i*13 + 7)
	}
	if err := os.WriteFile(src, payload, 0600); err != nil {
		t.Fatal(err)
	}

	opCtx, opCancel := context.WithTimeout(ctx, 10*time.Second)
	defer opCancel()

	cliAlice, err := Dial(opCtx, aliceCfg)
	if err != nil {
		t.Fatalf("Dial alice: %v", err)
	}
	defer cliAlice.Close()

	// MaxConnectionsPerUser = 1 应拒绝同用户的第二条并发连接（在 HELLO/HELLO_ACK 或流阶段关闭）
	if second, err := Dial(opCtx, aliceCfg); err == nil {
		_ = second.Close()
		t.Fatal("expected second concurrent connection for alice to be rejected")
	}

	// alice 有 upload 权限，bob 仅有 download 权限 -> bob 上传被 ACL 拒绝
	if _, err := cliAlice.UploadFile(opCtx, src, "alice-file.bin", nil); err != nil {
		t.Fatalf("alice UploadFile: %v", err)
	}

	cliBob, err := Dial(opCtx, bobCfg)
	if err != nil {
		t.Fatalf("Dial bob: %v", err)
	}
	defer cliBob.Close()

	if _, err := cliBob.UploadFile(opCtx, src, "denied.bin", nil); err == nil {
		t.Fatal("expected bob upload without upload ACL to fail")
	}

	// 热更新禁用 bob 并设置全局限速，bob 已建立的连接应被吊销关闭，且无法重连
	updated := srvCfg
	updated.AuthorizedClients = append([]config.AuthorizedClient(nil), srvCfg.AuthorizedClients...)
	updated.AuthorizedClients[1].Disabled = true
	gb := config.Bandwidth{UploadBytesPerSecond: 1024 * 1024, DownloadBytesPerSecond: 1024 * 1024, BurstBytes: 128 * 1024}
	updated.GlobalBandwidth = &gb
	if err := d.ApplySaved(updated); err != nil {
		t.Fatalf("ApplySaved: %v", err)
	}
	if err := cliBob.ListFiles(opCtx, func(_ protocol.FileEntry) error { return nil }); err == nil {
		t.Fatal("expected revoked bob connection to fail")
	}
	if _, err := cliAlice.DownloadFile(opCtx, "alice-file.bin", filepath.Join(t.TempDir(), "downloaded.bin"), nil); err != nil {
		t.Fatalf("alice DownloadFile after hot reload: %v", err)
	}
	if _, err := Dial(opCtx, bobCfg); err == nil {
		t.Fatal("expected revoked bob redial to fail")
	}

	// Snapshot 反映完成任务、失败计数与新热加载配置
	snap := d.Snapshot()
	if snap.Completed < 2 || snap.Failed < 1 || snap.State != "running" || snap.Config.GlobalBandwidth == nil {
		t.Fatalf("unexpected daemon snapshot: %+v", snap)
	}

	// 优雅停服
	d.Stop(false)
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve exit error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon did not shut down gracefully")
	}
}

func TestDaemonListDoesNotConsumeNewTaskQuota(t *testing.T) {
	srvCfg, aliceCfg, _ := daemonFixture(t)
	srvCfg.Resources.MaxNewTasksPerMinute = 1
	d, err := NewDaemon(srvCfg)
	if err != nil {
		t.Fatalf("NewDaemon: %v", err)
	}
	defer d.Close()

	ln, err := net.Listen("tcp", srvCfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	aliceCfg.ServerAddress = ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Serve(ctx, ln) }()

	opCtx, opCancel := context.WithTimeout(ctx, 10*time.Second)
	defer opCancel()
	cli, err := Dial(opCtx, aliceCfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cli.Close()

	for i := 0; i < 5; i++ {
		if err := cli.ListFiles(opCtx, func(protocol.FileEntry) error { return nil }); err != nil {
			t.Fatalf("ListFiles #%d failed: %v", i, err)
		}
	}
	src := filepath.Join(t.TempDir(), "ok.bin")
	if err := os.WriteFile(src, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.UploadFile(opCtx, src, "ok.bin", nil); err != nil {
		t.Fatalf("UploadFile after repeated LIST failed: %v", err)
	}
	if _, err := cli.UploadFile(opCtx, src, "ok2.bin", nil); err == nil {
		t.Fatal("expected second UploadFile to hit MaxNewTasksPerMinute=1 quota")
	}
}

func TestDaemonEmitsStructuredTransferLogsForUserAndFile(t *testing.T) {
	srvCfg, aliceCfg, _ := daemonFixture(t)
	var logs monitor.Logs
	logger := slog.New(logs.Handler(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})))

	d, err := NewDaemonWithLogger(srvCfg, logger)
	if err != nil {
		t.Fatalf("NewDaemonWithLogger: %v", err)
	}
	defer d.Close()

	ln, err := net.Listen("tcp", srvCfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	aliceCfg.ServerAddress = ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = d.Serve(ctx, ln) }()

	opCtx, opCancel := context.WithTimeout(ctx, 10*time.Second)
	defer opCancel()
	cli, err := Dial(opCtx, aliceCfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cli.Close()

	src := filepath.Join(t.TempDir(), "audit-log.txt")
	if err := os.WriteFile(src, []byte("hello audit log"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.UploadFile(opCtx, src, "audit-log.txt", nil); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}

	page := logs.Query(monitor.LogQuery{User: "alice", File: "audit-log.txt"})
	if len(page.Events) < 2 {
		t.Fatalf("expected structured transfer start and completion logs, got %+v", page.Events)
	}
	foundCompleted := false
	for _, ev := range page.Events {
		if ev.User == "alice" && ev.File == "audit-log.txt" && strings.Contains(ev.Detail, "result=completed") {
			foundCompleted = true
		}
	}
	if !foundCompleted {
		t.Fatalf("expected completed transfer log for alice/audit-log.txt, got %+v", page.Events)
	}
}
