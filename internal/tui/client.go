package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
type identityDirDiscoveredMsg struct {
	dir     string
	choices []identityChoice
}
type identityConnTestMsg struct {
	path    string
	latency time.Duration
	err     error
}
type identityTestState struct {
	testing bool
	tested  bool
	latency time.Duration
	err     string
}
type identitySwitchConfirm struct {
	choice      identityChoice
	customPath  string
	targetLabel string
	activeCount int
	pausedCount int
}
type identityActivatedMsg struct {
	cfg      config.Client
	label    string
	idKey    string
	activeID string
	manager  *queue.Manager
	runner   *transfer.QueueRunner
	resetOld bool
	err      error
}
type batchOpResultMsg struct {
	notice   string
	err      error
	quit     bool
	enqueued []queue.Task
}
type batchTracker struct {
	id      string
	started time.Time
	taskIDs []string
}
type batchFailedItem struct {
	Direction protocol.Direction
	Remote    string
	Path      string
	Error     string
}
type batchSummary struct {
	BatchID    string
	Started    time.Time
	Finished   time.Time
	Total      int
	Succeeded  int
	Failed     int
	Cancelled  int
	Skipped    int
	TotalBytes uint64
	FailedList []batchFailedItem
}

type model struct {
	opts      Options
	prefsPath string
	th        theme

	width, height int
	focus         focusArea
	historyView   bool
	modal         modalKind
	infoScroll    int

	// 文本输入框
	searchInput    textinput.Model
	pathInput      textinput.Model
	idInput        textinput.Model
	idLabelInput   textinput.Model
	renameInput    textinput.Model
	editingSearch  bool
	editingPath    bool
	idInputFocus   bool
	editingIDLabel bool

	// 身份与传输引擎
	cfg           *config.Client
	identityLabel string
	idKey         string
	activeID      string
	manager       *queue.Manager
	runner        *transfer.QueueRunner
	connStatus    string

	// 文件列表
	localDir              string
	localEpoch            int
	remoteEpoch           int
	loadingLocal          bool
	loadingRemote         bool
	localLoadStarted      time.Time
	remoteLoadStarted     time.Time
	pendingLocalFocusPath string
	remoteErr             string
	localEntries          []localEntry
	visibleLocal          []localEntry
	remoteEntries         []protocol.FileEntry
	visibleRemote         []protocol.FileEntry
	localSelected         map[string]bool
	remoteSelected        map[string]bool
	localCursor           int
	remoteCursor          int
	localSort             sortKind
	remoteSort            sortKind
	query                 string

	// 异步操作进度与防重入状态
	activatingID   bool
	testingConn    bool
	opBusy         bool
	opLabel        string
	opStart        time.Time
	spinIdx        int
	idTestResults  map[string]identityTestState
	pendingSwitch  *identitySwitchConfirm
	recoveryNotice string
	activeBatches  map[string]*batchTracker
	lastBatch      *batchSummary
	batchHistory   []batchSummary

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

	prefs := loadUIPrefs(prefsPath)
	m := &model{
		opts:           opts,
		prefsPath:      prefsPath,
		th:             makeTheme(!prefs.Light),
		localSort:      clampSortKind(prefs.LocalOrder),
		remoteSort:     clampSortKind(prefs.RemoteOrder),
		focus:          areaLocal,
		localDir:       opts.Directory,
		connStatus:     "未连接",
		identityLabel:  "未选择",
		localSelected:  make(map[string]bool),
		remoteSelected: make(map[string]bool),
		completedSeen:  make(map[string]bool),
		idTestResults:  make(map[string]identityTestState),
		activeBatches:  make(map[string]*batchTracker),
		searchInput:    makeInput("输入关键字筛选文件名…"),
		pathInput:      makeInput("输入本地绝对或相对路径…"),
		idInput:        makeInput("输入 client*.json 或导出目录路径…"),
		idLabelInput:   makeInput("输入自定义身份标签 (1–80 字符)…"),
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
	switch {
	case m.manager != nil && m.manager.HasRunning():
		interval = 50 * time.Millisecond
	case m.activatingID || m.testingConn || m.opBusy || m.loadingLocal || m.loadingRemote:
		interval = 100 * time.Millisecond
	}
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m *model) loadLocalCmd(dir string) tea.Cmd {
	m.localEpoch++
	epoch := m.localEpoch
	m.loadingLocal = true
	m.localLoadStarted = time.Now()
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
	m.remoteLoadStarted = time.Now()
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

func (m *model) unfinishedTaskCounts() (active, paused int) {
	if m.manager == nil {
		return 0, 0
	}
	m.refreshQueueSnapshots()
	for _, t := range m.activeTasks {
		if t.State.Active() {
			active++
		} else if !t.Terminal() {
			paused++
		}
	}
	return active, paused
}

func (m *model) beginIdentitySwitch(ch identityChoice, customPath string, bypassConfirm bool) tea.Cmd {
	if m.activatingID {
		m.statusToast = "正在切换或导入身份，请稍候…"
		return nil
	}
	targetLabel := customPath
	if targetLabel == "" {
		targetLabel = ch.profile.Label
	}
	if !bypassConfirm {
		activeN, pausedN := m.unfinishedTaskCounts()
		if customPath != "" {
			if info, err := os.Stat(customPath); err == nil && info.IsDir() {
				// 目录可能含多个配置，先扫描（多配置或存在未完成任务时不直接静默切换）
				m.activatingID = true
				m.opLabel = "正在扫描身份目录"
				m.opStart = time.Now()
				m.statusToast = "正在扫描目录中的身份配置…"
				return m.discoverOrActivatePathCmd(customPath, activeN+pausedN > 0)
			}
		}
		if activeN+pausedN > 0 {
			m.pendingSwitch = &identitySwitchConfirm{
				choice:      ch,
				customPath:  customPath,
				targetLabel: targetLabel,
				activeCount: activeN,
				pausedCount: pausedN,
			}
			m.statusToast = ""
			return nil
		}
	}
	m.pendingSwitch = nil
	m.activatingID = true
	if customPath != "" {
		m.opLabel = "正在导入身份"
		m.statusToast = "正在校验并导入身份…"
	} else {
		m.opLabel = "正在切换身份"
		m.statusToast = "正在切换身份…"
	}
	m.opStart = time.Now()
	return m.activateChoiceCmd(ch, customPath)
}

func (m *model) discoverOrActivatePathCmd(customPath string, confirmSingle bool) tea.Cmd {
	exeDir := m.opts.ExecutableDir
	actCustom := m.activateChoiceCmd(identityChoice{}, customPath)
	return func() tea.Msg {
		wd, _ := os.Getwd()
		candidates, discErr := identity.Discover(exeDir, wd, customPath)
		if discErr != nil {
			return identityActivatedMsg{err: discErr}
		}
		if len(candidates) == 0 {
			return identityActivatedMsg{err: fmt.Errorf("目录中未找到有效的客户端配置：%s", customPath)}
		}
		if len(candidates) > 1 || confirmSingle {
			var found []identityChoice
			for _, p := range candidates {
				if prof, e := identity.Preview(p, wd); e == nil {
					found = append(found, identityChoice{profile: prof, imported: false})
				}
			}
			if len(found) == 0 {
				return identityActivatedMsg{err: fmt.Errorf("目录中的客户端配置均无法解析：%s", customPath)}
			}
			return identityDirDiscoveredMsg{dir: customPath, choices: found}
		}
		return actCustom()
	}
}

func (m *model) testIdentityConnCmd(ch identityChoice) tea.Cmd {
	path := ch.profile.Path
	cfg := ch.profile.Config
	return func() tea.Msg {
		if path != "" {
			wd, _ := os.Getwd()
			if prof, err := identity.Preview(path, wd); err == nil {
				cfg = prof.Config
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		cli, err := transfer.Dial(ctx, cfg)
		elapsed := time.Since(start)
		if cli != nil {
			_ = cli.Close()
		}
		return identityConnTestMsg{path: path, latency: elapsed, err: err}
	}
}

func (m *model) activateChoiceCmd(ch identityChoice, customPath string) tea.Cmd {
	oldMgr := m.manager
	oldRunner := m.runner
	oldIDKey := m.idKey
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

		// 先完成身份索引与持久化校验，再暂停/关闭旧队列与连接，避免后续失败导致现有队列不可用。
		var cfg config.Client
		if useSel {
			cfg, err = identity.Select(exeDir, prof.ID, prof.Label)
		} else {
			cfg, err = identity.SelectPrepared(exeDir, prof, prof.Label)
		}
		if err != nil {
			return identityActivatedMsg{err: err}
		}

		newIDKey, idErr := security.ClientIdentityKey(cfg)
		if idErr == nil && newIDKey != oldIDKey {
			// 新旧身份状态库锁不同时，先打开新引擎，确认可用后再优雅关闭旧实例
			res := openIdentityStack(cfg, prof.Label)
			if res.err != nil {
				return res
			}
			if oldMgr != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err = oldMgr.Quiesce(ctx)
				cancel()
				if err != nil {
					if res.runner != nil {
						_ = res.runner.Close()
					}
					if res.manager != nil {
						_ = res.manager.Close()
					}
					return identityActivatedMsg{err: fmt.Errorf("切换身份前暂停当前任务失败：%w", err)}
				}
				_ = oldRunner.Close()
				_ = oldMgr.Close()
			}
			res.activeID = prof.ID
			return res
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
		res := openIdentityStack(cfg, prof.Label)
		res.activeID = prof.ID
		if res.err != nil && oldMgr != nil {
			res.resetOld = true
		}
		return res
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		m.spinIdx = (m.spinIdx + 1) % len(spinnerFrames)
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
			m.pendingLocalFocusPath = ""
			m.statusToast = "无法读取目录：" + msg.err.Error()
			return m, nil
		}
		prevDir := m.localDir
		m.localDir = msg.dir
		if !m.editingPath {
			m.pathInput.SetValue(msg.dir)
		}
		if msg.dir != prevDir && m.pendingLocalFocusPath == "" {
			m.visibleLocal = nil
			m.localCursor = 0
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
		existing := make(map[string]identityChoice, len(m.idChoices))
		for _, ch := range m.idChoices {
			existing[ch.profile.Path] = ch
		}
		// 保留未导入但用户刚编辑过标签或扫描自自定义目录的候选
		seen := make(map[string]bool, len(msg.choices))
		out := make([]identityChoice, 0, len(msg.choices))
		for _, ch := range msg.choices {
			seen[ch.profile.Path] = true
			if !ch.imported {
				if prev, ok := existing[ch.profile.Path]; ok && prev.profile.Label != "" {
					ch.profile.Label = prev.profile.Label
				}
			}
			out = append(out, ch)
		}
		for _, prev := range m.idChoices {
			if !prev.imported && !seen[prev.profile.Path] {
				out = append(out, prev)
			}
		}
		m.idChoices = out
		if m.idCursor >= len(m.idChoices) {
			m.idCursor = max(0, len(m.idChoices)-1)
		}
		if msg.err != nil && len(msg.choices) == 0 {
			m.statusToast = msg.err.Error()
		}
		return m, nil

	case identityDirDiscoveredMsg:
		m.activatingID = false
		m.opLabel = ""
		m.opStart = time.Time{}
		byPath := make(map[string]int, len(m.idChoices))
		for i, ch := range m.idChoices {
			byPath[ch.profile.Path] = i
		}
		firstIdx := -1
		for _, ch := range msg.choices {
			if idx, ok := byPath[ch.profile.Path]; ok {
				if firstIdx < 0 {
					firstIdx = idx
				}
				continue
			}
			idx := len(m.idChoices)
			m.idChoices = append(m.idChoices, ch)
			byPath[ch.profile.Path] = idx
			if firstIdx < 0 {
				firstIdx = idx
			}
		}
		if firstIdx >= 0 {
			m.idCursor = firstIdx
		}
		m.idInputFocus = false
		m.idInput.Blur()
		m.statusToast = fmt.Sprintf("目录发现 %d 个身份配置，请预览详情后按 Enter 激活或 t 测连", len(msg.choices))
		return m, nil

	case identityConnTestMsg:
		m.testingConn = false
		if msg.err != nil {
			m.idTestResults[msg.path] = identityTestState{tested: true, latency: msg.latency, err: msg.err.Error()}
			m.statusToast = "连接测试失败：" + msg.err.Error()
		} else {
			m.idTestResults[msg.path] = identityTestState{tested: true, latency: msg.latency}
			m.statusToast = fmt.Sprintf("连接测试成功，耗时 %s", msg.latency.Round(time.Millisecond))
		}
		return m, nil

	case identityActivatedMsg:
		m.activatingID = false
		if m.opLabel == "正在切换身份" {
			m.opLabel = ""
			m.opStart = time.Time{}
		}
		if msg.err != nil {
			if msg.resetOld {
				m.cfg = nil
				m.manager = nil
				m.runner = nil
				m.idKey = ""
				m.activeID = ""
				m.identityLabel = "未选择"
				m.activeTasks = nil
				m.historyTasks = nil
			}
			if m.cfg == nil {
				firstOpen := m.modal != modalIdentity
				m.modal = modalIdentity
				m.connStatus = "需要选择或导入身份"
				if firstOpen {
					m.statusToast = "未找到已激活身份，请从列表选择或输入配置目录导入"
				} else {
					m.statusToast = msg.err.Error()
				}
				return m, m.scanIdentitiesCmd()
			}
			m.statusToast = msg.err.Error()
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
		m.recoveryNotice = ""
		m.activeBatches = make(map[string]*batchTracker)
		m.lastBatch = nil
		m.refreshQueueSnapshots()
		var unfinishedN, pausedN, pendingN, waitingN int
		for _, t := range m.activeTasks {
			if t.Terminal() {
				continue
			}
			unfinishedN++
			switch t.State {
			case queue.Paused:
				pausedN++
			case queue.Pending:
				pendingN++
			default:
				waitingN++
			}
		}
		if unfinishedN > 0 {
			m.recoveryNotice = fmt.Sprintf("检测到 %d 个未完成断点任务（暂停 %d / 待处理 %d / 等待 %d），去队列按 [a] 恢复或 [F5] 继续", unfinishedN, pausedN, pendingN, waitingN)
			m.statusToast = m.recoveryNotice
		} else {
			m.statusToast = "已加载身份：" + msg.label
		}
		return m, m.loadRemoteCmd()

	case batchOpResultMsg:
		m.opBusy = false
		m.opLabel = ""
		m.opStart = time.Time{}
		if msg.err != nil {
			m.statusToast = msg.err.Error()
			return m, nil
		}
		if len(msg.enqueued) > 0 {
			m.recordEnqueuedBatch(msg.enqueued)
		}
		if msg.notice != "" {
			m.statusToast = msg.notice
		}
		if len(msg.enqueued) > 0 {
			m.refreshQueueSnapshots()
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
		return m, saveUIPrefsCmd(m.prefsPath, m.currentPrefs())
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
		m.infoScroll = 0
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
		switch m.focus {
		case areaRemote:
			m.remoteSort = (m.remoteSort + 1) % 4
			m.rebuildRemoteVisible()
			m.statusToast = "远端排序：" + m.sortLabel(m.remoteSort)
		default:
			m.localSort = (m.localSort + 1) % 4
			m.rebuildLocalVisible()
			m.statusToast = "本地排序：" + m.sortLabel(m.localSort)
		}
		return m, saveUIPrefsCmd(m.prefsPath, m.currentPrefs())
	case "r":
		if m.focus == areaQueue && m.historyView {
			return m, m.retryHistoryCmd()
		}
		return m, tea.Batch(m.loadLocalCmd(m.localDir), m.loadRemoteCmd())
	case "B":
		if m.lastBatch == nil && len(m.batchHistory) == 0 {
			m.statusToast = "当前会话暂无已完成批次结果摘要"
			return m, nil
		}
		m.infoScroll = 0
		m.modal = modalBatchSummary
		return m, nil
	case "R":
		if m.focus == areaQueue {
			return m, m.retryFailedTasksCmd()
		}
		return m, nil
	case "i", "f4":
		m.modal = modalIdentity
		m.pendingSwitch = nil
		m.editingIDLabel = false
		m.statusToast = ""
		m.idInputFocus = false
		m.idInput.Blur()
		m.idLabelInput.Blur()
		return m, m.scanIdentitiesCmd()
	case "a":
		if m.focus == areaQueue && !m.historyView {
			return m, m.toggleAllCmd()
		}
		return m, nil
	case "c":
		if m.focus == areaQueue && !m.historyView {
			return m, m.cancelSelectedCmd()
		}
		return m, nil
	case "u":
		return m, m.prepareUpload()
	case "d":
		return m, m.prepareDownload()
	case "f5":
		switch m.focus {
		case areaRemote:
			return m, m.prepareDownload()
		case areaQueue:
			return m, m.resumeOrRetrySelectedCmd()
		default:
			return m, m.prepareUpload()
		}
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

func (m *model) scrollClientInfoModal(k string) {
	title, lines, footer := m.infoModalData()
	_, maxScroll := renderModalScrollBox(m.width, m.height, m.th, title, lines, footer, m.infoScroll)
	next, ok := modalScrollStep(k, m.infoScroll, maxScroll, modalScrollBoxPageRows(m.height, footer))
	if ok {
		m.infoScroll = next
	}
}

func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || m.width <= 0 || m.height <= 0 {
		return m, nil
	}
	if m.modal == modalHelp || m.modal == modalDetail || m.modal == modalBatchSummary {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.scrollClientInfoModal("up")
		case tea.MouseButtonWheelDown:
			m.scrollClientInfoModal("down")
		}
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
		case tea.MouseButtonLeft:
			_, listStartRelY, maxShow, inputRelY := m.identityModalMetrics()
			boxW := min(max(44, m.width-4), 86)
			_, footer, lines := m.buildIdentityModalContent()
			boxH := lipgloss.Height(renderModalRawBox(m.width, m.th, "客户端 mTLS 身份管理", lines, footer))
			boxY := max(0, (m.height-boxH)/2)
			boxX := max(0, (m.width-boxW)/2)
			if msg.X < boxX || msg.X >= boxX+boxW {
				return m, nil
			}
			relY := msg.Y - (boxY + 4)
			if relY >= listStartRelY && relY < listStartRelY+maxShow && len(m.idChoices) > 0 {
				start := scrollOffset(m.idCursor, len(m.idChoices), maxShow)
				idx := start + (relY - listStartRelY)
				if idx >= 0 && idx < len(m.idChoices) {
					already := !m.idInputFocus && !m.editingIDLabel && m.idCursor == idx
					m.idInputFocus = false
					m.editingIDLabel = false
					m.idInput.Blur()
					m.idLabelInput.Blur()
					m.idCursor = idx
					if already {
						return m, m.beginIdentitySwitch(m.idChoices[idx], "", false)
					}
				}
			} else if relY >= inputRelY {
				m.editingIDLabel = false
				m.idLabelInput.Blur()
				m.idInputFocus = true
				m.idInput.Focus()
				return m, textinput.Blink
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
	queueRelY := msg.Y - (queueY + 2)
	if !m.historyView && m.recoveryNotice != "" && len(m.activeTasks) > 0 && visRows >= 3 {
		visRows--
		queueRelY--
	}
	moveList(&m.queueCursor, len(m.currentQueueTasks()), visRows, queueRelY)
	return m, nil
}

func (m *model) handleModalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	switch m.modal {
	case modalHelp:
		if k == "esc" || k == "?" || k == "enter" || k == "q" {
			m.modal = modalNone
			return m, nil
		}
		m.scrollClientInfoModal(k)
		return m, nil

	case modalDetail:
		if k == "esc" || k == "enter" || k == "q" {
			m.modal = modalNone
			return m, nil
		}
		m.scrollClientInfoModal(k)
		return m, nil

	case modalBatchSummary:
		switch k {
		case "esc", "enter", "q", "B":
			m.modal = modalNone
			return m, nil
		case "R":
			m.modal = modalNone
			return m, m.retryFailedTasksCmd()
		}
		m.scrollClientInfoModal(k)
		return m, nil

	case modalIdentity:
		if m.pendingSwitch != nil {
			switch k {
			case "esc", "n":
				m.pendingSwitch = nil
				m.statusToast = ""
				return m, nil
			case "enter", "y":
				ps := m.pendingSwitch
				m.pendingSwitch = nil
				return m, m.beginIdentitySwitch(ps.choice, ps.customPath, true)
			}
			return m, nil
		}
		if m.editingIDLabel {
			switch k {
			case "esc":
				m.editingIDLabel = false
				m.idLabelInput.Blur()
				m.statusToast = ""
				return m, nil
			case "enter":
				if m.idCursor < 0 || m.idCursor >= len(m.idChoices) {
					m.editingIDLabel = false
					m.idLabelInput.Blur()
					return m, nil
				}
				trimmed := strings.TrimSpace(m.idLabelInput.Value())
				if trimmed == "" || len([]rune(trimmed)) > 80 || strings.IndexFunc(trimmed, unicode.IsControl) >= 0 {
					m.statusToast = "身份标签须为 1–80 个字符且不能包含控制字符"
					m.idLabelInput.Focus()
					return m, textinput.Blink
				}
				ch := &m.idChoices[m.idCursor]
				if ch.imported && ch.profile.ID != "" {
					if err := identity.UpdateLabel(m.opts.ExecutableDir, ch.profile.ID, trimmed); err != nil {
						m.statusToast = err.Error()
						m.idLabelInput.Focus()
						return m, textinput.Blink
					}
					if m.activeID != "" && ch.profile.ID == m.activeID {
						m.identityLabel = trimmed
					}
					m.statusToast = "已更新身份标签：" + trimmed
				} else {
					m.statusToast = "已暂存身份标签，激活导入后持久化：" + trimmed
				}
				ch.profile.Label = trimmed
				m.editingIDLabel = false
				m.idLabelInput.Blur()
				return m, nil
			}
			var cmd tea.Cmd
			m.idLabelInput, cmd = m.idLabelInput.Update(msg)
			return m, cmd
		}
		if k == "esc" {
			m.modal = modalNone
			m.pendingSwitch = nil
			m.editingIDLabel = false
			m.idInputFocus = false
			m.idInput.Blur()
			m.idLabelInput.Blur()
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
				if path == "" {
					m.statusToast = "请输入要导入的配置文件或目录路径"
					return m, nil
				}
				m.idInput.SetValue("")
				return m, m.beginIdentitySwitch(identityChoice{}, path, false)
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
		case "e":
			if len(m.idChoices) > 0 && m.idCursor < len(m.idChoices) {
				m.editingIDLabel = true
				m.idLabelInput.SetValue(m.idChoices[m.idCursor].profile.Label)
				m.idLabelInput.Focus()
				m.idLabelInput.CursorEnd()
				m.statusToast = ""
				return m, textinput.Blink
			}
		case "t":
			if len(m.idChoices) > 0 && m.idCursor < len(m.idChoices) {
				if m.testingConn {
					m.statusToast = "正在测试连接，请稍候…"
					return m, nil
				}
				ch := m.idChoices[m.idCursor]
				m.testingConn = true
				m.idTestResults[ch.profile.Path] = identityTestState{testing: true}
				m.statusToast = "正在测试 mTLS 握手与身份认证…"
				return m, m.testIdentityConnCmd(ch)
			}
		case "enter":
			if len(m.idChoices) > 0 && m.idCursor < len(m.idChoices) {
				return m, m.beginIdentitySwitch(m.idChoices[m.idCursor], "", false)
			}
		}
		return m, nil

	case modalConflict:
		switch k {
		case "esc":
			m.modal = modalNone
			m.pendingSpecs = nil
			m.conflictNames = nil
			m.statusToast = ""
			return m, nil
		case "r", "a":
			specs := m.resolveConflictsAutoRename()
			m.clearSelectionForDir(m.pendingBatchDir)
			m.modal = modalNone
			m.pendingSpecs = nil
			m.conflictNames = nil
			m.statusToast = ""
			return m, m.enqueueSpecsCmd(specs)
		case "s":
			specs := m.resolveConflictsSkip()
			m.clearSelectionForDir(m.pendingBatchDir)
			m.modal = modalNone
			m.pendingSpecs = nil
			m.conflictNames = nil
			m.statusToast = ""
			return m, m.enqueueSpecsCmd(specs)
		}
		return m, nil

	case modalRename:
		switch k {
		case "esc":
			m.modal = modalNone
			m.renameTask = nil
			m.statusToast = ""
			return m, nil
		case "enter":
			t := m.renameTask
			if t == nil {
				m.modal = modalNone
				return m, nil
			}
			newName := strings.TrimSpace(m.renameInput.Value())
			if newName == "" {
				m.statusToast = "新文件名不能为空"
				m.renameInput.Focus()
				return m, textinput.Blink
			}
			if err := protocol.ValidateName(newName); err != nil {
				m.statusToast = "文件名无效：" + err.Error()
				m.renameInput.Focus()
				return m, textinput.Blink
			}
			spec := queue.Spec{Direction: t.Direction, Remote: t.Remote, Path: t.Path, Size: t.Size}
			if t.Direction == protocol.DirectionUpload {
				spec.Remote = newName
				for _, f := range m.remoteEntries {
					if f.FileName == newName {
						m.statusToast = "远端已存在同名文件：" + newName
						m.renameInput.Focus()
						return m, textinput.Blink
					}
				}
			} else {
				spec.Path = filepath.Join(filepath.Dir(t.Path), newName)
				if _, err := os.Lstat(spec.Path); err == nil {
					m.statusToast = "本地目录已存在同名文件：" + newName
					m.renameInput.Focus()
					return m, textinput.Blink
				} else if !os.IsNotExist(err) {
					m.statusToast = err.Error()
					m.renameInput.Focus()
					return m, textinput.Blink
				}
			}
			if m.manager == nil {
				m.statusToast = "当前队列不可用，请先激活身份"
				return m, nil
			}
			tasks, err := m.manager.Enqueue([]queue.Spec{spec})
			if err != nil {
				m.statusToast = err.Error()
				m.renameInput.Focus()
				return m, textinput.Blink
			}
			m.modal = modalNone
			m.renameTask = nil
			m.recordEnqueuedBatch(tasks)
			m.refreshQueueSnapshots()
			m.statusToast = fmt.Sprintf("已入队 %d 个传输任务", len(tasks))
			return m, nil
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
			if m.opBusy {
				m.statusToast = "已有退出或批量操作正在执行，请稍候…"
				return m, nil
			}
			m.modal = modalNone
			m.opBusy = true
			m.opLabel = "正在暂停并等待检查点确认"
			m.opStart = time.Now()
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
			m.pendingLocalFocusPath = m.localDir
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
				if e.name == "../" {
					m.pendingLocalFocusPath = m.localDir
				} else {
					m.pendingLocalFocusPath = ""
				}
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
			m.infoScroll = 0
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
			m.infoScroll = 0
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
		if t := m.selectedTask(); t != nil {
			if t.State != queue.Paused && t.State != queue.Pending {
				m.statusToast = "仅已暂停或待处理任务可改名另存重传"
				return m, nil
			}
			m.renameTask = t
			if t.Direction == protocol.DirectionUpload {
				m.renameInput.SetValue(t.Remote)
			} else {
				m.renameInput.SetValue(filepath.Base(t.Path))
			}
			m.statusToast = ""
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
	m.infoScroll = 0
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
	hasUnfinished := false
	for _, t := range m.activeTasks {
		if !t.Terminal() {
			hasUnfinished = true
			break
		}
	}
	if !hasUnfinished {
		m.recoveryNotice = ""
	}
	m.clampQueueCursor()
	m.evaluateBatches()

	refreshFiles := false
	for _, t := range m.activeTasks {
		if t.State == queue.Completed && !m.completedSeen[t.ID] {
			m.completedSeen[t.ID] = true
			refreshFiles = true
		}
	}
	return refreshFiles
}

func (m *model) recordEnqueuedBatch(tasks []queue.Task) {
	if len(tasks) == 0 {
		return
	}
	if m.activeBatches == nil {
		m.activeBatches = make(map[string]*batchTracker)
	}
	batchID := tasks[0].Batch
	if batchID == "" {
		batchID = tasks[0].ID
	}
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	m.activeBatches[batchID] = &batchTracker{
		id:      batchID,
		started: time.Now(),
		taskIDs: ids,
	}
}

func (m *model) evaluateBatches() {
	if len(m.activeBatches) == 0 {
		return
	}
	byID := make(map[string]queue.Task, len(m.activeTasks)+len(m.historyTasks))
	for _, t := range m.activeTasks {
		byID[t.ID] = t
	}
	for _, t := range m.historyTasks {
		byID[t.ID] = t
	}

	for id, bt := range m.activeBatches {
		var s batchSummary
		s.BatchID = id
		s.Started = bt.started
		s.Total = len(bt.taskIDs)
		settled := true
		for _, tid := range bt.taskIDs {
			t, ok := byID[tid]
			if !ok {
				continue
			}
			switch {
			case t.Terminal():
				switch t.State {
				case queue.Completed:
					s.Succeeded++
					s.TotalBytes += t.Size
				case queue.Skipped:
					s.Skipped++
				case queue.Cancelled:
					if t.Error != "" {
						s.Failed++
						s.FailedList = append(s.FailedList, batchFailedItem{Direction: t.Direction, Remote: t.Remote, Path: t.Path, Error: t.Error})
					} else {
						s.Cancelled++
					}
				}
			case !t.State.Active() && t.Error != "":
				// preflight 或执行出错转入 Pending/Paused 状态
				s.Failed++
				s.FailedList = append(s.FailedList, batchFailedItem{Direction: t.Direction, Remote: t.Remote, Path: t.Path, Error: t.Error})
			default:
				settled = false
			}
		}
		if !settled {
			continue
		}
		s.Finished = time.Now()
		delete(m.activeBatches, id)
		sCopy := s
		m.lastBatch = &sCopy
		m.batchHistory = append([]batchSummary{s}, m.batchHistory...)
		if len(m.batchHistory) > 20 {
			m.batchHistory = m.batchHistory[:20]
		}
		if s.Failed > 0 {
			m.statusToast = fmt.Sprintf("批次完成：成功 %d · 失败 %d · 跳过 %d（按 B 查摘要 / R 重失败）", s.Succeeded, s.Failed, s.Skipped)
		} else {
			m.statusToast = fmt.Sprintf("批次完成：成功 %d · 跳过 %d（按 B 查看摘要）", s.Succeeded, s.Skipped)
		}
	}
}

func (m *model) failedTaskCount() int {
	n := 0
	for _, t := range m.activeTasks {
		if !t.WantCancel && !t.State.Active() && t.Error != "" && (t.State == queue.Pending || t.State == queue.Paused) {
			n++
		}
	}
	return n
}

func (m *model) retryFailedTasksCmd() tea.Cmd {
	if m.manager == nil {
		m.statusToast = "请先配置或导入身份（按 i）"
		return nil
	}
	if m.opBusy {
		m.statusToast = "已有队列控制操作正在执行，请稍候…"
		return nil
	}
	var ids []string
	for _, t := range m.activeTasks {
		if !t.WantCancel && !t.State.Active() && t.Error != "" && (t.State == queue.Pending || t.State == queue.Paused) {
			ids = append(ids, t.ID)
		}
	}
	if len(ids) == 0 {
		m.statusToast = "当前队列没有失败或报错待处理的任务"
		return nil
	}
	mgr := m.manager
	m.opBusy = true
	m.opLabel = fmt.Sprintf("正在重试 %d 个失败任务", len(ids))
	m.opStart = time.Now()
	return func() tea.Msg {
		resumed := 0
		var firstErr error
		for _, id := range ids {
			if err := mgr.Resume(id); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			} else {
				resumed++
			}
		}
		if resumed == 0 && firstErr != nil {
			return batchOpResultMsg{err: firstErr}
		}
		notice := fmt.Sprintf("已重新提交 %d 个失败任务继续传输", resumed)
		if firstErr != nil {
			notice += "（部分失败：" + firstErr.Error() + "）"
		}
		return batchOpResultMsg{notice: notice}
	}
}

func (m *model) batchSummaryModalData() (title string, lines []string, footer string) {
	b := m.lastBatch
	if b == nil && len(m.batchHistory) > 0 {
		b = &m.batchHistory[0]
	}
	if b == nil {
		return "批次传输结果摘要", []string{"当前会话尚无已完成批次。"}, "Enter / Esc 关闭"
	}
	lines = []string{
		fmt.Sprintf("批次统计：总计 %d · 成功 %d · 失败 %d · 取消 %d · 跳过 %d", b.Total, b.Succeeded, b.Failed, b.Cancelled, b.Skipped),
		fmt.Sprintf("累计传输：%s · 耗时 %s", progress.FormatBytes(b.TotalBytes), b.Finished.Sub(b.Started).Round(time.Millisecond)),
	}
	if len(b.FailedList) > 0 {
		lines = append(lines, "", m.th.badgeErr.Render(fmt.Sprintf("失败项清单 (%d 项，按 [R] 一键重试)：", len(b.FailedList))))
		for _, item := range b.FailedList {
			lines = append(lines, fmt.Sprintf("  • %s %s — %s", directionIcon(item.Direction), item.Remote, item.Error))
		}
	} else {
		lines = append(lines, "", m.th.badgeOk.Render("✔ 本批无失败项"))
	}
	return "批次传输结果摘要", lines, "↑↓ 滚动 · R 重试失败项 · Enter / Esc 关闭"
}

func clampSortKind(v int) sortKind {
	if v < int(sortNameAsc) || v > int(sortSizeAsc) {
		return sortNameAsc
	}
	return sortKind(v)
}

func (m *model) currentPrefs() uiPrefs {
	return uiPrefs{
		Light:       !m.th.dark,
		LocalOrder:  int(m.localSort),
		RemoteOrder: int(m.remoteSort),
	}
}

func (m *model) activeSortLabel() string {
	if m.focus == areaRemote {
		return "远端:" + m.sortLabel(m.remoteSort)
	}
	return "本地:" + m.sortLabel(m.localSort)
}

func (m *model) sortLabel(mode sortKind) string {
	switch mode {
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
	focusPath := m.pendingLocalFocusPath
	m.pendingLocalFocusPath = ""
	if focusPath == "" && m.localCursor >= 0 && m.localCursor < len(m.visibleLocal) {
		focusPath = m.visibleLocal[m.localCursor].path
	}

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
		switch m.localSort {
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
	if focusPath != "" {
		for i, e := range out {
			if e.path == focusPath {
				m.localCursor = i
				return
			}
		}
	}
	if m.localCursor >= len(out) {
		m.localCursor = max(0, len(out)-1)
	}
}

func (m *model) rebuildRemoteVisible() {
	focusName := ""
	if m.remoteCursor >= 0 && m.remoteCursor < len(m.visibleRemote) {
		focusName = m.visibleRemote[m.remoteCursor].FileName
	}

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
		switch m.remoteSort {
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
	if focusName != "" {
		for i, f := range out {
			if f.FileName == focusName {
				m.remoteCursor = i
				return
			}
		}
	}
	if m.remoteCursor >= len(out) {
		m.remoteCursor = max(0, len(out)-1)
	}
}

func (m *model) clearSelectionForDir(dir protocol.Direction) {
	if dir == protocol.DirectionUpload {
		m.localSelected = make(map[string]bool)
	} else {
		m.remoteSelected = make(map[string]bool)
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

	specs := make([]queue.Spec, 0, len(chosen))
	for _, e := range chosen {
		specs = append(specs, queue.Spec{
			Direction: protocol.DirectionUpload,
			Remote:    filepath.Base(e.path),
			Path:      e.path,
			Size:      uint64(max(0, e.size)),
		})
	}
	return m.prepareBatch(protocol.DirectionUpload, specs, true)
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

	specs := make([]queue.Spec, 0, len(chosen))
	for _, f := range chosen {
		specs = append(specs, queue.Spec{
			Direction: protocol.DirectionDownload,
			Remote:    f.FileName,
			Path:      filepath.Join(m.localDir, f.FileName),
			Size:      f.Size,
		})
	}
	return m.prepareBatch(protocol.DirectionDownload, specs, true)
}

func (m *model) prepareBatch(dir protocol.Direction, specs []queue.Spec, clearSel bool) tea.Cmd {
	var conflicts []string
	if dir == protocol.DirectionUpload {
		remoteSet := make(map[string]bool, len(m.remoteEntries))
		for _, f := range m.remoteEntries {
			remoteSet[f.FileName] = true
		}
		for _, s := range specs {
			if remoteSet[s.Remote] {
				conflicts = append(conflicts, s.Remote)
			}
		}
	} else {
		for _, s := range specs {
			if _, err := os.Lstat(s.Path); err == nil {
				conflicts = append(conflicts, filepath.Base(s.Path))
			}
		}
	}
	if len(conflicts) > 0 {
		m.pendingBatchDir = dir
		m.pendingSpecs = specs
		m.conflictNames = conflicts
		m.statusToast = ""
		m.modal = modalConflict
		return nil
	}
	if clearSel {
		m.clearSelectionForDir(dir)
	}
	return m.enqueueSpecsCmd(specs)
}

func (m *model) resolveConflictsAutoRename() []queue.Spec {
	used := make(map[string]bool)
	ignoreCase := m.pendingBatchDir != protocol.DirectionUpload && runtime.GOOS == "windows"
	if m.pendingBatchDir == protocol.DirectionUpload {
		for _, f := range m.remoteEntries {
			used[f.FileName] = true
		}
	} else {
		for _, e := range m.localEntries {
			k := e.name
			if ignoreCase {
				k = strings.ToLower(k)
			}
			used[k] = true
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
			name := uniqueNameFold(base, used, ignoreCase)
			k := name
			if ignoreCase {
				k = strings.ToLower(k)
			}
			used[k] = true
			s.Path = filepath.Join(filepath.Dir(s.Path), name)
		}
		out = append(out, s)
	}
	return out
}

func (m *model) resolveConflictsSkip() []queue.Spec {
	conflictSet := make(map[string]bool, len(m.conflictNames))
	ignoreCase := m.pendingBatchDir != protocol.DirectionUpload && runtime.GOOS == "windows"
	for _, c := range m.conflictNames {
		if ignoreCase {
			conflictSet[strings.ToLower(c)] = true
		} else {
			conflictSet[c] = true
		}
	}
	out := make([]queue.Spec, 0, len(m.pendingSpecs))
	for _, s := range m.pendingSpecs {
		key := s.Remote
		if s.Direction == protocol.DirectionDownload {
			key = filepath.Base(s.Path)
			if ignoreCase {
				key = strings.ToLower(key)
			}
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
	return uniqueNameFold(orig, used, false)
}

func uniqueNameFold(orig string, used map[string]bool, ignoreCase bool) string {
	isUsed := func(name string) bool {
		if ignoreCase {
			return used[strings.ToLower(name)]
		}
		return used[name]
	}
	if !isUsed(orig) {
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
		if len(cand) <= protocol.MaxFileNameBytes && protocol.ValidateName(cand) == nil && !isUsed(cand) {
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
		return batchOpResultMsg{notice: fmt.Sprintf("已入队 %d 个传输任务", len(tasks)), enqueued: tasks}
	}
}

func resumePendingOrPausedTask(mgr *queue.Manager, task queue.Task) tea.Msg {
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
	if live := mgr.Task(t.ID); live != nil {
		if live.Terminal() {
			return nil
		}
		task = *live
	}
	return func() tea.Msg {
		switch {
		case task.State.Active():
			if err := mgr.Pause(task.ID); err != nil {
				return batchOpResultMsg{err: err}
			}
			return batchOpResultMsg{notice: "已请求暂停：" + task.Remote}
		case task.State == queue.Paused || task.State == queue.Pending:
			return resumePendingOrPausedTask(mgr, task)
		}
		return batchOpResultMsg{}
	}
}

// resumeOrRetrySelectedCmd 在队列/历史区按 F5 时根据选中任务状态执行恢复、重试或重传。
func (m *model) resumeOrRetrySelectedCmd() tea.Cmd {
	if m.manager == nil {
		m.statusToast = "请先配置或导入身份（按 i）"
		return nil
	}
	t := m.selectedTask()
	if t == nil {
		m.statusToast = "当前队列无选中任务"
		return nil
	}
	task := *t
	if live := m.manager.Task(t.ID); live != nil {
		task = *live
	}
	if m.historyView || task.Terminal() {
		spec := queue.Spec{Direction: task.Direction, Remote: task.Remote, Path: task.Path, Size: task.Size}
		return m.prepareBatch(task.Direction, []queue.Spec{spec}, false)
	}
	if task.State == queue.Paused || task.State == queue.Pending {
		mgr := m.manager
		return func() tea.Msg {
			return resumePendingOrPausedTask(mgr, task)
		}
	}
	m.statusToast = fmt.Sprintf("任务当前正在进行（%s），无需恢复或重试", task.State)
	return nil
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
	if m.opBusy {
		m.statusToast = "已有队列控制操作正在执行，请稍候…"
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
	m.opBusy = true
	m.opStart = time.Now()
	if hasActive {
		m.opLabel = "正在全部暂停"
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := mgr.PauseAll(ctx); err != nil {
				return batchOpResultMsg{err: err}
			}
			return batchOpResultMsg{notice: "已全部暂停"}
		}
	}
	m.opLabel = "正在全部恢复"
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
	return m.prepareBatch(t.Direction, []queue.Spec{spec}, false)
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
