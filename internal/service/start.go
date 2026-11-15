// Package service 以后台守护进程方式拉起同路径 sftd 可执行文件并等待管理 IPC 就绪。
package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/control"
	"sft-tui/internal/pal"
	"sft-tui/internal/security"
)

type Started struct {
	PID            int
	AlreadyRunning bool
	Log            string
}

func Start(ctx context.Context, executable, configPath string) (Started, error) {
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return Started{}, err
	}
	cfg, err := config.LoadServer(absConfig)
	if err != nil {
		return Started{}, err
	}
	root, err := control.Locate(absConfig, cfg.StorageRoot)
	if err != nil {
		return Started{}, err
	}
	if running, err := control.Probe(absConfig, root); err != nil {
		return Started{}, err
	} else if running {
		snap, err := control.Status(absConfig, root)
		if err != nil {
			return Started{}, err
		}
		if snap == nil || snap.State != "running" {
			return Started{}, fmt.Errorf("原实例正在停服，请等待完全停止后再启动")
		}
		return Started{AlreadyRunning: true}, nil
	}
	if _, _, err = security.ServerTLS(cfg); err != nil {
		return Started{}, err
	}
	logsDir := filepath.Join(cfg.StorageRoot, "logs")
	if err = pal.EnsurePrivateDir(logsDir); err != nil {
		return Started{}, err
	}
	logPath := filepath.Join(logsDir, "sftd.log")
	startupPath := filepath.Join(logsDir, "sftd-startup.log")
	if err := pal.CheckRegularOrAbsent(startupPath); err != nil {
		return Started{}, err
	}
	startupLog, err := os.OpenFile(startupPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return Started{}, err
	}
	defer startupLog.Close()

	input, err := os.Open(os.DevNull)
	if err != nil {
		return Started{}, err
	}
	defer input.Close()

	cmd := exec.Command(executable, "serve", "-config", absConfig, "-log-file", logPath)
	cmd.Stdin = input
	cmd.Stdout = startupLog
	cmd.Stderr = startupLog
	detach(cmd)
	if err = cmd.Start(); err != nil {
		return Started{}, err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ready := false
	defer func() {
		if !ready && cmd.Process != nil {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
	}()

	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()

	for {
		if running, e := control.Probe(absConfig, cfg.StorageRoot); e != nil {
			return Started{}, e
		} else if running {
			ready = true
			return Started{PID: cmd.Process.Pid, Log: logPath}, nil
		}
		select {
		case <-ctx.Done():
			return Started{}, ctx.Err()
		case <-deadline.C:
			return Started{}, fmt.Errorf("服务启动未就绪；请查看日志 %s 和 %s", logPath, startupPath)
		case waitErr := <-done:
			if running, _ := control.Probe(absConfig, cfg.StorageRoot); running {
				ready = true
				return Started{AlreadyRunning: true}, nil
			}
			ready = true
			return Started{}, fmt.Errorf("服务启动异常退出（%v）；请查看日志 %s 和 %s", waitErr, logPath, startupPath)
		case <-tick.C:
		}
	}
}
