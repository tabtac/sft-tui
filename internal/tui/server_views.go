package tui

import (
	"fmt"
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
		row += "   " + m.th.hintKey.Render("筛选: ") + m.queryInput.View()
	} else if m.statusToast != "" && avail >= 6 {
		row += "   " + m.th.toast.Render("· "+truncCell(m.statusToast, avail-2))
	}
	return lipgloss.NewStyle().Padding(0, 1).Width(m.width).Render(truncCell(row, m.width-2))
}

func (m *serverModel) renderFooter() string {
	var hints string
	switch {
	case m.editingQuery:
		hints = "筛选过滤：输入关键字 · Enter 应用 · Esc 清空"
	case m.page == 0:
		hints = "概览：[s] 启动服务  [x] 停止服务  [R] 热加载  [I] 部署向导  [1-6/Tab] 切页  [F6] 主题  [?] 帮助  [q] 退出"
	case m.page == 1:
		hints = "用户：[/] 搜索  [n] 新建  [t] 启用/吊销  [p] 改权限与带宽  [w] 续期证书  [C] CSR  [e] 导出  [Enter] 详情"
	case m.page == 2:
		hints = "传输：[/] 筛选用户或文件名  [↑↓] 选择  [Enter] 查看详情  [r] 刷新"
	case m.page == 3:
		hints = "证书：[K] 续期CA  [S] 轮换服务端证书  [C] 签发CSR  [Enter] 查看指纹详情"
	case m.page == 4:
		hints = "设置：[m] 编辑基础/带宽/存储  [a] 高级协议与资源限额  [L] 日志大小上限  [A] 应用热加载  [1-6/Tab] 切页"
	default:
		hints = "日志：[p] 暂停/跟随实时日志  [l] 最低等级(" + logLevelLabel(m.logMinLevel) + ")  [L] 日志上限  [/] 关键字过滤  [Enter] 详情"
	}
	return m.th.footerBar.Width(m.width).Render(hints)
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
				peerLines = append(peerLines, line)
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
				lines = append(lines, row)
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
				lines = append(lines, row)
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
				lines = append(lines, row)
			}
		}
	}
	return renderPanelBox(m.th, m.th.hintKey.Render("证书信任链与有效期体检"), lines, true, m.width, bodyH)
}

func (m *serverModel) settingsLines() []string {
	c := m.cfg
	diffTag := m.th.badgeMuted.Render("（服务离线，修改配置启动后生效）")
	if m.runtime != nil {
		if config.RestartRequired(m.runtime.Config, c) {
			diffTag = m.th.badgeWarn.Render("⚠ 磁盘配置与运行实例存在非热更差异（需重启生效）")
		} else {
			diffTag = m.th.badgeOk.Render("✔ 磁盘配置与运行实例基本设置一致")
		}
	}
	gbLine := "未启用（仅受单用户策略与全局物理接口限制）"
	if c.GlobalBandwidth != nil {
		gbLine = fmt.Sprintf("↑ %s  ·  ↓ %s  ·  突发 %s",
			progress.FormatRate(float64(c.GlobalBandwidth.UploadBytesPerSecond)),
			progress.FormatRate(float64(c.GlobalBandwidth.DownloadBytesPerSecond)),
			progress.FormatBytes(c.GlobalBandwidth.BurstBytes))
	}
	return []string{
		diffTag,
		"",
		m.th.hintKey.Render("【基础网络、存储与日志】  按 [m] 编辑基础/带宽/配额 · [a] 编辑全部高级参数 · [L] 调整日志上限 · [A] 热重载"),
		fmt.Sprintf("  监听地址 (listen)：          %s", c.Listen),
		fmt.Sprintf("  存储目录 (storage_root)：    %s", c.StorageRoot),
		fmt.Sprintf("  日志上限 (logging)：         %s（达上限清空当前文件重写）", progress.FormatBytes(c.Logging.MaxFileBytes)),
		"",
		m.th.hintKey.Render("【带宽限速策略】"),
		fmt.Sprintf("  默认单用户带宽：             ↑ %s  ·  ↓ %s  ·  突发 %s",
			progress.FormatRate(float64(c.Bandwidth.UploadBytesPerSecond)),
			progress.FormatRate(float64(c.Bandwidth.DownloadBytesPerSecond)),
			progress.FormatBytes(c.Bandwidth.BurstBytes)),
		fmt.Sprintf("  全局总带宽上限：             %s", gbLine),
		"",
		m.th.hintKey.Render("【容量、并发与频率限制 (resources)】"),
		fmt.Sprintf("  单文件 / 每用户存储配额：    %s  /  %s",
			progress.FormatBytes(c.Resources.MaxFileBytes), progress.FormatBytes(c.Resources.UserStorageBytes)),
		fmt.Sprintf("  全局连接 / 并发握手上限：    %d  /  %d",
			c.Resources.MaxConnections, c.Resources.MaxHandshakes),
		fmt.Sprintf("  每 IP 未认证 / 每用户连接：  %d  /  %d",
			c.Resources.MaxUnauthenticatedPerIP, c.Resources.MaxConnectionsPerUser),
		fmt.Sprintf("  每用户最大流 / 待处理任务：  %d  /  %d",
			c.Resources.MaxStreamsPerUser, c.Resources.MaxPendingTransfersPerUser),
		fmt.Sprintf("  每用户最大文件数：           %d 个", c.Resources.MaxFilesPerUser),
		fmt.Sprintf("  新建任务速率 / 检查点频率：  %d 次/分钟  ·  %d 次/秒",
			c.Resources.MaxNewTasksPerMinute, c.Resources.MaxUploadCheckpointsPerSecond),
		fmt.Sprintf("  断点检查点保留时长：         %s (%d 秒)",
			time.Duration(c.Resources.TransferRetentionSeconds)*time.Second, c.Resources.TransferRetentionSeconds),
		"",
		m.th.hintKey.Render("【协议流控与超时 (limits / timeouts)】"),
		fmt.Sprintf("  流控参数：                   每连接流=%d · 分块=%s · 窗口=%s",
			c.Limits.MaxStreams, progress.FormatBytes(uint64(c.Limits.ChunkSize)), progress.FormatBytes(uint64(c.Limits.WindowSize))),
		fmt.Sprintf("  超时秒数：                   TLS握手=%ds · HELLO=%ds · 读帧=%ds · 写=%ds · 空闲=%ds",
			c.Timeouts.HandshakeSeconds, c.Timeouts.HelloSeconds, c.Timeouts.FrameReadSeconds,
			c.Timeouts.WriteSeconds, c.Timeouts.IdleSeconds),
	}
}

func (m *serverModel) renderSettingsPage(bodyH int) string {
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
				lines = append(lines, m.th.rowCursor.Render("▸ "+ln))
			} else {
				lines = append(lines, ln)
			}
		}
	}
	return renderPanelBox(m.th, m.th.hintKey.Render("服务端安全参数与全部配置项（↑↓/滚轮浏览 · [m]基础 [a]高级 [L]日志）"), lines, true, m.width, bodyH)
}

func (m *serverModel) renderLogsPage(bodyH int) string {
	innerW := max(20, m.width-4)
	visRows := max(1, bodyH-3)

	status := m.th.badgeOk.Render("[实时跟随]")
	if m.logPaused {
		status = m.th.badgeWarn.Render("[已暂停]")
	}
	if m.logLost {
		status += " " + m.th.badgeWarn.Render("(部分旧日志已滚动覆盖)")
	}
	title := m.th.hintKey.Render("结构化运行日志") + "  " + status

	var lines []string
	switch {
	case m.logErr != "":
		lines = append(lines, m.th.badgeErr.Render("日志拉取错误："+m.logErr))
	case m.logFallback != "":
		raw := strings.Split(strings.TrimRight(m.logFallback, "\n"), "\n")
		start := scrollOffset(m.cursors[5], len(raw), visRows)
		for i := start; i < min(len(raw), start+visRows); i++ {
			lines = append(lines, truncCell(raw[i], innerW))
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
			row := fmt.Sprintf("%s#%d %s [%s] %s %s", mark, e.Sequence, e.Time.Local().Format("15:04:05"), e.Level, e.Message, e.Detail)
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

func (m *serverModel) renderModal(base string) string {
	switch m.modal {
	case srvModalHelp:
		lines := []string{
			m.th.hintKey.Render("全局快捷键"),
			"  1–6 / Tab      在 概览 / 用户 / 传输 / 证书 / 设置 / 日志 页面间切换",
			"  s / x / R      后台启动 sftd / 停止 sftd / 热加载证书与 ACL 配置",
			"  r / / / F6     手动刷新 / 过滤当前列表 / 切换深浅色主题",
			"  q / F10        退出管理界面（后台已启动的 sftd 服务继续保持运行）",
			"",
			m.th.hintKey.Render("页面专属动作"),
			"  概览页：  [I] 首次部署向导（缺失配置时自动弹出）  [Enter] 连接详情",
			"  用户页：  [n] 新建  [t] 启用/吊销  [p] 编辑权限与带宽  [w] 续期  [e] 导出包",
			"  证书页：  [K] 续期CA(365d)  [S] 轮换服务端证书  [C] 签发客户端CSR",
			"  设置页：  [m] 基础/带宽/存储  [a] 高级协议/超时/资源  [L] 日志上限  [A] 热加载",
			"  日志页：  [p] 暂停/跟随  [l] 切换日志等级  [L] 日志上限  [Enter] 事件详情",
		}
		return renderModalBox(m.width, m.height, m.th, "服务端管理快捷键", lines, "按 Esc / Enter / ? 关闭")

	case srvModalDetail:
		return renderModalBox(m.width, m.height, m.th, m.detailTitle, m.detailLines, "按 Enter 或 Esc 关闭")

	case srvModalStop:
		lines := []string{
			"请选择停止本机运行中 sftd 守护进程的方式：",
			"",
			"  " + m.th.hintKey.Render("[g / Enter]") + "  优雅停服（拒接新连接并等待当前活动传输安全落盘完成后退出）",
			"  " + m.th.hintKey.Render("[f]") + "          立即强制停服（断开所有连接）",
		}
		return renderModalBox(m.width, m.height, m.th, "停止 sftd 服务", lines, "g 优雅停服 · f 强制停服 · Esc 取消")
	}

	boxW := min(max(44, m.width-4), 86)
	innerW := boxW - 6
	lines, footer := m.renderFormLines(innerW)
	return renderModalBox(m.width, m.height, m.th, m.modalTitle(), lines, footer)
}
