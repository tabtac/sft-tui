package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"sft-tui/internal/config"
	"sft-tui/internal/identity"
	"sft-tui/internal/progress"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
	"sft-tui/internal/security"
	"sft-tui/internal/transfer"
)

type Options struct {
	ExecutableDir string
	Directory     string
	ConfigPath    string
	BundleDir     string
	PrefsPath     string
}

type focusArea int

const (
	areaLocal focusArea = iota
	areaRemote
	areaQueue
)

type sortKind int

const (
	sortNameAsc sortKind = iota
	sortNameDesc
	sortSizeDesc
	sortSizeAsc
)

type localEntry struct {
	name, path   string
	size         int64
	dir, regular bool
}

type identityChoice struct {
	profile  identity.Profile
	imported bool
}

// 异步消息定义
type tickMsg time.Time
type localLoadedMsg struct {
	epoch   int
	dir     string
	entries []localEntry
	err     error
}
type remoteLoadedMsg struct {
	epoch   int
	entries []protocol.FileEntry
	err     error
}
type identitiesScannedMsg struct {
	choices []identityChoice
	err     error
}
type identityActivatedMsg struct {
	cfg      config.Client
	label    string
	idKey    string
	activeID string
	manager  *queue.Manager
	runner   *transfer.QueueRunner
	err      error
}
type batchOpResultMsg struct {
	notice string
	err    error
	quit   bool
}

type model struct {
	opts      Options
	prefsPath string
	th        theme

	width, height int
	focus         focusArea
	historyView   bool
	modal         modalKind

	// 文本输入框
	searchInput   textinput.Model
	pathInput     textinput.Model
	idInput       textinput.Model
	renameInput   textinput.Model
	editingSearch bool
	editingPath   bool
	idInputFocus  bool

	// 身份与传输引擎
	cfg           *config.Client
	identityLabel string
	idKey         string
	activeID      string
	manager       *queue.Manager
	runner        *transfer.QueueRunner
	connStatus    string

	// 文件列表
	localDir       string
	localEpoch     int
	remoteEpoch    int
	loadingLocal   bool
	loadingRemote  bool
	remoteErr      string
	localEntries   []localEntry
	visibleLocal   []localEntry
	remoteEntries  []protocol.FileEntry
	visibleRemote  []protocol.FileEntry
	localSelected  map[string]bool
	remoteSelected map[string]bool
	localCursor    int
	remoteCursor   int
	sortMode       sortKind
	query          string

	// 队列快照视图（无锁）
	activeTasks   []queue.Task
	historyTasks  []queue.Task
	queueCursor   int
	completedSeen map[string]bool
	statusToast   string
	quitting      bool

	// 弹窗辅助数据
	detailTitle     string
	detailLines     []string
	idChoices       []identityChoice
	idCursor        int
	pendingBatchDir protocol.Direction
	pendingSpecs    []queue.Spec
	conflictNames   []string
	renameTask      *queue.Task
}

// Run 启动 Bubble Tea AltScreen 客户端界面并阻塞直至优雅退出。
func Run(opts Options) error {
	m := newModel(opts)
	defer m.cleanup()
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

func newModel(opts Options) *model {
	if opts.Directory == "" {
		if wd, err := os.Getwd(); err == nil {
			opts.Directory = wd
		} else {
			opts.Directory = "."
		}
	}
	if opts.ExecutableDir == "" {
		if exe, err := os.Executable(); err == nil {
			opts.ExecutableDir = filepath.Dir(exe)
		} else {
			opts.ExecutableDir = opts.Directory
		}
	}
	prefsPath := resolvePrefsPath(opts.PrefsPath, opts.ExecutableDir, ".sft-prefs.json")

	makeInput := func(placeholder string) textinput.Model {
		in := textinput.New()
		in.Placeholder = placeholder
		in.CharLimit = 512
		in.Width = 42
		return in
	}

	m := &model{
		opts:           opts,
		prefsPath:      prefsPath,
		th:             makeTheme(loadThemePref(prefsPath)),
		focus:          areaLocal,
		localDir:       opts.Directory,
		connStatus:     "未连接",
		identityLabel:  "未选择",
		localSelected:  make(map[string]bool),
		remoteSelected: make(map[string]bool),
		completedSeen:  make(map[string]bool),
		searchInput:    makeInput("输入关键字筛选文件名…"),
		pathInput:      makeInput("输入本地绝对或相对路径…"),
		idInput:        makeInput("输入 client*.json 或导出目录路径…"),
		renameInput:    makeInput("输入新的目标文件名…"),
	}
	m.pathInput.SetValue(opts.Directory)
	return m
}

func (m *model) cleanup() {
	if m.runner != nil {
		_ = m.runner.Close()
		m.runner = nil
	}
	if m.manager != nil {
		_ = m.manager.Close()
		m.manager = nil
	}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(
		m.loadLocalCmd(m.localDir),
		m.initIdentityCmd(),
		m.tickCmd(),
	)
}

func (m *model) tickCmd() tea.Cmd {
	interval := 200 * time.Millisecond
	if m.manager != nil && m.manager.HasRunning() {
		interval = 50 * time.Millisecond
	}
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *model) loadLocalCmd(dir string) tea.Cmd {
	m.localEpoch++
	epoch := m.localEpoch
	m.loadingLocal = true
	return func() tea.Msg {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return localLoadedMsg{epoch: epoch, err: err}
		}
		entries, err := os.ReadDir(abs)
		if err != nil {
			return localLoadedMsg{epoch: epoch, err: err}
		}
		var list []localEntry
		if parent := filepath.Dir(abs); parent != abs {
			list = append(list, localEntry{name: "../", path: parent, dir: true})
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			list = append(list, localEntry{
				name:    e.Name(),
				path:    filepath.Join(abs, e.Name()),
				size:    info.Size(),
				dir:     e.IsDir(),
				regular: info.Mode().IsRegular(),
			})
		}
		return localLoadedMsg{epoch: epoch, dir: abs, entries: list}
	}
}

func (m *model) loadRemoteCmd() tea.Cmd {
	if m.runner == nil {
		return nil
	}
	m.remoteEpoch++
	epoch := m.remoteEpoch
	m.loadingRemote = true
	runner := m.runner
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		entries, err := runner.List(ctx)
		return remoteLoadedMsg{epoch: epoch, entries: entries, err: err}
	}
}

func (m *model) initIdentityCmd() tea.Cmd {
	opts := m.opts
	return func() tea.Msg {
		wd, _ := os.Getwd()
		cfg, err := identity.Resolve(opts.ExecutableDir, wd, opts.BundleDir, opts.ConfigPath, false, nil)
		if err != nil {
			return identityActivatedMsg{err: err}
		}
		return openIdentityStack(cfg, "")
	}
}

func openIdentityStack(cfg config.Client, customLabel string) identityActivatedMsg {
	idKey, err := security.ClientIdentityKey(cfg)
	if err != nil {
		return identityActivatedMsg{err: err}
	}
	mgr, err := queue.Open(queue.Config{
		StateDir:     cfg.StateDir,
		Identity:     idKey,
		MaxFileBytes: cfg.MaxFileBytes,
		MaxStreams:   cfg.Limits.MaxStreams,
	})
	if err != nil {
		return identityActivatedMsg{err: err}
	}
	runner := transfer.NewQueueRunner(cfg, mgr)
	mgr.SetRunner(runner.Run)

	label := customLabel
	if label == "" {
		user := cfg.ServerName
		if cert, err := security.ReadCertificate(cfg.TLS.Certificate); err == nil && cert.Subject.CommonName != "" {
			user = cert.Subject.CommonName
		}
		label = user + "@" + cfg.ServerAddress
	}
	return identityActivatedMsg{
		cfg:     cfg,
		label:   label,
		idKey:   idKey,
		manager: mgr,
		runner:  runner,
	}
}

func (m *model) scanIdentitiesCmd() tea.Cmd {
	exeDir := m.opts.ExecutableDir
	return func() tea.Msg {
		wd, _ := os.Getwd()
		saved, _ := identity.Profiles(exeDir)
		discovered, err := identity.Discover(exeDir, wd, "")
		seenPath := make(map[string]bool)
		var out []identityChoice
		for _, p := range saved {
			seenPath[p.Path] = true
			out = append(out, identityChoice{profile: p, imported: true})
		}
		for _, path := range discovered {
			if seenPath[path] {
				continue
			}
			if prof, e := identity.Preview(path, wd); e == nil {
				out = append(out, identityChoice{profile: prof, imported: false})
			}
		}
		return identitiesScannedMsg{choices: out, err: err}
	}
}

func (m *model) activateChoiceCmd(ch identityChoice, customPath string) tea.Cmd {
	oldMgr := m.manager
	oldRunner := m.runner
	exeDir := m.opts.ExecutableDir
	return func() tea.Msg {
		wd, _ := os.Getwd()
		var (
			prof   identity.Profile
			useSel bool
			err    error
		)
		switch {
		case customPath != "":
			info, statErr := os.Stat(customPath)
			if statErr != nil {
				return identityActivatedMsg{err: statErr}
			}
			target := customPath
			if info.IsDir() {
				candidates, discErr := identity.Discover(exeDir, wd, customPath)
				if discErr != nil {
					return identityActivatedMsg{err: discErr}
				}
				if len(candidates) == 0 {
					return identityActivatedMsg{err: fmt.Errorf("目录中未找到有效的客户端配置：%s", customPath)}
				}
				if len(candidates) > 1 {
					return identityActivatedMsg{err: fmt.Errorf("目录中存在多个身份配置（%d 个），请直接指定配置文件路径", len(candidates))}
				}
				target = candidates[0]
			}
			prof, err = identity.PrepareImport(exeDir, wd, target)
			if err != nil {
				return identityActivatedMsg{err: err}
			}
		case ch.imported:
			prof = ch.profile
			if _, err = identity.Preview(prof.Path, wd); err != nil {
				return identityActivatedMsg{err: err}
			}
			useSel = true
		default:
			prof, err = identity.PrepareImport(exeDir, wd, ch.profile.Path)
			if err != nil {
				return identityActivatedMsg{err: err}
			}
		}
		if _, err = security.ClientTLS(prof.Config); err != nil {
			return identityActivatedMsg{err: err}
		}
		if oldMgr != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = oldMgr.Quiesce(ctx)
			cancel()
			if err != nil {
				return identityActivatedMsg{err: fmt.Errorf("切换身份前暂停当前任务失败：%w", err)}
			}
			_ = oldRunner.Close()
			_ = oldMgr.Close()
		}
		var cfg config.Client
		if useSel {
			cfg, err = identity.Select(exeDir, prof.ID, prof.Label)
		} else {
			cfg, err = identity.SelectPrepared(exeDir, prof, prof.Label)
		}
		if err != nil {
			return identityActivatedMsg{err: err}
		}
		res := openIdentityStack(cfg, prof.Label)
		res.activeID = prof.ID
		return res
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		if m.refreshQueueSnapshots() {
			return m, tea.Batch(m.tickCmd(), m.loadLocalCmd(m.localDir), m.loadRemoteCmd())
		}
		return m, m.tickCmd()

	case localLoadedMsg:
		if msg.epoch != m.localEpoch {
			return m, nil
		}
		m.loadingLocal = false
		if msg.err != nil {
			m.statusToast = "无法读取目录：" + msg.err.Error()
			return m, nil
		}
		m.localDir = msg.dir
		if !m.editingPath {
			m.pathInput.SetValue(msg.dir)
		}
		m.localEntries = msg.entries
		m.rebuildLocalVisible()
		return m, nil

	case remoteLoadedMsg:
		if msg.epoch != m.remoteEpoch {
			return m, nil
		}
		m.loadingRemote = false
		if msg.err != nil {
			m.remoteErr = msg.err.Error()
			return m, nil
		}
		m.remoteErr = ""
		m.remoteEntries = msg.entries
		m.rebuildRemoteVisible()
		return m, nil

	case identitiesScannedMsg:
		m.idChoices = msg.choices
		if m.idCursor >= len(m.idChoices) {
			m.idCursor = max(0, len(m.idChoices)-1)
		}
		if msg.err != nil && len(msg.choices) == 0 {
			m.statusToast = msg.err.Error()
		}
		return m, nil

	case identityActivatedMsg:
		if msg.err != nil {
			m.statusToast = msg.err.Error()
			if m.cfg == nil {
				m.modal = modalIdentity
				m.connStatus = "需要选择或导入身份"
				return m, m.scanIdentitiesCmd()
			}
			return m, nil
		}
		// 释放旧实例仅在替换时安全（通常 activateChoiceCmd 已关闭旧实例；启动期无旧实例）
		m.cfg = &msg.cfg
		m.identityLabel = msg.label
		m.idKey = msg.idKey
		m.activeID = msg.activeID
		m.manager = msg.manager
		m.runner = msg.runner
		m.modal = modalNone
		m.connStatus = "已配置，正在拉取远端文件"
		m.statusToast = "已加载身份：" + msg.label
		m.refreshQueueSnapshots()
		return m, m.loadRemoteCmd()

	case batchOpResultMsg:
		if msg.err != nil {
			m.statusToast = msg.err.Error()
			return m, nil
		}
		if msg.notice != "" {
			m.statusToast = msg.notice
		}
		if msg.quit {
			m.cleanup()
			return m, tea.Quit
		}
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

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m.requestExit()
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

	// 1. 文本编辑态优先独占输入（避免误触单字母快捷键）
	if m.editingSearch {
		switch k {
		case "esc":
			m.editingSearch = false
			m.searchInput.Blur()
			m.searchInput.SetValue("")
			m.query = ""
			m.rebuildLocalVisible()
			m.rebuildRemoteVisible()
			return m, nil
		case "enter":
			m.editingSearch = false
			m.searchInput.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		m.query = m.searchInput.Value()
		m.rebuildLocalVisible()
		m.rebuildRemoteVisible()
		return m, cmd
	}

	if m.editingPath {
		switch k {
		case "esc":
			m.editingPath = false
			m.pathInput.Blur()
			m.pathInput.SetValue(m.localDir)
			return m, nil
		case "enter":
			target := strings.TrimSpace(m.pathInput.Value())
			m.editingPath = false
			m.pathInput.Blur()
			if target != "" {
				return m, m.loadLocalCmd(target)
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.pathInput, cmd = m.pathInput.Update(msg)
		return m, cmd
	}

	// 2. 弹窗路由
	if m.modal != modalNone {
		return m.handleModalKey(msg)
	}

	// 3. 全局导航与视图快捷键
	switch k {
	case "q", "f10":
		return m.requestExit()
	case "?":
		m.modal = modalHelp
		return m, nil
	case "tab":
		m.cycleView(1)
		return m, nil
	case "shift+tab":
		m.cycleView(-1)
		return m, nil
	case "1":
		m.focus = areaLocal
		return m, nil
	case "2":
		m.focus = areaRemote
		return m, nil
	case "3":
		m.focus = areaQueue
		m.historyView = false
		m.clampQueueCursor()
		return m, nil
	case "4":
		m.focus = areaQueue
		m.historyView = true
		m.clampQueueCursor()
		return m, nil
	case "v", "h":
		m.focus = areaQueue
		m.historyView = !m.historyView
		m.clampQueueCursor()
		return m, nil
	case "/":
		m.editingSearch = true
		m.searchInput.Focus()
		return m, textinput.Blink
	case "p":
		if m.focus == areaQueue {
			return m, m.pauseSelectedCmd()
		}
		m.editingPath = true
		m.pathInput.SetValue(m.localDir)
		m.pathInput.Focus()
		m.pathInput.CursorEnd()
		return m, textinput.Blink
	case "o":
		m.sortMode = (m.sortMode + 1) % 4
		m.rebuildLocalVisible()
		m.rebuildRemoteVisible()
		m.statusToast = "排序：" + m.sortLabel()
		return m, nil
	case "r":
		if m.focus == areaQueue && m.historyView {
			return m, m.retryHistoryCmd()
		}
		return m, tea.Batch(m.loadLocalCmd(m.localDir), m.loadRemoteCmd())
	case "i", "f4":
		m.modal = modalIdentity
		m.idInputFocus = false
		m.idInput.Blur()
		return m, m.scanIdentitiesCmd()
	case "a":
		return m, m.toggleAllCmd()
	case "c":
		if m.focus == areaQueue {
			return m, m.cancelSelectedCmd()
		}
		return m, nil
	case "u":
		return m, m.prepareUpload()
	case "d":
		return m, m.prepareDownload()
	case "f5":
		if m.focus == areaRemote {
			return m, m.prepareDownload()
		}
		return m, m.prepareUpload()
	}

	// 4. 分区按键处理
	switch m.focus {
	case areaLocal:
		return m.handleLocalKey(k)
	case areaRemote:
		return m.handleRemoteKey(k)
	default:
		return m.handleQueueKey(k)
	}
}

func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || m.width <= 0 || m.height <= 0 {
		return m, nil
	}
	if m.modal == modalIdentity {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if m.idCursor > 0 {
				m.idCursor--
			}
		case tea.MouseButtonWheelDown:
			if m.idCursor < len(m.idChoices)-1 {
				m.idCursor++
			}
		}
		return m, nil
	}
	if m.modal != modalNone {
		return m, nil
	}

	const bodyY = 2
	bodyH := max(6, m.height-3)
	if msg.Y < bodyY || msg.Y >= bodyY+bodyH {
		return m, nil
	}

	moveList := func(cursor *int, total, visRows, relY int) {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if *cursor > 0 {
				*cursor--
			}
		case tea.MouseButtonWheelDown:
			if *cursor < total-1 {
				*cursor++
			}
		case tea.MouseButtonLeft:
			if relY >= 0 && relY < visRows && total > 0 {
				idx := scrollOffset(*cursor, total, visRows) + relY
				if idx >= 0 && idx < total {
					*cursor = idx
				}
			}
		}
	}

	if m.width < 96 {
		if msg.Y == bodyY {
			if msg.Button == tea.MouseButtonLeft {
				labels := []string{"1 本地文件", "2 远端文件", "3 活动队列", "4 历史归档"}
				activeTab := int(m.focus)
				if m.focus == areaQueue && m.historyView {
					activeTab = 3
				}
				switch tabHitIndex(msg.X, 0, labels, m.th, activeTab) {
				case 0:
					m.focus = areaLocal
				case 1:
					m.focus = areaRemote
				case 2:
					m.focus = areaQueue
					m.historyView = false
					m.clampQueueCursor()
				case 3:
					m.focus = areaQueue
					m.historyView = true
					m.clampQueueCursor()
				}
			}
			return m, nil
		}
		panelH := max(5, bodyH-1)
		visRows := max(1, panelH-3)
		relY := msg.Y - (bodyY + 3)
		if m.focus == areaQueue && msg.Y == bodyY+2 && msg.Button == tea.MouseButtonLeft {
			qLabels := []string{fmt.Sprintf("活动队列 (%d)", len(m.activeTasks)), fmt.Sprintf("历史归档 (%d)", len(m.historyTasks))}
			activeQ := 0
			if m.historyView {
				activeQ = 1
			}
			if idx := tabHitIndex(msg.X, 2, qLabels, m.th, activeQ); idx >= 0 {
				m.historyView = idx == 1
				m.clampQueueCursor()
			}
			return m, nil
		}
		switch m.focus {
		case areaLocal:
			moveList(&m.localCursor, len(m.visibleLocal), visRows, relY)
		case areaRemote:
			moveList(&m.remoteCursor, len(m.visibleRemote), visRows, relY)
		default:
			moveList(&m.queueCursor, len(m.currentQueueTasks()), visRows, relY)
		}
		return m, nil
	}

	topH := max(5, (bodyH*55)/100)
	botH := max(5, bodyH-topH)
	if topH+botH > bodyH {
		topH = bodyH - botH
	}
	leftW := m.width / 2
	if msg.Y < bodyY+topH {
		visRows := max(1, topH-3)
		relY := msg.Y - (bodyY + 2)
		if msg.X < leftW {
			m.focus = areaLocal
			moveList(&m.localCursor, len(m.visibleLocal), visRows, relY)
		} else {
			m.focus = areaRemote
			moveList(&m.remoteCursor, len(m.visibleRemote), visRows, relY)
		}
		return m, nil
	}

	m.focus = areaQueue
	queueY := bodyY + topH
	if msg.Y == queueY+1 && msg.Button == tea.MouseButtonLeft {
		qLabels := []string{fmt.Sprintf("活动队列 (%d)", len(m.activeTasks)), fmt.Sprintf("历史归档 (%d)", len(m.historyTasks))}
		activeQ := 0
		if m.historyView {
			activeQ = 1
		}
		if idx := tabHitIndex(msg.X, 2, qLabels, m.th, activeQ); idx >= 0 {
			m.historyView = idx == 1
			m.clampQueueCursor()
		}
		return m, nil
	}
	visRows := max(1, botH-3)
	moveList(&m.queueCursor, len(m.currentQueueTasks()), visRows, msg.Y-(queueY+2))
	return m, nil
}

func (m *model) handleModalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch m.modal {
	case modalHelp:
		if k == "esc" || k == "?" || k == "enter" || k == "q" {
			m.modal = modalNone
		}
		return m, nil

	case modalDetail:
		if k == "esc" || k == "enter" || k == "q" {
			m.modal = modalNone
		}
		return m, nil

	case modalIdentity:
		if k == "esc" {
			m.modal = modalNone
			return m, nil
		}
		if k == "tab" {
			m.idInputFocus = !m.idInputFocus
			if m.idInputFocus {
				m.idInput.Focus()
				return m, textinput.Blink
			}
			m.idInput.Blur()
			return m, nil
		}
		if m.idInputFocus {
			if k == "enter" {
				path := strings.TrimSpace(m.idInput.Value())
				if path != "" {
					m.idInput.SetValue("")
					m.statusToast = "正在校验并导入身份…"
					return m, m.activateChoiceCmd(identityChoice{}, path)
				}
				return m, nil
			}
			var cmd tea.Cmd
			m.idInput, cmd = m.idInput.Update(msg)
			return m, cmd
		}
		switch k {
		case "up", "k":
			if m.idCursor > 0 {
				m.idCursor--
			}
		case "down", "j":
			if m.idCursor < len(m.idChoices)-1 {
				m.idCursor++
			}
		case "enter":
			if len(m.idChoices) > 0 && m.idCursor < len(m.idChoices) {
				m.statusToast = "正在切换身份…"
				return m, m.activateChoiceCmd(m.idChoices[m.idCursor], "")
			}
		}
		return m, nil

	case modalConflict:
		switch k {
		case "esc":
			m.modal = modalNone
			m.pendingSpecs = nil
			m.conflictNames = nil
			return m, nil
		case "r", "a":
			specs := m.resolveConflictsAutoRename()
			m.modal = modalNone
			m.pendingSpecs = nil
			m.conflictNames = nil
			return m, m.enqueueSpecsCmd(specs)
		case "s":
			specs := m.resolveConflictsSkip()
			m.modal = modalNone
			m.pendingSpecs = nil
			m.conflictNames = nil
			return m, m.enqueueSpecsCmd(specs)
		}
		return m, nil

	case modalRename:
		switch k {
		case "esc":
			m.modal = modalNone
			m.renameTask = nil
			return m, nil
		case "enter":
			newName := strings.TrimSpace(m.renameInput.Value())
			t := m.renameTask
			m.modal = modalNone
			m.renameTask = nil
			if t == nil || newName == "" {
				return m, nil
			}
			spec := queue.Spec{Direction: t.Direction, Remote: t.Remote, Path: t.Path, Size: t.Size}
			if t.Direction == protocol.DirectionUpload {
				spec.Remote = newName
			} else {
				spec.Path = filepath.Join(filepath.Dir(t.Path), newName)
			}
			return m, m.enqueueSpecsCmd([]queue.Spec{spec})
		}
		var cmd tea.Cmd
		m.renameInput, cmd = m.renameInput.Update(msg)
		return m, cmd

	case modalQuit:
		switch k {
		case "esc":
			m.modal = modalNone
			return m, nil
		case "p":
			m.modal = modalNone
			m.statusToast = "正在等待服务端确认暂停检查点后退出…"
			return m, m.pauseAllAndQuitCmd()
		case "f", "q":
			m.cleanup()
			return m, tea.Quit
		}
		return m, nil

	case modalClearHistory:
		switch k {
		case "esc", "n":
			m.modal = modalNone
		case "enter", "y":
			m.modal = modalNone
			if m.manager != nil {
				err := m.manager.ClearHistory()
				m.refreshQueueSnapshots()
				if err != nil {
					m.statusToast = err.Error()
				} else {
					m.statusToast = "已清空历史记录"
				}
			}
		}
		return m, nil
	}
	return m, nil
}

func (m *model) cycleView(delta int) {
	// 视图顺序：0=本地 -> 1=远端 -> 2=活动队列 -> 3=历史归档
	cur := 0
	switch m.focus {
	case areaRemote:
		cur = 1
	case areaQueue:
		if m.historyView {
			cur = 3
		} else {
			cur = 2
		}
	}
	next := (cur + delta + 4) % 4
	switch next {
	case 0:
		m.focus = areaLocal
	case 1:
		m.focus = areaRemote
	case 2:
		m.focus = areaQueue
		m.historyView = false
		m.clampQueueCursor()
	case 3:
		m.focus = areaQueue
		m.historyView = true
		m.clampQueueCursor()
	}
}

func (m *model) requestExit() (tea.Model, tea.Cmd) {
	if m.manager != nil && m.manager.HasRunning() {
		m.modal = modalQuit
		return m, nil
	}
	m.cleanup()
	return m, tea.Quit
}

func (m *model) handleLocalKey(k string) (tea.Model, tea.Cmd) {
	n := len(m.visibleLocal)
	switch k {
	case "up", "k":
		if m.localCursor > 0 {
			m.localCursor--
		}
	case "down", "j":
		if m.localCursor < n-1 {
			m.localCursor++
		}
	case "pgup":
		m.localCursor = max(0, m.localCursor-10)
	case "pgdown":
		if n > 0 {
			m.localCursor = min(n-1, m.localCursor+10)
		}
	case "home", "g":
		m.localCursor = 0
	case "end", "G":
		if n > 0 {
			m.localCursor = n - 1
		}
	case "backspace":
		parent := filepath.Dir(m.localDir)
		if parent != m.localDir {
			return m, m.loadLocalCmd(parent)
		}
	case " ":
		if m.localCursor >= 0 && m.localCursor < n {
			e := m.visibleLocal[m.localCursor]
			if e.regular {
				if m.localSelected[e.path] {
					delete(m.localSelected, e.path)
				} else {
					m.localSelected[e.path] = true
				}
			}
			if m.localCursor < n-1 {
				m.localCursor++
			}
		}
	case "enter":
		if m.localCursor >= 0 && m.localCursor < n {
			e := m.visibleLocal[m.localCursor]
			if e.dir {
				return m, m.loadLocalCmd(e.path)
			}
			m.detailTitle = "本地文件信息"
			m.detailLines = []string{
				"名称：" + e.name,
				"路径：" + e.path,
				fmt.Sprintf("大小：%s (%d B)", progress.FormatBytes(uint64(max(0, e.size))), e.size),
				"",
				"按 u 或 F5 加入上传队列（按 Space 可多选）",
			}
			m.modal = modalDetail
		}
	}
	return m, nil
}

func (m *model) handleRemoteKey(k string) (tea.Model, tea.Cmd) {
	n := len(m.visibleRemote)
	switch k {
	case "up", "k":
		if m.remoteCursor > 0 {
			m.remoteCursor--
		}
	case "down", "j":
		if m.remoteCursor < n-1 {
			m.remoteCursor++
		}
	case "pgup":
		m.remoteCursor = max(0, m.remoteCursor-10)
	case "pgdown":
		if n > 0 {
			m.remoteCursor = min(n-1, m.remoteCursor+10)
		}
	case "home", "g":
		m.remoteCursor = 0
	case "end", "G":
		if n > 0 {
			m.remoteCursor = n - 1
		}
	case " ":
		if m.remoteCursor >= 0 && m.remoteCursor < n {
			name := m.visibleRemote[m.remoteCursor].FileName
			if m.remoteSelected[name] {
				delete(m.remoteSelected, name)
			} else {
				m.remoteSelected[name] = true
			}
			if m.remoteCursor < n-1 {
				m.remoteCursor++
			}
		}
	case "enter":
		if m.remoteCursor >= 0 && m.remoteCursor < n {
			f := m.visibleRemote[m.remoteCursor]
			m.detailTitle = "远端文件信息"
			m.detailLines = []string{
				"文件名：" + f.FileName,
				fmt.Sprintf("大小：  %s (%d B)", progress.FormatBytes(f.Size), f.Size),
				fmt.Sprintf("SHA-256：%x", f.SHA256),
				"",
				"按 d 或 F5 下载至当前本地目录（" + m.localDir + "）",
			}
			m.modal = modalDetail
		}
	}
	return m, nil
}

func (m *model) handleQueueKey(k string) (tea.Model, tea.Cmd) {
	tasks := m.currentQueueTasks()
	n := len(tasks)
	switch k {
	case "up", "k":
		if m.queueCursor > 0 {
			m.queueCursor--
		}
	case "down", "j":
		if m.queueCursor < n-1 {
			m.queueCursor++
		}
	case "pgup":
		m.queueCursor = max(0, m.queueCursor-8)
	case "pgdown":
		if n > 0 {
			m.queueCursor = min(n-1, m.queueCursor+8)
		}
	case " ":
		if !m.historyView {
			return m, m.toggleSelectedTaskCmd()
		}
		if t := m.selectedTask(); t != nil {
			m.openTaskDetail(*t)
		}
	case "n":
		if t := m.selectedTask(); t != nil && !t.Terminal() {
			m.renameTask = t
			if t.Direction == protocol.DirectionUpload {
				m.renameInput.SetValue(t.Remote)
			} else {
				m.renameInput.SetValue(filepath.Base(t.Path))
			}
			m.renameInput.Focus()
			m.renameInput.CursorEnd()
			m.modal = modalRename
			return m, textinput.Blink
		}
	case "X":
		if m.historyView && m.manager != nil {
			m.modal = modalClearHistory
		}
	case "enter":
		if t := m.selectedTask(); t != nil {
			m.openTaskDetail(*t)
		}
	}
	return m, nil
}

func (m *model) openTaskDetail(t queue.Task) {
	m.detailTitle = "任务详情"
	m.detailLines = buildTaskDetailLines(
		directionIcon(t.Direction),
		string(t.State),
		t.Remote,
		t.Path,
		t.Done,
		t.Size,
		t.SHA256,
		t.TransferID,
		t.Error,
		t.Updated,
	)
	m.modal = modalDetail
}

func (m *model) currentQueueTasks() []queue.Task {
	if m.historyView {
		return m.historyTasks
	}
	return m.activeTasks
}

func (m *model) selectedTask() *queue.Task {
	tasks := m.currentQueueTasks()
	if m.queueCursor < 0 || m.queueCursor >= len(tasks) {
		return nil
	}
	t := tasks[m.queueCursor]
	return &t
}

func (m *model) clampQueueCursor() {
	n := len(m.currentQueueTasks())
	if m.queueCursor >= n {
		m.queueCursor = max(0, n-1)
	}
}

func (m *model) refreshQueueSnapshots() bool {
	if m.runner != nil {
		m.connStatus = m.runner.Status()
	}
	if m.manager == nil {
		return false
	}
	m.activeTasks = m.manager.Snapshot(false)
	m.historyTasks = m.manager.Snapshot(true)
	m.clampQueueCursor()

	refreshFiles := false
	for _, t := range m.activeTasks {
		if t.State == queue.Completed && !m.completedSeen[t.ID] {
			m.completedSeen[t.ID] = true
			refreshFiles = true
		}
	}
	return refreshFiles
}

func (m *model) sortLabel() string {
	switch m.sortMode {
	case sortNameDesc:
		return "名称↓"
	case sortSizeDesc:
		return "大小↓"
	case sortSizeAsc:
		return "大小↑"
	default:
		return "名称↑"
	}
}

func (m *model) rebuildLocalVisible() {
	q := strings.ToLower(strings.TrimSpace(m.query))
	var out []localEntry
	for _, e := range m.localEntries {
		if q != "" && e.name != "../" && !strings.Contains(strings.ToLower(e.name), q) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.name == "../" || b.name == "../" {
			return a.name == "../"
		}
		if a.dir != b.dir {
			return a.dir
		}
		switch m.sortMode {
		case sortNameDesc:
			return strings.ToLower(a.name) > strings.ToLower(b.name)
		case sortSizeDesc:
			if a.size != b.size {
				return a.size > b.size
			}
		case sortSizeAsc:
			if a.size != b.size {
				return a.size < b.size
			}
		}
		return strings.ToLower(a.name) < strings.ToLower(b.name)
	})
	m.visibleLocal = out
	if m.localCursor >= len(out) {
		m.localCursor = max(0, len(out)-1)
	}
}

func (m *model) rebuildRemoteVisible() {
	q := strings.ToLower(strings.TrimSpace(m.query))
	var out []protocol.FileEntry
	for _, f := range m.remoteEntries {
		if q != "" && !strings.Contains(strings.ToLower(f.FileName), q) {
			continue
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch m.sortMode {
		case sortNameDesc:
			return strings.ToLower(a.FileName) > strings.ToLower(b.FileName)
		case sortSizeDesc:
			if a.Size != b.Size {
				return a.Size > b.Size
			}
		case sortSizeAsc:
			if a.Size != b.Size {
				return a.Size < b.Size
			}
		}
		return strings.ToLower(a.FileName) < strings.ToLower(b.FileName)
	})
	m.visibleRemote = out
	if m.remoteCursor >= len(out) {
		m.remoteCursor = max(0, len(out)-1)
	}
}

func (m *model) prepareUpload() tea.Cmd {
	if m.manager == nil {
		m.statusToast = "请先配置或导入身份（按 i）"
		return nil
	}
	var chosen []localEntry
	if len(m.localSelected) > 0 {
		for _, e := range m.localEntries {
			if e.regular && m.localSelected[e.path] {
				chosen = append(chosen, e)
			}
		}
	} else if m.localCursor >= 0 && m.localCursor < len(m.visibleLocal) {
		if e := m.visibleLocal[m.localCursor]; e.regular {
			chosen = append(chosen, e)
		}
	}
	if len(chosen) == 0 {
		m.statusToast = "请先选中至少一个普通文件"
		return nil
	}

	remoteSet := make(map[string]bool, len(m.remoteEntries))
	for _, f := range m.remoteEntries {
		remoteSet[f.FileName] = true
	}

	var specs []queue.Spec
	var conflicts []string
	for _, e := range chosen {
		specs = append(specs, queue.Spec{
			Direction: protocol.DirectionUpload,
			Remote:    filepath.Base(e.path),
			Path:      e.path,
			Size:      uint64(max(0, e.size)),
		})
		if remoteSet[filepath.Base(e.path)] {
			conflicts = append(conflicts, filepath.Base(e.path))
		}
	}
	m.localSelected = make(map[string]bool)
	if len(conflicts) > 0 {
		m.pendingBatchDir = protocol.DirectionUpload
		m.pendingSpecs = specs
		m.conflictNames = conflicts
		m.modal = modalConflict
		return nil
	}
	return m.enqueueSpecsCmd(specs)
}

func (m *model) prepareDownload() tea.Cmd {
	if m.manager == nil {
		m.statusToast = "请先配置或导入身份（按 i）"
		return nil
	}
	var chosen []protocol.FileEntry
	if len(m.remoteSelected) > 0 {
		for _, f := range m.remoteEntries {
			if m.remoteSelected[f.FileName] {
				chosen = append(chosen, f)
			}
		}
	} else if m.remoteCursor >= 0 && m.remoteCursor < len(m.visibleRemote) {
		chosen = append(chosen, m.visibleRemote[m.remoteCursor])
	}
	if len(chosen) == 0 {
		m.statusToast = "请先选中至少一个远端文件"
		return nil
	}

	var specs []queue.Spec
	var conflicts []string
	for _, f := range chosen {
		dst := filepath.Join(m.localDir, f.FileName)
		specs = append(specs, queue.Spec{
			Direction: protocol.DirectionDownload,
			Remote:    f.FileName,
			Path:      dst,
			Size:      f.Size,
		})
		if _, err := os.Lstat(dst); err == nil {
			conflicts = append(conflicts, f.FileName)
		}
	}
	m.remoteSelected = make(map[string]bool)
	if len(conflicts) > 0 {
		m.pendingBatchDir = protocol.DirectionDownload
		m.pendingSpecs = specs
		m.conflictNames = conflicts
		m.modal = modalConflict
		return nil
	}
	return m.enqueueSpecsCmd(specs)
}

func (m *model) resolveConflictsAutoRename() []queue.Spec {
	used := make(map[string]bool)
	if m.pendingBatchDir == protocol.DirectionUpload {
		for _, f := range m.remoteEntries {
			used[f.FileName] = true
		}
	} else {
		for _, e := range m.localEntries {
			used[e.name] = true
		}
	}
	out := make([]queue.Spec, 0, len(m.pendingSpecs))
	for _, s := range m.pendingSpecs {
		if m.pendingBatchDir == protocol.DirectionUpload {
			name := uniqueName(s.Remote, used)
			used[name] = true
			s.Remote = name
		} else {
			base := filepath.Base(s.Path)
			name := uniqueName(base, used)
			used[name] = true
			s.Path = filepath.Join(filepath.Dir(s.Path), name)
		}
		out = append(out, s)
	}
	return out
}

func (m *model) resolveConflictsSkip() []queue.Spec {
	conflictSet := make(map[string]bool, len(m.conflictNames))
	for _, c := range m.conflictNames {
		conflictSet[c] = true
	}
	out := make([]queue.Spec, 0, len(m.pendingSpecs))
	for _, s := range m.pendingSpecs {
		key := s.Remote
		if s.Direction == protocol.DirectionDownload {
			key = filepath.Base(s.Path)
		}
		if conflictSet[key] {
			s.Skip = "用户跳过重名文件"
		}
		out = append(out, s)
	}
	return out
}

func trimUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	n := 0
	for i, r := range s {
		w := utf8.RuneLen(r)
		if w < 0 || i+w > maxBytes {
			return s[:n]
		}
		n = i + w
	}
	return s[:n]
}

func uniqueName(orig string, used map[string]bool) string {
	if !used[orig] {
		return orig
	}
	ext := filepath.Ext(orig)
	stem := strings.TrimSuffix(orig, ext)
	if len(ext) > 240 {
		ext = ""
		stem = orig
	}
	for i := 1; i < 10000; i++ {
		suffix := fmt.Sprintf(" (%d)", i)
		budget := protocol.MaxFileNameBytes - len(suffix) - len(ext)
		trimmed := strings.TrimRight(trimUTF8Bytes(stem, budget), " .")
		cand := fmt.Sprintf("%s%s%s", trimmed, suffix, ext)
		if trimmed == "" && budget > 0 {
			cand = fmt.Sprintf("%s-%d%s", trimUTF8Bytes(stem, budget-2), i, ext)
		}
		if len(cand) <= protocol.MaxFileNameBytes && protocol.ValidateName(cand) == nil && !used[cand] {
			return cand
		}
	}
	suffix := fmt.Sprintf("-%d", time.Now().UnixNano())
	budget := protocol.MaxFileNameBytes - len(suffix) - len(ext)
	return fmt.Sprintf("%s%s%s", trimUTF8Bytes(stem, budget), suffix, ext)
}

func (m *model) enqueueSpecsCmd(specs []queue.Spec) tea.Cmd {
	mgr := m.manager
	return func() tea.Msg {
		tasks, err := mgr.Enqueue(specs)
		if err != nil {
			return batchOpResultMsg{err: err}
		}
		return batchOpResultMsg{notice: fmt.Sprintf("已入队 %d 个传输任务", len(tasks))}
	}
}

func (m *model) toggleSelectedTaskCmd() tea.Cmd {
	if m.manager == nil {
		return nil
	}
	t := m.selectedTask()
	if t == nil || t.Terminal() {
		return nil
	}
	mgr := m.manager
	task := *t
	return func() tea.Msg {
		switch {
		case task.State.Active():
			if err := mgr.Pause(task.ID); err != nil {
				return batchOpResultMsg{err: err}
			}
			return batchOpResultMsg{notice: "已请求暂停：" + task.Remote}
		case task.State == queue.Paused || task.State == queue.Pending:
			if task.WantCancel {
				if err := mgr.Cancel(task.ID); err != nil {
					return batchOpResultMsg{err: err}
				}
				return batchOpResultMsg{notice: "重试取消：" + task.Remote}
			}
			if err := mgr.Resume(task.ID); err != nil {
				return batchOpResultMsg{err: err}
			}
			return batchOpResultMsg{notice: "已恢复任务：" + task.Remote}
		}
		return batchOpResultMsg{}
	}
}

func (m *model) pauseSelectedCmd() tea.Cmd {
	t := m.selectedTask()
	if m.manager == nil || t == nil {
		return nil
	}
	mgr := m.manager
	id := t.ID
	name := t.Remote
	return func() tea.Msg {
		if err := mgr.Pause(id); err != nil {
			return batchOpResultMsg{err: err}
		}
		return batchOpResultMsg{notice: "已请求暂停：" + name}
	}
}

func (m *model) cancelSelectedCmd() tea.Cmd {
	t := m.selectedTask()
	if m.manager == nil || t == nil {
		return nil
	}
	mgr := m.manager
	id := t.ID
	name := t.Remote
	return func() tea.Msg {
		if err := mgr.Cancel(id); err != nil {
			return batchOpResultMsg{err: err}
		}
		return batchOpResultMsg{notice: "已请求取消：" + name}
	}
}

func (m *model) toggleAllCmd() tea.Cmd {
	if m.manager == nil {
		return nil
	}
	hasActive := false
	for _, t := range m.activeTasks {
		if t.State.Active() {
			hasActive = true
			break
		}
	}
	mgr := m.manager
	if hasActive {
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := mgr.PauseAll(ctx); err != nil {
				return batchOpResultMsg{err: err}
			}
			return batchOpResultMsg{notice: "已全部暂停"}
		}
	}
	return func() tea.Msg {
		if err := mgr.ResumeAll(); err != nil {
			return batchOpResultMsg{err: err}
		}
		return batchOpResultMsg{notice: "已全部恢复"}
	}
}

func (m *model) retryHistoryCmd() tea.Cmd {
	t := m.selectedTask()
	if m.manager == nil || t == nil {
		return nil
	}
	spec := queue.Spec{Direction: t.Direction, Remote: t.Remote, Path: t.Path, Size: t.Size}
	return m.enqueueSpecsCmd([]queue.Spec{spec})
}

func (m *model) pauseAllAndQuitCmd() tea.Cmd {
	mgr := m.manager
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := mgr.PauseAll(ctx); err != nil {
			return batchOpResultMsg{err: err}
		}
		return batchOpResultMsg{quit: true}
	}
}
