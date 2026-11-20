package tui

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

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

func TestServerMouseInteractionAndNoF6Toggle(t *testing.T) {
	exeDir := t.TempDir()
	m := newServerModel(ServerOptions{ExecutableDir: exeDir, ConfigPath: filepath.Join(t.TempDir(), "server.json")})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	if _, cmd := m.Update(keyType(tea.KeyF6)); cmd != nil || m.statusToast != "" {
		t.Fatalf("expected F6 theme toggle removed from server panel, cmd=%v toast=%q", cmd, m.statusToast)
	}
	if strings.Contains(ansi.Strip(m.renderFooter()), "F6") {
		t.Fatalf("expected F6 removed from server footer:\n%s", ansi.Strip(m.renderFooter()))
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

func TestServerRenewCertPreservesDisabledUserState(t *testing.T) {
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
	// 先独立禁用用户 alice
	u0 := cfg.AuthorizedClients[0]
	if _, err := certmanage.Execute("policy", certmanage.Options{
		ServerConfig: cfgPath,
		Dir:          certDir,
		Name:         u0.UserID,
		ExpectedUser: &u0,
		Policy: &certmanage.UserPolicy{
			Disabled:    true,
			Permissions: []string{"upload", "download"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	m := newServerModel(ServerOptions{ConfigPath: cfgPath, Page: 1})
	m.Update(m.loadConfigCmd()())
	if !m.cfg.AuthorizedClients[0].Disabled {
		t.Fatal("precondition failed: alice should be disabled before renewal")
	}

	// 按 w 续期用户证书，必须保持原禁用状态
	oldCertificate := m.cfg.AuthorizedClients[0].Certificate
	_, cmd := m.Update(keyRune('w'))
	if cmd == nil {
		t.Fatal("w did not trigger renewUserCertCmd")
	}
	res := cmd()
	action, ok := res.(serverActionResultMsg)
	if !ok || action.err != nil {
		t.Fatalf("certificate renewal must succeed before checking disable state: result=%T error=%v", res, action.err)
	}
	m.Update(res)
	m.Update(m.loadConfigCmd()())
	if m.cfg.AuthorizedClients[0].Certificate == oldCertificate {
		t.Error("certificate renewal did not publish a new certificate generation")
	}
	if !m.cfg.AuthorizedClients[0].Disabled {
		t.Fatalf("cert renewal unexpectedly re-enabled disabled user: %+v", m.cfg.AuthorizedClients[0])
	}

	// 重新启用必须是独立操作（例如按 t）
	_, enableCmd := m.Update(keyRune('t'))
	if enableCmd == nil {
		t.Fatal("t did not trigger toggleUserDisabledCmd")
	}
	m.Update(enableCmd())
	m.Update(m.loadConfigCmd()())
	if m.cfg.AuthorizedClients[0].Disabled {
		t.Fatal("independent enable action failed to re-enable user")
	}
}

func TestServerFormsRejectInvalidInputKeepValuesAndFocusErrorField(t *testing.T) {
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

	m := newServerModel(ServerOptions{ConfigPath: cfgPath, Page: 1})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	m.Update(m.loadConfigCmd()())

	// 1. 新建用户：非法有效天数（如 "bad" / "0" / "4000"）不得静默替换为 30，不得关闭弹窗，应定位到天数行
	for _, badDays := range []string{"bad", "0", "-7", "4000", ""} {
		m.openNewUserModal()
		m.formInputs[0].SetValue("bob")
		m.formInputs[1].SetValue(badDays)
		m.formFocus = len(m.formRows) // 模拟从确认按钮按 Enter 提交
		m.formBtnIdx = 1
		_, cmd := m.Update(keyType(tea.KeyEnter))
		if cmd != nil {
			// 只允许 Blink 命令，绝不能返回 execCertAndReload 命令
			if msg := cmd(); msg != nil {
				if _, isAction := msg.(serverActionResultMsg); isAction {
					t.Fatalf("invalid days %q submitted cert creation action", badDays)
				}
			}
		}
		if m.modal != srvModalNewUser {
			t.Fatalf("modal closed prematurely on invalid days %q", badDays)
		}
		if m.formInputs[0].Value() != "bob" || m.formInputs[1].Value() != badDays {
			t.Fatalf("form inputs lost user entries on invalid days %q: name=%q days=%q", badDays, m.formInputs[0].Value(), m.formInputs[1].Value())
		}
		if m.formFocus != 1 || !m.formInputs[1].Focused() {
			t.Fatalf("focus did not jump to offending days field on %q: focus=%d inputFocused=%v", badDays, m.formFocus, m.formInputs[1].Focused())
		}
		saved, _ := config.LoadServer(cfgPath)
		for _, u := range saved.AuthorizedClients {
			if u.UserID == "bob" {
				t.Fatalf("invalid days %q silently created user bob", badDays)
			}
		}
	}

	// 2. CSR 签发弹窗：非法天数禁止提交且定位到天数行 (row 2)
	m.openCSRModal("carol")
	m.formInputs[1].SetValue("/tmp/carol.csr")
	m.formInputs[2].SetValue("invalid")
	m.formFocus = len(m.formRows)
	m.formBtnIdx = 1
	m.Update(keyType(tea.KeyEnter))
	if m.modal != srvModalCSR || m.formInputs[1].Value() != "/tmp/carol.csr" || m.formFocus != 2 || !m.formInputs[2].Focused() {
		t.Fatalf("CSR modal failed validation focus check: modal=%v focus=%d csr=%q", m.modal, m.formFocus, m.formInputs[1].Value())
	}

	// 3. 轮换服务端证书：非法天数禁止提交且定位到天数行 (row 1)
	m.openRotateServerModal()
	m.formInputs[0].SetValue("example.com")
	m.formInputs[1].SetValue("9999")
	m.formFocus = len(m.formRows)
	m.formBtnIdx = 1
	m.Update(keyType(tea.KeyEnter))
	if m.modal != srvModalRotateServer || m.formInputs[0].Value() != "example.com" || m.formFocus != 1 || !m.formInputs[1].Focused() {
		t.Fatalf("rotate server modal failed validation focus check: modal=%v focus=%d", m.modal, m.formFocus)
	}

	// 4. 用户策略编辑器：非法自定义带宽保持弹窗、保留 targetUser 并定位到错误框
	m.Update(keyRune('2'))
	m.openEditPolicyModal(m.cfg.AuthorizedClients[0])
	m.formToggles[3] = false
	m.formInputs[1].SetValue("bad-num")
	m.formFocus = len(m.formRows)
	m.formBtnIdx = 1
	m.Update(keyType(tea.KeyEnter))
	if m.modal != srvModalEditPolicy || m.targetUser == nil || m.formInputs[1].Value() != "bad-num" || !m.formInputs[1].Focused() {
		t.Fatalf("edit policy modal did not retain targetUser or focus input[1]: modal=%v target=%v focus=%d", m.modal, m.targetUser, m.formFocus)
	}

	// 5. 导出分发包弹窗：空目标目录保持弹窗、保留 targetUser 并聚焦目录输入框
	m.openExportUserModal(m.cfg.AuthorizedClients[0])
	m.formInputs[0].SetValue("   ")
	m.formFocus = len(m.formRows)
	m.formBtnIdx = 1
	m.Update(keyType(tea.KeyEnter))
	if m.modal != srvModalExportUser || m.targetUser == nil || !m.formInputs[0].Focused() {
		t.Fatalf("export modal closed or lost focus on empty out dir: modal=%v target=%v focus=%d", m.modal, m.targetUser, m.formFocus)
	}
	m.Update(keyType(tea.KeyEsc))

	// 6. 基础设置编辑器：空存储根目录保持弹窗、保留已输入并定位到存储目录行
	m.openEditSettingsModal()
	m.formInputs[1].SetValue("")
	m.formFocus = len(m.formRows)
	m.formBtnIdx = 2 // "直接保存 ★"
	m.Update(keyType(tea.KeyEnter))
	if m.modal != srvModalEditSettings || !m.formInputs[1].Focused() {
		t.Fatalf("settings modal closed or did not focus storage row: modal=%v focus=%d", m.modal, m.formFocus)
	}
	m.Update(keyType(tea.KeyEsc))

	// 7. 部署向导第 3 步（未初始化配置模型）：非法有效天数阻塞跳转并定位到天数行
	wm := newServerModel(ServerOptions{ExecutableDir: dir, ConfigPath: filepath.Join(dir, "configs", "new.json")})
	wm.Update(tea.WindowSizeMsg{Width: 110, Height: 34})
	wm.Update(serverConfigLoadedMsg{err: os.ErrNotExist})
	if wm.deploy == nil {
		t.Fatal("missing config did not initialize deploy wizard")
	}
	wm.deploy.advanced = true
	wm.openDeployStep(3)
	// 找到有效期 inputIdx (内容默认为 "30")
	daysIdx := -1
	for i, in := range wm.formInputs {
		if in.Value() == "30" {
			daysIdx = i
			break
		}
	}
	if daysIdx < 0 {
		t.Fatal("could not locate days input in deploy step 3")
	}
	wm.formInputs[daysIdx].SetValue("0")
	wm.formFocus = len(wm.formRows)
	wm.formBtnIdx = 1
	wm.Update(keyType(tea.KeyEnter))
	if wm.deploy.step != 3 || !wm.formInputs[daysIdx].Focused() {
		t.Fatalf("deploy step 3 advanced on invalid days or failed focus: step=%d focus=%d", wm.deploy.step, wm.formFocus)
	}
}

func TestServerHelpAndDetailModalsScrollWithinTerminalHeight(t *testing.T) {
	m := newServerModel(ServerOptions{ExecutableDir: t.TempDir(), ConfigPath: "configs/server.local.json"})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 14})

	// 1. 帮助弹窗在 80x14 下支持滚动，标题与关闭提示始终在框内；在 80x24 下行不乱折
	m.Update(keyRune('?'))
	h0 := ansi.Strip(m.View())
	if lipgloss.Height(h0) > 14 || !strings.Contains(h0, "服务端管理快捷键") || !strings.Contains(h0, "关闭") {
		t.Fatalf("server help modal overflowed or clipped chrome:\n%s", h0)
	}
	_, helpLines, _ := m.infoModalData()
	for _, ln := range helpLines {
		if w := lipgloss.Width(ansi.Strip(ln)); w > 70 {
			t.Fatalf("server help line exceeds 80-col modal inner width (%d > 70): %q", w, ansi.Strip(ln))
		}
	}
	m.Update(keyType(tea.KeyDown))
	if m.infoScroll == 0 {
		t.Fatal("server help modal did not scroll on down key")
	}

	// 2. 长详情弹窗在 80x14 下通过键盘与滚轮滚动且保留底部分页/关闭栏
	m.detailTitle = "证书链详情测试"
	m.detailLines = make([]string, 25)
	for i := range m.detailLines {
		m.detailLines[i] = fmt.Sprintf("cert-line-%02d: %s", i, strings.Repeat("Z", 82))
	}
	m.infoScroll = 0
	m.modal = srvModalDetail
	m.Update(tea.MouseMsg{X: 40, Y: 7, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if m.infoScroll == 0 {
		t.Fatal("server detail modal did not scroll on wheel down")
	}
	m.Update(keyType(tea.KeyEnd))
	vEnd := ansi.Strip(m.View())
	if lipgloss.Height(vEnd) > 14 || !strings.Contains(vEnd, "证书链详情测试") || !strings.Contains(vEnd, "关闭") || !strings.Contains(vEnd, "cert-line-24") {
		t.Fatalf("server detail modal clipped chrome or failed scroll to bottom:\n%s", vEnd)
	}
}

func TestServerExternalKeyCSRRenewAndModalGuards(t *testing.T) {
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

	writeCSR := func(cn, name string) string {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, priv)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	csrPath := writeCSR("carol", "carol.csr")
	if _, err := certmanage.Execute("user", certmanage.Options{
		ServerConfig:    cfgPath,
		Dir:             certDir,
		ClientConfigDir: filepath.Dir(cfgPath),
		Name:            "carol",
		CSR:             csrPath,
		Days:            30,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := certmanage.Execute("policy", certmanage.Options{
		ServerConfig:    cfgPath,
		Dir:             certDir,
		ClientConfigDir: filepath.Dir(cfgPath),
		Name:            "carol",
		Policy:          &certmanage.UserPolicy{Disabled: true, Permissions: []string{"download"}},
	}); err != nil {
		t.Fatal(err)
	}

	m := newServerModel(ServerOptions{ExecutableDir: dir, ConfigPath: cfgPath, Page: 1})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
	m.Update(m.loadConfigCmd()())

	// 1. 后台任务执行期间按 q 不得直接退出
	m.busy = true
	_, qCmd := m.Update(keyRune('q'))
	if qCmd != nil {
		t.Fatal("expected q to be blocked while busy")
	}
	m.busy = false

	// 2. 新建用户校验重复用户名与保留名，禁止提前关闭弹窗
	m.openNewUserModal()
	m.formInputs[0].SetValue("alice")
	m.Update(keyType(tea.KeyEnter))
	if m.modal != srvModalNewUser || !strings.Contains(ansi.Strip(m.View()), "已存在") {
		t.Fatalf("expected duplicate user error inside srvModalNewUser, modal=%v view:\n%s", m.modal, m.View())
	}
	m.Update(keyType(tea.KeyEsc))

	// 3. 轮换服务端证书：非法 SAN 在提交前被拦截并聚焦第 0 项
	m.openRotateServerModal()
	m.formInputs[0].SetValue("bad host name with spaces")
	m.formInputs[1].SetValue("30")
	m.formFocus = len(m.formRows)
	m.formBtnIdx = 1
	m.Update(keyType(tea.KeyEnter))
	if m.modal != srvModalRotateServer || m.formFocus != 0 {
		t.Fatalf("expected invalid SAN to keep srvModalRotateServer open and focus SAN field, modal=%v focus=%d", m.modal, m.formFocus)
	}
	m.Update(keyType(tea.KeyEsc))

	// 4. 对 ExternalKey CSR 用户 carol 按 w 必须弹出 CSR 续期弹窗，校验 CSR 文件并保留 Disabled 状态
	m.cursors[1] = 1 // carol
	m.Update(keyRune('w'))
	if m.modal != srvModalCSR || m.formInputs[0].Value() != "carol" || m.targetUser == nil {
		t.Fatalf("expected w on ExternalKey user to open prefilled srvModalCSR, modal=%v user=%q target=%v", m.modal, m.formInputs[0].Value(), m.targetUser)
	}
	// 不存在的 CSR 路径不得关闭弹窗
	m.formInputs[1].SetValue(filepath.Join(dir, "missing.csr"))
	m.formInputs[2].SetValue("60")
	m.formFocus = len(m.formRows)
	m.formBtnIdx = 1
	m.Update(keyType(tea.KeyEnter))
	if m.modal != srvModalCSR || m.formFocus != 1 {
		t.Fatalf("expected missing CSR to keep modal open on path row, modal=%v focus=%d", m.modal, m.formFocus)
	}
	// 提供新 CSR 并提交后完成续期，且必须保留原 Disabled 状态
	nextCSR := writeCSR("carol", "carol-renew.csr")
	m.formInputs[1].SetValue(nextCSR)
	_, submitCmd := m.Update(keyType(tea.KeyEnter))
	if submitCmd == nil {
		t.Fatal("expected valid CSR renewal submit cmd")
	}
	m.Update(submitCmd())
	m.Update(m.loadConfigCmd()())
	if len(m.cfg.AuthorizedClients) < 2 || !m.cfg.AuthorizedClients[1].Disabled {
		t.Fatalf("expected carol Disabled state preserved after CSR renewal: %+v", m.cfg.AuthorizedClients)
	}
}

func TestServerBusyProgressDoubleSubmitGuardAndContextualFooter(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "configs", "server.local.json")
	if _, err := certmanage.Execute("init", certmanage.Options{
		ServerConfig:    cfgPath,
		Dir:             filepath.Join(dir, "certs"),
		ClientConfigDir: filepath.Dir(cfgPath),
		Name:            "alice",
		Hosts:           "localhost,127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}

	m := newServerModel(ServerOptions{ExecutableDir: dir, ConfigPath: cfgPath, Page: 0})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.Update(m.loadConfigCmd()())

	// 1. 离线状态下，概览页应展示不可用操作及原因，且保持单行 80x24
	f0 := ansi.Strip(m.renderFooter())
	if lipgloss.Height(f0) != 1 || !strings.Contains(f0, "未运行") || !strings.Contains(f0, "已初始化") {
		t.Fatalf("unexpected server overview footer:\n%s", f0)
	}

	// 2. 设置页：离线时热加载不可用并显示原因
	m.Update(keyRune('5'))
	f4 := ansi.Strip(m.renderFooter())
	if lipgloss.Height(f4) != 1 || !strings.Contains(f4, "服务离线") {
		t.Fatalf("unexpected server settings footer:\n%s", f4)
	}

	// 3. 后台任务执行时：Tab bar 呈现带旋转指示器的进度标签，且阻止 s/R/x/K 重复提交
	m.Update(keyRune('1'))
	m.startBusy("后台启动 sftd 服务")
	if !strings.Contains(ansi.Strip(m.renderTabBar()), "后台启动 sftd 服务") {
		t.Fatalf("expected busy progress indicator in tab bar:\n%s", ansi.Strip(m.renderTabBar()))
	}
	for _, k := range []rune{'s', 'x', 'R'} {
		if _, cmd := m.Update(keyRune(k)); cmd != nil || m.modal != srvModalNone {
			t.Fatalf("expected %q to be blocked while server busy", string(k))
		}
	}
	m.Update(keyRune('4'))
	if _, cmd := m.Update(keyRune('K')); cmd != nil {
		t.Fatal("expected K CA renew to be blocked while server busy")
	}
	m.Update(serverActionResultMsg{notice: "完成"})
	if m.busy {
		t.Fatal("expected busy flag cleared after serverActionResultMsg")
	}
}

func TestServerSettingsPerFieldRuntimeDiffAndReloadLabels(t *testing.T) {
	cfg := config.DefaultServer()
	cfg.Listen = "127.0.0.1:8443"
	rt := cfg

	m := newServerModel(ServerOptions{ExecutableDir: t.TempDir(), ConfigPath: "configs/server.local.json", Page: 4})
	m.loaded = true
	m.cfg = cfg
	m.runtime = &monitor.Snapshot{State: "running", Listen: rt.Listen, Started: time.Now(), Config: rt}
	m.Update(tea.WindowSizeMsg{Width: 132, Height: 34})

	// 1. 未修改时界面清爽：不展示多余的 [需重启]/[可热更] 或英文键名括号备注，也不显示独立 [L] 日志上限快捷键
	cleanView := ansi.Strip(m.View())
	for _, unwanted := range []string{"[需重启]", "[可热更]", "(resources)", "(limits", "(timeouts)", "(listen)", "(storage_root)", "[L]"} {
		if strings.Contains(cleanView, unwanted) {
			t.Fatalf("settings page still contains cluttered token %q:\n%s", unwanted, cleanView)
		}
	}
	// 日志上限修改已整合进基础设置 [m] 中，单独按 L 不得打开旧弹窗
	m.Update(keyRune('L'))
	if m.modal != srvModalNone {
		t.Fatalf("expected L key to no longer open standalone log limit modal, got %v", m.modal)
	}
	m.Update(keyRune('m'))
	if _, ok := m.inputByKey("logging.max_file_bytes"); !ok || m.modal != srvModalEditSettings {
		t.Fatalf("expected logging.max_file_bytes inside basic settings modal, modal=%v", m.modal)
	}
	m.Update(keyType(tea.KeyEsc))

	// 2. 当字段相对运行实例发生变更时，再针对变更项提示 [待热更] 或 [待重启]
	m.cfg.GlobalBandwidth = &config.Bandwidth{UploadBytesPerSecond: 2_000_000, DownloadBytesPerSecond: 3_000_000, BurstBytes: 256_000}
	m.cfg.Limits.MaxStreams = 16

	view := ansi.Strip(m.View())
	if !strings.Contains(view, "[待热更]") || !strings.Contains(view, "全局总带宽上限") || !strings.Contains(view, "运行: 未启用") {
		t.Fatalf("expected per-field hot-reload diff on GlobalBandwidth:\n%s", view)
	}
	if !strings.Contains(view, "[待重启]") || !strings.Contains(view, "流控参数") || !strings.Contains(view, "需重启生效") {
		t.Fatalf("expected per-field restart-required diff on Limits.MaxStreams:\n%s", view)
	}
}

func TestServerAdvancedLogFiltersAndJumpFromUsersAndTransfers(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "configs", "server.local.json")
	if _, err := certmanage.Execute("init", certmanage.Options{
		ServerConfig:    cfgPath,
		Dir:             filepath.Join(dir, "certs"),
		ClientConfigDir: filepath.Dir(cfgPath),
		Name:            "alice",
		Hosts:           "localhost,127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}

	m := newServerModel(ServerOptions{ExecutableDir: dir, ConfigPath: cfgPath, Page: 1})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	m.Update(m.loadConfigCmd()())

	// 1. 用户页按 g 跳转到日志页，应自动预设 User 过滤条件
	m.Update(keyRune('g'))
	if m.page != 5 || m.logQuery.User != "alice" {
		t.Fatalf("expected jump to page 5 with User=alice, got page=%d query=%+v", m.page, m.logQuery)
	}
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "用户:alice") {
		t.Fatalf("expected active log filter chip for user alice:\n%s", v)
	}

	// 2. 传输页按 g 跳转到日志页，应同时设置 User 与 File 过滤，并在离线 logFallback 生效
	m.logFallback = "2026 INFO user=alice file=report.bin ok\n2026 WARN user=bob file=other.bin slow\n2026 ERROR user=alice file=secret.bin denied\n"
	m.runtime = &monitor.Snapshot{
		State:  "running",
		Listen: "127.0.0.1:7443",
		Tasks:  []monitor.Task{{ID: 1, User: "alice", Direction: "upload", File: "report.bin", State: "transferring"}},
	}
	m.Update(keyRune('3'))
	m.Update(keyRune('g'))
	if m.page != 5 || m.logQuery.User != "alice" || m.logQuery.File != "report.bin" {
		t.Fatalf("expected jump from transfer to set User+File filter, got page=%d query=%+v", m.page, m.logQuery)
	}
	vTx := ansi.Strip(m.View())
	if !strings.Contains(vTx, "report.bin") || strings.Contains(vTx, "secret.bin") || strings.Contains(vTx, "other.bin") {
		t.Fatalf("expected offline log fallback filtered to alice + report.bin:\n%s", vTx)
	}

	// 3. 在日志页用 T 切换时间段，C 清空全部筛选
	m.Update(keyRune('T'))
	if m.logSinceIdx != 1 || m.effectiveLogQuery().Since.IsZero() {
		t.Fatalf("expected T to enable 5m time window filter: sinceIdx=%d", m.logSinceIdx)
	}
	m.Update(keyRune('C'))
	if m.hasLogFilters() {
		t.Fatalf("expected C to clear all log filters, got query=%+v level=%q sinceIdx=%d", m.logQuery, m.logMinLevel, m.logSinceIdx)
	}
}

func TestServerManualRefreshCompletionAndToastAutoExpiry(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "configs", "server.local.json")
	if _, err := certmanage.Execute("init", certmanage.Options{
		ServerConfig:    cfgPath,
		Dir:             filepath.Join(dir, "certs"),
		ClientConfigDir: filepath.Dir(cfgPath),
		Name:            "alice",
		Hosts:           "localhost,127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}

	m := newServerModel(ServerOptions{ExecutableDir: dir, ConfigPath: cfgPath})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	m.Update(m.loadConfigCmd()())

	// 1. 按 r 手动刷新：进行中保持提示，后台轮询不抢先覆盖，双路完成后显示结果并在 2-3 秒后自动清除
	_, cmd := m.Update(keyRune('r'))
	if cmd == nil {
		t.Fatal("expected batch cmd on pressing r")
	}
	if m.statusToast != "正在刷新配置与运行状态…" || !m.toastUntil.IsZero() {
		t.Fatalf("expected sticky refreshing toast, got toast=%q until=%v", m.statusToast, m.toastUntil)
	}
	// 无关后台轮询消息 (seq=0) 不得提前覆盖或结束手动刷新状态
	m.Update(m.pollRuntimeCmd()())
	if m.statusToast != "正在刷新配置与运行状态…" {
		t.Fatalf("stale background poll overwrote manual refresh toast: %q", m.statusToast)
	}

	batchMsg, ok := cmd().(tea.BatchMsg)
	if !ok || len(batchMsg) != 2 {
		t.Fatalf("expected 2 sub-cmds in manual refresh batch, got %T", cmd())
	}
	m.Update(batchMsg[0]())
	if m.statusToast != "正在刷新配置与运行状态…" {
		t.Fatalf("refresh toast cleared before both config and poll completed: %q", m.statusToast)
	}
	m.Update(batchMsg[1]())
	if !strings.Contains(m.statusToast, "已刷新配置") {
		t.Fatalf("expected refresh completion notice, got %q", m.statusToast)
	}
	ttl := time.Until(m.toastUntil)
	if ttl < 2*time.Second || ttl > 3*time.Second {
		t.Fatalf("expected toast TTL between 2s and 3s, got %v", ttl)
	}
	m.Update(serverTickMsg(time.Now().Add(1 * time.Second)))
	if m.statusToast == "" {
		t.Fatal("toast cleared too early (<2s)")
	}
	m.Update(serverTickMsg(time.Now().Add(3 * time.Second)))
	if m.statusToast != "" {
		t.Fatalf("expected manual refresh toast to auto-dismiss after 2-3s, still %q", m.statusToast)
	}

	// 2. 新建/修改用户等动作完成后显示结果并在 2-3 秒后自动消失
	m.Update(serverActionResultMsg{notice: "已新建用户 bob（已热加载到运行中的服务端）"})
	if !strings.Contains(m.statusToast, "已新建用户 bob") {
		t.Fatalf("expected action result toast, got %q", m.statusToast)
	}
	m.Update(serverTickMsg(time.Now().Add(3 * time.Second)))
	if m.statusToast != "" {
		t.Fatalf("expected action result toast to auto-dismiss, still %q", m.statusToast)
	}

	// 3. 弹窗内校验错误在弹窗打开期间不得自动消失，按 Esc 取消弹窗后自动清理以免泄漏到主栏
	m.openNewUserModal()
	m.formInputs[0].SetValue("bad user!")
	m.triggerPrimaryButton()
	if m.modal != srvModalNewUser || m.statusToast == "" {
		t.Fatalf("expected modal validation error, modal=%v toast=%q", m.modal, m.statusToast)
	}
	m.Update(serverTickMsg(time.Now().Add(10 * time.Second)))
	if m.statusToast == "" {
		t.Fatal("validation error inside open modal expired prematurely")
	}
	m.Update(keyType(tea.KeyEsc))
	if m.modal != srvModalNone || m.statusToast != "" {
		t.Fatalf("expected Esc to clear modal validation error, modal=%v toast=%q", m.modal, m.statusToast)
	}
}

func TestServerRuntimeMonitorCardAdaptivelyFillsPanelWidth(t *testing.T) {
	cfg := config.DefaultServer()
	cfg.Listen = "127.0.0.1:7443"
	cfg.StorageRoot = "/tmp/sftd-storage"
	m := newServerModel(ServerOptions{ExecutableDir: t.TempDir(), ConfigPath: "configs/server.local.json", Page: 0})
	m.loaded = true
	m.cfg = cfg
	m.runtime = &monitor.Snapshot{
		State:          "running",
		Listen:         cfg.Listen,
		Started:        time.Now().Add(-5 * time.Minute),
		StorageAt:      time.Now(),
		OnlineUsers:    2,
		Connections:    3,
		ActiveTasks:    1,
		PublishedFiles: 12,
		PublishedBytes: 4 << 20,
		ReceiveRate:    1_500_000,
		SendRate:       2_400_000,
		Received:       30 << 20,
		Sent:           45 << 20,
		Completed:      9,
		Failed:         1,
	}
	for i := 0; i < 40; i++ {
		m.trend = append(m.trend, [2]float64{float64((i%8)+1) * 1e5, float64(((i+3)%8)+1) * 1e5})
	}

	for _, dims := range [][2]int{{52, 14}, {80, 24}, {120, 34}, {160, 40}} {
		w, h := dims[0], dims[1]
		m.Update(tea.WindowSizeMsg{Width: w, Height: h})
		view := ansi.Strip(m.View())
		if gotH := lipgloss.Height(view); gotH > h {
			t.Fatalf("%dx%d view exceeded height: %d > %d\n%s", w, h, gotH, h, view)
		}
		for i, ln := range strings.Split(view, "\n") {
			if gotW := lipgloss.Width(ln); gotW > w {
				t.Fatalf("%dx%d line %d exceeded width: %d > %d: %q", w, h, i+1, gotW, w, ln)
			}
		}

		// 验证“上传趋势”与“下载趋势”均随卡片内宽自适应填满，不在右侧留出大片空白
		rightW := w
		if stacked, _, _, _ := overviewLayout(w, h-3); !stacked {
			rightW = w - w/2
		}
		card := ansi.Strip(m.renderRuntimeMonitorCard(rightW, 11))
		for _, ln := range strings.Split(card, "\n") {
			if strings.Contains(ln, "上传趋势：") || strings.Contains(ln, "上传：") {
				trimmed := strings.TrimSpace(strings.Trim(ln, "│ "))
				wantInner := max(16, rightW-4)
				if got := lipgloss.Width(trimmed); got != wantInner {
					t.Fatalf("%dx%d sparkline row did not fill card inner width: got=%d want=%d line=%q", w, h, got, wantInner, ln)
				}
			}
		}
		if w >= 80 {
			for _, token := range []string{"实时运行监控", "运行中", "已发布", "成功 9", "上传趋势", "下载趋势"} {
				if !strings.Contains(view, token) {
					t.Fatalf("%dx%d overview missing %q:\n%s", w, h, token, view)
				}
			}
		}
	}

	// 验证离线状态下标题与引导信息同样适配宽度且不换行
	m.runtime = nil
	m.runtimeState = "服务未运行（离线）"
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 34})
	offlineView := ansi.Strip(m.View())
	if !strings.Contains(offlineView, "○ 离线") || !strings.Contains(offlineView, "监听：127.0.0.1:7443") {
		t.Fatalf("expected offline monitor card to show right-aligned status and listen hint:\n%s", offlineView)
	}
}

func TestServerSettingsValidationFocusBackButtonAndLogsFixes(t *testing.T) {
	m := regressionServer(t, 120, 34)

	// 1. 验证部署向导第1步与基础设置均拦截非法端口 99999。
	wm := newServerModel(ServerOptions{ExecutableDir: t.TempDir(), ConfigPath: filepath.Join(t.TempDir(), "missing.json")})
	wm.Update(tea.WindowSizeMsg{Width: 120, Height: 34})
	wm.beginDeployWizard()
	wm.formInputs[1].SetValue("127.0.0.1:99999")
	wm.activateFormButton(1)
	if wm.deploy == nil || wm.deploy.step != 1 || !strings.Contains(wm.statusToast, "1–65535") {
		t.Fatalf("expected invalid listen port rejected on step 1: deploy=%v toast=%q", wm.deploy, wm.statusToast)
	}

	m.modal = srvModalNone
	m.openEditSettingsModal()
	if lIdx, ok := m.inputByKey("listen"); !ok {
		t.Fatal("missing listen field in basic settings")
	} else {
		m.formInputs[lIdx].SetValue("0.0.0.0:99999")
		m.activateFormButton(2)
		if m.modal != srvModalEditSettings || m.formFocus < 0 || !strings.Contains(m.statusToast, "1–65535") {
			t.Fatalf("expected invalid port rejected in basic settings: modal=%v toast=%q", m.modal, m.statusToast)
		}
	}

	// 2. 验证高级设置点击“返回基础设置”时，不因前方空字段丢失后方已修改的有效字段。
	m.openAdvancedSettingsModal()
	msIdx, ok1 := m.inputByKey("limits.max_streams")
	idleIdx, ok2 := m.inputByKey("timeouts.idle_seconds")
	if !ok1 || !ok2 {
		t.Fatal("missing expected keys in advanced settings")
	}
	m.formInputs[msIdx].SetValue("0")
	m.formInputs[idleIdx].SetValue("321")
	m.activateFormButton(0) // ◀ 返回基础设置
	if m.settingsDraft == nil || m.settingsDraft.Timeouts.IdleSeconds != 321 {
		t.Fatalf("expected valid timeout preserved when returning to basic settings: draft=%+v", m.settingsDraft)
	}

	// 3. 验证 HELLO 参数越界精确定位到对应字段。
	m.openAdvancedSettingsModal()
	m.formInputs[msIdx].SetValue("99")
	m.activateFormButton(1) // 保存全部设置 ★
	if m.modal != srvModalEditAdvancedSettings || m.formFocus >= len(m.formRows) || m.formRows[m.formFocus].inputIdx != msIdx {
		t.Fatalf("expected focus on max_streams (%d) after HELLO range error, got row=%d toast=%q", msIdx, m.formFocus, m.statusToast)
	}

	// 4. 验证文件日志级别过滤不再误伤含 warn/error 子串的文件名，且支持时间窗筛选。
	now := time.Now().UTC()
	oldTs := now.Add(-2 * time.Hour).Format(time.RFC3339)
	newTs := now.Add(-30 * time.Second).Format(time.RFC3339)
	m.logFallback = strings.Join([]string{
		fmt.Sprintf("time=%s level=INFO msg=done file=warning_report.txt", newTs),
		fmt.Sprintf("time=%s level=WARN msg=slow file=old.bin", oldTs),
		fmt.Sprintf("time=%s level=ERROR msg=fail file=new.bin", newTs),
	}, "\n")
	m.logMinLevel = "WARN"
	m.logSinceIdx = 1 // 近 5 分钟
	gotLines := m.filteredLogFallbackLines()
	if len(gotLines) != 1 || !strings.Contains(gotLines[0], "level=ERROR") {
		t.Fatalf("unexpected fallback log filtering result: %+v", gotLines)
	}

	// 5. 验证日志回卷重查保留 Lost 标记，且陈旧 logEpoch 轮询结果不会覆盖新筛选状态。
	svc, err := control.StartManaged(m.opts.ConfigPath, m.cfg.StorageRoot, control.Handlers{
		Snapshot: func() monitor.Snapshot {
			return monitor.Snapshot{Version: monitor.Version, State: "running", Started: now}
		},
		Logs: func(q monitor.LogQuery) monitor.LogPage {
			if q.After > 0 {
				return monitor.LogPage{Last: 10, Lost: true}
			}
			return monitor.LogPage{
				Last:   10,
				Lost:   false,
				Events: []monitor.Event{{Sequence: 10, Level: "INFO", Message: "fresh"}},
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	m.page = 5
	m.logPaused = false
	m.logCursor = 3
	m.logStarted = now
	m.logEpoch = 5
	polled, ok := m.pollRuntimeSeqCmd(0)().(serverRuntimePolledMsg)
	if !ok || !polled.logLost {
		t.Fatalf("expected Lost preserved across rewind re-query, got %+v", polled)
	}
	stale := polled
	stale.logEpoch = 4
	m.Update(stale)
	if len(m.logEvents) != 0 {
		t.Fatalf("stale logEpoch unexpectedly overwrote logEvents: %+v", m.logEvents)
	}
	m.Update(polled)
	if !m.logLost || len(m.logEvents) != 1 {
		t.Fatalf("expected fresh epoch applied with logLost=true: lost=%v events=%+v", m.logLost, m.logEvents)
	}
}

func TestServerNarrowWidthSearchInputAndToastFallbackVisible(t *testing.T) {
	m := newServerModel(ServerOptions{ExecutableDir: t.TempDir(), ConfigPath: "configs/server.local.json", Page: 1})
	m.loaded = true
	m.cfg = config.DefaultServer()
	m.Update(tea.WindowSizeMsg{Width: 52, Height: 18})

	// 1. 52 列极窄屏进入 / 搜索态，输入框必须可见且不超出宽度
	m.Update(keyRune('/'))
	m.Update(keyRune('a'))
	m.Update(keyRune('l'))
	tabView := ansi.Strip(m.renderTabBar())
	if !strings.Contains(tabView, "筛选:") || !strings.Contains(tabView, "al") {
		t.Fatalf("expected search input visible at 52 cols, got %q", tabView)
	}
	if lipgloss.Height(tabView) != 1 || lipgloss.Width(tabView) > 52 {
		t.Fatalf("narrow search tab bar overflowed: h=%d w=%d %q", lipgloss.Height(tabView), lipgloss.Width(tabView), tabView)
	}
	m.Update(keyType(tea.KeyEsc))

	// 2. 52 列极窄屏有 Toast 或忙碌状态时，应通过底栏回退确保提示可见
	m.setToast("已刷新配置")
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "已刷新配置") {
		t.Fatalf("expected toast visible at 52 cols via footer fallback:\n%s", v)
	}
	if lipgloss.Height(v) > 18 {
		t.Fatalf("52x18 view exceeded height: %d", lipgloss.Height(v))
	}
}
