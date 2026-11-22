package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"sft-tui/internal/identity"
	"sft-tui/internal/pal"
	"sft-tui/internal/progress"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
)

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m.requestExit()
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
	case "esc":
		if m.query != "" {
			m.query = ""
			m.searchInput.SetValue("")
			m.rebuildLocalVisible()
			m.rebuildRemoteVisible()
			m.setToast("已清空筛选条件")
			return m, nil
		}
		if m.focus == areaLocal && len(m.localSelected) > 0 {
			m.localSelected = make(map[string]bool)
			m.setToast("已清空本地多选")
			return m, nil
		}
		if m.focus == areaRemote && len(m.remoteSelected) > 0 {
			m.remoteSelected = make(map[string]bool)
			m.setToast("已清空远端多选")
			return m, nil
		}
		return m, nil
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
	case "v":
		m.focus = areaQueue
		m.historyView = !m.historyView
		m.clampQueueCursor()
		return m, nil
	case "h":
		if m.focus == areaQueue && m.historyView {
			m.historyView = false
		} else {
			m.focus = areaQueue
			m.historyView = true
		}
		m.clampQueueCursor()
		return m, nil
	case "/":
		m.editingSearch = true
		m.searchInput.Focus()
		return m, textinput.Blink
	case "p":
		if m.focus == areaQueue {
			if !m.historyView {
				return m, m.pauseSelectedCmd()
			}
			return m, nil
		}
		m.editingPath = true
		m.pathInput.SetValue(m.localDir)
		m.pathInput.Focus()
		m.pathInput.CursorEnd()
		return m, textinput.Blink
	case "o":
		switch m.focus {
		case areaLocal:
			m.localSort = (m.localSort + 1) % 4
			m.rebuildLocalVisible()
			m.setToast("本地排序：" + m.sortLabel(m.localSort))
			return m, saveUIPrefsCmd(m.prefsPath, m.currentPrefs())
		case areaRemote:
			m.remoteSort = (m.remoteSort + 1) % 4
			m.rebuildRemoteVisible()
			m.setToast("远端排序：" + m.sortLabel(m.remoteSort))
			return m, saveUIPrefsCmd(m.prefsPath, m.currentPrefs())
		default:
			m.setToast("排序仅适用于本地或远端文件列表")
			return m, nil
		}
	case "r":
		if m.focus == areaQueue && m.historyView {
			return m, m.retryHistoryCmd()
		}
		return m, tea.Batch(m.loadLocalCmd(m.localDir), m.loadRemoteCmd())
	case "B":
		if m.lastBatch == nil && len(m.batchHistory) == 0 {
			m.setToast("当前会话暂无已完成批次结果摘要")
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
		m.clearToast()
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
	scrollInfoModalStep(m.width, m.height, m.th, title, lines, footer, &m.infoScroll, k)
}

func (m *model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || m.width <= 0 || m.height <= 0 {
		return m, nil
	}
	if m.modal == modalHelp || m.modal == modalDetail || m.modal == modalBatchSummary || m.modal == modalConflict || m.modal == modalDeleteRemote {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.scrollClientInfoModal("up")
		case tea.MouseButtonWheelDown:
			m.scrollClientInfoModal("down")
		}
		return m, nil
	}
	if m.modal == modalIdentity {
		if m.pendingSwitch != nil || m.editingIDLabel || m.activatingID {
			return m, nil
		}
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
			boxW, _ := modalBoxDimensions(m.width)
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
			} else if relY == inputRelY {
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
		if m.focus == areaQueue {
			if msg.Y == bodyY+2 && msg.Button == tea.MouseButtonLeft {
				qLabels := []string{fmt.Sprintf("活动队列 (%d)", len(m.activeTasks)), fmt.Sprintf("历史归档 (%d)", len(m.historyTasks))}
				activeQ := 0
				if m.historyView {
					activeQ = 1
				}
				if idx := tabHitIndexGap(msg.X, 2, 1, qLabels, m.th, activeQ); idx >= 0 {
					m.historyView = idx == 1
					m.clampQueueCursor()
				}
				return m, nil
			}
			if !m.historyView && m.recoveryNotice != "" && len(m.activeTasks) > 0 && visRows >= 3 {
				visRows--
				relY--
			}
		}
		switch m.focus {
		case areaLocal:
			if hasTableHeader(visRows, len(m.visibleLocal)) {
				visRows--
				relY--
			}
			moveList(&m.localCursor, len(m.visibleLocal), visRows, relY)
		case areaRemote:
			if hasTableHeader(visRows, len(m.visibleRemote)) {
				visRows--
				relY--
			}
			moveList(&m.remoteCursor, len(m.visibleRemote), visRows, relY)
		default:
			tasks := m.currentQueueTasks()
			if hasTableHeader(visRows, len(tasks)) {
				visRows--
				relY--
			}
			moveList(&m.queueCursor, len(tasks), visRows, relY)
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
			wasLocal := m.focus == areaLocal
			prevIdx := m.localCursor
			m.focus = areaLocal
			if hasTableHeader(visRows, len(m.visibleLocal)) {
				visRows--
				relY--
			}
			moveList(&m.localCursor, len(m.visibleLocal), visRows, relY)
			// 再次单击已聚焦的目录项直接进入目录，与身份弹窗双击确认保持一致
			if msg.Button == tea.MouseButtonLeft && wasLocal && relY >= 0 && relY < visRows && m.localCursor == prevIdx && m.localCursor < len(m.visibleLocal) {
				if e := m.visibleLocal[m.localCursor]; e.dir {
					if e.name == "../" {
						m.pendingLocalFocusPath = m.localDir
					} else {
						m.pendingLocalFocusPath = ""
					}
					return m, m.loadLocalCmd(e.path)
				}
			}
		} else {
			m.focus = areaRemote
			if hasTableHeader(visRows, len(m.visibleRemote)) {
				visRows--
				relY--
			}
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
		if idx := tabHitIndexGap(msg.X, 2, 1, qLabels, m.th, activeQ); idx >= 0 {
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
	tasks := m.currentQueueTasks()
	if hasTableHeader(visRows, len(tasks)) {
		visRows--
		queueRelY--
	}
	moveList(&m.queueCursor, len(tasks), visRows, queueRelY)
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
				m.clearToast()
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
				m.clearToast()
				return m, nil
			case "enter":
				if m.idCursor < 0 || m.idCursor >= len(m.idChoices) {
					m.editingIDLabel = false
					m.idLabelInput.Blur()
					return m, nil
				}
				trimmed := strings.TrimSpace(m.idLabelInput.Value())
				if trimmed == "" || len([]rune(trimmed)) > 80 || strings.IndexFunc(trimmed, unicode.IsControl) >= 0 {
					m.setToastSticky("身份标签须为 1–80 个字符且不能包含控制字符")
					m.idLabelInput.Focus()
					return m, textinput.Blink
				}
				ch := &m.idChoices[m.idCursor]
				if ch.imported && ch.profile.ID != "" {
					if err := identity.UpdateLabel(m.opts.ExecutableDir, ch.profile.ID, trimmed); err != nil {
						m.setToastSticky(err.Error())
						m.idLabelInput.Focus()
						return m, textinput.Blink
					}
					if m.activeID != "" && ch.profile.ID == m.activeID {
						m.identityLabel = trimmed
					}
					m.setToastSticky("已更新身份标签：" + trimmed)
				} else {
					m.setToastSticky("已暂存身份标签，激活导入后持久化：" + trimmed)
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
			if m.activatingID {
				m.setToastSticky("正在切换或导入身份，请稍候…")
				return m, nil
			}
			m.modal = modalNone
			m.pendingSwitch = nil
			m.editingIDLabel = false
			m.idInputFocus = false
			m.idInput.Blur()
			m.idLabelInput.Blur()
			m.clearToast()
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
					m.setToastSticky("请输入要导入的配置文件或目录路径")
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
				m.clearToast()
				return m, textinput.Blink
			}
		case "t":
			if len(m.idChoices) > 0 && m.idCursor < len(m.idChoices) {
				if m.testingConn {
					m.setToastSticky("正在测试连接，请稍候…")
					return m, nil
				}
				ch := m.idChoices[m.idCursor]
				m.testingConn = true
				m.idTestResults[ch.profile.Path] = identityTestState{testing: true}
				m.setToastSticky("正在测试 mTLS 握手与身份认证…")
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
			m.pendingClearSel = false
			m.clearToast()
			return m, nil
		case "r", "a":
			specs := m.resolveConflictsAutoRename()
			clearSel := m.pendingClearSel
			m.pendingClearSel = false
			if clearSel {
				m.clearSelectionForDir(m.pendingBatchDir)
			}
			m.modal = modalNone
			m.pendingSpecs = nil
			m.conflictNames = nil
			m.clearToast()
			return m, m.enqueueSpecsCmd(specs)
		case "s":
			specs := m.resolveConflictsSkip()
			clearSel := m.pendingClearSel
			m.pendingClearSel = false
			if clearSel {
				m.clearSelectionForDir(m.pendingBatchDir)
			}
			m.modal = modalNone
			m.pendingSpecs = nil
			m.conflictNames = nil
			m.clearToast()
			return m, m.enqueueSpecsCmd(specs)
		}
		m.scrollClientInfoModal(k)
		return m, nil

	case modalRename:
		switch k {
		case "esc":
			m.modal = modalNone
			m.renameTask = nil
			m.clearToast()
			return m, nil
		case "enter":
			t := m.renameTask
			if t == nil {
				m.modal = modalNone
				return m, nil
			}
			newName := strings.TrimSpace(m.renameInput.Value())
			if newName == "" {
				m.setToastSticky("新文件名不能为空")
				m.renameInput.Focus()
				return m, textinput.Blink
			}
			if err := protocol.ValidateName(newName); err != nil {
				m.setToastSticky("文件名无效：" + err.Error())
				m.renameInput.Focus()
				return m, textinput.Blink
			}
			spec := queue.Spec{Direction: t.Direction, Remote: t.Remote, Path: t.Path, Size: t.Size}
			if t.Direction == protocol.DirectionUpload {
				spec.Remote = newName
				for _, f := range m.remoteEntries {
					if f.FileName == newName {
						m.setToastSticky("远端已存在同名文件：" + newName)
						m.renameInput.Focus()
						return m, textinput.Blink
					}
				}
			} else {
				spec.Path = filepath.Join(filepath.Dir(t.Path), newName)
				if _, err := os.Lstat(spec.Path); err == nil {
					m.setToastSticky("本地目录已存在同名文件：" + newName)
					m.renameInput.Focus()
					return m, textinput.Blink
				} else if !os.IsNotExist(err) {
					m.setToastSticky(err.Error())
					m.renameInput.Focus()
					return m, textinput.Blink
				}
			}
			if m.manager == nil {
				m.setToastSticky("当前队列不可用，请先激活身份")
				return m, nil
			}
			tasks, err := m.manager.Enqueue([]queue.Spec{spec})
			if err != nil {
				m.setToastSticky(err.Error())
				m.renameInput.Focus()
				return m, textinput.Blink
			}
			m.modal = modalNone
			m.renameTask = nil
			m.recordEnqueuedBatch(tasks)
			m.refreshQueueSnapshots()
			m.setToast(fmt.Sprintf("已入队 %d 个传输任务", len(tasks)))
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
				m.setToast("已有退出或批量操作正在执行，请稍候…")
				return m, nil
			}
			m.modal = modalNone
			m.opBusy = true
			m.opLabel = "正在暂停并等待检查点确认"
			m.opStart = time.Now()
			m.setToastSticky("正在等待服务端确认暂停检查点后退出…")
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
					m.setToastError(err.Error())
				} else {
					m.setToast("已清空历史记录")
				}
			}
		}
		return m, nil

	case modalDeleteRemote:
		switch k {
		case "esc", "n":
			m.modal = modalNone
			m.pendingDeleteNames = nil
			m.clearToast()
		case "enter", "y":
			if m.opBusy {
				m.setToast("已有操作正在执行，请稍候…")
				return m, nil
			}
			names := append([]string(nil), m.pendingDeleteNames...)
			m.modal = modalNone
			m.pendingDeleteNames = nil
			if len(names) == 0 {
				return m, nil
			}
			m.opBusy = true
			m.opLabel = "正在删除远端文件"
			m.opStart = time.Now()
			return m, m.deleteRemoteFilesCmd(names)
		case "up", "k", "down", "j", "pgup", "b", "pgdown", "f", " ", "home", "g", "end", "G":
			m.scrollClientInfoModal(k)
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
	if m.activatingID {
		m.setToastSticky("正在切换或导入身份，请稍候…")
		return m, nil
	}
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
	case "ctrl+a", "A":
		selectable := 0
		selected := 0
		for _, e := range m.visibleLocal {
			if e.regular {
				selectable++
				if m.localSelected[e.path] {
					selected++
				}
			}
		}
		if selectable == 0 {
			m.setToast("当前列表无可勾选的普通文件")
			return m, nil
		}
		if selected == selectable {
			for _, e := range m.visibleLocal {
				if e.regular {
					delete(m.localSelected, e.path)
				}
			}
			m.setToast("已取消全选本地文件")
		} else {
			for _, e := range m.visibleLocal {
				if e.regular {
					m.localSelected[e.path] = true
				}
			}
			m.setToast(fmt.Sprintf("已全选 %d 个本地文件", selectable))
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
				"名称：" + sanitizeInline(e.name),
				"路径：" + sanitizeInline(e.path),
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
	case "ctrl+a", "A":
		if n == 0 {
			m.setToast("当前远端列表为空")
			return m, nil
		}
		selected := 0
		for _, f := range m.visibleRemote {
			if m.remoteSelected[f.FileName] {
				selected++
			}
		}
		if selected == n {
			for _, f := range m.visibleRemote {
				delete(m.remoteSelected, f.FileName)
			}
			m.setToast("已取消全选远端文件")
		} else {
			for _, f := range m.visibleRemote {
				m.remoteSelected[f.FileName] = true
			}
			m.setToast(fmt.Sprintf("已全选 %d 个远端文件", n))
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
	case "x", "delete":
		m.prepareDeleteRemote()
	case "enter":
		if n == 0 && m.remoteErr != "" {
			m.detailTitle = "远端连接错误详情"
			m.detailLines = []string{
				"错误说明：",
				"  " + sanitizeInline(m.remoteErr),
				"",
				"排查建议：",
				"  • 按 r 重新刷新远端连接",
				"  • 按 i 打开身份管理并按 t 测试 mTLS 握手",
			}
			m.infoScroll = 0
			m.modal = modalDetail
			return m, nil
		}
		if m.remoteCursor >= 0 && m.remoteCursor < n {
			f := m.visibleRemote[m.remoteCursor]
			m.detailTitle = "远端文件信息"
			m.detailLines = []string{
				"文件名：" + sanitizeInline(f.FileName),
				fmt.Sprintf("大小：  %s (%d B)", progress.FormatBytes(f.Size), f.Size),
				fmt.Sprintf("SHA-256：%x", f.SHA256),
				"",
				"按 d 或 F5 下载至当前本地目录（" + sanitizeInline(m.localDir) + "）",
				"按 x 或 Del 可永久删除该远端文件",
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
	case "home", "g":
		m.queueCursor = 0
	case "end", "G":
		if n > 0 {
			m.queueCursor = n - 1
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
				m.setToast("仅已暂停或待处理任务可改名另存重传")
				return m, nil
			}
			m.renameTask = t
			if t.Direction == protocol.DirectionUpload {
				m.renameInput.SetValue(t.Remote)
			} else {
				base := filepath.Base(t.Path)
				if pal.ValidateLocalFilename(base) != nil {
					base = pal.SanitizeWindowsFilename(base)
				}
				m.renameInput.SetValue(base)
			}
			m.clearToast()
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
