// Package tui 提供基于 Bubble Tea + Lip Gloss 的跨平台（Linux/Windows）交互式终端客户端。
package tui

import (
	"encoding/json"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"sft-tui/internal/pal"
)

type uiPrefs struct {
	LocalOrder  int `json:"local_order,omitempty"`
	RemoteOrder int `json:"remote_order,omitempty"`
}

type prefsSavedMsg struct {
	err error
}

func resolvePrefsPath(override, exeDir, filename string) string {
	if override != "" {
		return override
	}
	if exeDir != "" {
		return filepath.Join(exeDir, filename)
	}
	return filename
}

func loadUIPrefs(path string) uiPrefs {
	data, err := pal.ReadSmall(path, 8192)
	if err != nil {
		return uiPrefs{}
	}
	var prefs uiPrefs
	if err := json.Unmarshal(data, &prefs); err != nil {
		return uiPrefs{}
	}
	return prefs
}

func saveUIPrefsCmd(path string, prefs uiPrefs) tea.Cmd {
	if path == "" {
		return nil
	}
	return func() tea.Msg {
		data, err := json.MarshalIndent(prefs, "", "  ")
		if err != nil {
			return prefsSavedMsg{err: err}
		}
		if err := pal.EnsurePrivateDir(filepath.Dir(path)); err != nil && !os.IsExist(err) {
			return prefsSavedMsg{err: err}
		}
		return prefsSavedMsg{err: pal.StageAndReplaceMode(path, append(data, '\n'), 0600, false)}
	}
}

func tabHitIndex(x, startOffset int, labels []string, th theme, activeIdx int) int {
	pos := startOffset
	for i, lbl := range labels {
		var w int
		if i == activeIdx {
			w = lipgloss.Width(th.tabActive.Render(lbl))
		} else {
			w = lipgloss.Width(th.tabInactive.Render(lbl))
		}
		if x >= pos && x < pos+w {
			return i
		}
		pos += w
	}
	return -1
}

type theme struct {
	primary     lipgloss.TerminalColor
	secondary   lipgloss.TerminalColor
	accent      lipgloss.TerminalColor
	success     lipgloss.TerminalColor
	warning     lipgloss.TerminalColor
	danger      lipgloss.TerminalColor
	muted       lipgloss.TerminalColor
	text        lipgloss.TerminalColor
	border      lipgloss.TerminalColor
	highlightBg lipgloss.TerminalColor

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

// makeTheme 构建基于终端深浅背景动态自适应的高对比度配色方案。
func makeTheme() theme {
	t := theme{
		primary:     lipgloss.AdaptiveColor{Dark: "#38BDF8", Light: "#0369A1"}, // 晴空蓝 / 深天蓝
		secondary:   lipgloss.AdaptiveColor{Dark: "#A5B4FC", Light: "#4338CA"}, // 靛紫
		accent:      lipgloss.AdaptiveColor{Dark: "#22D3EE", Light: "#0E7490"}, // 青蓝焦点
		success:     lipgloss.AdaptiveColor{Dark: "#34D399", Light: "#047857"}, // 翡翠绿
		warning:     lipgloss.AdaptiveColor{Dark: "#FBBF24", Light: "#B45309"}, // 琥珀黄
		danger:      lipgloss.AdaptiveColor{Dark: "#F87171", Light: "#B91C1C"}, // 珊瑚红
		muted:       lipgloss.AdaptiveColor{Dark: "#94A3B8", Light: "#475569"}, // 高对比次级文字
		text:        lipgloss.AdaptiveColor{Dark: "#F1F5F9", Light: "#0F172A"}, // 主正文
		border:      lipgloss.AdaptiveColor{Dark: "#475569", Light: "#94A3B8"}, // 细边框灰
		highlightBg: lipgloss.AdaptiveColor{Dark: "#1E293B", Light: "#DBEAFE"}, // 选中行底色
	}
	footerFg := lipgloss.AdaptiveColor{Dark: "#CBD5E1", Light: "#334155"}
	badgeAppFg := lipgloss.AdaptiveColor{Dark: "#0F172A", Light: "#FFFFFF"}

	t.headerBar = lipgloss.NewStyle().Foreground(t.text).Padding(0, 1)
	t.badgeApp = lipgloss.NewStyle().Background(t.primary).Foreground(badgeAppFg).Bold(true).Padding(0, 1)
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
		Foreground(badgeAppFg).
		Bold(true).
		Padding(0, 1)
	t.tabInactive = lipgloss.NewStyle().
		Foreground(t.muted).
		Padding(0, 1)

	t.rowNormal = lipgloss.NewStyle().Foreground(t.text)
	t.rowCursor = lipgloss.NewStyle().Background(t.highlightBg).Foreground(t.accent).Bold(true)
	t.rowSelected = lipgloss.NewStyle().Foreground(t.success).Bold(true)
	t.footerBar = lipgloss.NewStyle().Foreground(footerFg).Padding(0, 1)
	t.toast = lipgloss.NewStyle().Foreground(t.warning).Bold(true)

	t.modalBox = lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(t.accent).
		Padding(1, 2)
	t.modalTitle = lipgloss.NewStyle().Foreground(t.accent).Bold(true)
	t.hintKey = lipgloss.NewStyle().Foreground(t.primary).Bold(true)
	return t
}
