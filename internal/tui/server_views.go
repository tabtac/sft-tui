package tui

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"sft-tui/internal/config"
	"sft-tui/internal/progress"
)

func (m *serverModel) View() string {
	if m.width <= 0 || m.height <= 0 {
		return "正在初始化服务端管理界面…"
	}
	if m.width < 52 || m.height < 14 {
		return m.th.badgeWarn.Render(fmt.Sprintf("终端窗口过小（%d×%d），请至少调整至 52×14 以上", m.width, m.height))
	}

	header := m.renderHeader()
	tabBar := m.renderTabBar()
	footer := m.renderFooter()
	bodyH := max(6, m.height-lipgloss.Height(header)-lipgloss.Height(tabBar)-lipgloss.Height(footer))

	var body string
	switch m.page {
	case 0:
		body = m.renderOverviewPage(bodyH)
	case 1:
		body = m.renderUsersPage(bodyH)
	case 2:
		body = m.renderTransfersPage(bodyH)
	case 3:
		body = m.renderCertsPage(bodyH)
	case 4:
		body = m.renderSettingsPage(bodyH)
	default:
		body = m.renderLogsPage(bodyH)
	}

	base := lipgloss.JoinVertical(lipgloss.Left, header, tabBar, body, footer)
	if m.modal != srvModalNone {
		return m.renderModal(base)
	}
	return base
}

func (m *serverModel) renderHeader() string {
	brand := m.th.badgeApp.Render(" SFTD-UI ")

	stBadge := m.th.badgeMuted.Render("○ 离线")
	listenStr := m.cfg.Listen
	if listenStr == "" {
		listenStr = "—"
	}
	rightFull := "等待连接本机服务"
	rightShort := "待连接"
	if m.runtime != nil {
		listenStr = m.runtime.Listen
		switch m.runtime.State {
		case "running":
			stBadge = m.th.badgeOk.Render("● 运行中")
		case "draining":
			stBadge = m.th.badgeWarn.Render("◐ 排空停服")
		default:
			stBadge = m.th.badgeErr.Render("✕ " + m.runtime.State)
		}
		uptime := time.Since(m.runtime.Started).Round(time.Second)
		rightFull = fmt.Sprintf("在线:%d  连接:%d  活动:%d  ↑%s ↓%s  运行 %s",
			m.runtime.OnlineUsers, m.runtime.Connections, m.runtime.ActiveTasks,
			progress.FormatRate(m.runtime.ReceiveRate), progress.FormatRate(m.runtime.SendRate), uptime)
		rightShort = fmt.Sprintf("在线:%d ↑%s ↓%s",
			m.runtime.OnlineUsers,
			progress.FormatRate(m.runtime.ReceiveRate), progress.FormatRate(m.runtime.SendRate))
	} else if strings.Contains(m.runtimeState, "异常") {
		stBadge = m.th.badgeErr.Render("✕ " + truncCell(m.runtimeState, 24))
	}

	addrBadge := m.th.badgeID.Render(" " + listenStr + " ")
	left := lipgloss.JoinHorizontal(lipgloss.Center, brand, " ", stBadge, " ", addrBadge)
	avail := max(0, m.width-2-lipgloss.Width(left)-1)
	rightInfo := rightFull
	if lipgloss.Width(rightInfo) > avail {
		if lipgloss.Width(rightShort) <= avail {
			rightInfo = rightShort
		} else if avail >= 6 {
			rightInfo = truncCell(rightShort, avail)
		} else {
			rightInfo = ""
		}
	}
	stats := m.th.badgeMuted.Render(rightInfo)
	gap := max(1, m.width-2-lipgloss.Width(left)-lipgloss.Width(stats))
	if rightInfo == "" {
		return m.th.headerBar.Width(m.width).Render(truncCell(left, m.width-2))
	}
	return m.th.headerBar.Width(m.width).Render(truncCell(left+strings.Repeat(" ", gap)+stats, m.width-2))
}

func (m *serverModel) renderTabBar() string {
	names := []string{"1 概览", "2 用户", "3 传输", "4 证书", "5 设置", "6 日志"}
	var tabs []string
	for i, name := range names {
		if i == m.page {
			tabs = append(tabs, m.th.tabActive.Render(name))
		} else {
			tabs = append(tabs, m.th.tabInactive.Render(name))
		}
	}
	row := lipgloss.JoinHorizontal(lipgloss.Center, tabs...)
	avail := m.width - 2 - lipgloss.Width(row) - 3
	if m.editingQuery && avail > 8 {
		promptLbl := "筛选: "
		switch m.queryMode {
		case "log-user":
			promptLbl = "按用户筛选: "
		case "log-file":
			promptLbl = "按文件筛选: "
		}
		row += "   " + m.th.hintKey.Render(promptLbl) + m.queryInput.View()
	} else if m.busy && avail >= 6 {
		op := m.busyOp
		if op == "" {
			op = "操作执行中"
		}
		row += "   " + formatBusyBadge(m.th, m.busySpin, op, m.busyStart)
	} else if m.statusToast != "" && avail >= 6 {
		row += "   " + m.th.toast.Render("· "+truncCell(m.statusToast, avail-2))
	}
	return lipgloss.NewStyle().Padding(0, 1).Width(m.width).Render(truncCell(row, m.width-2))
}

func (m *serverModel) renderFooter() string {
	if m.editingQuery {
		hint := "筛选过滤：输入关键字 · Enter 应用 · Esc 清空"
		if m.queryMode == "log-user" {
			hint = "日志用户过滤：输入完整用户名 · Enter 应用 · Esc 清空"
		} else if m.queryMode == "log-file" {
			hint = "日志文件名过滤：输入文件名片段 · Enter 应用 · Esc 清空"
		}
		return m.th.footerBar.Width(m.width).Render(truncCell(hint, max(1, m.width-2)))
	}
	busyReason := ""
	if m.busy {
		busyReason = "忙"
	}
	cfgReason := ""
	if m.cfgErr != nil {
		cfgReason = "缺配置"
	}
	quitReason := ""
	if m.busy {
		quitReason = "执行中"
	}
	switch m.page {
	case 0:
		startReason := busyReason
		if startReason == "" {
			if m.runtime != nil && m.runtime.State == "running" {
				startReason = "运行中"
			} else if m.cfgErr != nil {
				startReason = "缺配置"
			}
		}
		stopReason := busyReason
		if stopReason == "" && m.runtime == nil {
			stopReason = "未运行"
		}
		reloadReason := busyReason
		if reloadReason == "" && m.runtime == nil {
			reloadReason = "未运行"
		}
		wizReason := ""
		if m.loaded && m.cfgErr == nil {
			wizReason = "已初始化"
		}
		detailReason := ""
		if m.runtime == nil || len(m.runtime.Peers) == 0 {
			detailReason = "无会话"
		}
		return renderActionFooter(m.th, m.width, "概览:", []footerChip{
			{key: "s", label: "启动", reason: startReason, essential: true},
			{key: "x", label: "停止", reason: stopReason, essential: true},
			{key: "R", label: "热加载", reason: reloadReason, essential: true},
			{key: "I", label: "向导", reason: wizReason, essential: true},
			{key: "Enter", label: "详情", reason: detailReason},
			{key: "Tab", label: "切页"},
			{key: "F6", label: "主题"},
			{key: "?", label: "帮助"},
			{key: "q", label: "退出", reason: quitReason, essential: true},
		})
	case 1:
		users := m.filteredUsers()
		noUser := ""
		toggleLabel := "禁用/启"
		renewLabel := "续期"
		if len(users) == 0 || m.cursors[1] >= len(users) {
			noUser = "无用户"
		} else {
			u := users[m.cursors[1]]
			if u.Disabled {
				toggleLabel = "重启用"
			} else {
				toggleLabel = "吊销"
			}
			if u.ExternalKey {
				renewLabel = "CSR续期"
			}
		}
		actReason := busyReason
		if actReason == "" && cfgReason != "" {
			actReason = cfgReason
		}
		userActReason := actReason
		if userActReason == "" {
			userActReason = noUser
		}
		return renderActionFooter(m.th, m.width, "用户:", []footerChip{
			{key: "n", label: "新建", reason: actReason, essential: true},
			{key: "t", label: toggleLabel, reason: userActReason, essential: true},
			{key: "p", label: "策略", reason: userActReason, essential: true},
			{key: "w", label: renewLabel, reason: userActReason, essential: true},
			{key: "e", label: "导出", reason: noUser, essential: true},
			{key: "g", label: "查日志", reason: noUser, essential: true},
			{key: "C", label: "CSR", reason: actReason},
			{key: "/", label: "搜索"},
			{key: "Enter", label: "详情", reason: noUser},
			{key: "?", label: "帮助"},
			{key: "q", label: "退出", reason: quitReason},
		})
	case 2:
		tasks := m.filteredTasks()
		detailReason := ""
		if m.runtime == nil {
			detailReason = "服务离线"
		} else if len(tasks) == 0 {
			detailReason = "无记录"
		}
		return renderActionFooter(m.th, m.width, "传输:", []footerChip{
			{key: "Enter", label: "详情", reason: detailReason, essential: true},
			{key: "g", label: "相关日志", reason: detailReason, essential: true},
			{key: "/", label: "筛选", essential: true},
			{key: "r", label: "刷新", essential: true},
			{key: "Tab", label: "切页"},
			{key: "?", label: "帮助"},
			{key: "q", label: "退出", reason: quitReason, essential: true},
		})
	case 3:
		certReason := busyReason
		if certReason == "" && cfgReason != "" {
			certReason = cfgReason
		}
		noCert := ""
		if len(m.certs) == 0 {
			noCert = "无证书"
		}
		return renderActionFooter(m.th, m.width, "证书:", []footerChip{
			{key: "K", label: "续CA", reason: certReason, essential: true},
			{key: "S", label: "轮换服务", reason: certReason, essential: true},
			{key: "C", label: "签CSR", reason: certReason, essential: true},
			{key: "Enter", label: "指纹", reason: noCert, essential: true},
			{key: "Tab", label: "切页"},
			{key: "?", label: "帮助"},
			{key: "q", label: "退出", reason: quitReason, essential: true},
		})
	case 4:
		hotReason := busyReason
		switch {
		case hotReason != "":
		case m.runtime == nil:
			hotReason = "服务离线"
		case m.cfgErr != nil:
			hotReason = "缺配置"
		case config.HotEqual(m.runtime.Config, m.cfg):
			hotReason = "热配置一致"
		}
		return renderActionFooter(m.th, m.width, "设置:", []footerChip{
			{key: "m", label: "基础", reason: cfgReason, essential: true},
			{key: "a", label: "高级", reason: cfgReason, essential: true},
			{key: "L", label: "日志限", reason: cfgReason, essential: true},
			{key: "A", label: "热加载", reason: hotReason, essential: true},
			{key: "Tab", label: "切页"},
			{key: "?", label: "帮助"},
			{key: "q", label: "退出", reason: quitReason, essential: true},
		})
	default:
		pauseLabel := "暂停"
		if m.logPaused {
			pauseLabel = "跟随"
		}
		detailReason := ""
		if m.logFallback != "" {
			detailReason = "离线原文"
		} else if len(m.logEvents) == 0 {
			detailReason = "无事件"
		}
		clearReason := ""
		if !m.hasLogFilters() {
			clearReason = "未筛选"
		}
		return renderActionFooter(m.th, m.width, "日志:", []footerChip{
			{key: "p", label: pauseLabel, essential: true},
			{key: "l", label: "等级(" + logLevelLabel(m.logMinLevel) + ")", essential: true},
			{key: "U", label: "用户", essential: true},
			{key: "F", label: "文件", essential: true},
			{key: "T", label: "时间(" + logSinceLabel(m.logSinceIdx) + ")", essential: true},
			{key: "/", label: "文本", essential: true},
			{key: "C", label: "清空", reason: clearReason},
			{key: "L", label: "上限", reason: cfgReason},
			{key: "Enter", label: "详情", reason: detailReason},
			{key: "?", label: "帮助"},
		})
	}
}

func logSinceLabel(idx int) string {
	switch idx {
	case 1:
		return "5m"
	case 2:
		return "1h"
	case 3:
		return "24h"
	default:
		return "全"
	}
}

func (m *serverModel) hasLogFilters() bool {
	return m.logMinLevel != "" || m.logQuery.User != "" || m.logQuery.File != "" || m.logQuery.Text != "" || m.logSinceIdx != 0
}

func (m *serverModel) filteredLogFallbackLines() []string {
	trimmed := strings.TrimRight(m.logFallback, "\n")
	if trimmed == "" {
		return nil
	}
	raw := strings.Split(trimmed, "\n")
	if m.logMinLevel == "" && m.logQuery.User == "" && m.logQuery.File == "" && m.logQuery.Text == "" {
		return raw
	}
	var out []string
	minLvl := strings.ToUpper(strings.TrimSpace(m.logMinLevel))
	u := strings.ToLower(strings.TrimSpace(m.logQuery.User))
	f := strings.ToLower(strings.TrimSpace(m.logQuery.File))
	txt := strings.ToLower(strings.TrimSpace(m.logQuery.Text))
	for _, ln := range raw {
		low := strings.ToLower(ln)
		if minLvl != "" {
			switch minLvl {
			case "WARN":
				if !strings.Contains(low, "warn") && !strings.Contains(low, "error") {
					continue
				}
			case "ERROR":
				if !strings.Contains(low, "error") {
					continue
				}
			case "INFO":
				if strings.Contains(low, "level=debug") || strings.Contains(low, "[debug]") {
					continue
				}
			}
		}
		if u != "" && !strings.Contains(low, u) {
			continue
		}
		if f != "" && !strings.Contains(low, f) {
			continue
		}
		if txt != "" && !strings.Contains(low, txt) {
			continue
		}
		out = append(out, ln)
	}
	return out
}

func logLevelLabel(l string) string {
	if l == "" {
		return "全部"
	}
	return l
}

func overviewLayout(width, bodyH int) (stacked bool, topCardH, topTotalH, botH int) {
	if width < 92 && bodyH >= 17 {
		topCardH = min(max(6, (bodyH-5)/2), 8)
		topTotalH = topCardH * 2
		botH = max(5, bodyH-topTotalH)
		return true, topCardH, topTotalH, botH
	}
	topCardH = min(max(6, bodyH/2), 11)
	botH = max(5, bodyH-topCardH)
	return false, topCardH, topCardH, botH
}

func (m *serverModel) renderOverviewPage(bodyH int) string {
	stacked, topH, _, botH := overviewLayout(m.width, bodyH)
	leftW, rightW := m.width, m.width
	if !stacked {
		leftW = m.width / 2
		rightW = m.width - leftW
	}
	leftInner := max(16, leftW-4)

	// 左侧：部署配置卡片
	var cfgLines []string
	if !m.loaded {
		cfgLines = append(cfgLines, m.th.badgeMuted.Render("正在读取服务端配置…"))
	} else if m.cfgErr != nil {
		cfgLines = append(cfgLines, m.th.badgeErr.Render("配置读取失败："+m.cfgErr.Error()))
		cfgLines = append(cfgLines, m.th.badgeWarn.Render("可按 [I] 键启动首次部署与证书初始化向导"))
	} else {
		warnCount := 0
		for _, c := range m.certs {
			if c.State != "有效" {
				warnCount++
			}
		}
		cfgLines = append(cfgLines,
			"配置："+truncCell(m.opts.ConfigPath, max(8, leftInner-6)),
			"存储："+truncCell(m.cfg.StorageRoot, max(8, leftInner-6)),
			fmt.Sprintf("用户：%d 个 · 限额 ↑%s ↓%s",
				len(m.cfg.AuthorizedClients),
				progress.FormatRate(float64(m.cfg.Bandwidth.UploadBytesPerSecond)),
				progress.FormatRate(float64(m.cfg.Bandwidth.DownloadBytesPerSecond))),
		)
		if m.cfg.GlobalBandwidth != nil {
			cfgLines = append(cfgLines, fmt.Sprintf("全局：↑%s  ↓%s",
				progress.FormatRate(float64(m.cfg.GlobalBandwidth.UploadBytesPerSecond)),
				progress.FormatRate(float64(m.cfg.GlobalBandwidth.DownloadBytesPerSecond))))
		}
		if warnCount > 0 {
			cfgLines = append(cfgLines, m.th.badgeWarn.Render(fmt.Sprintf("⚠ %d 张证书待处理，按 [4] 查看", warnCount)))
		}
	}
	leftCard := renderPanelBox(m.th, m.th.hintKey.Render("部署环境"), cfgLines, true, leftW, topH)

	// 右侧：流量与存储实时指标卡片
	var statLines []string
	if m.runtime == nil {
		statLines = append(statLines,
			m.th.badgeMuted.Render(m.runtimeState),
			"",
			"按 [s] 后台启动 sftd 后可实时监控连接与流量。",
		)
	} else {
		r := m.runtime
		statLines = append(statLines,
			fmt.Sprintf("已发布：%d 个 (%s)", r.PublishedFiles, progress.FormatBytes(r.PublishedBytes)),
			fmt.Sprintf("吞吐：入 %s  出 %s",
				m.th.badgeOk.Render(progress.FormatRate(r.ReceiveRate)),
				m.th.hintKey.Render(progress.FormatRate(r.SendRate))),
			fmt.Sprintf("累计：入 %s 出 %s · 成功%d/失败%d",
				progress.FormatBytes(r.Received), progress.FormatBytes(r.Sent), r.Completed, r.Failed),
			"上传趋势："+m.th.badgeOk.Render(sparklineString(m.trend, 0)),
			"下载趋势："+m.th.hintKey.Render(sparklineString(m.trend, 1)),
		)
	}
	rightCard := renderPanelBox(m.th, m.th.hintKey.Render("实时运行监控"), statLines, true, rightW, topH)
	topArea := lipgloss.JoinHorizontal(lipgloss.Top, leftCard, rightCard)
	if stacked {
		topArea = lipgloss.JoinVertical(lipgloss.Left, leftCard, rightCard)
	}

	// 底部：活跃连接列表
	innerW := max(16, m.width-4)
	visRows := max(1, botH-3)
	var peerLines []string
	if m.runtime == nil || len(m.runtime.Peers) == 0 {
		peerLines = append(peerLines, m.th.badgeMuted.Render("（当前无在线客户端连接）"))
	} else {
		peers := m.runtime.Peers
		start := scrollOffset(m.cursors[0], len(peers), visRows)
		for i := start; i < min(len(peers), start+visRows); i++ {
			p := peers[i]
			mark := "  "
			if i == m.cursors[0] {
				mark = "▶ "
			}
			line := fmt.Sprintf("%s#%-4d  %s  %s  在线 %s",
				mark, p.ID, padRightCell(p.User, min(16, max(8, innerW/5))), padRightCell(p.Remote, min(24, max(12, innerW/3))),
				time.Since(p.Since).Round(time.Second))
			if i == m.cursors[0] {
				peerLines = append(peerLines, m.th.rowCursor.Render(padRightCell(line, innerW)))
			} else {
				peerLines = append(peerLines, truncCell(line, innerW))
			}
		}
	}
	botCard := renderPanelBox(m.th, m.th.hintKey.Render("活跃 mTLS 会话"), peerLines, true, m.width, botH)
	return lipgloss.JoinVertical(lipgloss.Left, topArea, botCard)
}

func sparklineString(samples [][2]float64, dir int) string {
	if len(samples) == 0 {
		return "—"
	}
	bars := []rune("▁▂▃▄▅▆▇█")
	maxVal := 1.0
	for _, s := range samples {
		if s[dir] > maxVal {
			maxVal = s[dir]
		}
	}
	var b strings.Builder
	for _, s := range samples {
		idx := int((s[dir] / maxVal) * float64(len(bars)-1))
		idx = max(0, min(len(bars)-1, idx))
		b.WriteRune(bars[idx])
	}
	return b.String()
}

func (m *serverModel) renderUsersPage(bodyH int) string {
	innerW := max(20, m.width-4)
	visRows := max(1, bodyH-3)
	users := m.filteredUsers()

	var lines []string
	if len(users) == 0 {
		lines = append(lines, m.th.badgeMuted.Render("（暂无授权用户，按 [n] 新建用户）"))
	} else {
		certStateByUser := make(map[string]string, len(m.certs))
		for _, cr := range m.certs {
			certStateByUser[cr.Name] = cr.State
		}
		start := scrollOffset(m.cursors[1], len(users), visRows)
		for i := start; i < min(len(users), start+visRows); i++ {
			u := users[i]
			mark := "  "
			if i == m.cursors[1] {
				mark = "▶ "
			}
			st := m.th.badgeOk.Render("✔ 启用")
			if u.Disabled {
				st = m.th.badgeErr.Render("⊘ 吊销")
			}
			var perms string
			switch {
			case u.Permissions == nil:
				perms = "upload,download"
			case len(u.Permissions) == 0:
				perms = "无权限"
			default:
				perms = strings.Join(u.Permissions, ",")
			}
			bwStr := "默认"
			if u.Bandwidth != nil {
				bwStr = fmt.Sprintf("↑%s/↓%s", progress.FormatRate(float64(u.Bandwidth.UploadBytesPerSecond)), progress.FormatRate(float64(u.Bandwidth.DownloadBytesPerSecond)))
			}
			certSt := certStateByUser[u.UserID]
			if certSt == "" {
				certSt = "未知"
			}
			userW := 14
			permW := 16
			bwW := 20
			if innerW < 78 {
				userW = 10
				permW = 10
				bwW = max(10, innerW-userW-permW-26)
			}
			row := fmt.Sprintf("%s%s  %s  %s  %s  证书:%s",
				mark, padRightCell(u.UserID, userW), st, padRightCell(perms, permW), padRightCell(bwStr, bwW), certSt)
			if i == m.cursors[1] {
				lines = append(lines, m.th.rowCursor.Render(padRightCell(row, innerW)))
			} else {
				lines = append(lines, truncCell(row, innerW))
			}
		}
	}
	title := m.th.hintKey.Render(fmt.Sprintf("授权用户列表 (%d 项)", len(users)))
	return renderPanelBox(m.th, title, lines, true, m.width, bodyH)
}

func (m *serverModel) renderTransfersPage(bodyH int) string {
	innerW := max(20, m.width-4)
	visRows := max(1, bodyH-3)
	tasks := m.filteredTasks()

	var lines []string
	if m.runtime == nil {
		lines = append(lines, m.th.badgeMuted.Render("（服务离线，按 [s] 启动后实时展示传输队列）"))
	} else if len(tasks) == 0 {
		lines = append(lines, m.th.badgeMuted.Render("（当前无活动或最近传输记录）"))
	} else {
		start := scrollOffset(m.cursors[2], len(tasks), visRows)
		userW := min(12, max(8, innerW/7))
		fileW := min(24, max(10, innerW-userW-46))
		for i := start; i < min(len(tasks), start+visRows); i++ {
			t := tasks[i]
			mark := "  "
			if i == m.cursors[2] {
				mark = "▶ "
			}
			row := fmt.Sprintf("%s%s  %s  %s  %s  %s/%s  %s",
				mark, padRightCell(t.User, userW), padRightCell(t.Direction, 8), padRightCell(t.File, fileW),
				padRightCell(t.State, 10), progress.FormatBytes(t.Offset), progress.FormatBytes(t.Size),
				progress.FormatRate(t.Rate))
			if i == m.cursors[2] {
				lines = append(lines, m.th.rowCursor.Render(padRightCell(row, innerW)))
			} else {
				lines = append(lines, truncCell(row, innerW))
			}
		}
	}
	title := m.th.hintKey.Render(fmt.Sprintf("服务端传输监控 (%d 项)", len(tasks)))
	return renderPanelBox(m.th, title, lines, true, m.width, bodyH)
}

func (m *serverModel) renderCertsPage(bodyH int) string {
	innerW := max(20, m.width-4)
	visRows := max(1, bodyH-3)

	var lines []string
	if len(m.certs) == 0 {
		lines = append(lines, m.th.badgeMuted.Render("（暂无可检查的证书配置）"))
	} else {
		start := scrollOffset(m.cursors[3], len(m.certs), visRows)
		nameW := 12
		roleW := 12
		if innerW < 76 {
			nameW = 10
			roleW = 10
		}
		fpBudget := innerW - (2 + nameW + 2 + roleW + 2 + 8 + 2 + 21 + 5)
		for i := start; i < min(len(m.certs), start+visRows); i++ {
			c := m.certs[i]
			mark := "  "
			if i == m.cursors[3] {
				mark = "▶ "
			}
			st := m.th.badgeOk.Render("✔ " + c.State)
			if c.State != "有效" {
				st = m.th.badgeWarn.Render("⚠ " + c.State)
			}
			fpPart := ""
			if fpBudget >= 8 {
				fp := c.Fingerprint
				if len(fp) > fpBudget {
					fp = fp[:fpBudget] + "…"
				}
				fpPart = "  FP:" + fp
			}
			row := fmt.Sprintf("%s%s  %s  %s  到期:%s%s",
				mark, padRightCell(c.Name, nameW), padRightCell(c.Role, roleW), st, padRightCell(c.Expiry, 16), fpPart)
			if i == m.cursors[3] {
				lines = append(lines, m.th.rowCursor.Render(padRightCell(row, innerW)))
			} else {
				lines = append(lines, truncCell(row, innerW))
			}
		}
	}
	return renderPanelBox(m.th, m.th.hintKey.Render("证书信任链与有效期体检"), lines, true, m.width, bodyH)
}

func formatBandwidthSummary(bw config.Bandwidth) string {
	return fmt.Sprintf("↑ %s / ↓ %s / 突发 %s",
		progress.FormatRate(float64(bw.UploadBytesPerSecond)),
		progress.FormatRate(float64(bw.DownloadBytesPerSecond)),
		progress.FormatBytes(bw.BurstBytes))
}

func formatGlobalBandwidthSummary(gb *config.Bandwidth) string {
	if gb == nil {
		return "未启用"
	}
	return formatBandwidthSummary(*gb)
}

func formatTLSSummary(tls config.TLSFiles) string {
	return fmt.Sprintf("CA=%s Cert=%s", filepath.Base(tls.CACertificate), filepath.Base(tls.Certificate))
}

func (m *serverModel) settingsFieldLine(label, saved, runtime string, hasRuntime, hotReloadable, differ bool) string {
	mode := m.th.badgeMuted.Render("[需重启]")
	if hotReloadable {
		mode = m.th.badgeID.Render("[可热更]")
	}
	diffSuffix := ""
	if hasRuntime && differ {
		if hotReloadable {
			mode = m.th.badgeWarn.Render("[待热更]")
			diffSuffix = "  " + m.th.badgeWarn.Render("≠ 运行: "+runtime+" [按A热加载]")
		} else {
			mode = m.th.badgeErr.Render("[待重启]")
			diffSuffix = "  " + m.th.badgeErr.Render("≠ 运行: "+runtime+" [需重启生效]")
		}
	}
	return fmt.Sprintf("  %s %-26s %s%s", mode, label, saved, diffSuffix)
}

func (m *serverModel) settingsLines() []string {
	c := m.cfg
	hasRt := m.runtime != nil
	var rt config.Server
	if hasRt {
		rt = m.runtime.Config
	}
	diffTag := m.th.badgeMuted.Render("（服务离线，修改配置启动后生效）")
	if hasRt {
		switch {
		case config.RestartRequired(rt, c):
			diffTag = m.th.badgeWarn.Render("⚠ 磁盘配置与运行实例存在非热更差异（需重启生效）")
		case !config.HotEqual(rt, c):
			diffTag = m.th.badgeWarn.Render("⚠ 磁盘配置与运行实例存在未热加载的差异（按 [A] 应用热加载）")
		default:
			diffTag = m.th.badgeOk.Render("✔ 磁盘配置与运行实例一致")
		}
	}

	tlsEqual := true
	clientsEqual := true
	if hasRt {
		tmpDisk := config.Server{TLS: c.TLS, AuthorizedClients: c.AuthorizedClients}
		tmpRt := config.Server{TLS: rt.TLS, AuthorizedClients: rt.AuthorizedClients}
		tlsEqual = c.TLS == rt.TLS
		clientsEqual = config.HotEqual(tmpDisk, tmpRt)
	}

	return []string{
		diffTag,
		"",
		m.th.hintKey.Render("【基础网络、存储与日志】  按 [m] 编辑基础/带宽/配额 · [a] 编辑全部高级参数 · [L] 调整日志上限 · [A] 热重载"),
		m.settingsFieldLine("监听地址 (listen)：", c.Listen, rt.Listen, hasRt, false, c.Listen != rt.Listen),
		m.settingsFieldLine("存储目录 (storage_root)：", c.StorageRoot, rt.StorageRoot, hasRt, false, c.StorageRoot != rt.StorageRoot),
		m.settingsFieldLine("日志上限 (logging)：", progress.FormatBytes(c.Logging.MaxFileBytes)+"（达上限清空重写）", progress.FormatBytes(rt.Logging.MaxFileBytes), hasRt, false, c.Logging.MaxFileBytes != rt.Logging.MaxFileBytes),
		m.settingsFieldLine("TLS 证书链 (tls)：", formatTLSSummary(c.TLS), formatTLSSummary(rt.TLS), hasRt, true, !tlsEqual),
		m.settingsFieldLine("授权用户/ACL (clients)：", fmt.Sprintf("%d 个用户", len(c.AuthorizedClients)), fmt.Sprintf("%d 个用户", len(rt.AuthorizedClients)), hasRt, true, !clientsEqual),
		"",
		m.th.hintKey.Render("【带宽限速策略】"),
		m.settingsFieldLine("默认单用户带宽：", formatBandwidthSummary(c.Bandwidth), formatBandwidthSummary(rt.Bandwidth), hasRt, false, c.Bandwidth != rt.Bandwidth),
		m.settingsFieldLine("全局总带宽上限：", formatGlobalBandwidthSummary(c.GlobalBandwidth), formatGlobalBandwidthSummary(rt.GlobalBandwidth), hasRt, true, !reflect.DeepEqual(c.GlobalBandwidth, rt.GlobalBandwidth)),
		"",
		m.th.hintKey.Render("【容量、并发与频率限制 (resources)】"),
		m.settingsFieldLine("单文件/用户存储配额：",
			fmt.Sprintf("%s / %s", progress.FormatBytes(c.Resources.MaxFileBytes), progress.FormatBytes(c.Resources.UserStorageBytes)),
			fmt.Sprintf("%s / %s", progress.FormatBytes(rt.Resources.MaxFileBytes), progress.FormatBytes(rt.Resources.UserStorageBytes)),
			hasRt, false, c.Resources.MaxFileBytes != rt.Resources.MaxFileBytes || c.Resources.UserStorageBytes != rt.Resources.UserStorageBytes),
		m.settingsFieldLine("全局连接/并发握手上限：",
			fmt.Sprintf("%d / %d", c.Resources.MaxConnections, c.Resources.MaxHandshakes),
			fmt.Sprintf("%d / %d", rt.Resources.MaxConnections, rt.Resources.MaxHandshakes),
			hasRt, false, c.Resources.MaxConnections != rt.Resources.MaxConnections || c.Resources.MaxHandshakes != rt.Resources.MaxHandshakes),
		m.settingsFieldLine("每IP未认证/每用户连接：",
			fmt.Sprintf("%d / %d", c.Resources.MaxUnauthenticatedPerIP, c.Resources.MaxConnectionsPerUser),
			fmt.Sprintf("%d / %d", rt.Resources.MaxUnauthenticatedPerIP, rt.Resources.MaxConnectionsPerUser),
			hasRt, false, c.Resources.MaxUnauthenticatedPerIP != rt.Resources.MaxUnauthenticatedPerIP || c.Resources.MaxConnectionsPerUser != rt.Resources.MaxConnectionsPerUser),
		m.settingsFieldLine("每用户最大流/待处理：",
			fmt.Sprintf("%d / %d", c.Resources.MaxStreamsPerUser, c.Resources.MaxPendingTransfersPerUser),
			fmt.Sprintf("%d / %d", rt.Resources.MaxStreamsPerUser, rt.Resources.MaxPendingTransfersPerUser),
			hasRt, false, c.Resources.MaxStreamsPerUser != rt.Resources.MaxStreamsPerUser || c.Resources.MaxPendingTransfersPerUser != rt.Resources.MaxPendingTransfersPerUser),
		m.settingsFieldLine("每用户最大文件数：",
			fmt.Sprintf("%d 个", c.Resources.MaxFilesPerUser), fmt.Sprintf("%d 个", rt.Resources.MaxFilesPerUser),
			hasRt, false, c.Resources.MaxFilesPerUser != rt.Resources.MaxFilesPerUser),
		m.settingsFieldLine("新建任务/检查点频率：",
			fmt.Sprintf("%d/分 · %d/秒", c.Resources.MaxNewTasksPerMinute, c.Resources.MaxUploadCheckpointsPerSecond),
			fmt.Sprintf("%d/分 · %d/秒", rt.Resources.MaxNewTasksPerMinute, rt.Resources.MaxUploadCheckpointsPerSecond),
			hasRt, false, c.Resources.MaxNewTasksPerMinute != rt.Resources.MaxNewTasksPerMinute || c.Resources.MaxUploadCheckpointsPerSecond != rt.Resources.MaxUploadCheckpointsPerSecond),
		m.settingsFieldLine("断点检查点保留时长：",
			fmt.Sprintf("%s (%d秒)", time.Duration(c.Resources.TransferRetentionSeconds)*time.Second, c.Resources.TransferRetentionSeconds),
			fmt.Sprintf("%s (%d秒)", time.Duration(rt.Resources.TransferRetentionSeconds)*time.Second, rt.Resources.TransferRetentionSeconds),
			hasRt, false, c.Resources.TransferRetentionSeconds != rt.Resources.TransferRetentionSeconds),
		"",
		m.th.hintKey.Render("【协议流控与超时 (limits / timeouts)】"),
		m.settingsFieldLine("流控参数 (limits)：",
			fmt.Sprintf("流=%d · 分块=%s · 窗口=%s", c.Limits.MaxStreams, progress.FormatBytes(uint64(c.Limits.ChunkSize)), progress.FormatBytes(uint64(c.Limits.WindowSize))),
			fmt.Sprintf("流=%d · 分块=%s · 窗口=%s", rt.Limits.MaxStreams, progress.FormatBytes(uint64(rt.Limits.ChunkSize)), progress.FormatBytes(uint64(rt.Limits.WindowSize))),
			hasRt, false, c.Limits != rt.Limits),
		m.settingsFieldLine("超时秒数 (timeouts)：",
			fmt.Sprintf("握手=%d · HELLO=%d · 读=%d · 写=%d · 空闲=%ds", c.Timeouts.HandshakeSeconds, c.Timeouts.HelloSeconds, c.Timeouts.FrameReadSeconds, c.Timeouts.WriteSeconds, c.Timeouts.IdleSeconds),
			fmt.Sprintf("握手=%d · HELLO=%d · 读=%d · 写=%d · 空闲=%ds", rt.Timeouts.HandshakeSeconds, rt.Timeouts.HelloSeconds, rt.Timeouts.FrameReadSeconds, rt.Timeouts.WriteSeconds, rt.Timeouts.IdleSeconds),
			hasRt, false, c.Timeouts != rt.Timeouts),
	}
}

func (m *serverModel) renderSettingsPage(bodyH int) string {
	innerW := max(20, m.width-4)
	visRows := max(1, bodyH-3)
	var lines []string
	if m.cfgErr != nil {
		lines = append(lines, m.th.badgeErr.Render("加载配置失败："+m.cfgErr.Error()))
		lines = append(lines, m.th.badgeWarn.Render("可返回 [1 概览] 按 [I] 启动初始化向导"))
	} else {
		all := m.settingsLines()
		start := scrollOffset(m.cursors[4], len(all), visRows)
		end := min(len(all), start+visRows)
		for i := start; i < end; i++ {
			ln := all[i]
			if i == m.cursors[4] {
				lines = append(lines, m.th.rowCursor.Render(truncCell("▸ "+ln, innerW)))
			} else {
				lines = append(lines, truncCell(ln, innerW))
			}
		}
	}
	return renderPanelBox(m.th, m.th.hintKey.Render("服务端安全参数与全部配置项（↑↓/滚轮浏览 · [m]基础 [a]高级 [L]日志）"), lines, true, m.width, bodyH)
}

func (m *serverModel) renderLogsPage(bodyH int) string {
	innerW := max(20, m.width-4)
	var filterChips []string
	if m.logMinLevel != "" {
		filterChips = append(filterChips, "等级>="+m.logMinLevel)
	}
	if m.logQuery.User != "" {
		filterChips = append(filterChips, "用户:"+m.logQuery.User)
	}
	if m.logQuery.File != "" {
		filterChips = append(filterChips, "文件:"+m.logQuery.File)
	}
	if m.logQuery.Text != "" {
		filterChips = append(filterChips, "文本:"+m.logQuery.Text)
	}
	if m.logSinceIdx != 0 {
		filterChips = append(filterChips, "时段:"+logSinceLabel(m.logSinceIdx))
	}

	bannerH := 0
	if len(filterChips) > 0 {
		bannerH = 1
	}
	visRows := max(1, bodyH-3-bannerH)

	status := m.th.badgeOk.Render("[实时跟随]")
	if m.logPaused {
		status = m.th.badgeWarn.Render("[已暂停]")
	}
	if m.logLost {
		status += " " + m.th.badgeWarn.Render("(部分旧日志已滚动覆盖)")
	}
	title := m.th.hintKey.Render("结构化运行日志") + "  " + status

	var lines []string
	if len(filterChips) > 0 {
		bar := m.th.badgeID.Render(truncCell("筛选: "+strings.Join(filterChips, " · ")+"  ([C] 清空)", innerW))
		lines = append(lines, bar)
	}
	switch {
	case m.logErr != "":
		lines = append(lines, m.th.badgeErr.Render("日志拉取错误："+m.logErr))
	case m.logFallback != "":
		raw := m.filteredLogFallbackLines()
		if len(raw) == 0 {
			lines = append(lines, m.th.badgeMuted.Render("（当前筛选条件下无匹配文件日志）"))
		} else {
			start := scrollOffset(m.cursors[5], len(raw), visRows)
			for i := start; i < min(len(raw), start+visRows); i++ {
				lines = append(lines, truncCell(sanitizeInline(raw[i]), innerW))
			}
		}
	case len(m.logEvents) == 0:
		lines = append(lines, m.th.badgeMuted.Render("（暂无匹配日志事件）"))
	default:
		start := scrollOffset(m.cursors[5], len(m.logEvents), visRows)
		for i := start; i < min(len(m.logEvents), start+visRows); i++ {
			e := m.logEvents[i]
			mark := "  "
			if i == m.cursors[5] {
				mark = "▶ "
			}
			row := fmt.Sprintf("%s#%d %s [%s] %s %s", mark, e.Sequence, e.Time.Local().Format("15:04:05"), e.Level, sanitizeInline(e.Message), sanitizeInline(e.Detail))
			if i == m.cursors[5] {
				lines = append(lines, m.th.rowCursor.Render(truncCell(row, innerW)))
			} else {
				lines = append(lines, truncCell(row, innerW))
			}
		}
	}
	return renderPanelBox(m.th, title, lines, true, m.width, bodyH)
}

func (m *serverModel) modalTitle() string {
	switch m.modal {
	case srvModalNewUser:
		return "新建授权用户与证书"
	case srvModalCSR:
		return "签发客户端 CSR 请求"
	case srvModalEditPolicy:
		return "编辑用户权限与带宽策略"
	case srvModalExportUser:
		return "导出客户端安全分发包"
	case srvModalRotateServer:
		return "轮换服务端证书"
	case srvModalEditSettings:
		return "修改服务端基础、带宽与存储设置"
	case srvModalEditAdvancedSettings:
		return "修改高级协议流控、超时与资源限额"
	case srvModalEditLogLimit:
		return "修改服务日志单文件大小上限"
	case srvModalDeployWizard:
		if m.deploy != nil {
			switch m.deploy.step {
			case 1:
				return "部署向导（第 1 步 / 4）：场景与网络地址"
			case 2:
				return "部署向导（第 2 步 / 4）：存储目录配置"
			case 3:
				return "部署向导（第 3 步 / 4）：初始用户与限额设置"
			case 35:
				return "部署向导（第 3 步附加）：高级协议、总带宽与资源限制"
			case 4:
				return "部署向导（第 4 步 / 4）：确认初始化摘要"
			case 5:
				return "部署完成与用户分发包导出"
			}
		}
		return "服务端首次部署与证书初始化向导"
	}
	return "管理操作"
}

func (m *serverModel) infoModalData() (title string, lines []string, footer string) {
	switch m.modal {
	case srvModalHelp:
		return "服务端管理快捷键", []string{
			m.th.hintKey.Render("全局快捷键"),
			"  1–6 / Tab      在 概览 / 用户 / 传输 / 证书 / 设置 / 日志 页面间切换",
			"  s / x / R      后台启动 sftd / 停止 sftd / 热加载证书与 ACL 配置",
			"  r / / / F6     手动刷新 / 过滤当前列表 / 切换深浅色主题",
			"  q / F10        退出管理界面（后台已启动的 sftd 服务继续保持运行）",
			"",
			m.th.hintKey.Render("页面专属动作"),
			"  概览页：  [I] 首次部署向导（缺失配置时自动弹出）  [Enter] 连接详情",
			"  用户页：  [n] 新建  [t] 启用/吊销  [p] 编辑权限与带宽  [w] 续期  [e] 导出包  [g] 相关日志",
			"  传输页：  [Enter] 任务详情  [g] 相关日志  [/] 筛选",
			"  证书页：  [K] 续期CA(365d)  [S] 轮换服务端证书  [C] 签发客户端CSR",
			"  设置页：  [m] 基础/带宽/存储  [a] 高级协议/超时/资源  [L] 日志上限  [A] 热加载",
			"  日志页：  [p] 暂停/跟随  [l] 等级  [U] 用户  [F] 文件  [T] 时间窗  [C] 清空筛选  [Enter] 详情",
		}, "↑↓/滚轮 滚动 · Esc / Enter / ? 关闭"

	case srvModalDetail:
		return m.detailTitle, m.detailLines, "↑↓/滚轮 滚动 · Enter / Esc 关闭"
	}
	return "", nil, ""
}

func (m *serverModel) renderModal(base string) string {
	switch m.modal {
	case srvModalHelp, srvModalDetail:
		title, lines, footer := m.infoModalData()
		view, _ := renderModalScrollBox(m.width, m.height, m.th, title, lines, footer, m.infoScroll)
		return view

	case srvModalStop:
		lines := []string{
			"请选择停止本机运行中 sftd 守护进程的方式：",
			"",
			"  " + m.th.hintKey.Render("[g / Enter]") + "  优雅停服（拒接新连接并等待当前活动传输安全落盘完成后退出）",
			"  " + m.th.hintKey.Render("[f]") + "          立即强制停服（断开所有连接）",
		}
		view, _ := renderModalScrollBox(m.width, m.height, m.th, "停止 sftd 服务", lines, "g 优雅停服 · f 强制停服 · Esc 取消", 0)
		return view
	}

	_, innerW := modalBoxDimensions(m.width)
	lines, footer := m.renderFormLines(innerW)
	return renderModalBox(m.width, m.height, m.th, m.modalTitle(), lines, footer)
}
