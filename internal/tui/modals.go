package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"sft-tui/internal/progress"
	"sft-tui/internal/protocol"
)

type modalKind int

const (
	modalNone modalKind = iota
	modalHelp
	modalDetail
	modalIdentity
	modalConflict
	modalRename
	modalQuit
	modalClearHistory
	modalBatchSummary
	modalDeleteRemote
)

func modalBoxDimensions(width int) (boxW, innerW int) {
	boxW = min(max(44, width-4), 86)
	if width > 0 && width < 48 {
		boxW = max(18, width-2)
	}
	innerW = max(8, boxW-6) // 2 border + 4 horizontal padding
	return boxW, innerW
}

func wrapModalLines(lines []string, innerW int) []string {
	if innerW < 4 || len(lines) == 0 {
		out := make([]string, len(lines))
		copy(out, lines)
		return out
	}
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if !strings.Contains(ln, "\n") && lipgloss.Width(ln) <= innerW {
			out = append(out, ln)
			continue
		}
		wrapped := ansi.Hardwrap(ln, innerW, false)
		out = append(out, strings.Split(wrapped, "\n")...)
	}
	return out
}

func renderModalRawBox(width int, th theme, title string, lines []string, footer string) string {
	boxW, innerW := modalBoxDimensions(width)
	var body []string
	body = append(body, th.modalTitle.Render(truncPlainCell(title, innerW)))
	body = append(body, lipgloss.NewStyle().Foreground(th.border).Render(strings.Repeat("─", innerW)))
	for _, ln := range lines {
		body = append(body, ln)
	}
	if footer != "" {
		body = append(body, lipgloss.NewStyle().Foreground(th.border).Render(strings.Repeat("─", innerW)))
		body = append(body, th.badgeMuted.Render(truncPlainCell(footer, innerW)))
	}
	return th.modalBox.Width(boxW - 2).Render(strings.Join(body, "\n"))
}

// renderModalScrollBox 在任意终端高度下保证标题与关闭提示始终可见，超长内容支持按行/页滚动。
func renderModalScrollBox(width, height int, th theme, title string, lines []string, footer string, scroll int) (rendered string, maxScroll int) {
	_, innerW := modalBoxDimensions(width)
	wrapped := wrapModalLines(lines, innerW)
	chromeH := 6 // border(2) + padding(2) + title(1) + topRule(1)
	if footer != "" {
		chromeH = 8 // + bottomRule(1) + footer(1)
	}

	vis := len(wrapped)
	showIndicator := false
	if height > 0 {
		avail := max(1, height-chromeH)
		if len(wrapped) > avail {
			if avail >= 3 {
				vis = avail - 1
				showIndicator = true
			} else {
				vis = avail
			}
		}
	}

	maxScroll = max(0, len(wrapped)-vis)
	clamped := max(0, min(scroll, maxScroll))
	end := min(len(wrapped), clamped+vis)

	viewLines := make([]string, 0, vis+1)
	if len(wrapped) > 0 && clamped < end {
		viewLines = append(viewLines, wrapped[clamped:end]...)
	}
	footerText := footer
	if maxScroll > 0 {
		if showIndicator {
			ind := fmt.Sprintf("↕ ↑↓/PgUp/PgDn/滚轮 滚动  第 %d–%d / 共 %d 行", clamped+1, end, len(wrapped))
			viewLines = append(viewLines, th.badgeMuted.Render(truncCell(ind, innerW)))
		} else if footerText != "" {
			footerText = fmt.Sprintf("[%d-%d/%d] %s", clamped+1, end, len(wrapped), footerText)
		}
	}

	box := renderModalRawBox(width, th, title, viewLines, footerText)
	rendered = lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box,
		lipgloss.WithWhitespaceChars(" "),
	)
	return rendered, maxScroll
}

func modalScrollBoxPageRows(height int, footer string) int {
	chromeH := 6
	if footer != "" {
		chromeH = 8
	}
	if height <= 0 {
		return 10
	}
	avail := max(1, height-chromeH)
	if avail >= 3 {
		return max(1, avail-1)
	}
	return avail
}

func modalScrollStep(key string, scroll, maxScroll, pageRows int) (int, bool) {
	step := max(1, pageRows-1)
	switch key {
	case "up", "k":
		return max(0, scroll-1), true
	case "down", "j":
		return min(maxScroll, scroll+1), true
	case "pgup", "b":
		return max(0, scroll-step), true
	case "pgdown", "f", " ":
		return min(maxScroll, scroll+step), true
	case "home", "g":
		return 0, true
	case "end", "G":
		return maxScroll, true
	default:
		return scroll, false
	}
}

func renderModalBox(width, height int, th theme, title string, lines []string, footer string) string {
	box := renderModalRawBox(width, th, title, lines, footer)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box,
		lipgloss.WithWhitespaceChars(" "),
	)
}

func renderModalOverlay(_ string, title string, lines []string, footer string, m *model) string {
	return renderModalBox(m.width, m.height, m.th, title, lines, footer)
}

func helpKeyRow(th theme, indent, keys, desc string, keyColW int) string {
	return indent + th.hintKey.Render(padPlainRightCell(keys, keyColW)) + " " + th.rowNormal.Render(desc)
}

func (m *model) infoModalData() (title string, lines []string, footer string) {
	switch m.modal {
	case modalHelp:
		const kW = 17
		return "键盘快捷键说明", []string{
			m.th.hintKey.Render("全局导航"),
			helpKeyRow(m.th, "  ", "[Tab] / [1-4]", "在 本地/远端/活动队列/历史归档 间切换", kW),
			helpKeyRow(m.th, "  ", "[v] / [h]", "在 活动队列 与 历史归档 视图间切换", kW),
			helpKeyRow(m.th, "  ", "[i] / [F4]", "打开客户端 mTLS 身份管理与连接测试", kW),
			helpKeyRow(m.th, "  ", "[?] / [q]", "开关本帮助 / 安全退出客户端", kW),
			"",
			m.th.hintKey.Render("文件浏览（本地 / 远端）"),
			helpKeyRow(m.th, "  ", "[↑↓]/[g/G]/[Bksp]", "移动/首尾跳转 / 返回本地上级目录", kW),
			helpKeyRow(m.th, "  ", "[Space] / [A]", "多选单文件 / 全选或取消当前可见文件", kW),
			helpKeyRow(m.th, "  ", "[Enter] / [Esc]", "打开目录或详情 / 清空筛选或取消多选", kW),
			helpKeyRow(m.th, "  ", "[u] / [d] / [F5]", "上传所选 / 下载所选 / 执行当前栏传输", kW),
			helpKeyRow(m.th, "  ", "[x] / [Del]", "删除自己上传的远端文件（需二次确认）", kW),
			helpKeyRow(m.th, "  ", "[/] / [p]", "筛选搜索文件名 / 输入本地目录跳转", kW),
			helpKeyRow(m.th, "  ", "[o] / [r]", "切换排序方式 / 刷新本地与远端列表", kW),
			"",
			m.th.hintKey.Render("队列与断点控制（统一 SQLite 断点）"),
			helpKeyRow(m.th, "  ", "[Space]", "暂停进行中任务，或恢复暂停/待处理任务", kW),
			helpKeyRow(m.th, "  ", "[F5] / [r]", "按状态恢复、重试或重新入队传输", kW),
			helpKeyRow(m.th, "  ", "[p] / [c] / [a]", "暂停单项 / 取消任务 / 全暂停或恢复(仅队列)", kW),
			helpKeyRow(m.th, "  ", "[n] / [X]", "冲突改名另存重传 / 清空历史归档", kW),
			helpKeyRow(m.th, "  ", "[B] / [R]", "查看批次结果摘要 / 一键重试失败任务", kW),
			helpKeyRow(m.th, "  ", "[Enter]", "查看任务进度、路径与校验详情", kW),
		}, "↑↓/PgUp/PgDn/滚轮 滚动 · Esc / Enter / ? 关闭"
	case modalDetail:
		return m.detailTitle, m.detailLines, "↑↓/滚轮 滚动 · Enter 或 Esc 关闭"
	case modalBatchSummary:
		return m.batchSummaryModalData()
	case modalConflict:
		return m.conflictModalData()
	case modalDeleteRemote:
		return m.deleteRemoteModalData()
	}
	return "", nil, ""
}

func (m *model) deleteRemoteModalData() (title string, lines []string, footer string) {
	_, innerW := modalBoxDimensions(m.width)
	lines = []string{
		m.th.badgeErr.Render(fmt.Sprintf("⚠ 确认永久删除以下 %d 个远端文件？删除后无法恢复。", len(m.pendingDeleteNames))),
		m.th.badgeMuted.Render("（仅允许删除当前 mTLS 认证身份名下且具备上传写权限的文件）"),
		"",
	}
	for _, name := range m.pendingDeleteNames {
		lines = append(lines, "  • "+sanitizeInline(name))
	}
	lines = appendModalNoticeLines(lines, m.th, m.statusToast, innerW)
	return "删除远端文件确认", lines, "Enter / y 确认永久删除 · Esc / n 取消"
}

func (m *model) conflictModalData() (title string, lines []string, footer string) {
	_, innerW := modalBoxDimensions(m.width)
	dirLabel := "下载目标本地目录已存在同名文件："
	if m.pendingBatchDir == protocol.DirectionUpload {
		dirLabel = "远端已存在同名文件："
	}
	lines = []string{
		m.th.badgeWarn.Render(dirLabel),
		"",
	}
	for _, name := range m.conflictNames {
		lines = append(lines, "  • "+sanitizeInline(name))
	}
	lines = append(lines, "",
		fmt.Sprintf("共待入队 %d 个文件（冲突 %d 个）。请选择处理策略：", len(m.pendingSpecs), len(m.conflictNames)),
		"  "+m.th.hintKey.Render("[r]")+" 自动为冲突项生成不重复新文件名（如 file (1).ext）后全部入队",
		"  "+m.th.hintKey.Render("[s]")+" 跳过冲突文件，仅入队其余无冲突文件",
		"  "+m.th.hintKey.Render("[Esc]")+" 取消本次批量传输（保留已勾选状态）",
	)
	lines = appendModalNoticeLines(lines, m.th, m.statusToast, innerW)
	return "文件名冲突安全确认（禁止静默覆盖）", lines, "r 自动改名 · s 跳过冲突 · ↑↓ 滚动 · Esc 取消"
}

func appendModalNoticeLines(lines []string, th theme, notice string, innerW int) []string {
	cleaned := sanitizeInline(strings.TrimSpace(notice))
	if cleaned == "" {
		return lines
	}
	wrapped := wrapModalLines([]string{"! " + cleaned}, innerW)
	out := make([]string, 0, len(lines)+len(wrapped)+1)
	out = append(out, lines...)
	out = append(out, "")
	for _, ln := range wrapped {
		out = append(out, th.badgeErr.Render(ln))
	}
	return out
}

func (m *model) viewModal(base string) string {
	_, innerW := modalBoxDimensions(m.width)
	switch m.modal {
	case modalHelp, modalDetail, modalBatchSummary, modalConflict, modalDeleteRemote:
		title, lines, footer := m.infoModalData()
		view, _ := renderModalScrollBox(m.width, m.height, m.th, title, lines, footer, m.infoScroll)
		return view

	case modalIdentity:
		title, footer, lines := m.buildIdentityModalContent()
		return renderModalOverlay(base, title, lines, footer, m)

	case modalRename:
		ri := m.renameInput
		ri.Width = min(ri.Width, max(8, innerW-4))
		lines := append(wrapModalLines([]string{"为任务设置新的目标名称以继续重传（原任务保持状态不覆盖）："}, innerW), "", ri.View())
		lines = appendModalNoticeLines(lines, m.th, m.statusToast, innerW)
		return renderModalOverlay(base, "改名另存并入队", lines, "Enter 确认入队 · Esc 取消", m)

	case modalQuit:
		lines := []string{
			m.th.badgeWarn.Render("仍有传输任务正在进行中！"),
			"",
			"  " + m.th.hintKey.Render("[p]") + "  安全暂停所有任务并等待服务端检查点确认后退出（推荐，下次可续传）",
			"  " + m.th.hintKey.Render("[f]") + "  立即强制退出（已确认检查点仍保存在 SQLite 中）",
			"  " + m.th.hintKey.Render("[Esc]") + "  返回客户端界面继续传输",
		}
		view, _ := renderModalScrollBox(m.width, m.height, m.th, "退出确认", lines, "按 p 安全暂停退出 · f 强制退出 · Esc 返回", 0)
		return view

	case modalClearHistory:
		lines := []string{
			"确定清空所有已完成 / 已取消 / 已跳过的历史归档记录吗？",
			m.th.badgeMuted.Render("（注：仅清理 SQLite 历史表，绝不删除本地文件或影响活动队列）"),
		}
		view, _ := renderModalScrollBox(m.width, m.height, m.th, "清空历史记录", lines, "Enter / y 确认清空 · Esc 取消", 0)
		return view
	}
	return base
}

func buildTaskDetailLines(dirLabel, stateText, remote, localPath string, done, size uint64, shaHex, tid, errMsg string, updated time.Time) []string {
	kv := func(label, val string) string {
		return padPlainRightCell(label, 12) + " " + sanitizeInline(val)
	}
	lines := []string{
		kv("方向与状态：", fmt.Sprintf("%s · %s", sanitizeInline(dirLabel), sanitizeInline(stateText))),
		kv("远端文件名：", remote),
		kv("本地路径：", localPath),
		kv("进度与大小：", fmt.Sprintf("%s / %s", progress.FormatBytes(done), progress.FormatBytes(size))),
		kv("最近更新：", updated.Format("2006-01-02 15:04:05")),
	}
	if tid != "" {
		lines = append(lines, kv("TransferID：", tid))
	}
	if shaHex != "" {
		lines = append(lines, kv("SHA-256：", shaHex))
	}
	if errMsg != "" {
		lines = append(lines, "", "错误/提示说明：", "  "+sanitizeInline(errMsg))
	}
	return lines
}

func (m *model) identityModalMetrics() (innerW, listStartRelY, maxShow, inputRelY int) {
	_, innerW = modalBoxDimensions(m.width)
	maxBody := 1 << 20
	if m.height > 0 {
		maxBody = max(6, m.height-8)
	}
	listStartRelY = 2
	noticeLines := appendModalNoticeLines(nil, m.th, m.statusToast, innerW)
	previewRows := 0
	if len(m.idChoices) > 0 {
		previewRows = 5 // 空行 + 4行详情预览
	}
	inputRows := 3 // 空行 + 提示 + 输入框
	availList := maxBody - listStartRelY - previewRows - inputRows - len(noticeLines)
	if len(m.idChoices) == 0 {
		return innerW, listStartRelY, 0, listStartRelY + 3
	}
	maxShow = max(1, min(6, min(len(m.idChoices), availList)))
	inputRelY = listStartRelY + maxShow + previewRows + 2
	return innerW, listStartRelY, maxShow, inputRelY
}

func (m *model) buildIdentityModalContent() (title, footer string, lines []string) {
	innerW, _, maxShow, _ := m.identityModalMetrics()
	if m.pendingSwitch != nil {
		ps := m.pendingSwitch
		total := ps.activeCount + ps.pausedCount
		lines = []string{
			m.th.badgeWarn.Render(truncPlainCell("⚠ 当前身份队列仍有未完成任务，确认切换？", innerW)),
			"",
			truncPlainCell("  • 目标身份："+ps.targetLabel, innerW),
			truncPlainCell(fmt.Sprintf("  • 未完成任务：共 %d 个（进行中 %d · 已暂停/待处理 %d）", total, ps.activeCount, ps.pausedCount), innerW),
			"",
			m.th.badgeMuted.Render(truncCell("  切换影响：进行中任务将被安全暂停，断点保留在当前身份 SQLite；", innerW)),
			m.th.badgeMuted.Render(truncCell("  切回当前身份后可按 [a] 或 [F5] 从断点处继续传输。", innerW)),
		}
		lines = appendModalNoticeLines(lines, m.th, m.statusToast, innerW)
		return "身份切换前影响确认", "Enter / y 确认暂停并切换 · Esc 返回重选", lines
	}
	lines = []string{
		truncCell("已保存或发现的身份包（↑↓ 选择 · Enter 切换 · e 改标签 · t 测连）：", innerW),
		"",
	}
	if len(m.idChoices) == 0 {
		if m.idInitFailedLabel != "" {
			lines = append(lines, m.th.badgeWarn.Render(truncPlainCell(fmt.Sprintf("  已识别身份 %s，但%s（详情见下方提示）。", sanitizeInline(m.idInitFailedLabel), identityStageStatusText(m.idInitFailedStage)), innerW)))
		} else {
			lines = append(lines, m.th.badgeWarn.Render(truncCell("  未在当前目录及程序旁发现可用的 client*.json，请在下方输入路径导入。", innerW)))
		}
	} else {
		start := scrollOffset(m.idCursor, len(m.idChoices), maxShow)
		for i := start; i < start+maxShow; i++ {
			ch := m.idChoices[i]
			mark := "  "
			style := m.th.rowNormal
			if i == m.idCursor && !m.idInputFocus && !m.editingIDLabel {
				mark = "▶ "
				style = m.th.rowCursor
			}
			tag := "已导入"
			if !ch.imported {
				tag = "待导入"
			}
			if ch.profile.ID != "" && m.idKey != "" && ch.profile.ID == m.activeID {
				tag = "当前"
			}
			testTag := ""
			if ts, ok := m.idTestResults[ch.profile.Path]; ok {
				switch {
				case ts.testing:
					testTag = " [" + spinnerChar(m.spinIdx) + "测连中]"
				case ts.tested && ts.err == "":
					testTag = fmt.Sprintf(" [✔%dms]", max(1, int(ts.latency.Milliseconds())))
				case ts.tested && ts.err != "":
					testTag = " [✕不可达]"
				}
			}
			exp := ch.profile.Expires.Format("2006-01-02")
			row := fmt.Sprintf("%s[%s] %s  (到期 %s)%s", mark, tag, sanitizeInline(ch.profile.Label), exp, testTag)
			lines = append(lines, style.Render(truncPlainCell(row, innerW)))
		}

		// 当前高亮身份预览卡
		if m.idCursor >= 0 && m.idCursor < len(m.idChoices) {
			ch := m.idChoices[m.idCursor]
			p := ch.profile
			lines = append(lines, "")
			user := sanitizeInline(p.User)
			if user == "" {
				user = "-"
			}
			srv := sanitizeInline(p.Config.ServerAddress)
			if srv == "" {
				srv = "-"
			}
			san := sanitizeInline(p.Config.ServerName)
			if san == "" {
				san = "-"
			}
			lines = append(lines, m.th.hintKey.Render(truncPlainCell(fmt.Sprintf("预览: 用户=%s · 地址=%s · SAN=%s", user, srv, san), innerW)))

			fpShort := sanitizeInline(p.Fingerprint)
			if len(fpShort) > 20 {
				fpShort = fpShort[:12] + "…" + fpShort[len(fpShort)-6:]
			} else if fpShort == "" {
				fpShort = "-"
			}
			daysLeft := int(time.Until(p.Expires).Hours() / 24)
			expNote := fmt.Sprintf("%s (剩%d天)", p.Expires.Format("2006-01-02"), daysLeft)
			expired := !p.Expires.IsZero() && time.Now().After(p.Expires)
			if p.Expires.IsZero() {
				expNote = "-"
			} else if expired {
				expNote = fmt.Sprintf("%s (已过期!)", p.Expires.Format("2006-01-02"))
			}
			expLine := truncPlainCell(fmt.Sprintf("指纹: %s · 到期: %s", fpShort, expNote), innerW)
			switch {
			case expired:
				lines = append(lines, m.th.badgeErr.Render(expLine))
			case !p.Expires.IsZero() && daysLeft <= 14:
				lines = append(lines, m.th.badgeWarn.Render(expLine))
			default:
				lines = append(lines, m.th.badgeMuted.Render(expLine))
			}

			stateDir := sanitizeInline(p.Config.StateDir)
			if stateDir == "" {
				stateDir = "-"
			}
			lines = append(lines, m.th.badgeMuted.Render(truncPlainCell(fmt.Sprintf("限额: 单文件=%s · 状态目录=%s", progress.FormatBytes(p.Config.MaxFileBytes), stateDir), innerW)))

			connPreview := m.th.badgeMuted.Render(truncCell("连通: 未测试（按 t 测试 mTLS 握手与认证）", innerW))
			if ts, ok := m.idTestResults[p.Path]; ok {
				switch {
				case ts.testing:
					connPreview = truncCell(formatBusyBadge(m.th, m.spinIdx, "连通: 正在测试 mTLS 握手…", time.Time{}), innerW)
				case ts.tested && ts.err == "":
					connPreview = m.th.badgeOk.Render(truncCell(fmt.Sprintf("连通: ✔ mTLS 握手通过（耗时 %s）", ts.latency.Round(time.Millisecond)), innerW))
				case ts.tested && ts.err != "":
					connPreview = m.th.badgeErr.Render(truncPlainCell("连通: ✕ "+ts.err, innerW))
				}
			}
			lines = append(lines, connPreview)
		}
	}

	if m.editingIDLabel {
		ti := m.idLabelInput
		ti.Width = min(ti.Width, max(8, innerW-4))
		lines = append(lines, "", truncCell("修改选中身份标签（Enter 保存 · Esc 取消）：", innerW), ti.View())
		footer = "编辑标签：Enter 保存 · Esc 取消"
	} else {
		ti := m.idInput
		ti.Width = min(ti.Width, max(8, innerW-4))
		lines = append(lines, "", truncCell("从目录或配置文件导入（支持多配置目录扫描）：", innerW), ti.View())
		footer = "Tab 切列表/路径 · e 改标签 · t 测连 · Enter 切换/导入 · Esc 关"
	}
	lines = appendModalNoticeLines(lines, m.th, m.statusToast, innerW)
	return "客户端 mTLS 身份管理", footer, lines
}
