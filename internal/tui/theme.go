// Package tui 提供基于 Bubble Tea + Lip Gloss 的跨平台（Linux/Windows）交互式终端客户端。
package tui

import "github.com/charmbracelet/lipgloss"

type theme struct {
	dark        bool
	primary     lipgloss.Color
	secondary   lipgloss.Color
	accent      lipgloss.Color
	success     lipgloss.Color
	warning     lipgloss.Color
	danger      lipgloss.Color
	muted       lipgloss.Color
	text        lipgloss.Color
	border      lipgloss.Color
	panelBg     lipgloss.Color
	highlightBg lipgloss.Color

	headerBar   lipgloss.Style
	badgeApp    lipgloss.Style
	badgeID     lipgloss.Style
	badgeOk     lipgloss.Style
	badgeWarn   lipgloss.Style
	badgeErr    lipgloss.Style
	badgeMuted  lipgloss.Style
	panel       lipgloss.Style
	panelActive lipgloss.Style
	tabActive   lipgloss.Style
	tabInactive lipgloss.Style
	rowNormal   lipgloss.Style
	rowCursor   lipgloss.Style
	rowSelected lipgloss.Style
	footerBar   lipgloss.Style
	toast       lipgloss.Style
	modalBox    lipgloss.Style
	modalTitle  lipgloss.Style
	hintKey     lipgloss.Style
}

func makeTheme(dark bool) theme {
	t := theme{dark: dark}
	if dark {
		t.primary = lipgloss.Color("#38BDF8")     // 晴空蓝
		t.secondary = lipgloss.Color("#818CF8")   // 靛紫
		t.accent = lipgloss.Color("#22D3EE")      // 青蓝焦点
		t.success = lipgloss.Color("#34D399")     // 翡翠绿
		t.warning = lipgloss.Color("#FBBF24")     // 琥珀黄
		t.danger = lipgloss.Color("#F87171")      // 珊瑚红
		t.muted = lipgloss.Color("#64748B")       // 暗灰
		t.text = lipgloss.Color("#E2E8F0")        // 柔白
		t.border = lipgloss.Color("#334155")      // 细边框灰
		t.highlightBg = lipgloss.Color("#1E293B") // 选中行底色
	} else {
		t.primary = lipgloss.Color("#0284C7")
		t.secondary = lipgloss.Color("#4F46E5")
		t.accent = lipgloss.Color("#0369A1")
		t.success = lipgloss.Color("#059669")
		t.warning = lipgloss.Color("#D97706")
		t.danger = lipgloss.Color("#DC2626")
		t.muted = lipgloss.Color("#6B7280")
		t.text = lipgloss.Color("#0F172A")
		t.border = lipgloss.Color("#CBD5E1")
		t.highlightBg = lipgloss.Color("#E0F2FE")
	}

	t.headerBar = lipgloss.NewStyle().Foreground(t.text).Padding(0, 1)
	t.badgeApp = lipgloss.NewStyle().Background(t.primary).Foreground(lipgloss.Color("#0F172A")).Bold(true).Padding(0, 1)
	t.badgeID = lipgloss.NewStyle().Background(t.highlightBg).Foreground(t.text).Bold(true).Padding(0, 1)
	t.badgeOk = lipgloss.NewStyle().Foreground(t.success).Bold(true)
	t.badgeWarn = lipgloss.NewStyle().Foreground(t.warning).Bold(true)
	t.badgeErr = lipgloss.NewStyle().Foreground(t.danger).Bold(true)
	t.badgeMuted = lipgloss.NewStyle().Foreground(t.muted)

	t.panel = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.border).
		Padding(0, 1)
	t.panelActive = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(t.accent).
		Padding(0, 1)

	t.tabActive = lipgloss.NewStyle().
		Background(t.primary).
		Foreground(lipgloss.Color("#0F172A")).
		Bold(true).
		Padding(0, 1)
	t.tabInactive = lipgloss.NewStyle().
		Foreground(t.muted).
		Padding(0, 1)

	t.rowNormal = lipgloss.NewStyle().Foreground(t.text)
	t.rowCursor = lipgloss.NewStyle().Background(t.highlightBg).Foreground(t.accent).Bold(true)
	t.rowSelected = lipgloss.NewStyle().Foreground(t.success).Bold(true)
	t.footerBar = lipgloss.NewStyle().Foreground(t.muted).Padding(0, 1)
	t.toast = lipgloss.NewStyle().Foreground(t.warning).Bold(true)

	t.modalBox = lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(t.accent).
		Padding(1, 2)
	t.modalTitle = lipgloss.NewStyle().Foreground(t.accent).Bold(true)
	t.hintKey = lipgloss.NewStyle().Foreground(t.primary).Bold(true)
	return t
}
