package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/control"
)

func serviceConfigFixture(t *testing.T) (string, config.Server) {
	t.Helper()
	root := t.TempDir()
	cfg := config.DefaultServer()
	cfg.StorageRoot = filepath.Join(root, "storage")
	path := filepath.Join(root, "configs", "server.json")
	result, err := certmanage.Execute("init", certmanage.Options{Dir: filepath.Join(root, "certs"), ServerConfig: path, ClientConfigDir: filepath.Dir(path), Name: "alice", Hosts: "localhost,127.0.0.1", InitialServer: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	// 证书导出地址需要实际端口；完成初始化后再让测试服务绑定临时端口。
	result.Server.Listen = "127.0.0.1:0"
	if err := config.SaveJSON(path, result.Server); err != nil {
		t.Fatal(err)
	}
	return path, result.Server
}

func TestStartAlreadyCancelledDoesNotCreateLogsOrLaunchProcess(t *testing.T) {
	path, cfg := serviceConfigFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Start(ctx, filepath.Join(t.TempDir(), "missing-sftd"), path)
	// BUG: Start 未在启动前检查 ctx，取消后仍创建日志并尝试执行子进程。
	if !errors.Is(err, context.Canceled) {
		t.Errorf("BUG: already-cancelled startup returned %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StorageRoot, "logs", "sftd-startup.log")); !os.IsNotExist(err) {
		t.Errorf("BUG: cancelled startup created/truncated log: %v", err)
	}
}

func TestBackgroundStartReusesInstanceAndStopsCleanly(t *testing.T) {
	path, cfg := serviceConfigFixture(t)
	binary := filepath.Join(t.TempDir(), "sftd")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, "../../cmd/sftd")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build native sftd: %v\n%s", err, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started, err := Start(ctx, binary, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = control.Stop(path, cfg.StorageRoot, true)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			running, err := control.Probe(path, cfg.StorageRoot)
			if err == nil && !running {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		if process, err := os.FindProcess(started.PID); err == nil {
			_ = process.Kill()
			_ = process.Release()
		}
		t.Error("background daemon did not stop within cleanup deadline")
	})
	if started.PID <= 0 || started.AlreadyRunning || started.Log == "" {
		t.Fatalf("invalid start result: %+v", started)
	}
	again, err := Start(ctx, binary, path)
	if err != nil || !again.AlreadyRunning {
		t.Fatalf("second Start did not reuse running instance: %+v %v", again, err)
	}
	snapshot, err := control.Status(path, cfg.StorageRoot)
	if err != nil || snapshot == nil || snapshot.State != "running" {
		t.Fatalf("background status=%+v err=%v", snapshot, err)
	}
	if ok, err := control.Stop(path, cfg.StorageRoot, false); err != nil || !ok {
		t.Fatalf("graceful background stop: %v %v", ok, err)
	}
}
