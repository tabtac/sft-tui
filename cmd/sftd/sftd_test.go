package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/control"
)

func TestInitCertAndServeLifecycle(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "configs", "server.local.json")
	certDir := filepath.Join(dir, "certs")

	var out, errOut bytes.Buffer
	code := runCommand([]string{
		"init",
		"-config", cfgPath,
		"-dir", certDir,
		"-name", "alice",
		"-hosts", "localhost,127.0.0.1",
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("sftd init failed (%d): %s %s", code, out.String(), errOut.String())
	}

	// 将配置改用 127.0.0.1:0 临时端口和 TempDir 下的 storage root 以便并发运行测试
	cfg, err := config.LoadServer(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = "127.0.0.1:0"
	cfg.StorageRoot = filepath.Join(dir, "storage")
	if err := config.SaveJSON(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	var srvOut, srvErr bytes.Buffer
	serveExit := make(chan int, 1)
	go func() {
		serveExit <- runCommand([]string{"serve", "-config", cfgPath}, &srvOut, &srvErr)
	}()

	// 等待控制端点就绪
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, err := control.Status(cfgPath, cfg.StorageRoot)
		if err == nil && snap != nil && snap.State == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sftd did not become ready; stderr=%s", srvErr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	// sftd status 命令
	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"status", "-config", cfgPath}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "running") {
		t.Fatalf("sftd status failed (%d): %s %s", code, out.String(), errOut.String())
	}

	// sftd cert user + 自动触发热重载
	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"cert", "user", "-dir", certDir, "-server-config", cfgPath, "-client-config-dir", filepath.Dir(cfgPath), "-server-address", "127.0.0.1:7443", "-name", "bob"}, &out, &errOut); code != 0 {
		t.Fatalf("sftd cert user failed (%d): %s %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "已热加载") {
		t.Fatalf("expected hot reload confirmation, got: %s", out.String())
	}

	// sftd reload & sftd logs 命令
	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"reload", "-config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("sftd reload failed (%d): %s %s", code, out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"logs", "-config", cfgPath}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "热更新") {
		t.Fatalf("sftd logs failed (%d): %s %s", code, out.String(), errOut.String())
	}

	// sftd stop 优雅停止
	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"stop", "-config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("sftd stop failed (%d): %s %s", code, out.String(), errOut.String())
	}
	select {
	case code := <-serveExit:
		if code != 0 {
			t.Fatalf("sftd serve exited non-zero (%d): %s", code, srvErr.String())
		}
	case <-time.After(5 * time.Second):
		_ = os.RemoveAll(cfg.StorageRoot)
		t.Fatal("sftd serve did not exit after stop")
	}
}

func TestSFTDUISubcommandNonTTYAndHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runCommand([]string{"ui", "-h"}, &out, &errOut); code != 0 {
		t.Fatalf("sftd ui -h failed (%d): %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"users", "-h"}, &out, &errOut); code != 0 {
		t.Fatalf("sftd users -h failed (%d): %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := runCommand([]string{"ui"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "交互式终端") {
		t.Fatalf("expected non-TTY error for sftd ui, got code=%d stderr=%q", code, errOut.String())
	}
}
