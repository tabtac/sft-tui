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
		if r < ' ' || (r >= 0x7f && r <= 0x9f) {
			return ' '
		}
		return r
	}, s)
}

// truncCell 按显示宽度截断文本并保留已有合法 ANSI 样式序列。
// 来自外部文件名、日志或错误信息的未受信任纯文本应先调用 sanitizeInline / truncPlainCell 再着色。
func truncCell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// truncPlainCell 先清理控制字符，再按显示宽度截断未着色的外部或原始文本。
func truncPlainCell(s string, width int) string {
	return truncCell(sanitizeInline(s), width)
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

func padPlainRightCell(s string, width int) string {
	return padRightCell(sanitizeInline(s), width)
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

// highlightRow 用光标高亮样式渲染可能包含内嵌 ANSI 颜色序列的行，确保内嵌重置不会打断整行背景色。
func highlightRow(th theme, styled string, width int) string {
	padded := padRightCell(styled, width)
	probe := th.rowCursor.Render("\x00")
	open, closeSeq, ok := strings.Cut(probe, "\x00")
	if !ok || open == "" {
		return padded
	}
	replacer := strings.NewReplacer(
		"\x1b[0m", "\x1b[0m"+open,
		"\x1b[m", "\x1b[m"+open,
	)
	body := strings.TrimSuffix(replacer.Replace(padded), open)
	return open + body + closeSeq
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

func taskStateText(task queue.Task) string {
	if task.WantCancel && task.State == queue.Pending {
		return "取消待重试"
	}
	switch task.State {
	case queue.Transferring:
		return "传输中"
	case queue.Waiting:
		return "等待中"
	case queue.Preparing:
		return "准备中"
	case queue.Hashing:
		return "摘要中"
	case queue.Verifying:
		return "校验中"
	case queue.Pausing:
		return "暂停中"
	case queue.Paused:
		return "已暂停"
	case queue.Reconnecting:
		return "重连中"
	case queue.Pending:
		return "待处理"
	case queue.Cancelling:
		return "取消中"
	case queue.Completed:
		return "已完成"
	case queue.Cancelled:
		return "已取消"
	case queue.Skipped:
		return "已跳过"
	default:
		return sanitizeInline(string(task.State))
	}
}

func stateBadge(t theme, task queue.Task) string {
	if task.WantCancel && task.State == queue.Pending {
		return t.badgeWarn.Render("! 取消待重试")
	}
	switch task.State {
	case queue.Transferring:
		return t.badgeOk.Render("▶ 传输中")
	case queue.Waiting:
		return t.badgeMuted.Render("◷ 等待中")
	case queue.Preparing:
		return t.badgeMuted.Render("◈ 准备中")
	case queue.Hashing:
		return t.badgeMuted.Render("♯ 摘要中")
	case queue.Verifying:
		return t.badgeOk.Render("✓ 校验中")
	case queue.Pausing:
		return t.badgeWarn.Render("⏸ 暂停中")
	case queue.Paused:
		return t.badgeWarn.Render("‖ 已暂停")
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
		return t.badgeMuted.Render(sanitizeInline(string(task.State)))
	}
}

func serverDirectionText(dir string) string {
	switch strings.ToLower(strings.TrimSpace(dir)) {
	case "upload":
		return "↑ 上传"
	case "download":
		return "↓ 下载"
	default:
		return sanitizeInline(dir)
	}
}

func serverTaskStateText(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "transferring", "running", "active":
		return "传输中"
	case "waiting", "queued":
		return "等待中"
	case "preparing":
		return "准备中"
	case "verifying":
		return "校验中"
	case "paused":
		return "已暂停"
	case "completed", "done":
		return "已完成"
	case "cancelled", "canceled", "aborted":
		return "已取消"
	case "failed", "error":
		return "失败"
	default:
		return sanitizeInline(state)
	}
}

func serverTaskStateBadge(t theme, state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "transferring", "running", "active":
		return t.badgeOk.Render("▶ 传输中")
	case "waiting", "queued":
		return t.badgeMuted.Render("◷ 等待中")
	case "preparing":
		return t.badgeMuted.Render("◈ 准备中")
	case "verifying":
		return t.badgeOk.Render("✓ 校验中")
	case "paused":
		return t.badgeWarn.Render("‖ 已暂停")
	case "completed", "done":
		return t.badgeOk.Render("✔ 已完成")
	case "cancelled", "canceled", "aborted":
		return t.badgeMuted.Render("⊘ 已取消")
	case "failed", "error":
		return t.badgeErr.Render("✕ 失败")
	default:
		return t.badgeMuted.Render(sanitizeInline(state))
	}
}

func certStateBadge(t theme, state string) string {
	text := sanitizeInline(state)
	switch {
	case state == "有效":
		return t.badgeOk.Render("✔ " + text)
	case strings.Contains(state, "即将"):
		return t.badgeWarn.Render("⚠ " + text)
	case strings.Contains(state, "过期") || strings.Contains(state, "缺失") || strings.Contains(state, "无效") || strings.Contains(state, "损坏") || strings.Contains(state, "错误") || strings.Contains(state, "失败"):
		return t.badgeErr.Render("✕ " + text)
	default:
		return t.badgeWarn.Render("⚠ " + text)
	}
}

func formatCertExpiry(expiry string, notAfter time.Time) string {
	if notAfter.IsZero() {
		return sanitizeInline(expiry)
	}
	dateStr := notAfter.Local().Format("2006-01-02")
	rem := time.Until(notAfter)
	if rem <= 0 {
		return dateStr + " (已过期)"
	}
	days := int(rem.Hours() / 24)
	if days < 1 {
		return dateStr + " (<1天)"
	}
	return fmt.Sprintf("%s (剩%d天)", dateStr, days)
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
		return sizeStr + "  ·  " + sanitizeInline(t.Error)
	}
	return sizeStr
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func spinnerChar(tick int) string {
	return spinnerFrames[max(0, tick)%len(spinnerFrames)]
}

func formatBusyBadge(th theme, tick int, label string, started time.Time) string {
	txt := spinnerChar(tick) + " " + sanitizeInline(label)
	if !started.IsZero() {
		if sec := int(time.Since(started).Seconds()); sec >= 1 {
			txt += fmt.Sprintf(" (%ds)", sec)
		}
	}
	return th.badgeWarn.Render(txt)
}

const (
	toastDuration      = 2500 * time.Millisecond
	toastErrorDuration = 3000 * time.Millisecond
)

type toastLevel uint8

const (
	toastInfo toastLevel = iota
	toastSuccess
	toastWarn
	toastError
)

func inferToastLevel(msg string) toastLevel {
	switch {
	case strings.Contains(msg, "失败") || strings.Contains(msg, "错误") || strings.Contains(msg, "无法") || strings.Contains(msg, "无效"):
		return toastError
	case strings.Contains(msg, "请先") || strings.Contains(msg, "请稍候") || strings.Contains(msg, "仅") || strings.Contains(msg, "暂无") || strings.Contains(msg, "未运行") || strings.Contains(msg, "检测到"):
		return toastWarn
	case strings.HasPrefix(msg, "已") || strings.Contains(msg, "成功") || strings.Contains(msg, "完成"):
		return toastSuccess
	default:
		return toastInfo
	}
}

func renderToastInline(th theme, level toastLevel, msg string, maxW int) string {
	clean := sanitizeInline(msg)
	if clean == "" {
		return ""
	}
	var icon string
	var style lipgloss.Style
	switch level {
	case toastSuccess:
		icon = "✔ "
		style = th.badgeOk
	case toastError:
		icon = "✕ "
		style = th.badgeErr
	case toastWarn:
		icon = "⚠ "
		style = th.toast
	default:
		icon = "· "
		style = th.hintKey
	}
	txt := icon + clean
	if maxW > 0 {
		txt = truncPlainCell(txt, maxW)
	}
	return style.Render(txt)
}

func scrollPosBadge(th theme, cursor, total, visRows int) string {
	if total <= 0 || total <= visRows {
		return ""
	}
	pos := max(1, min(total, cursor+1))
	return th.badgeMuted.Render(fmt.Sprintf("%d/%d", pos, total))
}

func applyToast(dst *string, until *time.Time, msg string, ttl time.Duration) {
	*dst = msg
	if msg == "" || ttl <= 0 {
		*until = time.Time{}
		return
	}
	*until = time.Now().Add(ttl)
}

func expireToast(dst *string, until *time.Time, now time.Time) {
	if *dst == "" || until.IsZero() {
		return
	}
	if !now.Before(*until) {
		*dst = ""
		*until = time.Time{}
	}
}

func effectiveTickNow(tickAt time.Time) time.Time {
	now := time.Now()
	if tickAt.After(now) {
		return tickAt
	}
	return now
}

func remainingToastInterval(toast string, until time.Time, base time.Duration) time.Duration {
	if toast == "" || until.IsZero() {
		return base
	}
	rem := time.Until(until)
	if rem <= 0 {
		return min(base, 50*time.Millisecond)
	}
	if rem < base {
		return rem
	}
	return base
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
		key := sanitizeInline(c.key)
		label := sanitizeInline(c.label)
		reason := sanitizeInline(c.reason)
		var txt string
		if reason != "" {
			txt = th.badgeMuted.Render(fmt.Sprintf("[%s]%s", key, label))
		} else {
			txt = th.hintKey.Render("["+key+"]") + th.rowNormal.Render(label)
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
		prefixRendered = th.hintKey.Render(sanitizeInline(areaPrefix))
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
