package tui

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"sft-tui/internal/config"
	"sft-tui/internal/control"
	"sft-tui/internal/monitor"
	"sft-tui/internal/security"
)

type ServerOptions struct {
	ExecutableDir string
	ConfigPath    string
	LogPath       string
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
	srvModalRotateServer
	srvModalDeployWizard
	srvModalConfirm
)

type serverConfirmAction struct {
	title     string
	lines     []string
	onConfirm func() tea.Cmd
}

type certRow struct {
	Name        string
	Role        string
	Path        string
	State       string
	Expiry      string
	NotAfter    time.Time
	Fingerprint string
}

type serverConfigLoadedMsg struct {
	seq   uint64
	cfg   config.Server
	certs []certRow
	err   error
}

type serverRuntimePolledMsg struct {
	seq          uint64
	logEpoch     uint64
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
	trendStarted time.Time

	// 日志状态
	logQuery     monitor.LogQuery
	logEpoch     uint64
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
	cursors        [6]int
	modal          serverModalKind
	infoScroll     int
	detailTitle    string
	detailLines    []string
	statusToast    string
	toastLevel     toastLevel
	toastUntil     time.Time
	refreshSeq     uint64
	refreshCfgSeq  uint64
	refreshPollSeq uint64
	refreshCfgErr  error
	refreshPollErr error
	busy           bool
	busyOp         string
	busyStart      time.Time
	busySpin       int
	lastPoll       time.Time

	// 多字段表单输入与向导状态
	formState
	targetUser         *config.AuthorizedClient
	userDraft          *userFormDraft
	confirm            *serverConfirmAction
	deployAutoPrompted bool
	deploy             *deployWizardDraft
	settingsExpected   config.Server
	settingsDraft      *config.Server
}

type userFormDraft struct {
	name      string
	days      string
	enabled   bool
	allowUp   bool
	allowDown bool
	inheritBW bool
	upNum     string
	upUnit    int
	dnNum     string
	dnUnit    int
	burstNum  string
	burstUnit int
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
	qi := textinput.New()
	qi.Placeholder = "输入关键字筛选…"
	qi.CharLimit = 256
	qi.Width = 40
	return &serverModel{
		opts:         opts,
		th:           makeServerTheme(),
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
	} else if m.modal == srvModalNone {
		interval = remainingToastInterval(m.statusToast, m.toastUntil, interval)
	}
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return serverTickMsg(t)
	})
}

func (m *serverModel) setToast(msg string) {
	m.refreshCfgSeq = 0
	m.refreshPollSeq = 0
	m.toastLevel = inferToastLevel(msg)
	applyToast(&m.statusToast, &m.toastUntil, msg, toastDuration)
}

func (m *serverModel) setToastError(msg string) {
	m.refreshCfgSeq = 0
	m.refreshPollSeq = 0
	m.toastLevel = toastError
	applyToast(&m.statusToast, &m.toastUntil, msg, toastErrorDuration)
}

func (m *serverModel) setToastSticky(msg string) {
	m.refreshCfgSeq = 0
	m.refreshPollSeq = 0
	m.toastLevel = inferToastLevel(msg)
	applyToast(&m.statusToast, &m.toastUntil, msg, 0)
}

func (m *serverModel) clearToast() {
	m.setToast("")
}

func (m *serverModel) completeManualRefresh() {
	if m.refreshCfgSeq != 0 || m.refreshPollSeq != 0 || m.modal != srvModalNone {
		return
	}
	switch {
	case m.refreshCfgErr != nil:
		m.setToastError("刷新完成，配置加载失败：" + m.refreshCfgErr.Error())
	case m.refreshPollErr != nil:
		m.setToastError("配置已刷新；运行状态查询异常：" + m.refreshPollErr.Error())
	case m.runtime != nil:
		m.setToast("已刷新配置与运行状态")
	default:
		m.setToast("已刷新配置（服务未运行）")
	}
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
		m.setToast(fmt.Sprintf("正在执行%s（已耗时 %ds），请稍候完成…", op, sec))
	} else {
		m.setToast(fmt.Sprintf("正在执行%s，请稍候完成…", op))
	}
	return true
}

func (m *serverModel) loadConfigCmd() tea.Cmd {
	return m.loadConfigSeqCmd(0)
}

func (m *serverModel) loadConfigSeqCmd(seq uint64) tea.Cmd {
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
					row.NotAfter = ins.Certificate.NotAfter
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
		return serverConfigLoadedMsg{seq: seq, cfg: c, certs: certs, err: err}
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
	m.logEpoch++
	m.logCursor = 0
	m.logLost = false
	m.logEvents = nil
	m.logPaused = false
	return m.pollRuntimeCmd()
}

func (m *serverModel) pollRuntimeCmd() tea.Cmd {
	return m.pollRuntimeSeqCmd(0)
}

func (m *serverModel) pollRuntimeSeqCmd(seq uint64) tea.Cmd {
	cfgPath := m.opts.ConfigPath
	root := m.endpointRoot
	diskRoot := ""
	if m.cfgErr == nil {
		diskRoot = m.cfg.StorageRoot
	}
	wantLogs := m.page == 5 && !m.logPaused
	logEpoch := m.logEpoch
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
		out.seq = seq
		out.logEpoch = logEpoch
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
					wasLost := page.Lost
					q.After = 0
					prevEvents = nil
					page, logErr = control.QueryLogs(cfgPath, root, q)
					out.logLost = wasLost || page.Lost
				} else if logErr == nil {
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
		now := effectiveTickNow(time.Time(msg))
		if m.modal == srvModalNone {
			expireToast(&m.statusToast, &m.toastUntil, now)
		}
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
		if msg.seq != 0 && msg.seq == m.refreshCfgSeq {
			m.refreshCfgSeq = 0
			m.refreshCfgErr = msg.err
			m.completeManualRefresh()
		}
		return m, nil

	case serverRuntimePolledMsg:
		m.endpointRoot = msg.endpointRoot
		m.runtime = msg.snap
		m.runtimeState = msg.runtimeState
		if msg.snap != nil {
			if !m.trendStarted.IsZero() && !m.trendStarted.Equal(msg.snap.Started) {
				m.trend = nil
			}
			m.trendStarted = msg.snap.Started
			m.trend = append(m.trend, [2]float64{msg.snap.ReceiveRate, msg.snap.SendRate})
			if len(m.trend) > 120 {
				m.trend = m.trend[len(m.trend)-120:]
			}
		}
		if m.page == 5 && !m.logPaused && msg.logEpoch == m.logEpoch {
			m.logErr = msg.logErr
			m.logFallback = msg.logFallback
			m.logLost = m.logLost || msg.logLost
			if msg.logErr == "" && msg.logFallback == "" {
				m.logEvents = msg.logEvents
				m.logCursor = msg.logLast
				m.logStarted = msg.logStarted
			}
			m.cursors[5] = max(0, m.currentPageLen()-1)
		}
		m.clampCursors()
		if msg.seq != 0 && msg.seq == m.refreshPollSeq {
			m.refreshPollSeq = 0
			m.refreshPollErr = msg.err
			m.completeManualRefresh()
		}
		return m, nil

	case serverActionResultMsg:
		m.stopBusy()
		if msg.err != nil {
			m.setToastError("操作失败：" + msg.err.Error())
		} else if msg.notice != "" {
			m.setToast(msg.notice)
		} else if m.toastUntil.IsZero() {
			m.clearToast()
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
				m.setToastError("部署初始化失败：" + msg.err.Error())
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
		m.setToast("部署初始化已完成")
		return m, tea.Batch(m.loadConfigCmd(), m.pollRuntimeCmd())

	case deployExportDoneMsg:
		m.stopBusy()
		if msg.err != nil {
			if m.deploy != nil {
				m.deploy.finishMsg = "部署已保存；导出失败：" + msg.err.Error()
				if m.modal == srvModalDeployWizard && m.deploy.step == 5 {
					m.openDeployStep(5)
				}
			}
			m.setToastError("导出失败：" + msg.err.Error())
			return m, nil
		}
		if m.deploy != nil {
			m.deploy.finishMsg = "用户包已成功导出至：" + msg.outDir
			if m.modal == srvModalDeployWizard && m.deploy.step == 5 {
				m.openDeployStep(5)
			}
		}
		m.setToast("用户包已导出：" + msg.outDir)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}
