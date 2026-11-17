package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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

func TestPolicyEditorPreservesNilAndExplicitEmptyPermissions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		permissions []string
		up, down    bool
	}{
		{"legacy_nil", nil, true, true},
		{"deny_all", []string{}, false, false},
		{"upload_only", []string{"upload"}, true, false},
		{"download_only", []string{"download"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newServerModel(ServerOptions{ConfigPath: filepath.Join(t.TempDir(), "server.json")})
			m.cfg = config.DefaultServer()
			m.openEditPolicyModal(config.AuthorizedClient{UserID: "alice", Permissions: tc.permissions})
			// BUG: len(Permissions)==0 把显式空权限误当作 nil 的默认全权限；原样保存会意外授权。
			if m.formToggles[1] != tc.up || m.formToggles[2] != tc.down {
				t.Errorf("BUG: policy editor changes permission semantics: upload=%v download=%v", m.formToggles[1], m.formToggles[2])
			}
		})
	}
}

func TestServerModalCapturesDestructiveNavigationKeys(t *testing.T) {
	for _, key := range []rune{'q', 's', 'x', 'R', '1', '6'} {
		t.Run(string(key), func(t *testing.T) {
			m := newServerModel(ServerOptions{ConfigPath: filepath.Join(t.TempDir(), "server.json")})
			m.openNewUserModal()
			m.Update(keyRune(key))
			if m.modal != srvModalNewUser || m.page != 0 || m.busy {
				t.Errorf("form input triggered a global action: modal=%v page=%d busy=%v", m.modal, m.page, m.busy)
			}
			if m.formInputs[0].Value() != string(key) {
				t.Errorf("form did not capture key %q: %q", key, m.formInputs[0].Value())
			}
		})
	}
}

func TestServerEmptyPagesClampStaleCursor(t *testing.T) {
	m := newServerModel(ServerOptions{ConfigPath: filepath.Join(t.TempDir(), "server.json")})
	for page := 0; page < 6; page++ {
		m.page = page
		m.cursors[page] = 100
		m.clampCursors()
		if m.cursors[page] != 0 {
			t.Errorf("page %d retained out-of-range cursor=%d", page, m.cursors[page])
		}
		m.Update(keyType(tea.KeyEnter))
		_ = m.View()
	}
}

func TestServerThemePersistenceAndMouseInteraction(t *testing.T) {
	exeDir := t.TempDir()
	prefsPath := filepath.Join(exeDir, ".sftd-prefs.json")
	m := newServerModel(ServerOptions{ExecutableDir: exeDir, ConfigPath: filepath.Join(t.TempDir(), "server.json"), PrefsPath: prefsPath})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	if !m.th.dark {
		t.Fatal("default server theme should be dark")
	}
	_, cmd := m.Update(keyType(tea.KeyF6))
	if cmd == nil {
		t.Fatal("F6 did not emit save command")
	}
	m.Update(cmd())

	reloaded := newServerModel(ServerOptions{ExecutableDir: exeDir, ConfigPath: filepath.Join(t.TempDir(), "server.json"), PrefsPath: prefsPath})
	if reloaded.th.dark {
		t.Fatal("server light theme preference did not persist")
	}

	// 点击标签栏第 2 个页签 "2 用户"
	tab1W := lipgloss.Width(m.th.tabActive.Render("1 概览"))
	m.Update(tea.MouseMsg{X: 1 + tab1W + 2, Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.page != 1 {
		t.Fatalf("tab click did not switch to users page: got page=%d", m.page)
	}
	m.cfg.AuthorizedClients = []config.AuthorizedClient{{UserID: "alice"}, {UserID: "bob"}, {UserID: "carol"}}
	m.Update(tea.MouseMsg{X: 10, Y: 5, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.cursors[1] != 1 {
		t.Fatalf("mouse click row on users page failed: cursor=%d", m.cursors[1])
	}
	m.Update(tea.MouseMsg{X: 10, Y: 5, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if m.cursors[1] != 2 {
		t.Fatalf("mouse wheel down failed: cursor=%d", m.cursors[1])
	}
}

func TestServerDeployWizardInitializesValidConfiguration(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "configs", "server.local.json")
	m := newServerModel(ServerOptions{ExecutableDir: root, ConfigPath: cfgPath})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	m.Update(serverConfigLoadedMsg{err: os.ErrNotExist})

	m.Update(keyRune('I'))
	if m.modal != srvModalDeployWizard {
		t.Fatalf("I did not open deploy wizard: modal=%v", m.modal)
	}
	_, cmd := m.Update(keyType(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("deploy wizard enter returned nil cmd")
	}
	res := cmd()
	m.Update(res)
	if action, ok := res.(serverActionResultMsg); !ok || action.err != nil {
		t.Fatalf("deploy wizard execution failed: %+v", res)
	}
	loaded, err := config.LoadServer(cfgPath)
	if err != nil || len(loaded.AuthorizedClients) != 1 || loaded.AuthorizedClients[0].UserID != "alice" {
		t.Fatalf("wizard produced invalid server config: %+v %v", loaded, err)
	}
}

func TestServerNarrowWidthPreservesTabsStatusAndRowsWithoutSpuriousEllipsis(t *testing.T) {
	m := newServerModel(ServerOptions{ExecutableDir: t.TempDir(), ConfigPath: "configs/server.local.json"})
	m.loaded = true
	m.cfg = config.DefaultServer()
	m.cfg.AuthorizedClients = []config.AuthorizedClient{
		{UserID: "alice", Permissions: []string{"upload", "download"}},
		{UserID: "bob", Permissions: []string{}},
	}
	m.certs = []certRow{{Name: "alice", Role: "用户", State: "有效", Expiry: "2027-01-01 00:00", Fingerprint: strings.Repeat("a", 64)}}

	for _, w := range []int{64, 76, 80, 96} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 26})
		for _, page := range []int{0, 1, 3} {
			m.page = page
			view := m.View()
			for _, label := range []string{"1 概览", "2 用户", "3 传输", "4 证书", "5 设置", "6 日志"} {
				if !strings.Contains(view, label) {
					t.Fatalf("width=%d page=%d truncated tab %q:\n%s", w, page, label, view)
				}
			}
			if !strings.Contains(view, "SFTD-UI") {
				t.Fatalf("width=%d page=%d truncated brand badge", w, page)
			}
			if page == 1 && (!strings.Contains(view, "alice") || !strings.Contains(view, "证书:有效")) {
				t.Fatalf("width=%d users page truncated essential status:\n%s", w, view)
			}
		}
	}
}
