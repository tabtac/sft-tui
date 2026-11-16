package tui

import (
	"fmt"
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
	idBadge := m.th.badgeID.Render(" 身份: " + m.identityLabel + " ")

	connStyle := m.th.badgeMuted
	connIcon := "○ "
	switch {
	case strings.Contains(m.connStatus, "已连接"):
		connStyle = m.th.badgeOk
		connIcon = "● "
	case strings.Contains(m.connStatus, "重连") || strings.Contains(m.connStatus, "正在连接"):
		connStyle = m.th.badgeWarn
		connIcon = "◐ "
	case strings.Contains(m.connStatus, "失败") || strings.Contains(m.connStatus, "断开"):
		connStyle = m.th.badgeErr
		connIcon = "✕ "
	}
	connBadge := connStyle.Render(connIcon + truncCell(m.connStatus, 26))

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

	left := lipgloss.JoinHorizontal(lipgloss.Center, brand, " ", idBadge, " ", connBadge)
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(stats)-2)
	line := left + strings.Repeat(" ", gap) + stats
	return m.th.headerBar.Width(m.width).Render(truncCell(line, m.width-2))
}

func (m *model) renderSubHeader() string {
	pathView := m.th.badgeMuted.Render("目录: ") + truncCell(m.localDir, max(12, m.width/3))
	if m.editingPath {
		pathView = m.pathInput.View()
	}

	searchView := m.th.badgeMuted.Render("筛选(/): ")
	if m.editingSearch {
		searchView += m.searchInput.View()
	} else if m.query != "" {
		searchView += m.th.badgeOk.Render(truncCell(m.query, 24))
	} else {
		searchView += m.th.badgeMuted.Render("全部")
	}

	sortLabel := m.th.badgeMuted.Render("排序(o): " + m.sortLabel())
	msg := ""
	if m.statusToast != "" {
		msg = "  " + m.th.toast.Render("· "+truncCell(m.statusToast, max(10, m.width/4)))
	}
	line := pathView + "   " + searchView + "   " + sortLabel + msg
	return lipgloss.NewStyle().Padding(0, 1).Width(m.width).Render(truncCell(line, m.width-2))
}

func (m *model) renderFooter() string {
	var hints string
	switch {
	case m.editingPath:
		hints = "路径跳转：Enter 打开目录 · Esc 取消"
	case m.editingSearch:
		hints = "实时筛选：输入关键字 · Enter 确定 · Esc 清空"
	case m.focus == areaLocal:
		hints = "本地：↑↓ 移动  Space 多选  Enter 进目录/详情  u/F5 上传选中  Backspace 上级  p 路径  / 筛选  o 排序  r 刷新  i 身份  Tab 切区  ? 帮助  q 退出"
	case m.focus == areaRemote:
		hints = "远端：↑↓ 移动  Space 多选  Enter 详情  d/F5 下载到当前目录  r 刷新  / 筛选  o 排序  i 身份  Tab 切区  ? 帮助  q 退出"
	default:
		if m.historyView {
			hints = "历史归档：↑↓ 移动  Enter 详情  r 重传  X 清空历史  Tab/v 切回活动队列  F6 主题  ? 帮助  q 退出"
		} else {
			hints = "活动队列：↑↓ 移动  Space 暂停/恢复  c 取消  a 全部暂停/恢复  n 改名重传  Enter 详情  Tab/v 切历史  ? 帮助  q 退出"
		}
	}
	return m.th.footerBar.Width(m.width).Render(truncCell(hints, m.width-2))
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

func (m *model) renderLocalPanel(outerW, outerH int) string {
	innerW := max(10, outerW-4)
	visRows := max(1, outerH-3)

	selCount := len(m.localSelected)
	title := m.th.hintKey.Render(fmt.Sprintf("本地文件 (%d 项 · 已选 %d)", len(m.visibleLocal), selCount))
	if m.loadingLocal {
		title += m.th.badgeWarn.Render(" [读取中…]")
	}

	var lines []string
	if len(m.visibleLocal) == 0 {
		lines = append(lines, m.th.badgeMuted.Render("（当前目录无可匹配文件）"))
	} else {
		start := scrollOffset(m.localCursor, len(m.visibleLocal), visRows)
		sizeW := 10
		nameW := max(8, innerW-sizeW-6)
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
			row := fmt.Sprintf("%s %s%s %s", mark, icon, padRightCell(e.name, nameW), sz)
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
		title += m.th.badgeWarn.Render(" [同步中…]")
	}

	var lines []string
	switch {
	case m.cfg == nil:
		lines = append(lines, m.th.badgeMuted.Render("尚未配置或导入身份，按 i 打开身份配置"))
	case m.remoteErr != "":
		lines = append(lines, m.th.badgeErr.Render(truncCell("远端列表暂不可用："+m.remoteErr, innerW)))
		lines = append(lines, m.th.badgeMuted.Render("按 r 重新刷新连接，本地队列与断点继续可用"))
	case len(m.visibleRemote) == 0:
		lines = append(lines, m.th.badgeMuted.Render("（远端暂无匹配文件，选中本地文件后按 u/F5 上传）"))
	default:
		start := scrollOffset(m.remoteCursor, len(m.visibleRemote), visRows)
		sizeW := 10
		nameW := max(8, innerW-sizeW-5)
		for i := start; i < min(len(m.visibleRemote), start+visRows); i++ {
			f := m.visibleRemote[i]
			mark := "[ ]"
			if m.remoteSelected[f.FileName] {
				mark = "[✓]"
			}
			row := fmt.Sprintf("%s %s %s", mark, padRightCell(f.FileName, nameW), padLeftCell(progress.FormatBytes(f.Size), sizeW))
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
	title := activeTab + " " + histTab

	tasks := m.currentQueueTasks()
	var lines []string
	if len(tasks) == 0 {
		if m.historyView {
			lines = append(lines, m.th.badgeMuted.Render("（历史归档为空）"))
		} else {
			lines = append(lines, m.th.badgeMuted.Render("（活动队列为空：在本地或远端选择文件并按 u / d / F5 开始传输）"))
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
			rowText := fmt.Sprintf("%s%s  %s  %s  %s  %s",
				pointer,
				directionIcon(t.Direction),
				padRightCell(t.Remote, nameW),
				badge,
				bar,
				truncCell(metrics, max(10, innerW-nameW-barW-26)),
			)
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
