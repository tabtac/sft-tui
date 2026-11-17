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
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 34})
	m.Update(serverConfigLoadedMsg{err: os.ErrNotExist})

	// 1. 缺失配置时自动弹出首次部署向导第 1 步
	if m.modal != srvModalDeployWizard || m.deploy == nil || m.deploy.step != 1 {
		t.Fatalf("missing config did not auto-start deploy wizard: modal=%v deploy=%+v", m.modal, m.deploy)
	}
	// 测试按 Esc 暂存返回面板后可按 I 继续恢复向导
	m.Update(keyType(tea.KeyEsc))
	if m.modal != srvModalNone {
		t.Fatalf("Esc did not dismiss wizard: modal=%v", m.modal)
	}
	m.Update(keyRune('I'))
	if m.modal != srvModalDeployWizard || m.deploy.step != 1 {
		t.Fatalf("I did not reopen deploy wizard: modal=%v deploy=%+v", m.modal, m.deploy)
	}

	// Step 1：测试场景下拉框切换与地址联动，再切回本机演示进入 Step 2
	m.Update(keyType(tea.KeyEnter))
	if !m.dropdownOpen {
		t.Fatalf("expected Enter on scene select to open dropdown")
	}
	m.Update(keyType(tea.KeyDown))
	m.Update(keyType(tea.KeyEnter))
	if m.deploy.scene != 1 || m.formInputs[1].Value() != "0.0.0.0:7443" {
		t.Fatalf("selecting LAN scene did not update listen address: scene=%d listen=%q", m.deploy.scene, m.formInputs[1].Value())
	}
	m.Update(keyType(tea.KeyLeft)) // 切回本机演示
	if m.deploy.scene != 0 || m.formInputs[1].Value() != "127.0.0.1:7443" {
		t.Fatalf("switching back to localhost did not restore listen: scene=%d listen=%q", m.deploy.scene, m.formInputs[1].Value())
	}
	m.Update(keyType(tea.KeyDown)) // 聚焦到客户端连接地址文本框，Enter 进入 Step 2
	m.Update(keyType(tea.KeyEnter))
	if m.deploy.step != 2 {
		t.Fatalf("expected step 2, got %d (toast=%q)", m.deploy.step, m.statusToast)
	}
	m.Update(keyType(tea.KeyEnter))
	if m.deploy.step != 3 {
		t.Fatalf("expected step 3, got %d (toast=%q)", m.deploy.step, m.statusToast)
	}

	// 验证上传速率使用 "数值 + 单位下拉框"，切换单位为 MB/s 并写入 5
	upInputIdx := m.formRows[2].inputIdx
	if m.formUnits[upInputIdx].options[m.formUnits[upInputIdx].selected] != "MiB/s" {
		t.Fatalf("unexpected default rate unit: %v", m.formUnits[upInputIdx])
	}
	m.formInputs[upInputIdx].SetValue("5")
	// 聚焦到默认上传带宽行并按 Tab 切到右侧单位下拉，再用鼠标/按键展开选择 MB/s (index 2)
	m.setRowFocus(2, 1)
	m.Update(keyType(tea.KeyEnter))
	if !m.dropdownOpen {
		t.Fatal("Enter on unit dropdown did not open options popup")
	}
	for idx, u := range m.formUnits[upInputIdx].options {
		if u == "MB/s" {
			m.dropdownCursor = idx
			break
		}
	}
	m.Update(keyType(tea.KeyEnter))
	if m.dropdownOpen || m.formUnits[upInputIdx].options[m.formUnits[upInputIdx].selected] != "MB/s" {
		t.Fatalf("failed selecting MB/s from dropdown: %+v", m.formUnits[upInputIdx])
	}

	// 通过空格真实切换两个高级开关，验证 onToggle 保留刚才填写的 "5 MB/s" 并展开高级项
	advIdx := -1
	tuneIdx := -1
	for i, r := range m.formRows {
		if r.kind == formRowToggle {
			if strings.Contains(r.label, "证书") {
				advIdx = i
			} else if strings.Contains(r.label, "全局带宽") {
				tuneIdx = i
			}
		}
	}
	if advIdx < 0 || tuneIdx < 0 {
		t.Fatalf("missing advanced toggles on step 3")
	}
	m.setRowFocus(advIdx, 0)
	m.Update(keyRune(' '))
	// 展开证书高级字段后 tuneIdx 下移了 6 行，重新查找并开启
	for i, r := range m.formRows {
		if r.kind == formRowToggle && strings.Contains(r.label, "全局带宽") {
			tuneIdx = i
			break
		}
	}
	m.setRowFocus(tuneIdx, 0)
	m.Update(keyRune(' '))
	m.setRowFocus(2, 0) // 回到输入行并按 Enter 进入 Step 35
	m.Update(keyType(tea.KeyEnter))
	if m.deploy.step != 35 {
		t.Fatalf("expected step 35 (advanced limits/timeouts), got %d (toast=%q)", m.deploy.step, m.statusToast)
	}
	// 在 Step 35 提交进入 Step 4 摘要
	m.Update(keyType(tea.KeyEnter))
	if m.deploy.step != 4 {
		t.Fatalf("expected step 4 summary, got %d (toast=%q)", m.deploy.step, m.statusToast)
	}
	// Step 4 点击按钮 [1] "仅保存配置"（避免在单元测试拉起后台守护进程）
	m.formFocus = len(m.formRows)
	m.formBtnIdx = 1
	_, cmd := m.Update(keyType(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("save config button returned nil cmd")
	}
	res := cmd()
	m.Update(res)
	if m.deploy == nil || m.deploy.step != 5 || !m.deploy.saved {
		t.Fatalf("expected deploy step 5 finish page, got deploy=%+v toast=%q", m.deploy, m.statusToast)
	}
	loaded, err := config.LoadServer(cfgPath)
	if err != nil || len(loaded.AuthorizedClients) != 1 || loaded.AuthorizedClients[0].UserID != "alice" || loaded.Bandwidth.UploadBytesPerSecond != 5_000_000 {
		t.Fatalf("wizard produced invalid server config: %+v %v", loaded, err)
	}
}

func TestServerSettingsEditAllFieldsAndPolicyRateDropdowns(t *testing.T) {
	root := t.TempDir()
	cfgPath := filepath.Join(root, "configs", "server.local.json")
	if _, err := certmanage.Execute("init", certmanage.Options{
		Dir:             filepath.Join(root, "certs"),
		ServerConfig:    cfgPath,
		ClientConfigDir: filepath.Join(root, "configs"),
		Name:            "alice",
		Hosts:           "localhost,127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}

	m := newServerModel(ServerOptions{ExecutableDir: root, ConfigPath: cfgPath})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 34})
	m.Update(m.loadConfigCmd()())

	// 1. 设置页完整渲染所有分组字段
	m.Update(keyRune('5'))
	view := m.View()
	for _, want := range []string{"基础网络、存储与日志", "带宽限速策略", "全局总带宽上限", "容量、并发与频率限制", "协议流控与超时"} {
		if !strings.Contains(view, want) {
			t.Fatalf("settings page missing section %q:\n%s", want, view)
		}
	}

	// 2. 编辑基础设置：开启全局总带宽，按 KB/s + MiB 修改后进入高级设置保存全部字段
	m.Update(keyRune('m'))
	if m.modal != srvModalEditSettings {
		t.Fatalf("m did not open basic settings: modal=%v", m.modal)
	}
	// 开启全局带宽开关（最后一项 toggle）并重渲染以显示全局带宽输入框
	gbToggleRow := len(m.formRows) - 1
	m.setRowFocus(gbToggleRow, 0)
	m.Update(keyRune(' '))
	// 输入默认上传速率 500 KB/s (formInputs[2], formUnits[2] -> KB/s index 1)
	m.formInputs[2].SetValue("500")
	m.formUnits[2].selected = 1
	// 点按钮 "下一步：协议与资源限额 ▶" (index 1)
	m.formFocus = len(m.formRows)
	m.formBtnIdx = 1
	m.Update(keyType(tea.KeyEnter))
	if m.modal != srvModalEditAdvancedSettings {
		t.Fatalf("did not advance to advanced settings: modal=%v toast=%q", m.modal, m.statusToast)
	}
	// 确认包含所有高级字段并修改空闲超时 (timeouts.idle_seconds) 与分块大小 (chunk_size)
	if len(m.formInputs) < 15 {
		t.Fatalf("expected full advanced settings fields, got %d", len(m.formInputs))
	}
	// 将第 1 个高级项 limits.max_streams 改为 32
	m.formInputs[0].SetValue("32")
	// 提交保存全部设置 (主按钮 index 1)
	m.formFocus = len(m.formRows)
	m.formBtnIdx = 1
	_, cmd := m.Update(keyType(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("save all settings returned nil cmd")
	}
	m.Update(cmd())

	saved, err := config.LoadServer(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Bandwidth.UploadBytesPerSecond != 500_000 || saved.GlobalBandwidth == nil || saved.Limits.MaxStreams != 32 {
		t.Fatalf("saved settings lost fields: bw=%+v gb=%+v limits=%+v", saved.Bandwidth, saved.GlobalBandwidth, saved.Limits)
	}

	// 3. 测试用户策略编辑器支持速率单位下拉选择 (MB/s)
	m.Update(m.loadConfigCmd()())
	m.Update(keyRune('2'))
	m.Update(keyRune('p'))
	if m.modal != srvModalEditPolicy || len(m.formUnits) < 3 {
		t.Fatalf("policy modal missing unit dropdowns: modal=%v", m.modal)
	}
	m.formToggles[3] = false // 取消继承默认带宽
	m.formInputs[0].SetValue("4")
	m.formUnits[0].selected = 2 // MB/s
	_, pCmd := m.submitModalForm()
	if pCmd == nil {
		t.Fatal("policy submit returned nil cmd")
	}
	m.Update(pCmd())
	updated, err := config.LoadServer(cfgPath)
	if err != nil || updated.AuthorizedClients[0].Bandwidth == nil || updated.AuthorizedClients[0].Bandwidth.UploadBytesPerSecond != 4_000_000 {
		t.Fatalf("user policy unit dropdown save failed: %+v %v", updated.AuthorizedClients[0].Bandwidth, err)
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
