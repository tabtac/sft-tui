package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"sft-tui/internal/progress"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
)

// sanitizeInline 替换换行符与控制字符为空格，防止日志或异常名称破坏单行渲染或注入转义序列。
func sanitizeInline(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

func truncCell(s string, width int) string {
	s = sanitizeInline(s)
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

func padRightCell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = truncCell(s, width)
	w := lipgloss.Width(s)
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

func padLeftCell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = truncCell(s, width)
	w := lipgloss.Width(s)
	if w < width {
		return strings.Repeat(" ", width-w) + s
	}
	return s
}

func formatETA(d time.Duration) string {
	if d <= 0 {
		return "估算中"
	}
	sec := int(d.Round(time.Second).Seconds())
	if sec < 60 {
		return fmt.Sprintf("%ds", max(1, sec))
	}
	if sec < 3600 {
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh%02dm", sec/3600, (sec%3600)/60)
}

func stateBadge(t theme, task queue.Task) string {
	if task.WantCancel && task.State == queue.Pending {
		return t.badgeWarn.Render("! 取消待重试")
	}
	switch task.State {
	case queue.Transferring:
		return t.badgeOk.Render("▶ 传输中")
	case queue.Waiting:
		return t.badgeMuted.Render("⏳ 等待中")
	case queue.Preparing:
		return t.badgeMuted.Render("◈ 准备中")
	case queue.Hashing:
		return t.badgeMuted.Render("♯ 摘要中")
	case queue.Verifying:
		return t.badgeOk.Render("✓ 校验中")
	case queue.Pausing:
		return t.badgeWarn.Render("⏸ 暂停中")
	case queue.Paused:
		return t.badgeWarn.Render("▮▮ 已暂停")
	case queue.Reconnecting:
		return t.badgeWarn.Render("↻ 重连中")
	case queue.Pending:
		return t.badgeErr.Render("! 待处理")
	case queue.Cancelling:
		return t.badgeErr.Render("✕ 取消中")
	case queue.Completed:
		return t.badgeOk.Render("✔ 已完成")
	case queue.Cancelled:
		return t.badgeMuted.Render("⊘ 已取消")
	case queue.Skipped:
		return t.badgeMuted.Render("↷ 已跳过")
	default:
		return t.badgeMuted.Render(string(task.State))
	}
}

func directionIcon(d protocol.Direction) string {
	if d == protocol.DirectionUpload {
		return "↑ 上传"
	}
	return "↓ 下载"
}

func renderProgressBar(t theme, done, total uint64, st queue.State, barWidth int) string {
	if barWidth < 6 {
		barWidth = 6
	}
	frac := 0.0
	if total > 0 {
		frac = math.Min(1, float64(done)/float64(total))
	} else if st == queue.Completed {
		frac = 1
	}
	filled := int(math.Round(frac * float64(barWidth)))
	filled = max(0, min(barWidth, filled))

	fillColor := t.primary
	if st == queue.Completed {
		fillColor = t.success
	} else if st == queue.Paused || st == queue.Pausing || st == queue.Reconnecting {
		fillColor = t.warning
	} else if st == queue.Pending || st == queue.Cancelling {
		fillColor = t.danger
	}

	bar := lipgloss.NewStyle().Foreground(fillColor).Render(strings.Repeat("━", filled)) +
		lipgloss.NewStyle().Foreground(t.border).Render(strings.Repeat("─", barWidth-filled))
	pct := fmt.Sprintf("%3.0f%%", frac*100)
	return bar + " " + pct
}

func formatTaskMetrics(t queue.Task) string {
	sizeStr := fmt.Sprintf("%s/%s", progress.FormatBytes(t.Done), progress.FormatBytes(t.Size))
	if t.State == queue.Transferring {
		return fmt.Sprintf("%s  %s  ETA %s", sizeStr, progress.FormatRate(t.Speed), formatETA(t.ETA))
	}
	if t.Error != "" {
		return sizeStr + "  ·  " + t.Error
	}
	return sizeStr
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func spinnerChar(tick int) string {
	return spinnerFrames[max(0, tick)%len(spinnerFrames)]
}

func formatBusyBadge(th theme, tick int, label string, started time.Time) string {
	txt := spinnerChar(tick) + " " + label
	if !started.IsZero() {
		if sec := int(time.Since(started).Seconds()); sec >= 1 {
			txt += fmt.Sprintf(" (%ds)", sec)
		}
	}
	return th.badgeWarn.Render(txt)
}

type footerChip struct {
	key       string
	label     string
	reason    string // 非空表示当前不可用及原因
	essential bool
}

func renderActionFooter(th theme, width int, areaPrefix string, chips []footerChip) string {
	budget := max(1, width-2)
	type renderedChip struct {
		text      string
		width     int
		essential bool
	}
	items := make([]renderedChip, 0, len(chips))
	for _, c := range chips {
		var txt string
		if c.reason != "" {
			txt = th.badgeMuted.Render(fmt.Sprintf("[%s]%s(%s)", c.key, c.label, c.reason))
		} else {
			txt = th.hintKey.Render("["+c.key+"]") + th.rowNormal.Render(c.label)
		}
		items = append(items, renderedChip{text: txt, width: lipgloss.Width(txt), essential: c.essential})
	}

	sep := "  "
	sepW := 2
	if width <= 96 {
		sep = " "
		sepW = 1
	}

	var prefixRendered string
	prefixW := 0
	if areaPrefix != "" {
		prefixRendered = th.hintKey.Render(areaPrefix)
		prefixW = lipgloss.Width(prefixRendered)
	}

	// 第一遍优先纳入 essential 操作（含不可用原因），第二遍按剩余宽度顺序填充辅助提示
	included := make([]bool, len(items))
	used := prefixW
	hasAny := prefixW > 0
	tryAdd := func(idx int, forceFirst bool) bool {
		addW := items[idx].width
		if hasAny {
			addW += sepW
		}
		if used+addW > budget && !forceFirst {
			return false
		}
		included[idx] = true
		used += addW
		hasAny = true
		return true
	}

	for i, it := range items {
		if it.essential {
			tryAdd(i, !hasAny || i == 0)
		}
	}
	for i, it := range items {
		if !it.essential && !included[i] {
			tryAdd(i, false)
		}
	}

	var parts []string
	if prefixRendered != "" {
		parts = append(parts, prefixRendered)
	}
	for i, it := range items {
		if included[i] {
			parts = append(parts, it.text)
		}
	}
	return th.footerBar.Width(width).Render(truncCell(strings.Join(parts, sep), budget))
}
