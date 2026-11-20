package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"sft-tui/internal/progress"
)

func (m *model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return "正在初始化界面..."
	}
	if m.width < 48 || m.height < 14 {
		return m.th.badgeWarn.Render(fmt.Sprintf("终端窗口过小（%d×%d），请至少调整至 48×14 以上（推荐 ≥ 96×28）", m.width, m.height))
	}

	header := m.renderHeader()
	toolbar := m.renderSubHeader()
	footer := m.renderFooter()
	bodyH := max(6, m.height-lipgloss.Height(header)-lipgloss.Height(toolbar)-lipgloss.Height(footer))

	var body string
	if m.width < 96 {
		body = m.renderNarrowBody(bodyH)
	} else {
		body = m.renderWideBody(bodyH)
	}

	base := lipgloss.JoinVertical(lipgloss.Left, header, toolbar, body, footer)
	if m.modal != modalNone {
		return m.viewModal(base)
	}
	return base
}

func (m *model) renderHeader() string {
	brand := m.th.badgeApp.Render(" SFT-TUI ")
	idBadge := m.th.badgeID.Render(" 身份: " + sanitizeInline(m.identityLabel) + " ")

	connStyle := m.th.badgeMuted
	connIcon := "○ "
	connText := m.connStatus
	switch {
	case m.activatingID:
		connStyle = m.th.badgeWarn
		connIcon = spinnerChar(m.spinIdx) + " "
		connText = "正在切换身份"
	case strings.Contains(m.connStatus, "已连接"):
		connStyle = m.th.badgeOk
		connIcon = "● "
	case strings.Contains(m.connStatus, "重连") || strings.Contains(m.connStatus, "正在连接"):
		connStyle = m.th.badgeWarn
		connIcon = spinnerChar(m.spinIdx) + " "
	case strings.Contains(m.connStatus, "失败") || strings.Contains(m.connStatus, "断开"):
		connStyle = m.th.badgeErr
		connIcon = "✕ "
	}
	connBadge := connStyle.Render(connIcon + truncPlainCell(connText, 26))

	var runCount, pauseCount int
	var totalRate float64
	for _, t := range m.activeTasks {
		switch {
		case t.State.Active():
			runCount++
			totalRate += t.Speed
		case t.State == "paused" || t.State == "pending":
			pauseCount++
		}
	}
	stats := m.th.badgeMuted.Render(fmt.Sprintf("进行:%d  暂停:%d  历史:%d  %s",
		runCount, pauseCount, len(m.historyTasks), progress.FormatRate(totalRate)))

	leftBudget := max(16, m.width-2-lipgloss.Width(stats)-1)
	left := truncCell(lipgloss.JoinHorizontal(lipgloss.Center, brand, " ", idBadge, " ", connBadge), leftBudget)
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(stats)-2)
	line := left + strings.Repeat(" ", gap) + stats
	return m.th.headerBar.Width(m.width).Render(truncCell(line, m.width-2))
}

func (m *model) renderSubHeader() string {
	pathView := m.th.badgeMuted.Render("目录: ") + truncPlainCell(m.localDir, max(12, m.width/3))
	if m.editingPath {
		pi := m.pathInput
		pi.Width = min(42, max(10, m.width/2-4))
		pathView = pi.View()
	}

	searchView := m.th.badgeMuted.Render("筛选(/): ")
	if m.editingSearch {
		si := m.searchInput
		si.Width = min(42, max(10, m.width/3-4))
		searchView += si.View()
	} else if m.query != "" {
		searchView += m.th.badgeOk.Render(truncPlainCell(m.query, 24))
	} else {
		searchView += m.th.badgeMuted.Render("全部")
	}

	sortLabel := m.th.badgeMuted.Render("排序(o): " + m.activeSortLabel())
	msg := ""
	switch {
	case m.opBusy && m.opLabel != "":
		msg = "  " + formatBusyBadge(m.th, m.spinIdx, m.opLabel, m.opStart)
	case m.activatingID:
		msg = "  " + formatBusyBadge(m.th, m.spinIdx, "身份切换中", m.opStart)
	case m.statusToast != "":
		msg = "  " + m.th.toast.Render("· "+truncPlainCell(m.statusToast, max(10, m.width/4)))
	}
	line := pathView + "   " + searchView + "   " + sortLabel + msg
	return lipgloss.NewStyle().Padding(0, 1).Width(m.width).Render(truncCell(line, m.width-2))
}

func (m *model) renderFooter() string {
	switch {
	case m.editingPath:
		return m.th.footerBar.Width(m.width).Render(truncCell("路径跳转：Enter 打开目录 · Esc 取消", m.width-2))
	case m.editingSearch:
		return m.th.footerBar.Width(m.width).Render(truncCell("实时筛选：输入关键字 · Enter 确定 · Esc 清空", m.width-2))
	case m.focus == areaLocal:
		upReason := ""
		upLabel := "上传"
		var curLocal *localEntry
		if m.localCursor >= 0 && m.localCursor < len(m.visibleLocal) {
			e := m.visibleLocal[m.localCursor]
			curLocal = &e
		}
		switch {
		case m.manager == nil:
			upReason = "需先选身份"
		case len(m.localSelected) > 0:
			upLabel = fmt.Sprintf("上传(%d)", len(m.localSelected))
		case curLocal == nil || !curLocal.regular:
			upReason = "需选文件"
		default:
			upLabel = "上传当前"
		}
		parentReason := ""
		if filepath.Dir(m.localDir) == m.localDir {
			parentReason = "根目录"
		}
		refreshReason := ""
		if m.loadingLocal || m.loadingRemote {
			refreshReason = "加载中"
		}
		return renderActionFooter(m.th, m.width, "本地:", []footerChip{
			{key: "u/F5", label: upLabel, reason: upReason, essential: true},
			{key: "a", label: "全暂停", reason: "仅队列区", essential: true},
			{key: "Spc", label: "多选", essential: true},
			{key: "Enter", label: "开/详"},
			{key: "Bksp", label: "上级", reason: parentReason},
			{key: "r", label: "刷新", reason: refreshReason},
			{key: "/", label: "筛选"},
			{key: "o", label: "排序"},
			{key: "p", label: "路径"},
			{key: "i", label: "身份", essential: true},
			{key: "?", label: "帮助"},
			{key: "q", label: "退出"},
		})
	case m.focus == areaRemote:
		dnReason := ""
		dnLabel := "下载"
		switch {
		case m.cfg == nil:
			dnReason = "需先选身份"
		case m.remoteErr != "":
			dnReason = "远端离线"
		case len(m.remoteSelected) > 0:
			dnLabel = fmt.Sprintf("下载(%d)", len(m.remoteSelected))
		case m.remoteCursor < 0 || m.remoteCursor >= len(m.visibleRemote):
			dnReason = "无文件"
		default:
			dnLabel = "下载当前"
		}
		refreshReason := ""
		switch {
		case m.cfg == nil:
			refreshReason = "需先选身份"
		case m.loadingRemote:
			refreshReason = "同步中"
		}
		return renderActionFooter(m.th, m.width, "远端:", []footerChip{
			{key: "d/F5", label: dnLabel, reason: dnReason, essential: true},
			{key: "a", label: "全暂停", reason: "仅队列区", essential: true},
			{key: "Spc", label: "多选", essential: true},
			{key: "Enter", label: "详情"},
			{key: "r", label: "刷新", reason: refreshReason, essential: true},
			{key: "/", label: "筛选"},
			{key: "o", label: "排序"},
			{key: "i", label: "身份", essential: true},
			{key: "?", label: "帮助"},
			{key: "q", label: "退出"},
		})
	default:
		t := m.selectedTask()
		failCount := m.failedTaskCount()
		retryReason := ""
		if failCount == 0 {
			retryReason = "无失败项"
		}
		batchReason := ""
		if m.lastBatch == nil {
			batchReason = "暂无"
		}
		if m.historyView {
			emptyReason := ""
			if len(m.historyTasks) == 0 {
				emptyReason = "无历史"
			}
			return renderActionFooter(m.th, m.width, "历史:", []footerChip{
				{key: "r/F5", label: "重传", reason: emptyReason, essential: true},
				{key: "R", label: "重失败", reason: retryReason, essential: true},
				{key: "B", label: "批次", reason: batchReason, essential: true},
				{key: "Enter", label: "详情", reason: emptyReason, essential: true},
				{key: "X", label: "清空", reason: emptyReason},
				{key: "v", label: "活动队列", essential: true},
				{key: "?", label: "帮助"},
				{key: "q", label: "退出"},
			})
		}
		spaceLabel, spaceReason := "暂停/恢复", ""
		f5Reason := ""
		renameReason := ""
		cancelReason := ""
		switch {
		case t == nil:
			spaceReason, f5Reason, renameReason, cancelReason = "无任务", "无任务", "无任务", "无任务"
		case t.State.Active():
			spaceLabel = "暂停"
			f5Reason = "进行中"
			renameReason = "需先暂停"
		case t.State == "paused" || t.State == "pending":
			spaceLabel = "恢复"
		default:
			spaceReason = "控制中"
			f5Reason = "不可重试"
			renameReason = "不可改名"
		}
		allReason := ""
		if len(m.activeTasks) == 0 {
			allReason = "队列空"
		} else if m.opBusy {
			allReason = "执行中"
		}
		return renderActionFooter(m.th, m.width, "队列:", []footerChip{
			{key: "Spc", label: spaceLabel, reason: spaceReason, essential: true},
			{key: "F5", label: "恢复/试", reason: f5Reason, essential: true},
			{key: "R", label: "重失败", reason: retryReason, essential: true},
			{key: "B", label: "批次", reason: batchReason, essential: true},
			{key: "n", label: "改名", reason: renameReason},
			{key: "c", label: "取消", reason: cancelReason},
			{key: "a", label: "全暂/恢", reason: allReason, essential: true},
			{key: "v", label: "历史"},
			{key: "?", label: "帮助"},
		})
	}
}

func (m *model) renderWideBody(bodyH int) string {
	topH := max(5, (bodyH*55)/100)
	botH := max(5, bodyH-topH)
	if topH+botH > bodyH {
		topH = bodyH - botH
	}
	leftW := m.width / 2
	rightW := m.width - leftW

	leftPanel := m.renderLocalPanel(leftW, topH)
	rightPanel := m.renderRemotePanel(rightW, topH)
	topRow := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel)
	botRow := m.renderQueuePanel(m.width, botH)
	return lipgloss.JoinVertical(lipgloss.Left, topRow, botRow)
}

func (m *model) renderNarrowBody(bodyH int) string {
	tabs := []struct {
		label  string
		active bool
	}{
		{"1 本地文件", m.focus == areaLocal},
		{"2 远端文件", m.focus == areaRemote},
		{"3 活动队列", m.focus == areaQueue && !m.historyView},
		{"4 历史归档", m.focus == areaQueue && m.historyView},
	}
	var tabStrs []string
	for _, tb := range tabs {
		if tb.active {
			tabStrs = append(tabStrs, m.th.tabActive.Render(tb.label))
		} else {
			tabStrs = append(tabStrs, m.th.tabInactive.Render(tb.label))
		}
	}
	bar := lipgloss.JoinHorizontal(lipgloss.Center, tabStrs...)
	panelH := max(5, bodyH-1)
	var pane string
	switch m.focus {
	case areaLocal:
		pane = m.renderLocalPanel(m.width, panelH)
	case areaRemote:
		pane = m.renderRemotePanel(m.width, panelH)
	default:
		pane = m.renderQueuePanel(m.width, panelH)
	}
	return lipgloss.JoinVertical(lipgloss.Left, bar, pane)
}

func renderPanelBox(th theme, title string, lines []string, active bool, outerW, outerH int) string {
	contentH := max(1, outerH-2)
	trimmed := make([]string, 0, contentH)
	trimmed = append(trimmed, title)
	for i := 0; i < contentH-1 && i < len(lines); i++ {
		trimmed = append(trimmed, lines[i])
	}
	st := th.panel
	if active {
		st = th.panelActive
	}
	return st.Width(max(4, outerW-2)).Height(max(1, outerH-2)).Render(strings.Join(trimmed, "\n"))
}

func (m *model) renderBox(title string, lines []string, active bool, outerW, outerH int) string {
	return renderPanelBox(m.th, title, lines, active, outerW, outerH)
}

// panelNameSizeWidths 计算文件面板的单行名称列与大小列宽度，保证在 80×24 等窄屏下大小与单位不换行。
func panelNameSizeWidths(innerW, fixedOverhead int) (nameW, sizeW int) {
	sizeW = 10
	if innerW < fixedOverhead+sizeW+6 {
		sizeW = max(5, innerW-fixedOverhead-6)
	}
	nameW = max(1, innerW-sizeW-fixedOverhead)
	return nameW, sizeW
}

func (m *model) renderLocalPanel(outerW, outerH int) string {
	innerW := max(10, outerW-4)
	visRows := max(1, outerH-3)

	selCount := len(m.localSelected)
	title := m.th.hintKey.Render(fmt.Sprintf("本地文件 (%d 项 · 已选 %d)", len(m.visibleLocal), selCount))
	if m.loadingLocal {
		title += " " + formatBusyBadge(m.th, m.spinIdx, "读取中", m.localLoadStarted)
	}
	title = truncCell(title, innerW)

	var lines []string
	if len(m.visibleLocal) == 0 {
		lines = append(lines, m.th.badgeMuted.Render(truncCell("（当前目录无可匹配文件）", innerW)))
	} else {
		start := scrollOffset(m.localCursor, len(m.visibleLocal), visRows)
		// 行模板: mark(3) + ' '(1) + icon(2) + name(nameW) + ' '(1) + sz(sizeW) => 固定开销 7 列
		nameW, sizeW := panelNameSizeWidths(innerW, 7)
		for i := start; i < min(len(m.visibleLocal), start+visRows); i++ {
			e := m.visibleLocal[i]
			mark := "   "
			if e.regular {
				if m.localSelected[e.path] {
					mark = "[✓]"
				} else {
					mark = "[ ]"
				}
			}
			icon := "  "
			sz := padLeftCell(progress.FormatBytes(uint64(max(0, e.size))), sizeW)
			if e.dir {
				icon = "▸ "
				sz = padLeftCell("<DIR>", sizeW)
			}
			row := truncPlainCell(fmt.Sprintf("%s %s%s %s", mark, icon, padPlainRightCell(e.name, nameW), sz), innerW)
			if i == m.localCursor && m.focus == areaLocal {
				lines = append(lines, m.th.rowCursor.Render(padRightCell(row, innerW)))
			} else if e.regular && m.localSelected[e.path] {
				lines = append(lines, m.th.rowSelected.Render(row))
			} else {
				lines = append(lines, m.th.rowNormal.Render(row))
			}
		}
	}
	return m.renderBox(title, lines, m.focus == areaLocal, outerW, outerH)
}

func (m *model) renderRemotePanel(outerW, outerH int) string {
	innerW := max(10, outerW-4)
	visRows := max(1, outerH-3)

	title := m.th.hintKey.Render(fmt.Sprintf("远端存储 (%d 项 · 已选 %d)", len(m.visibleRemote), len(m.remoteSelected)))
	if m.loadingRemote {
		title += " " + formatBusyBadge(m.th, m.spinIdx, "同步中", m.remoteLoadStarted)
	}
	title = truncCell(title, innerW)

	var lines []string
	switch {
	case m.cfg == nil:
		lines = append(lines, m.th.badgeMuted.Render(truncCell("尚未配置或导入身份，按 i 打开身份配置", innerW)))
	case m.remoteErr != "":
		lines = append(lines, m.th.badgeErr.Render(truncPlainCell("远端列表暂不可用："+m.remoteErr, innerW)))
		lines = append(lines, m.th.badgeMuted.Render(truncCell("按 r 重新刷新连接，本地队列与断点继续可用", innerW)))
	case len(m.visibleRemote) == 0:
		lines = append(lines, m.th.badgeMuted.Render(truncCell("（远端暂无匹配文件，选中本地文件后按 u/F5 上传）", innerW)))
	default:
		start := scrollOffset(m.remoteCursor, len(m.visibleRemote), visRows)
		// 行模板: mark(3) + ' '(1) + name(nameW) + ' '(1) + sz(sizeW) => 固定开销 5 列
		nameW, sizeW := panelNameSizeWidths(innerW, 5)
		for i := start; i < min(len(m.visibleRemote), start+visRows); i++ {
			f := m.visibleRemote[i]
			mark := "[ ]"
			if m.remoteSelected[f.FileName] {
				mark = "[✓]"
			}
			row := truncPlainCell(fmt.Sprintf("%s %s %s", mark, padPlainRightCell(f.FileName, nameW), padLeftCell(progress.FormatBytes(f.Size), sizeW)), innerW)
			if i == m.remoteCursor && m.focus == areaRemote {
				lines = append(lines, m.th.rowCursor.Render(padRightCell(row, innerW)))
			} else if m.remoteSelected[f.FileName] {
				lines = append(lines, m.th.rowSelected.Render(row))
			} else {
				lines = append(lines, m.th.rowNormal.Render(row))
			}
		}
	}
	return m.renderBox(title, lines, m.focus == areaRemote, outerW, outerH)
}

func (m *model) renderQueuePanel(outerW, outerH int) string {
	innerW := max(16, outerW-4)
	visRows := max(1, outerH-3)

	var activeTab, histTab string
	if !m.historyView {
		activeTab = m.th.tabActive.Render(fmt.Sprintf("活动队列 (%d)", len(m.activeTasks)))
		histTab = m.th.tabInactive.Render(fmt.Sprintf("历史归档 (%d)", len(m.historyTasks)))
	} else {
		activeTab = m.th.tabInactive.Render(fmt.Sprintf("活动队列 (%d)", len(m.activeTasks)))
		histTab = m.th.tabActive.Render(fmt.Sprintf("历史归档 (%d)", len(m.historyTasks)))
	}
	title := truncCell(activeTab+" "+histTab, innerW)

	tasks := m.currentQueueTasks()
	var lines []string
	if !m.historyView && m.recoveryNotice != "" && len(m.activeTasks) > 0 && visRows >= 3 {
		lines = append(lines, m.th.badgeWarn.Render(truncPlainCell("↻ "+m.recoveryNotice, innerW)))
		visRows--
	}
	if len(tasks) == 0 {
		if m.historyView {
			lines = append(lines, m.th.badgeMuted.Render(truncCell("（历史归档为空）", innerW)))
		} else {
			lines = append(lines, m.th.badgeMuted.Render(truncCell("（活动队列为空：在本地或远端选择文件并按 u / d / F5 开始传输）", innerW)))
		}
	} else {
		start := scrollOffset(m.queueCursor, len(tasks), visRows)
		barW := 16
		if innerW < 82 {
			barW = 10
		}
		nameW := max(10, (innerW-barW-44)/2)
		for i := start; i < min(len(tasks), start+visRows); i++ {
			t := tasks[i]
			pointer := "  "
			if i == m.queueCursor && m.focus == areaQueue {
				pointer = "▶ "
			}
			bar := renderProgressBar(m.th, t.Done, t.Size, t.State, barW)
			badge := stateBadge(m.th, t)
			metrics := formatTaskMetrics(t)
			rowText := truncCell(fmt.Sprintf("%s%s  %s  %s  %s  %s",
				pointer,
				directionIcon(t.Direction),
				padPlainRightCell(t.Remote, nameW),
				badge,
				bar,
				truncPlainCell(metrics, max(10, innerW-nameW-barW-26)),
			), innerW)
			if i == m.queueCursor && m.focus == areaQueue {
				lines = append(lines, m.th.rowCursor.Render(padRightCell(rowText, innerW)))
			} else {
				lines = append(lines, rowText)
			}
		}
	}
	return m.renderBox(title, lines, m.focus == areaQueue, outerW, outerH)
}

func scrollOffset(cursor, total, visible int) int {
	if total <= visible || visible <= 0 {
		return 0
	}
	start := cursor - visible/2
	if start < 0 {
		start = 0
	}
	if start+visible > total {
		start = total - visible
	}
	return start
}
