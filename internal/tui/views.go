package tui

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"sft-tui/internal/progress"
	"sft-tui/internal/queue"
)

func (m *model) View() string {
	if m.width <= 0 || m.height <= 0 {
		return "正在初始化界面..."
	}
	if m.width < 48 || m.height < 14 {
		msg := fmt.Sprintf("终端窗口过小（%d×%d），请至少调整至 48×14 以上（推荐 ≥ 96×28）", m.width, m.height)
		lines := wrapModalLines([]string{msg}, max(8, m.width-2))
		for i, ln := range lines {
			lines[i] = m.th.badgeWarn.Render(ln)
		}
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, strings.Join(lines, "\n"))
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
	case strings.Contains(m.connStatus, "重连") || strings.Contains(m.connStatus, "正在"):
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
		case t.State == queue.Paused || t.State == queue.Pending:
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
	budget := max(10, m.width-2)
	sep := "   "
	if m.width <= 96 {
		sep = "  "
	}
	sepW := len(sep)

	if m.editingPath {
		lbl := m.th.hintKey.Render("目录: ")
		pi := m.pathInput
		pi.Width = min(48, max(8, budget-lipgloss.Width(lbl)-2))
		line := lbl + pi.View()
		return lipgloss.NewStyle().Padding(0, 1).Width(m.width).Render(truncCell(line, budget))
	}

	if m.editingSearch {
		lbl := m.th.hintKey.Render("筛选(/): ")
		si := m.searchInput
		si.Width = min(40, max(8, budget-lipgloss.Width(lbl)-14))
		searchPart := lbl + si.View()
		rem := budget - lipgloss.Width(searchPart) - sepW
		if rem >= 12 {
			pathPart := m.th.badgeMuted.Render("目录: ") + truncPlainCell(m.localDir, max(6, rem-6))
			line := pathPart + sep + searchPart
			return lipgloss.NewStyle().Padding(0, 1).Width(m.width).Render(truncCell(line, budget))
		}
		return lipgloss.NewStyle().Padding(0, 1).Width(m.width).Render(truncCell(searchPart, budget))
	}

	var msg string
	switch {
	case m.opBusy && m.opLabel != "":
		msg = formatBusyBadge(m.th, m.spinIdx, m.opLabel, m.opStart)
	case m.activatingID:
		msg = formatBusyBadge(m.th, m.spinIdx, "身份切换中", m.opStart)
	case m.statusToast != "":
		msg = renderToastInline(m.th, m.toastLevel, m.statusToast, 0)
	}

	searchView := m.th.badgeMuted.Render("筛选(/): ")
	if m.query != "" {
		searchView += m.th.badgeOk.Render(truncPlainCell(m.query, 24))
	} else {
		searchView += m.th.badgeMuted.Render("全部")
	}
	sortLabel := m.th.badgeMuted.Render("排序(o): " + m.activeSortLabel())

	if msg != "" {
		// 有活动状态或 Toast 时优先保证其可见，避免被左侧长路径挤出屏幕。
		msgBudget := min(lipgloss.Width(msg), max(16, budget/2))
		msgRendered := truncCell(msg, msgBudget)
		msgW := lipgloss.Width(msgRendered)
		rem := budget - msgW - sepW

		var parts []string
		if rem >= 12 {
			pathMax := min(max(12, m.width/3), max(6, rem-6))
			if m.query != "" && rem >= 30 {
				qPart := m.th.badgeMuted.Render("筛选: ") + m.th.badgeOk.Render(truncPlainCell(m.query, 12))
				qW := lipgloss.Width(qPart)
				pathMax = min(pathMax, max(6, rem-qW-sepW-6))
				parts = append(parts, m.th.badgeMuted.Render("目录: ")+truncPlainCell(m.localDir, pathMax), qPart)
			} else {
				parts = append(parts, m.th.badgeMuted.Render("目录: ")+truncPlainCell(m.localDir, pathMax))
			}
			// 若剩余宽度仍充裕，补齐筛选与排序信息
			used := 0
			for i, p := range parts {
				used += lipgloss.Width(p)
				if i > 0 {
					used += sepW
				}
			}
			if m.query == "" && used+sepW+lipgloss.Width(searchView) <= rem {
				parts = append(parts, searchView)
				used += sepW + lipgloss.Width(searchView)
			}
			if used+sepW+lipgloss.Width(sortLabel) <= rem {
				parts = append(parts, sortLabel)
			}
		}
		parts = append(parts, msgRendered)
		return lipgloss.NewStyle().Padding(0, 1).Width(m.width).Render(truncCell(strings.Join(parts, sep), budget))
	}

	usedRight := lipgloss.Width(searchView) + sepW + lipgloss.Width(sortLabel)
	pathMax := max(10, min(max(12, m.width/3), budget-usedRight-sepW-6))
	pathView := m.th.badgeMuted.Render("目录: ") + truncPlainCell(m.localDir, pathMax)
	line := pathView + sep + searchView + sep + sortLabel
	return lipgloss.NewStyle().Padding(0, 1).Width(m.width).Render(truncCell(line, budget))
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
			{key: "Spc", label: "多选", essential: true},
			{key: "A", label: "全选", essential: true},
			{key: "Enter", label: "开/详"},
			{key: "Bksp", label: "上级", reason: parentReason},
			{key: "r", label: "刷新", reason: refreshReason},
			{key: "/", label: "筛选"},
			{key: "o", label: "排序"},
			{key: "p", label: "路径"},
			{key: "i", label: "身份", essential: true},
			{key: "?", label: "帮助", essential: true},
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
			{key: "Spc", label: "多选", essential: true},
			{key: "A", label: "全选", essential: true},
			{key: "Enter", label: "详情"},
			{key: "r", label: "刷新", reason: refreshReason, essential: true},
			{key: "/", label: "筛选"},
			{key: "o", label: "排序"},
			{key: "i", label: "身份", essential: true},
			{key: "?", label: "帮助", essential: true},
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
				{key: "r/F5", label: "重传", reason: emptyReason, essential: len(m.historyTasks) > 0},
				{key: "R", label: "重失败", reason: retryReason, essential: failCount > 0},
				{key: "B", label: "批次", reason: batchReason, essential: m.lastBatch != nil},
				{key: "Enter", label: "详情", reason: emptyReason, essential: len(m.historyTasks) > 0},
				{key: "X", label: "清空", reason: emptyReason},
				{key: "v", label: "活动队列", essential: true},
				{key: "?", label: "帮助", essential: true},
				{key: "q", label: "退出", essential: true},
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
		case t.State == queue.Paused || t.State == queue.Pending:
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
		hasTasks := len(m.activeTasks) > 0
		return renderActionFooter(m.th, m.width, "队列:", []footerChip{
			{key: "Spc", label: spaceLabel, reason: spaceReason, essential: true},
			{key: "F5", label: "恢复/试", reason: f5Reason, essential: hasTasks && f5Reason == ""},
			{key: "R", label: "重失败", reason: retryReason, essential: failCount > 0},
			{key: "B", label: "批次", reason: batchReason, essential: m.lastBatch != nil},
			{key: "n", label: "改名", reason: renameReason},
			{key: "c", label: "取消", reason: cancelReason},
			{key: "a", label: "全暂/恢", reason: allReason, essential: hasTasks},
			{key: "v", label: "历史", essential: true},
			{key: "?", label: "帮助", essential: true},
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

func hasTableHeader(visRows, itemCount int) bool {
	return visRows >= 4 && itemCount > 0
}

func (m *model) renderLocalPanel(outerW, outerH int) string {
	innerW := max(10, outerW-4)
	visRows := max(1, outerH-3)

	selCount := len(m.localSelected)
	title := m.th.hintKey.Render(fmt.Sprintf("本地文件 (%d 项 · 已选 %d)", len(m.visibleLocal), selCount))
	if m.loadingLocal {
		title += " " + formatBusyBadge(m.th, m.spinIdx, "读取中", m.localLoadStarted)
	}

	var lines []string
	dataRows := visRows
	if len(m.visibleLocal) == 0 {
		lines = append(lines, m.th.badgeMuted.Render(truncCell("（当前目录无可匹配文件）", innerW)))
	} else {
		// 行模板: mark(3) + ' '(1) + icon(2) + name(nameW) + ' '(1) + sz(sizeW) => 固定开销 7 列
		nameW, sizeW := panelNameSizeWidths(innerW, 7)
		if hasTableHeader(visRows, len(m.visibleLocal)) {
			hdr := fmt.Sprintf("      %s %s", padPlainRightCell("名称", nameW), padLeftCell("大小", sizeW))
			lines = append(lines, m.th.badgeMuted.Render(truncCell(hdr, innerW)))
			dataRows--
		}
		start := scrollOffset(m.localCursor, len(m.visibleLocal), dataRows)
		for i := start; i < min(len(m.visibleLocal), start+dataRows); i++ {
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
				lines = append(lines, highlightRow(m.th, row, innerW))
			} else if e.dir {
				lines = append(lines, m.th.hintKey.Render(row))
			} else if e.regular && m.localSelected[e.path] {
				lines = append(lines, m.th.rowSelected.Render(row))
			} else {
				lines = append(lines, m.th.rowNormal.Render(row))
			}
		}
	}
	if pos := scrollPosBadge(m.th, m.localCursor, len(m.visibleLocal), dataRows); pos != "" {
		title = spreadRow(title, pos, innerW)
	} else {
		title = truncCell(title, innerW)
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

	var lines []string
	dataRows := visRows
	switch {
	case m.cfg == nil:
		lines = append(lines, m.th.badgeMuted.Render(truncCell("尚未配置或导入身份，按 i 打开身份配置", innerW)))
	case m.remoteErr != "":
		errLines := wrapModalLines([]string{"远端列表暂不可用：" + sanitizeInline(m.remoteErr)}, innerW)
		maxErr := max(1, visRows-1)
		for i := 0; i < min(len(errLines), maxErr); i++ {
			lines = append(lines, m.th.badgeErr.Render(errLines[i]))
		}
		lines = append(lines, m.th.badgeMuted.Render(truncCell("按 r 刷新连接 · Enter 查看完整错误，本地队列与断点继续可用", innerW)))
	case len(m.visibleRemote) == 0:
		lines = append(lines, m.th.badgeMuted.Render(truncCell("（远端暂无匹配文件，选中本地文件后按 u/F5 上传）", innerW)))
	default:
		// 行模板: mark(3) + ' '(1) + name(nameW) + ' '(1) + sz(sizeW) => 固定开销 5 列
		nameW, sizeW := panelNameSizeWidths(innerW, 5)
		if hasTableHeader(visRows, len(m.visibleRemote)) {
			hdr := fmt.Sprintf("    %s %s", padPlainRightCell("文件名", nameW), padLeftCell("大小", sizeW))
			lines = append(lines, m.th.badgeMuted.Render(truncCell(hdr, innerW)))
			dataRows--
		}
		start := scrollOffset(m.remoteCursor, len(m.visibleRemote), dataRows)
		for i := start; i < min(len(m.visibleRemote), start+dataRows); i++ {
			f := m.visibleRemote[i]
			mark := "[ ]"
			if m.remoteSelected[f.FileName] {
				mark = "[✓]"
			}
			row := truncPlainCell(fmt.Sprintf("%s %s %s", mark, padPlainRightCell(f.FileName, nameW), padLeftCell(progress.FormatBytes(f.Size), sizeW)), innerW)
			if i == m.remoteCursor && m.focus == areaRemote {
				lines = append(lines, highlightRow(m.th, row, innerW))
			} else if m.remoteSelected[f.FileName] {
				lines = append(lines, m.th.rowSelected.Render(row))
			} else {
				lines = append(lines, m.th.rowNormal.Render(row))
			}
		}
	}
	if pos := scrollPosBadge(m.th, m.remoteCursor, len(m.visibleRemote), dataRows); pos != "" {
		title = spreadRow(title, pos, innerW)
	} else {
		title = truncCell(title, innerW)
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
	title := activeTab + " " + histTab

	tasks := m.currentQueueTasks()
	var lines []string
	if !m.historyView && m.recoveryNotice != "" && len(m.activeTasks) > 0 && visRows >= 3 {
		lines = append(lines, m.th.badgeWarn.Render(truncPlainCell("↻ "+m.recoveryNotice, innerW)))
		visRows--
	}
	dataRows := visRows
	if len(tasks) == 0 {
		if m.historyView {
			lines = append(lines, m.th.badgeMuted.Render(truncCell("（历史归档为空）", innerW)))
		} else {
			lines = append(lines, m.th.badgeMuted.Render(truncCell("（活动队列为空：在本地或远端选择文件并按 u / d / F5 开始传输）", innerW)))
		}
	} else {
		barW := 16
		if innerW < 82 {
			barW = 10
		}
		const dirW = 6   // "↑ 上传" / "↓ 下载"
		const badgeW = 10 // 统一对齐状态列宽，避免进度条左右抖动
		// 固定列开销: pointer(2) + dir(6) + sep(2) + sep(2) + badge(10) + sep(2) + bar(barW+5) + sep(2) = barW + 31
		fixedW := barW + 31
		remain := max(16, innerW-fixedW)
		if hasTableHeader(visRows, len(tasks)) {
			dataRows--
		}
		start := scrollOffset(m.queueCursor, len(tasks), dataRows)
		// 动态检测可见行中是否存在传输速率/错误，决定右侧 metrics 预算与文件名预算
		needWideMetrics := false
		for i := start; i < min(len(tasks), start+dataRows); i++ {
			if tasks[i].State == queue.Transferring || tasks[i].Error != "" {
				needWideMetrics = true
				break
			}
		}
		metricsW := min(22, remain/2)
		if needWideMetrics {
			metricsW = min(max(24, remain/2), remain-10)
		}
		nameW := max(10, remain-metricsW)
		if hasTableHeader(visRows, len(tasks)) {
			hdr := fmt.Sprintf("  %s  %s  %s  %s  %s",
				padPlainRightCell("方向", dirW),
				padPlainRightCell("文件名", nameW),
				padPlainRightCell("状态", badgeW),
				padPlainRightCell("进度", barW+5),
				padPlainRightCell("大小 / 速率", max(8, metricsW)),
			)
			lines = append(lines, m.th.badgeMuted.Render(truncCell(hdr, innerW)))
		}
		for i := start; i < min(len(tasks), start+dataRows); i++ {
			t := tasks[i]
			pointer := "  "
			if i == m.queueCursor && m.focus == areaQueue {
				pointer = "▶ "
			}
			bar := renderProgressBar(m.th, t.Done, t.Size, t.State, barW)
			badge := padRightCell(stateBadge(m.th, t), badgeW)
			metrics := formatTaskMetrics(t)
			rowText := truncCell(fmt.Sprintf("%s%s  %s  %s  %s  %s",
				pointer,
				padPlainRightCell(directionIcon(t.Direction), dirW),
				padPlainRightCell(t.Remote, nameW),
				badge,
				bar,
				truncPlainCell(metrics, max(8, metricsW)),
			), innerW)
			if i == m.queueCursor && m.focus == areaQueue {
				lines = append(lines, highlightRow(m.th, rowText, innerW))
			} else {
				lines = append(lines, rowText)
			}
		}
	}
	if pos := scrollPosBadge(m.th, m.queueCursor, len(tasks), dataRows); pos != "" {
		title = spreadRow(title, pos, innerW)
	} else {
		title = truncCell(title, innerW)
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
