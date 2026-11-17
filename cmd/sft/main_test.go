package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/progress"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
	"sft-tui/internal/testcert"
	"sft-tui/internal/transfer"
)

func setupCLIFixture(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	certsDir := filepath.Join(dir, "certs")
	if err := testcert.Generate(certsDir); err != nil {
		t.Fatal(err)
	}
	if err := testcert.IssueClient(certsDir, "alice"); err != nil {
		t.Fatal(err)
	}

	srvCfg := config.DefaultServer()
	srvCfg.Listen = "127.0.0.1:0"
	srvCfg.StorageRoot = filepath.Join(dir, "storage")
	srvCfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "server.pem"),
		PrivateKey:    filepath.Join(certsDir, "server-key.pem"),
	}
	srvCfg.AuthorizedClients = []config.AuthorizedClient{
		{UserID: "alice", Certificate: filepath.Join(certsDir, "alice.pem"), Permissions: []string{"upload", "download"}},
	}

	d, err := transfer.NewDaemon(srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Serve(ctx, ln)
	}()

	cliCfg := config.DefaultClient()
	cliCfg.StateDir = filepath.Join(dir, "cli-state")
	cliCfg.ServerAddress = ln.Addr().String()
	cliCfg.ServerName = "localhost"
	cliCfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "alice.pem"),
		PrivateKey:    filepath.Join(certsDir, "alice-key.pem"),
	}
	cfgPath := filepath.Join(dir, "client.local.json")
	if err := config.SaveJSON(cfgPath, cliCfg); err != nil {
		d.Stop(true)
		cancel()
		<-done
		d.Close()
		t.Fatal(err)
	}

	cleanup := func() {
		d.Stop(true)
		cancel()
		<-done
		_ = d.Close()
	}
	return cfgPath, cleanup
}

func TestHeadlessCLIUploadListDownloadAndStatus(t *testing.T) {
	cfgPath, cleanup := setupCLIFixture(t)
	defer cleanup()

	srcDir := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "dl")
	aPath := filepath.Join(srcDir, "alpha.txt")
	bPath := filepath.Join(srcDir, "beta.bin")
	if err := os.WriteFile(aPath, []byte("alpha-content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bPath, bytes.Repeat([]byte{0x5A}, 64*1024), 0600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := runCommand([]string{"-config", cfgPath, "-progress", "never", "upload", aPath, bPath}, &out, &errOut); code != 0 {
		t.Fatalf("cli upload failed (%d): %s %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "alpha.txt") || !strings.Contains(out.String(), "beta.bin") {
		t.Fatalf("unexpected upload output: %s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"-config", cfgPath, "list"}, &out, &errOut); code != 0 {
		t.Fatalf("cli list failed (%d): %s %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "alpha.txt") || !strings.Contains(out.String(), "beta.bin") {
		t.Fatalf("unexpected list output: %s", out.String())
	}

	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"-config", cfgPath, "-progress", "never", "-out", outDir, "download", "alpha.txt", "beta.bin"}, &out, &errOut); code != 0 {
		t.Fatalf("cli download failed (%d): %s %s", code, out.String(), errOut.String())
	}
	gotB, err := os.ReadFile(filepath.Join(outDir, "beta.bin"))
	if err != nil || !bytes.Equal(gotB, bytes.Repeat([]byte{0x5A}, 64*1024)) {
		t.Fatalf("downloaded beta mismatch: %v len=%d", err, len(gotB))
	}

	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"-config", cfgPath, "status"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "没有待恢复") {
		t.Fatalf("unexpected status output (%d): %s %s", code, out.String(), errOut.String())
	}
}

func TestCLIRejectsInvalidArgumentsBeforeOpeningIdentity(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"}, {"status", "extra"}, {"pause"}, {"abort", "a", "b"}, {"upload"}, {"download"},
		{"-parallel", "0", "upload", "a"}, {"-parallel", "65", "upload", "a"}, {"-progress", "invalid", "upload", "a"},
		{"ui", "extra"}, {"-unknown"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var out, stderr bytes.Buffer
			if code := runCommand(args, &out, &stderr); code != 2 {
				t.Errorf("invalid CLI arguments exit=%d stderr=%q", code, stderr.String())
			}
			if stderr.Len() == 0 {
				t.Error("invalid CLI arguments omitted usage/error")
			}
		})
	}
}

func TestClientHelpAndVersionNeedNoIdentity(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"-version"}, {"ui", "-h"}, {"init", "-h"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var out, stderr bytes.Buffer
			if code := runCommand(args, &out, &stderr); code != 0 {
				t.Errorf("help/version exit=%d stderr=%q", code, stderr.String())
			}
			if out.Len()+stderr.Len() == 0 {
				t.Error("help/version produced no output")
			}
		})
	}
}

func TestInteractiveInitSelectsAmongMultipleBundles(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "configs", "server.local.json")
	if _, err := certmanage.Execute("init", certmanage.Options{
		Dir:             filepath.Join(dir, "certs"),
		ServerConfig:    cfgPath,
		ClientConfigDir: filepath.Join(dir, "bundles"),
		Name:            "alice",
		Hosts:           "localhost,127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := certmanage.Execute("user", certmanage.Options{
		Dir:             filepath.Join(dir, "certs"),
		ServerConfig:    cfgPath,
		ClientConfigDir: filepath.Join(dir, "bundles"),
		Name:            "bob",
	}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := runCommandWithInput([]string{"init", "-dir", filepath.Join(dir, "bundles")}, strings.NewReader("2\n"), &out, &errOut)
	if code != 0 {
		t.Fatalf("interactive init failed (%d): out=%q err=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "发现多个客户端身份") || !strings.Contains(out.String(), "bob") {
		t.Fatalf("unexpected interactive output: %q", out.String())
	}
}

func TestParseJobsValidatesFullRemoteDownloadName(t *testing.T) {
	for _, bad := range []string{"sub/file.txt", "../escape", `a\b`} {
		if _, err := parseJobs("download", []string{bad}); err == nil {
			t.Errorf("parseJobs accepted invalid remote name %q", bad)
		}
		if _, err := parseJobs("transfer", []string{"get:" + bad}); err == nil {
			t.Errorf("parseJobs transfer get: accepted invalid remote name %q", bad)
		}
	}
}

func TestRunTransfersRejectsWantCancelAndExitsOnPaused(t *testing.T) {
	dir := t.TempDir()
	mgr, err := queue.Open(queue.Config{
		StateDir:     dir,
		Identity:     "alice-test",
		MaxFileBytes: 1 << 30,
		MaxStreams:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	// 1. 构造 paused 任务且运行时立刻再次返回 ErrPaused，验证 runTransfers 不会死循环等待
	src := filepath.Join(dir, "sample.bin")
	if err := os.WriteFile(src, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	pausedRunner := func(_ context.Context, _ queue.Task, _ *queue.Operation, _ func(uint64, uint64, queue.State)) error {
		return transfer.ErrPaused
	}
	mgr.SetRunner(pausedRunner)
	tasks, err := mgr.Enqueue([]queue.Spec{{Direction: protocol.DirectionUpload, Remote: "sample.bin", Path: src, Size: 3}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tk := mgr.Task(tasks[0].ID); tk != nil && tk.State == queue.Paused {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 验证 Pending/Paused 的 WantCancel 任务会直接被 runTransfers 拒绝而不是 Resume
	_ = mgr.UpdateTask(tasks[0].ID, func(t *queue.Task) {
		t.State = queue.Paused
		t.WantCancel = true
	})
	var out, errOut bytes.Buffer
	code := runTransfers(mgr, config.DefaultClient(), []cliJob{{upload: true, target: src}}, dir, "never", &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "取消确认") {
		t.Fatalf("expected runTransfers to reject WantCancel task, got code=%d err=%q", code, errOut.String())
	}

	// 验证 Paused 状态会让 waitTrackedTransfers 及时结束并返回非零码而非死等
	_ = mgr.UpdateTask(tasks[0].ID, func(t *queue.Task) {
		t.WantCancel = false
		t.State = queue.Paused
	})
	disp, err := progress.New("never", &errOut, &out)
	if err != nil {
		t.Fatal(err)
	}
	defer disp.Close()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	if code := waitTrackedTransfers(waitCtx, mgr, []trackedJob{{idx: 0, id: tasks[0].ID, dir: "upload", label: "sample.bin"}}, disp); code != 1 {
		t.Fatalf("expected non-zero exit when tracked task is Paused, got %d", code)
	}
}
