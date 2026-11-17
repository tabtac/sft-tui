package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

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

func renderModalBox(width, height int, th theme, title string, lines []string, footer string) string {
	boxW := min(max(44, width-4), 86)
	innerW := boxW - 6 // 2 border + 4 horizontal padding
	var body []string
	body = append(body, th.modalTitle.Render(truncCell(title, innerW)))
	body = append(body, lipgloss.NewStyle().Foreground(th.border).Render(strings.Repeat("─", innerW)))
	for _, ln := range lines {
		body = append(body, ln)
	}
	if footer != "" {
		body = append(body, lipgloss.NewStyle().Foreground(th.border).Render(strings.Repeat("─", innerW)))
		body = append(body, th.badgeMuted.Render(footer))
	}
	box := th.modalBox.Width(boxW - 2).Render(strings.Join(body, "\n"))
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box,
		lipgloss.WithWhitespaceChars(" "),
	)
}

func renderModalOverlay(_ string, title string, lines []string, footer string, m *model) string {
	return renderModalBox(m.width, m.height, m.th, title, lines, footer)
}

func (m *model) viewModal(base string) string {
	switch m.modal {
	case modalHelp:
		lines := []string{
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
			"  p / c / a        暂停单项 / 取消任务 / 全部暂停或全部恢复",
			"  n / X / Enter    冲突改名重传 / 清空历史归档 / 查看任务与校验详情",
		}
		return renderModalOverlay(base, "键盘快捷键说明", lines, "按 Esc 或 ? 关闭", m)

	case modalDetail:
		return renderModalOverlay(base, m.detailTitle, m.detailLines, "按 Enter 或 Esc 关闭", m)

	case modalIdentity:
		lines := []string{
			"已保存或发现的身份包（↑/↓ 选择后按 Enter 激活切换）：",
			"",
		}
		if len(m.idChoices) == 0 {
			lines = append(lines, m.th.badgeWarn.Render("  未在当前目录及程序旁发现可用的 client*.json，请在下方输入路径导入。"))
		} else {
			maxShow := min(8, len(m.idChoices))
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
				lines = append(lines, style.Render(truncCell(row, 74)))
			}
		}
		lines = append(lines, "", "从指定目录或配置文件导入新身份：")
		lines = append(lines, m.idInput.View())
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
			lines = append(lines, "  • "+truncCell(m.conflictNames[i], 68))
		}
		if len(m.conflictNames) > limit {
			lines = append(lines, fmt.Sprintf("  … 另有 %d 个冲突文件", len(m.conflictNames)-limit))
		}
		lines = append(lines, "",
			fmt.Sprintf("共待入队 %d 个文件（冲突 %d 个）。请选择处理策略：", len(m.pendingSpecs), len(m.conflictNames)),
			"  "+m.th.hintKey.Render("[r]")+" 自动为冲突项生成不重复新文件名（如 file (1).ext）后全部入队",
			"  "+m.th.hintKey.Render("[s]")+" 跳过冲突文件，仅入队其余无冲突文件",
			"  "+m.th.hintKey.Render("[Esc]")+" 取消本次批量传输",
		)
		return renderModalOverlay(base, "文件名冲突安全确认（禁止静默覆盖）", lines, "按键选择：r 自动改名 / s 跳过冲突 / Esc 取消", m)

	case modalRename:
		lines := []string{
			"为任务设置新的目标名称以继续重传（原任务保持状态不覆盖）：",
			"",
			m.renameInput.View(),
		}
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

func taskDetailLines(t, now queueTaskDetail) []string {
	// placeholder kept clean via direct helper below
	_ = t
	_ = now
	return nil
}

type queueTaskDetail struct{}

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
