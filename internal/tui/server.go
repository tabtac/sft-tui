package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/control"
	"sft-tui/internal/monitor"
	"sft-tui/internal/pal"
	"sft-tui/internal/security"
	"sft-tui/internal/service"
)

type ServerOptions struct {
	ExecutableDir string
	ConfigPath    string
	LogPath       string
	PrefsPath     string
	Page          int // 0=概览, 1=用户, 2=传输, 3=证书, 4=设置, 5=日志
}

type serverModalKind int

const (
	srvModalNone serverModalKind = iota
	srvModalHelp
	srvModalDetail
	srvModalStop
	srvModalNewUser
	srvModalEditPolicy
	srvModalExportUser
	srvModalCSR
	srvModalEditSettings
	srvModalEditAdvancedSettings
	srvModalEditLogLimit
	srvModalRotateServer
	srvModalDeployWizard
)

type certRow struct {
	Name        string
	Role        string
	Path        string
	State       string
	Expiry      string
	Fingerprint string
}

type serverConfigLoadedMsg struct {
	cfg   config.Server
	certs []certRow
	err   error
}

type serverRuntimePolledMsg struct {
	endpointRoot string
	snap         *monitor.Snapshot
	runtimeState string
	logEvents    []monitor.Event
	logLast      uint64
	logStarted   time.Time
	logLost      bool
	logFallback  string
	logErr       string
	err          error
}

type serverActionResultMsg struct {
	notice  string
	err     error
	refresh bool
	poll    bool
}

type serverTickMsg time.Time

type serverModel struct {
	opts          ServerOptions
	prefsPath     string
	th            theme
	width, height int
	page          int // 0..5

	// 磁盘与运行期快照
	loaded       bool
	cfg          config.Server
	cfgErr       error
	certs        []certRow
	endpointRoot string
	runtimeState string
	runtime      *monitor.Snapshot
	trend        [][2]float64

	// 日志状态
	logQuery     monitor.LogQuery
	logCursor    uint64
	logStarted   time.Time
	logPaused    bool
	logLost      bool
	logEvents    []monitor.Event
	logFallback  string
	logErr       string
	logMinLevel  string
	userFilter   string
	txFilter     string
	editingQuery bool
	queryInput   textinput.Model

	// 游标与详情弹窗
	cursors     [6]int
	modal       serverModalKind
	detailTitle string
	detailLines []string
	statusToast string
	busy        bool

	// 多字段表单输入与向导状态
	formRows           []formRow
	formInputs         []textinput.Model
	formUnits          []*formUnitField
	formSelects        []formSelectField
	formLabels         []string
	formFocus          int
	formSubFocus       int
	formScroll         int
	formToggles        []bool
	formToggleLbl      []string
	formButtons        []formButton
	formBtnIdx         int
	dropdownOpen       bool
	dropdownRow        int
	dropdownCursor     int
	targetUser         *config.AuthorizedClient
	deployAutoPrompted bool
	deploy             *deployWizardDraft
	settingsExpected   config.Server
	settingsDraft      *config.Server
}

// RunServer 启动服务端 Bubble Tea 管理面板并阻塞直至管理员按 q/Esc/F10 退出。
func RunServer(opts ServerOptions) error {
	if opts.ConfigPath == "" {
		opts.ConfigPath = filepath.Join("configs", "server.local.json")
	}
	if opts.ExecutableDir == "" {
		if exe, err := os.Executable(); err == nil {
			opts.ExecutableDir = filepath.Dir(exe)
		}
	}
	m := newServerModel(opts)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

func newServerModel(opts ServerOptions) *serverModel {
	if opts.ExecutableDir == "" {
		if exe, err := os.Executable(); err == nil {
			opts.ExecutableDir = filepath.Dir(exe)
		}
	}
	prefsPath := resolvePrefsPath(opts.PrefsPath, opts.ExecutableDir, ".sftd-prefs.json")
	qi := textinput.New()
	qi.Placeholder = "输入关键字筛选…"
	qi.CharLimit = 256
	qi.Width = 40
	return &serverModel{
		opts:         opts,
		prefsPath:    prefsPath,
		th:           makeTheme(loadThemePref(prefsPath)),
		page:         max(0, min(5, opts.Page)),
		runtimeState: "正在连接本机控制端点…",
		queryInput:   qi,
	}
}

func (m *serverModel) Init() tea.Cmd {
	return tea.Batch(
		m.loadConfigCmd(),
		m.pollRuntimeCmd(),
		m.tickCmd(),
	)
}

func (m *serverModel) tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return serverTickMsg(t)
	})
}

func (m *serverModel) loadConfigCmd() tea.Cmd {
	cfgPath := m.opts.ConfigPath
	return func() tea.Msg {
		c, err := config.LoadServerDraft(cfgPath)
		var certs []certRow
		if err == nil {
			now := time.Now()
			addCert := func(name, role, path string) {
				ins := security.InspectCertificate(path, now)
				row := certRow{Name: name, Role: role, Path: path, State: ins.State, Fingerprint: ins.Fingerprint}
				if ins.Error != nil {
					row.Expiry = ins.Error.Error()
				} else {
					row.Expiry = ins.Certificate.NotAfter.Local().Format("2006-01-02 15:04")
				}
				certs = append(certs, row)
			}
			addCert("CA", "信任锚", c.TLS.CACertificate)
			addCert("server", "服务端", c.TLS.Certificate)
			for _, u := range c.AuthorizedClients {
				role := "用户(已授权)"
				if u.Disabled {
					role = "用户(已吊销)"
				}
				addCert(u.UserID, role, u.Certificate)
			}
		}
		return serverConfigLoadedMsg{cfg: c, certs: certs, err: err}
	}
}

func (m *serverModel) pollRuntimeCmd() tea.Cmd {
	cfgPath := m.opts.ConfigPath
	root := m.endpointRoot
	diskRoot := ""
	if m.cfgErr == nil {
		diskRoot = m.cfg.StorageRoot
	}
	wantLogs := m.page == 5 && !m.logPaused
	q := m.logQuery
	q.Level = m.logMinLevel
	q.Text = m.queryInput.Value()
	if !wantLogs {
		q = monitor.LogQuery{}
	}
	after := m.logCursor
	started := m.logStarted
	logPath := m.opts.LogPath
	prevEvents := append([]monitor.Event(nil), m.logEvents...)

	return func() tea.Msg {
		located, locErr := control.Locate(cfgPath, root)
		if locErr == nil && located != "" {
			root = located
		}
		if root == "" && diskRoot != "" {
			root = diskRoot
		}
		var snap *monitor.Snapshot
		err := locErr
		if err == nil && root != "" {
			snap, err = control.Status(cfgPath, root)
		}
		if err == nil && snap == nil && diskRoot != "" && diskRoot != root {
			root = diskRoot
			snap, err = control.Status(cfgPath, root)
		}

		var out serverRuntimePolledMsg
		out.endpointRoot = root
		out.err = err
		out.snap = snap
		if err != nil {
			out.runtimeState = "管理连接异常：" + err.Error()
		} else if snap == nil {
			out.runtimeState = "服务未运行（离线）"
		} else {
			out.runtimeState = serverStateText(snap.State)
		}

		if wantLogs {
			switch {
			case logPath != "":
				out.logFallback = readTailText(logPath)
			case err == nil && snap != nil:
				if !snap.Started.Equal(started) {
					after = 0
					prevEvents = nil
				}
				q.After = after
				page, logErr := control.QueryLogs(cfgPath, root, q)
				if logErr == nil && (page.Lost || after > page.Last) {
					q.After = 0
					prevEvents = nil
					page, logErr = control.QueryLogs(cfgPath, root, q)
					out.logLost = page.Lost
				}
				if logErr != nil {
					out.logErr = logErr.Error()
				} else {
					merged := append(prevEvents, page.Events...)
					if len(merged) > 200 {
						merged = merged[len(merged)-200:]
					}
					out.logEvents = merged
					out.logLast = page.Last
					out.logStarted = snap.Started
				}
			case root != "":
				out.logFallback = readTailText(filepath.Join(root, "logs", "sftd.log"))
			default:
				out.logFallback = "尚未检测到本机服务存储目录，暂无日志文件。"
			}
		}
		return out
	}
}

func serverStateText(s string) string {
	switch s {
	case "running":
		return "运行中"
	case "draining":
		return "排空停服中"
	case "stopping":
		return "停止中"
	default:
		return s
	}
}

func readTailText(path string) string {
	data, err := pal.ReadSmall(path, 64*1024)
	if err != nil {
		return "无法读取日志文件：" + err.Error()
	}
	return string(data)
}

func (m *serverModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case serverTickMsg:
		return m, tea.Batch(m.tickCmd(), m.pollRuntimeCmd())

	case serverConfigLoadedMsg:
		m.loaded = true
		m.cfg = msg.cfg
		m.certs = msg.certs
		m.cfgErr = msg.err
		if msg.err == nil {
			if m.endpointRoot == "" {
				m.endpointRoot = msg.cfg.StorageRoot
			}
			m.deployAutoPrompted = true
		} else if os.IsNotExist(msg.err) && !m.deployAutoPrompted && m.modal == srvModalNone {
			m.deployAutoPrompted = true
			m.beginDeployWizard()
		}
		m.clampCursors()
		return m, nil

	case serverRuntimePolledMsg:
		m.endpointRoot = msg.endpointRoot
		m.runtime = msg.snap
		m.runtimeState = msg.runtimeState
		if msg.snap != nil {
			m.trend = append(m.trend, [2]float64{msg.snap.ReceiveRate, msg.snap.SendRate})
			if len(m.trend) > 24 {
				m.trend = m.trend[len(m.trend)-24:]
			}
		}
		if m.page == 5 && !m.logPaused {
			m.logErr = msg.logErr
			m.logFallback = msg.logFallback
			m.logLost = msg.logLost
			if msg.logErr == "" && msg.logFallback == "" {
				m.logEvents = msg.logEvents
				m.logCursor = msg.logLast
				m.logStarted = msg.logStarted
			}
		}
		m.clampCursors()
		return m, nil

	case serverActionResultMsg:
		m.busy = false
		if msg.err != nil {
			m.statusToast = "操作失败：" + msg.err.Error()
		} else if msg.notice != "" {
			m.statusToast = msg.notice
		}
		var cmds []tea.Cmd
		if msg.refresh {
			cmds = append(cmds, m.loadConfigCmd())
		}
		if msg.poll {
			cmds = append(cmds, m.pollRuntimeCmd())
		}
		return m, tea.Batch(cmds...)

	case deployInitDoneMsg:
		m.busy = false
		if msg.err != nil {
			if m.deploy != nil {
				m.deploy.summaryErr = msg.err.Error()
				m.openDeployStep(4)
			} else {
				m.statusToast = "部署初始化失败：" + msg.err.Error()
			}
			return m, nil
		}
		if m.deploy != nil {
			m.deploy.d = msg.d
			m.deploy.saved = true
			m.deploy.started = msg.started
			m.deploy.finishMsg = msg.notice
			m.openDeployStep(5)
		}
		m.statusToast = "部署初始化已完成"
		return m, tea.Batch(m.loadConfigCmd(), m.pollRuntimeCmd())

	case deployExportDoneMsg:
		m.busy = false
		if msg.err != nil {
			if m.deploy != nil {
				m.deploy.finishMsg = "部署已保存；导出失败：" + msg.err.Error()
				m.openDeployStep(5)
			}
			m.statusToast = "导出失败：" + msg.err.Error()
			return m, nil
		}
		if m.deploy != nil {
			m.deploy.finishMsg = "用户包已成功导出至：" + msg.outDir
			m.openDeployStep(5)
		}
		m.statusToast = "用户包已导出：" + msg.outDir
		return m, nil

	case themeSavedMsg:
		if msg.err != nil {
			m.statusToast = "无法保存主题偏好：" + msg.err.Error()
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}

func (m *serverModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	if k == "f6" {
		m.th = makeTheme(!m.th.dark)
		if m.th.dark {
			m.statusToast = "已切换为深色主题"
		} else {
			m.statusToast = "已切换为浅色主题"
		}
		return m, saveThemePrefCmd(m.prefsPath, m.th.dark)
	}

	if m.editingQuery {
		switch k {
		case "esc":
			m.editingQuery = false
			m.queryInput.Blur()
			m.queryInput.SetValue("")
			m.updatePageFilter("")
			return m, nil
		case "enter":
			m.editingQuery = false
			m.queryInput.Blur()
			if m.page == 5 {
				m.logCursor = 0
				m.logEvents = nil
				return m, m.pollRuntimeCmd()
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.queryInput, cmd = m.queryInput.Update(msg)
		m.updatePageFilter(m.queryInput.Value())
		return m, cmd
	}

	if m.modal != srvModalNone {
		return m.handleModalKey(msg)
	}

	switch k {
	case "q", "f10":
		return m, tea.Quit
	case "?":
		m.modal = srvModalHelp
		return m, nil
	case "1", "2", "3", "4", "5", "6":
		idx := int(k[0] - '1')
		return m.switchPage(idx)
	case "tab":
		return m.switchPage((m.page + 1) % 6)
	case "shift+tab":
		return m.switchPage((m.page + 5) % 6)
	case "/":
		m.editingQuery = true
		m.queryInput.Focus()
		return m, textinput.Blink
	case "r":
		m.statusToast = "正在刷新配置与运行状态…"
		return m, tea.Batch(m.loadConfigCmd(), m.pollRuntimeCmd())
	case "R":
		return m, m.reloadServiceCmd()
	case "s":
		return m, m.startServiceCmd()
	case "x":
		if m.runtime != nil {
			m.modal = srvModalStop
		} else {
			m.statusToast = "本机服务当前未运行"
		}
		return m, nil
	case "up", "k":
		if m.cursors[m.page] > 0 {
			m.cursors[m.page]--
		}
		return m, nil
	case "down", "j":
		if m.cursors[m.page] < m.currentPageLen()-1 {
			m.cursors[m.page]++
		}
		return m, nil
	case "pgup":
		m.cursors[m.page] = max(0, m.cursors[m.page]-8)
		return m, nil
	case "pgdown":
		if n := m.currentPageLen(); n > 0 {
			m.cursors[m.page] = min(n-1, m.cursors[m.page]+8)
		}
		return m, nil
	}

	// 页面专属操作键
	switch m.page {
	case 0: // 概览
		switch k {
		case "I":
			m.beginDeployWizard()
			return m, textinput.Blink
		case "enter":
			if m.runtime != nil && m.cursors[0] < len(m.runtime.Peers) {
				p := m.runtime.Peers[m.cursors[0]]
				m.detailTitle = "连接详情"
				m.detailLines = []string{
					fmt.Sprintf("连接 ID： %d", p.ID),
					fmt.Sprintf("授权用户：%s", p.User),
					fmt.Sprintf("远端地址：%s", p.Remote),
					fmt.Sprintf("建立时间：%s", p.Since.Local().Format("2006-01-02 15:04:05")),
				}
				m.modal = srvModalDetail
			}
		}
	case 1: // 用户管理
		users := m.filteredUsers()
		switch k {
		case "n":
			m.openNewUserModal()
			return m, textinput.Blink
		case "C":
			m.openCSRModal("")
			return m, textinput.Blink
		case "t":
			if len(users) > 0 && m.cursors[1] < len(users) {
				return m, m.toggleUserDisabledCmd(users[m.cursors[1]])
			}
		case "p":
			if len(users) > 0 && m.cursors[1] < len(users) {
				m.openEditPolicyModal(users[m.cursors[1]])
				return m, textinput.Blink
			}
		case "w":
			if len(users) > 0 && m.cursors[1] < len(users) {
				return m, m.renewUserCertCmd(users[m.cursors[1]])
			}
		case "e":
			if len(users) > 0 && m.cursors[1] < len(users) {
				m.openExportUserModal(users[m.cursors[1]])
				return m, textinput.Blink
			}
		case "enter":
			if len(users) > 0 && m.cursors[1] < len(users) {
				m.showUserDetailModal(users[m.cursors[1]])
			}
		}
	case 2: // 实时传输
		tasks := m.filteredTasks()
		if k == "enter" && m.cursors[2] < len(tasks) {
			t := tasks[m.cursors[2]]
			m.detailTitle = "服务端传输任务详情"
			m.detailLines = []string{
				fmt.Sprintf("任务 / 连接：%d  (连接 #%d · 流 #%d)", t.ID, t.Connection, t.Stream),
				fmt.Sprintf("用户与方向：%s · %s", t.User, t.Direction),
				fmt.Sprintf("文件名：    %s", t.File),
				fmt.Sprintf("进度与速率：%d / %d B · %.1f B/s", t.Offset, t.Size, t.Rate),
				fmt.Sprintf("当前状态：  %s", t.State),
			}
			if t.Error != "" {
				m.detailLines = append(m.detailLines, "", "错误信息：", "  "+t.Error)
			}
			m.modal = srvModalDetail
		}
	case 3: // 证书状态
		switch k {
		case "K":
			return m, m.renewCACmd()
		case "S":
			m.openRotateServerModal()
			return m, textinput.Blink
		case "C":
			m.openCSRModal("")
			return m, textinput.Blink
		case "enter":
			if m.cursors[3] < len(m.certs) {
				c := m.certs[m.cursors[3]]
				m.detailTitle = "证书详情 · " + c.Name
				m.detailLines = []string{
					"角色：    " + c.Role,
					"路径：    " + c.Path,
					"状态：    " + c.State,
					"到期/备注：" + c.Expiry,
					"SHA-256： " + c.Fingerprint,
				}
				m.modal = srvModalDetail
			}
		}
	case 4: // 设置
		switch k {
		case "m":
			if m.cfgErr == nil {
				m.openEditSettingsModal()
				return m, textinput.Blink
			}
		case "a":
			if m.cfgErr == nil {
				m.openAdvancedSettingsModal()
				return m, textinput.Blink
			}
		case "L":
			if m.cfgErr == nil {
				m.openLogLimitModal()
				return m, textinput.Blink
			}
		case "A":
			return m, m.reloadServiceCmd()
		}
	case 5: // 日志
		switch k {
		case "p":
			m.logPaused = !m.logPaused
			if !m.logPaused {
				return m, m.pollRuntimeCmd()
			}
		case "L":
			if m.cfgErr == nil {
				m.openLogLimitModal()
				return m, textinput.Blink
			}
		case "l":
			order := []string{"", "INFO", "WARN", "ERROR"}
			cur := 0
			for i, v := range order {
				if v == m.logMinLevel {
					cur = i
					break
				}
			}
			m.logMinLevel = order[(cur+1)%len(order)]
			m.logCursor = 0
			m.logEvents = nil
			return m, m.pollRuntimeCmd()
		case "enter":
			if m.logFallback == "" && m.cursors[5] < len(m.logEvents) {
				e := m.logEvents[m.cursors[5]]
				m.logPaused = true
				m.detailTitle = fmt.Sprintf("日志事件 #%d [%s]", e.Sequence, e.Level)
				m.detailLines = []string{
					"时间：  " + e.Time.Local().Format("2006-01-02 15:04:05"),
					"消息：  " + e.Message,
					"用户：  " + e.User,
					"文件：  " + e.File,
					"详情：  " + e.Detail,
				}
				m.modal = srvModalDetail
			}
		}
	}
	return m, nil
}

func (m *serverModel) switchPage(idx int) (tea.Model, tea.Cmd) {
	m.page = max(0, min(5, idx))
	m.clampCursors()
	if m.page == 5 {
		return m, m.pollRuntimeCmd()
	}
	return m, nil
}

func (m *serverModel) updatePageFilter(val string) {
	switch m.page {
	case 1:
		m.userFilter = val
	case 2:
		m.txFilter = val
	case 5:
		m.logQuery.Text = val
	}
	m.clampCursors()
}

func (m *serverModel) filteredUsers() []config.AuthorizedClient {
	q := strings.ToLower(strings.TrimSpace(m.userFilter))
	var out []config.AuthorizedClient
	for _, u := range m.cfg.AuthorizedClients {
		if q != "" && !strings.Contains(strings.ToLower(u.UserID), q) {
			continue
		}
		out = append(out, u)
	}
	return out
}

func (m *serverModel) filteredTasks() []monitor.Task {
	if m.runtime == nil {
		return nil
	}
	q := strings.ToLower(strings.TrimSpace(m.txFilter))
	all := make([]monitor.Task, 0, len(m.runtime.Tasks)+len(m.runtime.History))
	all = append(all, m.runtime.Tasks...)
	all = append(all, m.runtime.History...)
	if q == "" {
		return all
	}
	var out []monitor.Task
	for _, t := range all {
		if strings.Contains(strings.ToLower(t.User+" "+t.File+" "+t.State), q) {
			out = append(out, t)
		}
	}
	return out
}

func (m *serverModel) currentPageLen() int {
	switch m.page {
	case 0:
		if m.runtime == nil {
			return 0
		}
		return len(m.runtime.Peers)
	case 1:
		return len(m.filteredUsers())
	case 2:
		return len(m.filteredTasks())
	case 3:
		return len(m.certs)
	case 4:
		if !m.loaded || m.cfgErr != nil {
			return 0
		}
		return len(m.settingsLines())
	case 5:
		if m.logFallback != "" {
			return len(strings.Split(strings.TrimSpace(m.logFallback), "\n"))
		}
		return len(m.logEvents)
	}
	return 0
}

func (m *serverModel) clampCursors() {
	for i := 0; i < 6; i++ {
		if i == m.page {
			n := m.currentPageLen()
			if m.cursors[i] >= n {
				m.cursors[i] = max(0, n-1)
			}
		}
	}
}

func (m *serverModel) initForm(labels, defaults, toggleLabels []string, toggles []bool) {
	m.resetForm()
	for i, lbl := range labels {
		def := ""
		if i < len(defaults) {
			def = defaults[i]
		}
		m.addTextRow(lbl, def)
	}
	for i, lbl := range toggleLabels {
		checked := false
		if i < len(toggles) {
			checked = toggles[i]
		}
		m.addToggleRow(lbl, checked, nil)
	}
	m.formButtons = []formButton{
		{label: "取消", onClick: func() (tea.Model, tea.Cmd) {
			m.modal = srvModalNone
			m.targetUser = nil
			return m, nil
		}},
		{label: "确认提交 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) {
			return m.submitModalForm()
		}},
	}
	m.finalizeFormFocus(true)
}

func (m *serverModel) openNewUserModal() {
	m.resetForm()
	m.modal = srvModalNewUser
	m.addTextRow("用户名", "")
	m.addTextRow("有效期(天)", "30")
	m.addToggleRow("允许上传", true, nil)
	m.addToggleRow("允许下载", true, nil)
	m.addToggleRow("继承默认带宽(关闭后下方自定义限速生效)", true, nil)
	bw := config.DefaultServer().Bandwidth
	if m.cfgErr == nil {
		bw = m.cfg.Bandwidth
	}
	m.addSizeUnitRow("自定义上传速率", bw.UploadBytesPerSecond, true)
	m.addSizeUnitRow("自定义下载速率", bw.DownloadBytesPerSecond, true)
	m.addSizeUnitRow("自定义突发配额", bw.BurstBytes, false)
	m.formButtons = []formButton{
		{label: "取消", onClick: func() (tea.Model, tea.Cmd) { m.modal = srvModalNone; return m, nil }},
		{label: "创建用户 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) { return m.submitModalForm() }},
	}
	m.finalizeFormFocus(true)
}

func (m *serverModel) openCSRModal(user string) {
	m.initForm([]string{"用户名", "CSR 文件路径", "有效期(天)"}, []string{user, "", "30"}, nil, nil)
	m.modal = srvModalCSR
}

func (m *serverModel) openEditPolicyModal(u config.AuthorizedClient) {
	hasUp, hasDown := u.Permissions == nil, u.Permissions == nil
	for _, p := range u.Permissions {
		if p == "upload" {
			hasUp = true
		} else if p == "download" {
			hasDown = true
		}
	}
	bw := m.cfg.Bandwidth
	inherit := u.Bandwidth == nil
	if u.Bandwidth != nil {
		bw = *u.Bandwidth
	}
	uCopy := u
	m.targetUser = &uCopy

	m.resetForm()
	m.modal = srvModalEditPolicy
	m.addToggleRow("启用该用户", !u.Disabled, nil)
	m.addToggleRow("允许上传", hasUp, nil)
	m.addToggleRow("允许下载", hasDown, nil)
	m.addToggleRow("继承默认带宽(关闭后使用下方自定义速率)", inherit, nil)
	m.addSizeUnitRow("上传速率", bw.UploadBytesPerSecond, true)
	m.addSizeUnitRow("下载速率", bw.DownloadBytesPerSecond, true)
	m.addSizeUnitRow("突发配额", bw.BurstBytes, false)
	m.formButtons = []formButton{
		{label: "取消", onClick: func() (tea.Model, tea.Cmd) { m.modal = srvModalNone; m.targetUser = nil; return m, nil }},
		{label: "保存策略 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) { return m.submitModalForm() }},
	}
	m.finalizeFormFocus(false)
}

func (m *serverModel) openExportUserModal(u config.AuthorizedClient) {
	uCopy := u
	m.targetUser = &uCopy
	outDir := filepath.Join("bundles", u.UserID)
	m.initForm([]string{"导出目标目录"}, []string{outDir}, []string{"仅导出公开证书(-public-only，不含私钥)"}, []bool{u.ExternalKey})
	m.modal = srvModalExportUser
}

func (m *serverModel) openRotateServerModal() {
	m.initForm([]string{"服务端 SAN(逗号分隔 DNS/IP，留空沿用)", "有效期(天)"}, []string{"", "30"}, nil, nil)
	m.modal = srvModalRotateServer
}

func (m *serverModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || m.width <= 0 || m.height <= 0 {
		return m, nil
	}
	if m.modal != srvModalNone {
		return m.handleModalMouse(msg)
	}
	if msg.Y == 1 && msg.Button == tea.MouseButtonLeft {
		names := []string{"1 概览", "2 用户", "3 传输", "4 证书", "5 设置", "6 日志"}
		if idx := tabHitIndex(msg.X, 1, names, m.th, m.page); idx >= 0 {
			return m.switchPage(idx)
		}
		return m, nil
	}
	const bodyY = 2
	bodyH := max(6, m.height-3)
	if msg.Y < bodyY || msg.Y >= bodyY+bodyH {
		return m, nil
	}
	total := m.currentPageLen()
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if m.cursors[m.page] > 0 {
			m.cursors[m.page]--
		}
	case tea.MouseButtonWheelDown:
		if m.cursors[m.page] < total-1 {
			m.cursors[m.page]++
		}
	case tea.MouseButtonLeft:
		if total == 0 {
			return m, nil
		}
		rowStartY := bodyY + 2
		visRows := max(1, bodyH-3)
		if m.page == 0 {
			_, _, topTotalH, botH := overviewLayout(m.width, bodyH)
			rowStartY = bodyY + topTotalH + 2
			visRows = max(1, botH-3)
		}
		relY := msg.Y - rowStartY
		if relY >= 0 && relY < visRows {
			idx := scrollOffset(m.cursors[m.page], total, visRows) + relY
			if idx >= 0 && idx < total {
				m.cursors[m.page] = idx
			}
		}
	}
	return m, nil
}

func (m *serverModel) showUserDetailModal(u config.AuthorizedClient) {
	status := "已启用"
	if u.Disabled {
		status = "已吊销"
	}
	bwText := "继承默认策略"
	if u.Bandwidth != nil {
		bwText = fmt.Sprintf("↑ %d B/s · ↓ %d B/s · Burst %d B", u.Bandwidth.UploadBytesPerSecond, u.Bandwidth.DownloadBytesPerSecond, u.Bandwidth.BurstBytes)
	}
	ins := security.InspectCertificate(u.Certificate, time.Now())
	m.detailTitle = "用户详情 · " + u.UserID
	m.detailLines = []string{
		"授权状态：" + status,
		"操作权限：" + strings.Join(u.Permissions, ", "),
		"带宽策略：" + bwText,
		"证书路径：" + u.Certificate,
		fmt.Sprintf("证书状态：%s · 指纹 %s", ins.State, ins.Fingerprint),
	}
	m.modal = srvModalDetail
}

func (m *serverModel) handleModalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "esc" && !m.dropdownOpen {
		m.modal = srvModalNone
		m.targetUser = nil
		m.settingsDraft = nil
		return m, nil
	}
	if m.modal == srvModalHelp || m.modal == srvModalDetail {
		if k == "enter" || k == "q" || k == "?" {
			m.modal = srvModalNone
		}
		return m, nil
	}
	if m.modal == srvModalStop {
		switch k {
		case "g", "enter":
			m.modal = srvModalNone
			return m, m.stopServiceCmd(false)
		case "f":
			m.modal = srvModalNone
			return m, m.stopServiceCmd(true)
		}
		return m, nil
	}
	return m.handleFormKey(msg)
}

func (m *serverModel) certBaseOptions(name string) certmanage.Options {
	dir := "certs"
	if m.cfgErr == nil && m.cfg.TLS.CACertificate != "" {
		dir = filepath.Dir(m.cfg.TLS.CACertificate)
	}
	return certmanage.Options{
		Dir:             dir,
		ServerConfig:    m.opts.ConfigPath,
		ClientConfigDir: filepath.Dir(m.opts.ConfigPath),
		Name:            name,
		Days:            30,
	}
}

func (m *serverModel) execCertAndReload(action string, o certmanage.Options, notice string) tea.Cmd {
	m.busy = true
	cfgPath := m.opts.ConfigPath
	return func() tea.Msg {
		res, err := certmanage.Execute(action, o)
		if err != nil {
			return serverActionResultMsg{err: err}
		}
		if res.Changed {
			if applied, rErr := control.Reload(cfgPath, res.Server.StorageRoot); rErr != nil {
				return serverActionResultMsg{err: fmt.Errorf("已保存，但热加载失败：%w", rErr), refresh: true}
			} else if applied {
				notice += "（已热加载到运行中的服务端）"
			}
		}
		return serverActionResultMsg{notice: notice, refresh: true, poll: true}
	}
}

func (m *serverModel) submitModalForm() (tea.Model, tea.Cmd) {
	kind := m.modal
	m.modal = srvModalNone
	switch kind {
	case srvModalNewUser:
		name := strings.TrimSpace(m.formInputs[0].Value())
		days, err := strconv.Atoi(strings.TrimSpace(m.formInputs[1].Value()))
		if err != nil || days < 1 {
			days = 30
		}
		perms := []string{}
		if m.formToggles[0] {
			perms = append(perms, "upload")
		}
		if m.formToggles[1] {
			perms = append(perms, "download")
		}
		var bw *config.Bandwidth
		if len(m.formToggles) > 2 && !m.formToggles[2] {
			up, e1 := m.parseSizeInput(2, "自定义上传速率")
			dn, e2 := m.parseSizeInput(3, "自定义下载速率")
			bu, e3 := m.parseSizeInput(4, "自定义突发配额")
			if e1 != nil || e2 != nil || e3 != nil {
				for _, e := range []error{e1, e2, e3} {
					if e != nil {
						m.statusToast = e.Error()
						break
					}
				}
				m.modal = srvModalNewUser
				return m, nil
			}
			bw = &config.Bandwidth{UploadBytesPerSecond: up, DownloadBytesPerSecond: dn, BurstBytes: bu}
		}
		o := m.certBaseOptions(name)
		o.Days = days
		o.Policy = &certmanage.UserPolicy{Permissions: perms, Bandwidth: bw}
		return m, m.execCertAndReload("user", o, "已新建用户 "+name)

	case srvModalCSR:
		name := strings.TrimSpace(m.formInputs[0].Value())
		csr := strings.TrimSpace(m.formInputs[1].Value())
		days, _ := strconv.Atoi(strings.TrimSpace(m.formInputs[2].Value()))
		o := m.certBaseOptions(name)
		o.CSR = csr
		if days > 0 {
			o.Days = days
		}
		return m, m.execCertAndReload("user", o, "CSR 已签发给 "+name)

	case srvModalEditPolicy:
		if m.targetUser == nil {
			return m, nil
		}
		u := *m.targetUser
		perms := []string{}
		if m.formToggles[1] {
			perms = append(perms, "upload")
		}
		if m.formToggles[2] {
			perms = append(perms, "download")
		}
		var bw *config.Bandwidth
		if !m.formToggles[3] {
			up, e1 := m.parseSizeInput(0, "上传速率")
			dn, e2 := m.parseSizeInput(1, "下载速率")
			bu, e3 := m.parseSizeInput(2, "突发配额")
			if e1 != nil || e2 != nil || e3 != nil {
				for _, e := range []error{e1, e2, e3} {
					if e != nil {
						m.statusToast = e.Error()
						break
					}
				}
				m.modal = srvModalEditPolicy
				return m, nil
			}
			bw = &config.Bandwidth{UploadBytesPerSecond: up, DownloadBytesPerSecond: dn, BurstBytes: bu}
		}
		m.targetUser = nil
		o := m.certBaseOptions(u.UserID)
		o.ExpectedUser = &u
		o.Policy = &certmanage.UserPolicy{
			Disabled:    !m.formToggles[0],
			Permissions: perms,
			Bandwidth:   bw,
		}
		return m, m.execCertAndReload("policy", o, "已更新用户策略 "+u.UserID)

	case srvModalExportUser:
		if m.targetUser == nil {
			return m, nil
		}
		u := *m.targetUser
		m.targetUser = nil
		o := m.certBaseOptions(u.UserID)
		o.Out = strings.TrimSpace(m.formInputs[0].Value())
		o.CertificateOnly = m.formToggles[0]
		m.busy = true
		return m, func() tea.Msg {
			if _, err := certmanage.Execute("export", o); err != nil {
				return serverActionResultMsg{err: err}
			}
			return serverActionResultMsg{notice: "已导出用户分发目录：" + o.Out}
		}

	case srvModalRotateServer:
		o := m.certBaseOptions("")
		o.Hosts = strings.TrimSpace(m.formInputs[0].Value())
		if d, err := strconv.Atoi(strings.TrimSpace(m.formInputs[1].Value())); err == nil && d > 0 {
			o.Days = d
		}
		return m, m.execCertAndReload("server", o, "服务端证书已轮换")
	}
	return m, nil
}

func (m *serverModel) toggleUserDisabledCmd(u config.AuthorizedClient) tea.Cmd {
	o := m.certBaseOptions(u.UserID)
	uCopy := u
	o.ExpectedUser = &uCopy
	o.Policy = &certmanage.UserPolicy{
		Disabled:    !u.Disabled,
		Permissions: u.Permissions,
		Bandwidth:   u.Bandwidth,
	}
	label := "已吊销用户 " + u.UserID
	if u.Disabled {
		label = "已重新启用用户 " + u.UserID
	}
	return m.execCertAndReload("policy", o, label)
}

func (m *serverModel) renewUserCertCmd(u config.AuthorizedClient) tea.Cmd {
	o := m.certBaseOptions(u.UserID)
	o.Renew = true
	o.Enable = u.Disabled
	return m.execCertAndReload("user", o, "已刷新用户证书 "+u.UserID)
}

func (m *serverModel) renewCACmd() tea.Cmd {
	o := m.certBaseOptions("")
	o.Renew = true
	o.Days = 365
	return m.execCertAndReload("ca", o, "CA 证书已续期 365 天")
}

func (m *serverModel) reloadServiceCmd() tea.Cmd {
	cfgPath := m.opts.ConfigPath
	root := m.endpointRoot
	m.busy = true
	return func() tea.Msg {
		ok, err := control.Reload(cfgPath, root)
		if err != nil {
			return serverActionResultMsg{err: err}
		}
		if !ok {
			return serverActionResultMsg{notice: "未检测到运行中的服务，启动后生效"}
		}
		return serverActionResultMsg{notice: "已触发运行中服务热加载证书与 ACL", refresh: true, poll: true}
	}
}

func (m *serverModel) startServiceCmd() tea.Cmd {
	if m.runtime != nil && m.runtime.State == "running" {
		m.statusToast = "服务已在运行中"
		return nil
	}
	cfgPath := m.opts.ConfigPath
	exeDir := m.opts.ExecutableDir
	m.busy = true
	m.statusToast = "正在后台启动 sftd 服务…"
	return func() tea.Msg {
		exe, err := os.Executable()
		if err != nil {
			exe = filepath.Join(exeDir, "sftd")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
		defer cancel()
		st, err := service.Start(ctx, exe, cfgPath)
		if err != nil {
			return serverActionResultMsg{err: err}
		}
		if st.AlreadyRunning {
			return serverActionResultMsg{notice: "服务已在运行中", poll: true}
		}
		return serverActionResultMsg{notice: fmt.Sprintf("服务已后台启动（PID %d）", st.PID), poll: true}
	}
}

func (m *serverModel) stopServiceCmd(immediate bool) tea.Cmd {
	cfgPath := m.opts.ConfigPath
	root := m.endpointRoot
	m.busy = true
	return func() tea.Msg {
		ok, err := control.Stop(cfgPath, root, immediate)
		if err != nil {
			return serverActionResultMsg{err: err}
		}
		if !ok {
			return serverActionResultMsg{notice: "未检测到运行中的服务"}
		}
		msg := "已请求服务优雅排空后停机"
		if immediate {
			msg = "已发送立即停服指令"
		}
		return serverActionResultMsg{notice: msg, poll: true}
	}
}
