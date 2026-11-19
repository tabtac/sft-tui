package tui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/control"
	"sft-tui/internal/monitor"
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
	logSinceIdx  int
	userFilter   string
	txFilter     string
	queryMode    string // "", "log-user", "log-file"
	editingQuery bool
	queryInput   textinput.Model

	// 游标与详情弹窗
	cursors     [6]int
	modal       serverModalKind
	infoScroll  int
	detailTitle string
	detailLines []string
	statusToast string
	busy        bool
	busyOp      string
	busyStart   time.Time
	busySpin    int
	lastPoll    time.Time

	// 多字段表单输入与向导状态
	formState
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
	m.lastPoll = time.Now()
	return tea.Batch(
		m.loadConfigCmd(),
		m.pollRuntimeCmd(),
		m.tickCmd(),
	)
}

func (m *serverModel) tickCmd() tea.Cmd {
	interval := time.Second
	if m.busy {
		interval = 150 * time.Millisecond
	}
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return serverTickMsg(t)
	})
}

func (m *serverModel) startBusy(op string) {
	m.busy = true
	m.busyOp = op
	m.busyStart = time.Now()
}

func (m *serverModel) stopBusy() {
	m.busy = false
	m.busyOp = ""
	m.busyStart = time.Time{}
}

func (m *serverModel) rejectIfBusy() bool {
	if !m.busy {
		return false
	}
	op := m.busyOp
	if op == "" {
		op = "后台操作"
	}
	if sec := int(time.Since(m.busyStart).Seconds()); sec >= 1 {
		m.statusToast = fmt.Sprintf("正在执行%s（已耗时 %ds），请稍候完成…", op, sec)
	} else {
		m.statusToast = fmt.Sprintf("正在执行%s，请稍候完成…", op)
	}
	return true
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

func (m *serverModel) effectiveLogQuery() monitor.LogQuery {
	q := m.logQuery
	q.Level = m.logMinLevel
	switch m.logSinceIdx {
	case 1:
		q.Since = time.Now().Add(-5 * time.Minute)
	case 2:
		q.Since = time.Now().Add(-1 * time.Hour)
	case 3:
		q.Since = time.Now().Add(-24 * time.Hour)
	default:
		q.Since = time.Time{}
	}
	return q
}

func (m *serverModel) resetLogsAndPollCmd() tea.Cmd {
	m.logCursor = 0
	m.logEvents = nil
	m.logPaused = false
	return m.pollRuntimeCmd()
}

func (m *serverModel) pollRuntimeCmd() tea.Cmd {
	cfgPath := m.opts.ConfigPath
	root := m.endpointRoot
	diskRoot := ""
	if m.cfgErr == nil {
		diskRoot = m.cfg.StorageRoot
	}
	wantLogs := m.page == 5 && !m.logPaused
	q := m.effectiveLogQuery()
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
	const maxTail = 64 * 1024
	info, err := os.Lstat(path)
	if err != nil {
		return "无法读取日志文件：" + err.Error()
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "无法读取日志文件：仅支持读取常规日志文件"
	}
	f, err := os.Open(path)
	if err != nil {
		return "无法读取日志文件：" + err.Error()
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "无法读取日志文件：" + err.Error()
	}
	if !st.Mode().IsRegular() || !os.SameFile(info, st) {
		return "无法读取日志文件：仅支持读取常规日志文件"
	}
	size := st.Size()
	offset := int64(0)
	if size > maxTail {
		offset = size - maxTail
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return "无法读取日志文件：" + err.Error()
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTail))
	if err != nil {
		return "无法读取日志文件：" + err.Error()
	}
	if offset > 0 {
		// 跳过截断首行，避免半行文本或劈裂的多字节 UTF-8 字符
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			data = data[idx+1:]
		}
	}
	return string(bytes.ToValidUTF8(data, []byte("?")))
}

func (m *serverModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case serverTickMsg:
		m.busySpin = (m.busySpin + 1) % len(spinnerFrames)
		if !m.busy || time.Since(m.lastPoll) >= time.Second {
			m.lastPoll = time.Now()
			return m, tea.Batch(m.tickCmd(), m.pollRuntimeCmd())
		}
		return m, m.tickCmd()

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
			m.cursors[5] = max(0, m.currentPageLen()-1)
		}
		m.clampCursors()
		return m, nil

	case serverActionResultMsg:
		m.stopBusy()
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
		m.stopBusy()
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
		if m.opts.ConfigPath == "" && msg.d.Config != "" {
			m.opts.ConfigPath = msg.d.Config
		}
		m.statusToast = "部署初始化已完成"
		return m, tea.Batch(m.loadConfigCmd(), m.pollRuntimeCmd())

	case deployExportDoneMsg:
		m.stopBusy()
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
			mode := m.queryMode
			m.editingQuery = false
			m.queryMode = ""
			m.queryInput.Blur()
			m.queryInput.SetValue("")
			switch mode {
			case "log-user":
				m.logQuery.User = ""
				return m, m.resetLogsAndPollCmd()
			case "log-file":
				m.logQuery.File = ""
				return m, m.resetLogsAndPollCmd()
			default:
				m.updatePageFilter("")
				if m.page == 5 {
					return m, m.resetLogsAndPollCmd()
				}
			}
			return m, nil
		case "enter":
			mode := m.queryMode
			val := strings.TrimSpace(m.queryInput.Value())
			m.editingQuery = false
			m.queryMode = ""
			m.queryInput.Blur()
			switch mode {
			case "log-user":
				m.logQuery.User = val
				return m, m.resetLogsAndPollCmd()
			case "log-file":
				m.logQuery.File = val
				return m, m.resetLogsAndPollCmd()
			default:
				if m.page == 5 {
					m.logQuery.Text = val
					return m, m.resetLogsAndPollCmd()
				}
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.queryInput, cmd = m.queryInput.Update(msg)
		if m.queryMode == "" {
			m.updatePageFilter(m.queryInput.Value())
		}
		return m, cmd
	}

	if m.modal != srvModalNone {
		return m.handleModalKey(msg)
	}

	switch k {
	case "q", "f10":
		if m.rejectIfBusy() {
			return m, nil
		}
		return m, tea.Quit
	case "?":
		m.infoScroll = 0
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
		m.queryMode = ""
		switch m.page {
		case 1:
			m.queryInput.SetValue(m.userFilter)
		case 2:
			m.queryInput.SetValue(m.txFilter)
		case 5:
			m.queryInput.SetValue(m.logQuery.Text)
		default:
			m.queryInput.SetValue("")
		}
		m.queryInput.Focus()
		m.queryInput.CursorEnd()
		return m, textinput.Blink
	case "r":
		m.statusToast = "正在刷新配置与运行状态…"
		return m, tea.Batch(m.loadConfigCmd(), m.pollRuntimeCmd())
	case "R":
		return m, m.reloadServiceCmd()
	case "s":
		return m, m.startServiceCmd()
	case "x":
		if m.rejectIfBusy() {
			return m, nil
		}
		if m.runtime != nil {
			m.modal = srvModalStop
		} else {
			m.statusToast = "本机服务当前未运行"
		}
		return m, nil
	case "up", "k":
		if m.page == 5 {
			m.logPaused = true
		}
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
		if m.page == 5 {
			m.logPaused = true
		}
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
					fmt.Sprintf("授权用户：%s", sanitizeInline(p.User)),
					fmt.Sprintf("远端地址：%s", sanitizeInline(p.Remote)),
					fmt.Sprintf("建立时间：%s", p.Since.Local().Format("2006-01-02 15:04:05")),
				}
				m.infoScroll = 0
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
				u := users[m.cursors[1]]
				if u.ExternalKey {
					m.openCSRModalForRenew(u)
					return m, textinput.Blink
				}
				return m, m.renewUserCertCmd(u)
			}
		case "e":
			if len(users) > 0 && m.cursors[1] < len(users) {
				m.openExportUserModal(users[m.cursors[1]])
				return m, textinput.Blink
			}
		case "g":
			if len(users) > 0 && m.cursors[1] < len(users) {
				u := users[m.cursors[1]]
				m.logQuery = monitor.LogQuery{User: u.UserID}
				m.logMinLevel = ""
				m.logSinceIdx = 0
				m.logCursor = 0
				m.logEvents = nil
				m.logPaused = false
				m.statusToast = "已按用户过滤并跳转至相关日志：" + u.UserID
				return m.switchPage(5)
			}
		case "enter":
			if len(users) > 0 && m.cursors[1] < len(users) {
				m.showUserDetailModal(users[m.cursors[1]])
			}
		}
	case 2: // 实时传输
		tasks := m.filteredTasks()
		switch k {
		case "g":
			if len(tasks) > 0 && m.cursors[2] < len(tasks) {
				t := tasks[m.cursors[2]]
				m.logQuery = monitor.LogQuery{User: t.User, File: t.File}
				m.logMinLevel = ""
				m.logSinceIdx = 0
				m.logCursor = 0
				m.logEvents = nil
				m.logPaused = false
				m.statusToast = fmt.Sprintf("已跳转至 %s / %s 的相关日志", t.User, t.File)
				return m.switchPage(5)
			}
		case "enter":
			if m.cursors[2] < len(tasks) {
				t := tasks[m.cursors[2]]
				m.detailTitle = "服务端传输任务详情"
				m.detailLines = []string{
					fmt.Sprintf("任务 / 连接：%d  (连接 #%d · 流 #%d)", t.ID, t.Connection, t.Stream),
					fmt.Sprintf("用户与方向：%s · %s", sanitizeInline(t.User), sanitizeInline(t.Direction)),
					fmt.Sprintf("文件名：    %s", sanitizeInline(t.File)),
					fmt.Sprintf("进度与速率：%d / %d B · %.1f B/s", t.Offset, t.Size, t.Rate),
					fmt.Sprintf("当前状态：  %s", sanitizeInline(t.State)),
				}
				if t.Error != "" {
					m.detailLines = append(m.detailLines, "", "错误信息：", "  "+sanitizeInline(t.Error))
				}
				m.infoScroll = 0
				m.modal = srvModalDetail
			}
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
				m.detailTitle = "证书详情 · " + sanitizeInline(c.Name)
				m.detailLines = []string{
					"角色：    " + sanitizeInline(c.Role),
					"路径：    " + sanitizeInline(c.Path),
					"状态：    " + sanitizeInline(c.State),
					"到期/备注：" + sanitizeInline(c.Expiry),
					"SHA-256： " + sanitizeInline(c.Fingerprint),
				}
				m.infoScroll = 0
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
			return m, m.resetLogsAndPollCmd()
		case "U":
			m.editingQuery = true
			m.queryMode = "log-user"
			m.queryInput.SetValue(m.logQuery.User)
			m.queryInput.Focus()
			m.queryInput.CursorEnd()
			return m, textinput.Blink
		case "F":
			m.editingQuery = true
			m.queryMode = "log-file"
			m.queryInput.SetValue(m.logQuery.File)
			m.queryInput.Focus()
			m.queryInput.CursorEnd()
			return m, textinput.Blink
		case "T":
			m.logSinceIdx = (m.logSinceIdx + 1) % 4
			return m, m.resetLogsAndPollCmd()
		case "C":
			m.logQuery = monitor.LogQuery{}
			m.logMinLevel = ""
			m.logSinceIdx = 0
			m.statusToast = "已清空全部日志筛选条件"
			return m, m.resetLogsAndPollCmd()
		case "enter":
			if m.logFallback == "" && m.cursors[5] < len(m.logEvents) {
				e := m.logEvents[m.cursors[5]]
				m.logPaused = true
				m.detailTitle = fmt.Sprintf("日志事件 #%d [%s]", e.Sequence, sanitizeInline(e.Level))
				m.detailLines = []string{
					"时间：  " + e.Time.Local().Format("2006-01-02 15:04:05"),
					"消息：  " + sanitizeInline(e.Message),
					"用户：  " + sanitizeInline(e.User),
					"文件：  " + sanitizeInline(e.File),
					"详情：  " + sanitizeInline(e.Detail),
				}
				m.infoScroll = 0
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
			return len(m.filteredLogFallbackLines())
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
	m.statusToast = ""
	m.targetUser = nil
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
		{label: "取消", onClick: func() (tea.Model, tea.Cmd) { m.modal = srvModalNone; m.statusToast = ""; return m, nil }},
		{label: "创建用户 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) { return m.submitModalForm() }},
	}
	m.finalizeFormFocus(true)
}

func (m *serverModel) openCSRModal(user string) {
	m.statusToast = ""
	m.targetUser = nil
	m.initForm([]string{"用户名", "CSR 文件路径", "有效期(天)"}, []string{user, "", "30"}, nil, nil)
	m.modal = srvModalCSR
}

func (m *serverModel) openCSRModalForRenew(u config.AuthorizedClient) {
	m.openCSRModal(u.UserID)
	uCopy := u
	m.targetUser = &uCopy
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
	m.statusToast = ""
	m.modal = srvModalEditPolicy
	m.addToggleRow("启用该用户", !u.Disabled, nil)
	m.addToggleRow("允许上传", hasUp, nil)
	m.addToggleRow("允许下载", hasDown, nil)
	m.addToggleRow("继承默认带宽(关闭后使用下方自定义速率)", inherit, nil)
	m.addSizeUnitRow("上传速率", bw.UploadBytesPerSecond, true)
	m.addSizeUnitRow("下载速率", bw.DownloadBytesPerSecond, true)
	m.addSizeUnitRow("突发配额", bw.BurstBytes, false)
	m.formButtons = []formButton{
		{label: "取消", onClick: func() (tea.Model, tea.Cmd) { m.modal = srvModalNone; m.targetUser = nil; m.statusToast = ""; return m, nil }},
		{label: "保存策略 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) { return m.submitModalForm() }},
	}
	m.finalizeFormFocus(false)
}

func (m *serverModel) openExportUserModal(u config.AuthorizedClient) {
	uCopy := u
	m.targetUser = &uCopy
	m.statusToast = ""
	outDir := filepath.Join("bundles", u.UserID)
	m.initForm([]string{"导出目标目录"}, []string{outDir}, []string{"仅导出公开证书(-public-only，不含私钥)"}, []bool{u.ExternalKey})
	m.modal = srvModalExportUser
}

func (m *serverModel) openRotateServerModal() {
	m.statusToast = ""
	m.targetUser = nil
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
	footerH := lipgloss.Height(m.renderFooter())
	bodyH := max(6, m.height-2-footerH)
	if msg.Y < bodyY || msg.Y >= bodyY+bodyH {
		return m, nil
	}
	total := m.currentPageLen()
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if m.page == 5 {
			m.logPaused = true
		}
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
	m.detailTitle = "用户详情 · " + sanitizeInline(u.UserID)
	m.detailLines = []string{
		"授权状态：" + status,
		"操作权限：" + sanitizeInline(strings.Join(u.Permissions, ", ")),
		"带宽策略：" + bwText,
		"证书路径：" + sanitizeInline(u.Certificate),
		fmt.Sprintf("证书状态：%s · 指纹 %s", sanitizeInline(ins.State), sanitizeInline(ins.Fingerprint)),
	}
	m.infoScroll = 0
	m.modal = srvModalDetail
}

func (m *serverModel) scrollServerInfoModal(k string) {
	title, lines, footer := m.infoModalData()
	_, maxScroll := renderModalScrollBox(m.width, m.height, m.th, title, lines, footer, m.infoScroll)
	next, ok := modalScrollStep(k, m.infoScroll, maxScroll, modalScrollBoxPageRows(m.height, footer))
	if ok {
		m.infoScroll = next
	}
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
		if k == "enter" || k == "q" || (m.modal == srvModalHelp && k == "?") {
			m.modal = srvModalNone
			return m, nil
		}
		m.scrollServerInfoModal(k)
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
	if m.rejectIfBusy() {
		return nil
	}
	opLabel := "执行证书与权限变更"
	switch action {
	case "user":
		if o.Renew {
			opLabel = "续期用户证书"
		} else {
			opLabel = "签发用户证书"
		}
	case "policy":
		opLabel = "保存用户策略"
	case "ca":
		opLabel = "续期 CA 证书"
	case "server":
		opLabel = "轮换服务端证书"
	}
	m.startBusy(opLabel)
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

func parseStrictCertDays(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("有效天数不能为空，请输入 1–3650 之间的整数")
	}
	days, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("有效天数 %q 无效：请输入 1–3650 之间的整数", s)
	}
	if days < 1 || days > 3650 {
		return 0, fmt.Errorf("有效天数 %d 超出范围，须在 1–3650 天之间", days)
	}
	return days, nil
}

func (m *serverModel) validateBandwidthInputs(upIdx, dnIdx, burstIdx int) (*config.Bandwidth, int, error) {
	fields := []struct {
		idx   int
		label string
	}{
		{upIdx, "上传速率"},
		{dnIdx, "下载速率"},
		{burstIdx, "突发配额"},
	}
	vals := make([]uint64, len(fields))
	for i, f := range fields {
		v, err := m.parseSizeInput(f.idx, f.label)
		if err != nil {
			return nil, f.idx, err
		}
		vals[i] = v
	}
	const maxSafeBandwidth = uint64(1) << 53
	if vals[0] == 0 || vals[0] > maxSafeBandwidth {
		return nil, upIdx, fmt.Errorf("上传速率必须大于 0 且不超过 %d B/s", maxSafeBandwidth)
	}
	if vals[1] == 0 || vals[1] > maxSafeBandwidth {
		return nil, dnIdx, fmt.Errorf("下载速率必须大于 0 且不超过 %d B/s", maxSafeBandwidth)
	}
	chunk := uint64(config.DefaultServer().Limits.ChunkSize)
	if m.cfgErr == nil && m.cfg.Limits.ChunkSize > 0 {
		chunk = uint64(m.cfg.Limits.ChunkSize)
	}
	if vals[2] < chunk || vals[2] > 1<<30 {
		return nil, burstIdx, fmt.Errorf("突发配额须在分块大小 (%d B) 与 1 GiB 之间", chunk)
	}
	return &config.Bandwidth{
		UploadBytesPerSecond:   vals[0],
		DownloadBytesPerSecond: vals[1],
		BurstBytes:             vals[2],
	}, -1, nil
}

func (m *serverModel) findAuthorizedClient(name string) (config.AuthorizedClient, bool) {
	for _, u := range m.cfg.AuthorizedClients {
		match := u.UserID == name
		if runtime.GOOS == "windows" {
			match = strings.EqualFold(u.UserID, name)
		}
		if match {
			return u, true
		}
	}
	return config.AuthorizedClient{}, false
}

func isReservedIdentityName(name string) bool {
	return strings.EqualFold(name, "ca") || strings.EqualFold(name, "server")
}

func (m *serverModel) submitModalForm() (tea.Model, tea.Cmd) {
	switch m.modal {
	case srvModalNewUser:
		name := strings.TrimSpace(m.formInputs[0].Value())
		if !config.ValidUserPath(name) || isReservedIdentityName(name) {
			m.statusToast = "用户名无效（须为字母/数字/._-，且不能为系统保留名）"
			return m, m.focusInputRow(0)
		}
		if existing, ok := m.findAuthorizedClient(name); ok {
			m.statusToast = fmt.Sprintf("用户 %q 已存在", existing.UserID)
			return m, m.focusInputRow(0)
		}
		days, err := parseStrictCertDays(m.formInputs[1].Value())
		if err != nil {
			m.statusToast = err.Error()
			return m, m.focusInputRow(1)
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
			var badIdx int
			bw, badIdx, err = m.validateBandwidthInputs(2, 3, 4)
			if err != nil {
				m.statusToast = err.Error()
				return m, m.focusInputRow(badIdx)
			}
		}
		m.statusToast = ""
		m.modal = srvModalNone
		o := m.certBaseOptions(name)
		o.Days = days
		o.Policy = &certmanage.UserPolicy{Permissions: perms, Bandwidth: bw}
		return m, m.execCertAndReload("user", o, "已新建用户 "+name)

	case srvModalCSR:
		name := strings.TrimSpace(m.formInputs[0].Value())
		if !config.ValidUserPath(name) || isReservedIdentityName(name) {
			m.statusToast = "用户名无效（须为字母/数字/._-，且不能为系统保留名）"
			return m, m.focusInputRow(0)
		}
		renewTarget := m.targetUser != nil && (m.targetUser.UserID == name || (runtime.GOOS == "windows" && strings.EqualFold(m.targetUser.UserID, name)))
		if !renewTarget {
			if existing, ok := m.findAuthorizedClient(name); ok {
				m.statusToast = fmt.Sprintf("用户 %q 已存在（如需续期请从用户列表按 [w]）", existing.UserID)
				return m, m.focusInputRow(0)
			}
		}
		csr := strings.TrimSpace(m.formInputs[1].Value())
		if csr == "" {
			m.statusToast = "CSR 文件路径不能为空"
			return m, m.focusInputRow(1)
		}
		days, err := parseStrictCertDays(m.formInputs[2].Value())
		if err != nil {
			m.statusToast = err.Error()
			return m, m.focusInputRow(2)
		}
		if _, err := security.ReadCSR(csr); err != nil {
			m.statusToast = err.Error()
			return m, m.focusInputRow(1)
		}
		var expected *config.AuthorizedClient
		if renewTarget {
			uCopy := *m.targetUser
			expected = &uCopy
			name = uCopy.UserID
		}
		m.statusToast = ""
		m.modal = srvModalNone
		m.targetUser = nil
		o := m.certBaseOptions(name)
		o.CSR = csr
		o.Days = days
		o.Renew = renewTarget
		o.ExpectedUser = expected
		notice := "CSR 已签发给 " + name
		if renewTarget {
			notice = "已使用外部 CSR 续期用户 " + name
		}
		return m, m.execCertAndReload("user", o, notice)

	case srvModalEditPolicy:
		if m.targetUser == nil {
			m.modal = srvModalNone
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
			var badIdx int
			var err error
			bw, badIdx, err = m.validateBandwidthInputs(0, 1, 2)
			if err != nil {
				m.statusToast = err.Error()
				return m, m.focusInputRow(badIdx)
			}
		}
		m.statusToast = ""
		m.modal = srvModalNone
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
			m.modal = srvModalNone
			return m, nil
		}
		if m.rejectIfBusy() {
			return m, nil
		}
		out := strings.TrimSpace(m.formInputs[0].Value())
		if out == "" {
			m.statusToast = "导出目标目录不能为空"
			return m, m.focusInputRow(0)
		}
		u := *m.targetUser
		m.statusToast = ""
		m.modal = srvModalNone
		m.targetUser = nil
		o := m.certBaseOptions(u.UserID)
		o.Out = out
		o.CertificateOnly = m.formToggles[0]
		m.startBusy("导出用户分发包")
		return m, func() tea.Msg {
			if _, err := certmanage.Execute("export", o); err != nil {
				return serverActionResultMsg{err: err}
			}
			return serverActionResultMsg{notice: "已导出用户分发目录：" + o.Out}
		}

	case srvModalRotateServer:
		hosts := strings.TrimSpace(m.formInputs[0].Value())
		if hosts != "" {
			if _, _, err := security.ParseServerNames(hosts); err != nil {
				m.statusToast = err.Error()
				return m, m.focusInputRow(0)
			}
		}
		days, err := parseStrictCertDays(m.formInputs[1].Value())
		if err != nil {
			m.statusToast = err.Error()
			return m, m.focusInputRow(1)
		}
		m.statusToast = ""
		m.modal = srvModalNone
		o := m.certBaseOptions("")
		o.Hosts = hosts
		o.Days = days
		return m, m.execCertAndReload("server", o, "服务端证书已轮换")
	}
	m.modal = srvModalNone
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
	uCopy := u
	o.ExpectedUser = &uCopy
	o.Renew = true
	notice := "已刷新用户证书 " + u.UserID
	if u.Disabled {
		notice += "（保留原禁用状态）"
	}
	return m.execCertAndReload("user", o, notice)
}

func (m *serverModel) renewCACmd() tea.Cmd {
	o := m.certBaseOptions("")
	o.Renew = true
	o.Days = 365
	return m.execCertAndReload("ca", o, "CA 证书已续期 365 天")
}

func (m *serverModel) reloadServiceCmd() tea.Cmd {
	if m.rejectIfBusy() {
		return nil
	}
	cfgPath := m.opts.ConfigPath
	root := m.endpointRoot
	m.startBusy("热加载服务配置")
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
	if m.rejectIfBusy() {
		return nil
	}
	if m.runtime != nil && m.runtime.State == "running" {
		m.statusToast = "服务已在运行中"
		return nil
	}
	cfgPath := m.opts.ConfigPath
	exeDir := m.opts.ExecutableDir
	m.startBusy("后台启动 sftd 服务")
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
	if m.rejectIfBusy() {
		return nil
	}
	cfgPath := m.opts.ConfigPath
	root := m.endpointRoot
	m.startBusy("停止 sftd 服务")
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
