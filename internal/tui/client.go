package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"sft-tui/internal/config"
	"sft-tui/internal/identity"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
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
type identityInitStage uint8

const (
	identityStageLookup identityInitStage = iota
	identityStageTLS
	identityStageQueue
)

type identityActivatedMsg struct {
	cfg      config.Client
	hasCfg   bool
	label    string
	idKey    string
	activeID string
	stage    identityInitStage
	manager  *queue.Manager
	runner   *transfer.QueueRunner
	resetOld bool
	err      error
}
type batchOpResultMsg struct {
	notice     string
	err        error
	quit       bool
	isGlobalOp bool
	enqueued   []queue.Task
}
type remoteDeletedMsg struct {
	deleted  []string
	failed   []string
	firstErr error
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
	cfg               *config.Client
	identityLabel     string
	idKey             string
	activeID          string
	manager           *queue.Manager
	runner            *transfer.QueueRunner
	connStatus        string
	idInitFailedStage identityInitStage
	idInitFailedLabel string

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
	toastLevel    toastLevel
	toastUntil    time.Time

	// 弹窗辅助数据
	detailTitle     string
	detailLines     []string
	idChoices       []identityChoice
	idCursor        int
	pendingBatchDir protocol.Direction
	pendingSpecs    []queue.Spec
	pendingClearSel    bool
	conflictNames      []string
	pendingDeleteNames []string
	renameTask         *queue.Task
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
		th:             makeTheme(),
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
	m.activatingID = true
	return tea.Batch(
		m.loadLocalCmd(m.localDir),
		m.initIdentityCmd(),
		m.tickCmd(),
	)
}

func (m *model) setToast(msg string) {
	m.toastLevel = inferToastLevel(msg)
	applyToast(&m.statusToast, &m.toastUntil, msg, toastDuration)
}

func (m *model) setToastError(msg string) {
	m.toastLevel = toastError
	applyToast(&m.statusToast, &m.toastUntil, msg, toastErrorDuration)
}

func (m *model) setToastSticky(msg string) {
	m.toastLevel = inferToastLevel(msg)
	applyToast(&m.statusToast, &m.toastUntil, msg, 0)
}

func (m *model) clearToast() {
	m.setToast("")
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

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		m.spinIdx = (m.spinIdx + 1) % len(spinnerFrames)
		now := effectiveTickNow(time.Time(msg))
		if m.modal == modalNone && !m.activatingID && !m.testingConn && !m.opBusy {
			expireToast(&m.statusToast, &m.toastUntil, now)
		}
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
			m.setToastError("无法读取目录：" + msg.err.Error())
			return m, nil
		}
		prevDir := m.localDir
		m.localDir = msg.dir
		if !m.editingPath {
			m.pathInput.SetValue(msg.dir)
		}
		if msg.dir != prevDir {
			m.localSelected = make(map[string]bool)
			if m.pendingLocalFocusPath == "" {
				m.visibleLocal = nil
				m.localCursor = 0
			}
		}
		m.localEntries = msg.entries
		if len(m.localSelected) > 0 {
			live := make(map[string]bool, len(msg.entries))
			for _, e := range msg.entries {
				if e.regular {
					live[e.path] = true
				}
			}
			for path := range m.localSelected {
				if !live[path] {
					delete(m.localSelected, path)
				}
			}
		}
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
		if len(m.remoteSelected) > 0 {
			live := make(map[string]bool, len(msg.entries))
			for _, f := range msg.entries {
				live[f.FileName] = true
			}
			for name := range m.remoteSelected {
				if !live[name] {
					delete(m.remoteSelected, name)
				}
			}
		}
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
			m.setToastError(msg.err.Error())
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
		m.setToastSticky(fmt.Sprintf("目录发现 %d 个身份配置，请预览详情后按 Enter 激活或 t 测连", len(msg.choices)))
		return m, nil

	case identityConnTestMsg:
		m.testingConn = false
		if msg.err != nil {
			m.idTestResults[msg.path] = identityTestState{tested: true, latency: msg.latency, err: msg.err.Error()}
			if m.modal == modalIdentity {
				m.setToastSticky("连接测试失败：" + msg.err.Error())
			}
		} else {
			m.idTestResults[msg.path] = identityTestState{tested: true, latency: msg.latency}
			if m.modal == modalIdentity {
				m.setToastSticky(fmt.Sprintf("连接测试成功，耗时 %s", msg.latency.Round(time.Millisecond)))
			}
		}
		return m, nil

	case identityActivatedMsg:
		m.activatingID = false
		if m.opLabel == "正在切换身份" || m.opLabel == "正在导入身份" || m.opLabel == "正在扫描身份目录" {
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
				m.modal = modalIdentity
				if msg.stage == identityStageLookup && isMissingIdentityErr(msg.err) {
					m.idInitFailedStage = 0
					m.idInitFailedLabel = ""
					m.identityLabel = "未选择"
					m.connStatus = "需要选择或导入身份"
					m.setToastSticky("未找到已激活身份，请从列表选择或输入配置目录导入")
				} else {
					m.idInitFailedStage = msg.stage
					if msg.hasCfg && msg.label != "" {
						m.idInitFailedLabel = msg.label
						m.identityLabel = msg.label + " (未就绪)"
					} else {
						m.idInitFailedLabel = ""
					}
					m.connStatus = identityStageStatusText(msg.stage)
					m.setToastSticky(msg.err.Error())
				}
				return m, m.scanIdentitiesCmd()
			}
			m.setToastError(msg.err.Error())
			return m, nil
		}
		// 若与 Init 等流程并发打开，释放未被替掉的旧实例
		if m.runner != nil && m.runner != msg.runner {
			_ = m.runner.Close()
		}
		if m.manager != nil && m.manager != msg.manager {
			_ = m.manager.Close()
		}
		m.idInitFailedStage = 0
		m.idInitFailedLabel = ""
		m.cfg = &msg.cfg
		m.identityLabel = msg.label
		m.idKey = msg.idKey
		m.activeID = msg.activeID
		m.manager = msg.manager
		m.runner = msg.runner
		m.modal = modalNone
		m.connStatus = "已配置，正在拉取远端文件"
		m.remoteEntries = nil
		m.visibleRemote = nil
		m.remoteSelected = make(map[string]bool)
		m.remoteErr = ""
		m.remoteCursor = 0
		m.completedSeen = make(map[string]bool)
		m.batchHistory = nil
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
			m.setToast(m.recoveryNotice)
		} else {
			m.setToast("已加载身份：" + msg.label)
		}
		return m, m.loadRemoteCmd()

	case batchOpResultMsg:
		if msg.isGlobalOp {
			m.opBusy = false
			m.opLabel = ""
			m.opStart = time.Time{}
		}
		if msg.err != nil {
			m.setToastError(msg.err.Error())
			return m, nil
		}
		if len(msg.enqueued) > 0 {
			m.recordEnqueuedBatch(msg.enqueued)
		}
		if msg.notice != "" {
			m.setToast(msg.notice)
		} else if m.toastUntil.IsZero() {
			m.clearToast()
		}
		if len(msg.enqueued) > 0 {
			m.refreshQueueSnapshots()
		}
		if msg.quit {
			m.cleanup()
			return m, tea.Quit
		}
		return m, nil

	case remoteDeletedMsg:
		m.opBusy = false
		m.opLabel = ""
		m.opStart = time.Time{}
		for _, name := range msg.deleted {
			delete(m.remoteSelected, name)
		}
		switch {
		case len(msg.failed) == 0 && len(msg.deleted) == 1:
			m.setToast("已删除远端文件：" + msg.deleted[0])
		case len(msg.failed) == 0 && len(msg.deleted) > 1:
			m.setToast(fmt.Sprintf("已删除 %d 个远端文件", len(msg.deleted)))
		case len(msg.deleted) > 0 && len(msg.failed) > 0:
			errMsg := ""
			if msg.firstErr != nil {
				errMsg = "：" + msg.firstErr.Error()
			}
			m.setToastError(fmt.Sprintf("已删除 %d 个，%d 个失败%s", len(msg.deleted), len(msg.failed), errMsg))
		case msg.firstErr != nil:
			m.setToastError("删除失败：" + msg.firstErr.Error())
		default:
			m.setToastError("未删除任何文件")
		}
		if len(msg.deleted) > 0 {
			return m, m.loadRemoteCmd()
		}
		return m, nil

	case prefsSavedMsg:
		if msg.err != nil {
			m.setToastError("无法保存界面偏好：" + msg.err.Error())
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}
	return m, nil
}
