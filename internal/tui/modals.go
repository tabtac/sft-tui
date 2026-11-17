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
	body = append(body, th.modalTitle.Render(truncCell(title, innerW)))
	body = append(body, lipgloss.NewStyle().Foreground(th.border).Render(strings.Repeat("─", innerW)))
	for _, ln := range lines {
		body = append(body, ln)
	}
	if footer != "" {
		body = append(body, lipgloss.NewStyle().Foreground(th.border).Render(strings.Repeat("─", innerW)))
		body = append(body, th.badgeMuted.Render(truncCell(footer, innerW)))
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

func (m *model) infoModalData() (title string, lines []string, footer string) {
	switch m.modal {
	case modalHelp:
		return "键盘快捷键说明", []string{
			m.th.hintKey.Render("全局导航"),
			"  Tab / Shift+Tab  在 本地 → 远端 → 活动队列 → 历史归档 间切换",
			"  1 / 2 / 3 / 4    直接跳转 本地 / 远端 / 活动队列 / 历史归档",
			"  v / h            在 活动队列 与 历史归档 之间切换视图",
			"  F6 / q / ?       切换深浅主题 / 优雅退出 / 开关本帮助",
			"",
			m.th.hintKey.Render("文件浏览（本地 / 远端）"),
			"  ↑↓ / j k         移动光标 · Backspace 返回上级目录 · Enter 进目录/详情",
			"  Space            多选普通文件并下移一行",
			"  u / d / F5       上传本地所选 / 下载远端所选 / 自动执行当前栏传输",
			"  /  ·  p  ·  o  ·  r   筛选搜索 · 输入目录跳转 · 切换排序 · 刷新列表",
			"  i / F4           打开身份配置与导入切换面板",
			"",
			m.th.hintKey.Render("队列与断点控制（统一 SQLite 断点）"),
			"  Space            智能暂停正在传输的任务，或恢复已暂停/待处理任务",
			"  F5 / r           在队列或历史中按任务状态恢复、重试或重新入队传输",
			"  p / c / a        暂停单项 / 取消任务 / 全部暂停或恢复(仅队列区)",
			"  n / X / Enter    冲突改名重传 / 清空历史归档 / 查看任务与校验详情",
		}, "↑↓/滚轮 滚动 · Esc 或 ? 关闭"
	case modalDetail:
		return m.detailTitle, m.detailLines, "↑↓/滚轮 滚动 · Enter 或 Esc 关闭"
	}
	return "", nil, ""
}

func appendModalNoticeLines(lines []string, th theme, notice string, innerW int) []string {
	if strings.TrimSpace(notice) == "" {
		return lines
	}
	wrapped := wrapModalLines([]string{"! " + notice}, innerW)
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
	case modalHelp, modalDetail:
		title, lines, footer := m.infoModalData()
		view, _ := renderModalScrollBox(m.width, m.height, m.th, title, lines, footer, m.infoScroll)
		return view

	case modalIdentity:
		lines := []string{
			"已保存或发现的身份包（↑/↓ 或鼠标点击选择后按 Enter 激活）：",
			"",
		}
		if len(m.idChoices) == 0 {
			lines = append(lines, m.th.badgeWarn.Render("  未在当前目录及程序旁发现可用的 client*.json，请在下方输入路径导入。"))
		} else {
			maxShow := 8
			if m.height > 0 {
				maxShow = min(8, max(2, m.height-16))
			}
			maxShow = min(maxShow, len(m.idChoices))
			start := scrollOffset(m.idCursor, len(m.idChoices), maxShow)
			for i := start; i < start+maxShow; i++ {
				ch := m.idChoices[i]
				mark := "  "
				style := m.th.rowNormal
				if i == m.idCursor && !m.idInputFocus {
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
				exp := ch.profile.Expires.Format("2006-01-02")
				row := fmt.Sprintf("%s[%s] %s  (到期 %s)", mark, tag, ch.profile.Label, exp)
				lines = append(lines, style.Render(truncCell(row, innerW)))
			}
		}
		lines = append(lines, "", "从指定目录或配置文件导入新身份：")
		lines = append(lines, m.idInput.View())
		lines = appendModalNoticeLines(lines, m.th, m.statusToast, innerW)
		footer := "Tab 切换列表/路径框 · Enter 切换或导入 · Esc 关闭"
		return renderModalOverlay(base, "客户端 mTLS 身份管理", lines, footer, m)

	case modalConflict:
		dirLabel := "下载目标本地目录已存在同名文件："
		if m.pendingBatchDir == protocol.DirectionUpload {
			dirLabel = "远端已存在同名文件："
		}
		lines := []string{
			m.th.badgeWarn.Render(dirLabel),
			"",
		}
		limit := min(5, len(m.conflictNames))
		for i := 0; i < limit; i++ {
			lines = append(lines, "  • "+truncCell(m.conflictNames[i], max(12, innerW-6)))
		}
		if len(m.conflictNames) > limit {
			lines = append(lines, fmt.Sprintf("  … 另有 %d 个冲突文件", len(m.conflictNames)-limit))
		}
		lines = append(lines, "",
			fmt.Sprintf("共待入队 %d 个文件（冲突 %d 个）。请选择处理策略：", len(m.pendingSpecs), len(m.conflictNames)),
			"  "+m.th.hintKey.Render("[r]")+" 自动为冲突项生成不重复新文件名（如 file (1).ext）后全部入队",
			"  "+m.th.hintKey.Render("[s]")+" 跳过冲突文件，仅入队其余无冲突文件",
			"  "+m.th.hintKey.Render("[Esc]")+" 取消本次批量传输（保留已勾选状态）",
		)
		lines = appendModalNoticeLines(lines, m.th, m.statusToast, innerW)
		return renderModalOverlay(base, "文件名冲突安全确认（禁止静默覆盖）", lines, "按键选择：r 自动改名 / s 跳过冲突 / Esc 取消", m)

	case modalRename:
		lines := []string{
			"为任务设置新的目标名称以继续重传（原任务保持状态不覆盖）：",
			"",
			m.renameInput.View(),
		}
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
		return renderModalOverlay(base, "退出确认", lines, "按 p 安全暂停退出 · f 强制退出 · Esc 返回", m)

	case modalClearHistory:
		lines := []string{
			"确定清空所有已完成 / 已取消 / 已跳过的历史归档记录吗？",
			m.th.badgeMuted.Render("（注：仅清理 SQLite 历史表，绝不删除本地文件或影响活动队列）"),
		}
		return renderModalOverlay(base, "清空历史记录", lines, "Enter / y 确认清空 · Esc 取消", m)
	}
	return base
}

func buildTaskDetailLines(dirLabel, stateText, remote, localPath string, done, size uint64, shaHex, tid, errMsg string, updated time.Time) []string {
	lines := []string{
		fmt.Sprintf("方向与状态：%s · %s", dirLabel, stateText),
		fmt.Sprintf("远端文件名：%s", remote),
		fmt.Sprintf("本地路径：  %s", localPath),
		fmt.Sprintf("进度与大小：%s / %s", progress.FormatBytes(done), progress.FormatBytes(size)),
		fmt.Sprintf("最近更新：  %s", updated.Format("2006-01-02 15:04:05")),
	}
	if tid != "" {
		lines = append(lines, "TransferID："+tid)
	}
	if shaHex != "" {
		lines = append(lines, "SHA-256：   "+shaHex)
	}
	if errMsg != "" {
		lines = append(lines, "", "错误/提示说明：", "  "+errMsg)
	}
	return lines
}
