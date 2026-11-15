//go:build !windows

package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/control"
	"sft-tui/internal/monitor"
)

func TestServeSIGHUPHotReload(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "configs", "server.local.json")
	certDir := filepath.Join(dir, "certs")

	if _, err := certmanage.Execute("init", certmanage.Options{
		ServerConfig: cfgPath,
		Dir:          certDir,
		Name:         "alice",
		Hosts:        "localhost,127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}
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
	exitCh := make(chan int, 1)
	go func() {
		exitCh <- runCommand([]string{"serve", "-config", cfgPath}, &srvOut, &srvErr)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if snap, err := control.Status(cfgPath, cfg.StorageRoot); err == nil && snap != nil && snap.State == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not become ready: %s", srvErr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 更新配置（吊销 alice、添加全局限速）后向进程发送 SIGHUP
	cfg.AuthorizedClients[0].Disabled = true
	gb := config.Bandwidth{UploadBytesPerSecond: 512 * 1024, DownloadBytesPerSecond: 512 * 1024, BurstBytes: 64 * 1024}
	cfg.GlobalBandwidth = &gb
	if err := config.SaveJSON(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}

	reloadDeadline := time.Now().Add(3 * time.Second)
	for {
		snap, err := control.Status(cfgPath, cfg.StorageRoot)
		if err == nil && snap != nil && snap.Config.AuthorizedClients[0].Disabled && snap.Config.GlobalBandwidth != nil {
			break
		}
		if time.Now().After(reloadDeadline) {
			page, _ := control.QueryLogs(cfgPath, cfg.StorageRoot, monitor.LogQuery{})
			t.Fatalf("SIGHUP did not reload config in time; logs=%+v", page.Events)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 验证 graceful stop
	if _, err := control.Stop(cfgPath, cfg.StorageRoot, false); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-exitCh:
		if code != 0 || !strings.Contains(srvErr.String(), "热更新") {
			t.Fatalf("unexpected serve exit (%d): %s", code, srvErr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit cleanly after SIGHUP test")
	}
}
