package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/control"
	"sft-tui/internal/monitor"
	"sft-tui/internal/transfer"
)

func TestServerBubbleTeaPagesLiveControlAndUserPolicy(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "configs", "server.local.json")
	certDir := filepath.Join(dir, "certs")

	if _, err := certmanage.Execute("init", certmanage.Options{
		ServerConfig:    cfgPath,
		Dir:             certDir,
		ClientConfigDir: filepath.Dir(cfgPath),
		Name:            "alice",
		Hosts:           "localhost,127.0.0.1",
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

	// 启动真实服务端 Daemon + control IPC 端点，测试 TUI 管理面板全流程
	daemon, err := transfer.NewDaemon(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	logs := &monitor.Logs{}
	ctrl, err := control.StartManaged(cfgPath, cfg.StorageRoot, control.Handlers{
		Reload: func() error {
			updated, err := config.LoadServer(cfgPath)
			if err == nil {
				err = daemon.ApplySaved(updated)
			}
			return err
		},
		Snapshot: daemon.Snapshot,
		Logs:     logs.Query,
		Stop:     daemon.Stop,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()

	// 以 Page=1 (sftd users) 初始化服务端模型
	m := newServerModel(ServerOptions{ConfigPath: cfgPath, Page: 1})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 34})
	m.Update(m.loadConfigCmd()())
	m.Update(m.pollRuntimeCmd()())

	// 校验顶部状态、运行态及用户页呈现 alice
	usersView := m.View()
	if !strings.Contains(usersView, "SFTD-UI") || !strings.Contains(usersView, "运行中") || !strings.Contains(usersView, "alice") {
		t.Fatalf("unexpected sftd users view:\n%s", usersView)
	}

	// 按 t 吊销/切换 alice 并验证热重载到运行态 Snapshot
	_, toggleCmd := m.Update(keyRune('t'))
	if toggleCmd == nil {
		t.Fatal("expected toggle command on pressing t")
	}
	res := toggleCmd()
	m.Update(res)
	m.Update(m.loadConfigCmd()())
	m.Update(m.pollRuntimeCmd()())
	if !m.cfg.AuthorizedClients[0].Disabled || !daemon.Snapshot().Config.AuthorizedClients[0].Disabled {
		t.Fatal("expected user alice disabled both on disk and in live daemon snapshot")
	}

	// 切换至 6 个页面均正常渲染无崩溃（概览/用户/传输/证书/设置/日志）
	for page := 0; page < 6; page++ {
		m.Update(keyRune(rune('1' + page)))
		m.Update(m.pollRuntimeCmd()())
		v := m.View()
		if len(v) == 0 || !strings.Contains(v, "SFTD-UI") {
			t.Fatalf("page %d rendered empty/invalid view", page)
		}
	}

	// 按 ? 打开帮助弹窗，Esc 关闭
	m.Update(keyRune('?'))
	if m.modal != srvModalHelp || !strings.Contains(m.View(), "服务端管理快捷键") {
		t.Fatal("expected help modal overlay")
	}
	m.Update(keyType(tea.KeyEsc))
	if m.modal != srvModalNone {
		t.Fatal("expected Esc to close modal")
	}

	// 验证 '/' 搜索隔离期间输入 'q' 不退出面板
	m.Update(keyRune('2'))
	m.Update(keyRune('/'))
	_, qCmd := m.Update(keyRune('q'))
	_ = qCmd
	if !m.editingQuery || m.userFilter != "q" {
		t.Fatalf("expected filter capture, editing=%v filter=%q", m.editingQuery, m.userFilter)
	}
	m.Update(keyType(tea.KeyEsc))

	// 验证上下文背景清理
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	cancel()
	_ = ctx
}
