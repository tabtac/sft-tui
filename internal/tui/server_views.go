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
	rightInfo := "等待连接本机服务"
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
		rightInfo = fmt.Sprintf("在线:%d  连接:%d  活动:%d  ↑%s ↓%s  运行 %s",
			m.runtime.OnlineUsers, m.runtime.Connections, m.runtime.ActiveTasks,
			progress.FormatRate(m.runtime.ReceiveRate), progress.FormatRate(m.runtime.SendRate), uptime)
	} else if strings.Contains(m.runtimeState, "异常") {
		stBadge = m.th.badgeErr.Render("✕ " + truncCell(m.runtimeState, 24))
	}

	addrBadge := m.th.badgeID.Render(" " + listenStr + " ")
	left := lipgloss.JoinHorizontal(lipgloss.Center, brand, " ", stBadge, " ", addrBadge)
	stats := m.th.badgeMuted.Render(rightInfo)
	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(stats)-2)
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
	if m.editingQuery {
		row += "   " + m.th.hintKey.Render("筛选: ") + m.queryInput.View()
	} else if m.statusToast != "" {
		row += "   " + m.th.toast.Render("· "+truncCell(m.statusToast, max(10, m.width/3)))
	}
	return lipgloss.NewStyle().Padding(0, 1).Width(m.width).Render(truncCell(row, m.width-2))
}

func (m *serverModel) renderFooter() string {
	var hints string
	switch {
	case m.editingQuery:
		hints = "筛选过滤：输入关键字 · Enter 应用 · Esc 清空"
	case m.page == 0:
		hints = "概览：[s] 启动服务  [x] 停止服务  [R] 热加载  [I] 快速初始化  [1-6/Tab] 切页  [F6] 主题  [?] 帮助  [q] 退出面板"
	case m.page == 1:
		hints = "用户：[/] 搜索  [n] 新建  [t] 启用/吊销  [p] 改权限与带宽  [w] 续期证书  [C] 签发CSR  [e] 导出包  [Enter] 详情"
	case m.page == 2:
		hints = "传输：[/] 筛选用户或文件名  [↑↓] 选择  [Enter] 查看详情  [r] 刷新"
	case m.page == 3:
		hints = "证书：[K] 续期CA  [S] 轮换服务端证书  [C] 签发CSR  [Enter] 查看指纹细节"
	case m.page == 4:
		hints = "设置：[m] 编辑配置参数  [R] 热加载变更  [1-6/Tab] 切页  [q] 退出"
	default:
		hints = "日志：[p] 暂停/跟随实时日志  [l] 切换最低等级(" + logLevelLabel(m.logMinLevel) + ")  [/] 关键字过滤  [Enter] 事件详情"
	}
	return m.th.footerBar.Width(m.width).Render(truncCell(hints, m.width-2))
}

func logLevelLabel(l string) string {
	if l == "" {
		return "全部"
	}
	return l
}

func (m *serverModel) renderOverviewPage(bodyH int) string {
	topH := min(max(6, bodyH/2), 11)
	botH := max(5, bodyH-topH)
	leftW := m.width / 2
	rightW := m.width - leftW

	// 左侧：部署配置卡片
	var cfgLines []string
	if !m.loaded {
		cfgLines = append(cfgLines, m.th.badgeMuted.Render("正在读取服务端配置…"))
	} else if m.cfgErr != nil {
		cfgLines = append(cfgLines, m.th.badgeErr.Render("配置读取失败："+m.cfgErr.Error()))
		cfgLines = append(cfgLines, m.th.badgeWarn.Render("可按 [I] 键直接按默认安全模板初始化整套证书与配置"))
	} else {
		warnCount := 0
		for _, c := range m.certs {
			if c.State != "有效" {
				warnCount++
			}
		}
		cfgLines = append(cfgLines,
			"配置文件： "+truncCell(m.opts.ConfigPath, leftW-16),
			"存储目录： "+truncCell(m.cfg.StorageRoot, leftW-16),
			fmt.Sprintf("授权用户： %d 个   ·   默认限额：每用户 ↑%s ↓%s",
				len(m.cfg.AuthorizedClients),
				progress.FormatRate(float64(m.cfg.Bandwidth.UploadBytesPerSecond)),
				progress.FormatRate(float64(m.cfg.Bandwidth.DownloadBytesPerSecond))),
		)
		if m.cfg.GlobalBandwidth != nil {
			cfgLines = append(cfgLines, fmt.Sprintf("全局总上限：↑%s  ↓%s",
				progress.FormatRate(float64(m.cfg.GlobalBandwidth.UploadBytesPerSecond)),
				progress.FormatRate(float64(m.cfg.GlobalBandwidth.DownloadBytesPerSecond))))
		}
		if warnCount > 0 {
			cfgLines = append(cfgLines, m.th.badgeWarn.Render(fmt.Sprintf("⚠ 注意：%d 张证书待处理，请按 [4] 查看详情", warnCount)))
		}
	}
	leftCard := renderPanelBox(m.th, m.th.hintKey.Render("部署环境"), cfgLines, true, leftW, topH)

	// 右侧：流量与存储实时指标卡片
	var statLines []string
	if m.runtime == nil {
		statLines = append(statLines,
			m.th.badgeMuted.Render(m.runtimeState),
			"",
			"按 [s] 后台启动 sftd 守护进程后可实时监控流量趋势与连接。",
		)
	} else {
		r := m.runtime
		statLines = append(statLines,
			fmt.Sprintf("已发布对象：%d 个 (%s)", r.PublishedFiles, progress.FormatBytes(r.PublishedBytes)),
			fmt.Sprintf("实时吞吐：入站(上传) %s   出站(下载) %s",
				m.th.badgeOk.Render(progress.FormatRate(r.ReceiveRate)),
				m.th.hintKey.Render(progress.FormatRate(r.SendRate))),
			fmt.Sprintf("累计流量：入 %s  出 %s   任务：成功 %d / 失败 %d",
				progress.FormatBytes(r.Received), progress.FormatBytes(r.Sent), r.Completed, r.Failed),
			"上传趋势："+m.th.badgeOk.Render(sparklineString(m.trend, 0)),
			"下载趋势："+m.th.hintKey.Render(sparklineString(m.trend, 1)),
		)
	}
	rightCard := renderPanelBox(m.th, m.th.hintKey.Render("实时运行监控"), statLines, true, rightW, topH)
	topRow := lipgloss.JoinHorizontal(lipgloss.Top, leftCard, rightCard)

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
			line := fmt.Sprintf("%s#%-4d  %-16s  %-24s  在线 %s",
				mark, p.ID, padRightCell(p.User, 16), padRightCell(p.Remote, 24),
				time.Since(p.Since).Round(time.Second))
			if i == m.cursors[0] {
				peerLines = append(peerLines, m.th.rowCursor.Render(padRightCell(line, innerW)))
			} else {
				peerLines = append(peerLines, line)
			}
		}
	}
	botCard := renderPanelBox(m.th, m.th.hintKey.Render("活跃 mTLS 会话"), peerLines, true, m.width, botH)
	return lipgloss.JoinVertical(lipgloss.Left, topRow, botCard)
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
			perms := strings.Join(u.Permissions, ",")
			if perms == "" {
				perms = "upload,download"
			}
			bwStr := "默认策略"
			if u.Bandwidth != nil {
				bwStr = fmt.Sprintf("↑%s/↓%s", progress.FormatRate(float64(u.Bandwidth.UploadBytesPerSecond)), progress.FormatRate(float64(u.Bandwidth.DownloadBytesPerSecond)))
			}
			certSt := certStateByUser[u.UserID]
			if certSt == "" {
				certSt = "未知"
			}
			row := fmt.Sprintf("%s%-14s  %s  %-16s  %-22s  证书:%s",
				mark, padRightCell(u.UserID, 14), st, padRightCell(perms, 16), padRightCell(bwStr, 22), certSt)
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
		for i := start; i < min(len(tasks), start+visRows); i++ {
			t := tasks[i]
			mark := "  "
			if i == m.cursors[2] {
				mark = "▶ "
			}
			row := fmt.Sprintf("%s%-12s  %-8s  %-22s  %-10s  %s/%s  %s",
				mark, padRightCell(t.User, 12), padRightCell(t.Direction, 8), padRightCell(t.File, 22),
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
			fp := c.Fingerprint
			if len(fp) > 16 {
				fp = fp[:16] + "…"
			}
			row := fmt.Sprintf("%s%-12s  %-14s  %s  到期:%-16s  FP:%s",
				mark, padRightCell(c.Name, 12), padRightCell(c.Role, 14), st, padRightCell(c.Expiry, 16), fp)
			if i == m.cursors[3] {
				lines = append(lines, m.th.rowCursor.Render(padRightCell(row, innerW)))
			} else {
				lines = append(lines, row)
			}
		}
	}
	return renderPanelBox(m.th, m.th.hintKey.Render("证书信任链与有效期体检"), lines, true, m.width, bodyH)
}

func (m *serverModel) renderSettingsPage(bodyH int) string {
	var lines []string
	if m.cfgErr != nil {
		lines = append(lines, m.th.badgeErr.Render("加载配置失败："+m.cfgErr.Error()))
	} else {
		diffTag := m.th.badgeMuted.Render("（服务离线，修改配置启动后生效）")
		if m.runtime != nil {
			if config.RestartRequired(m.runtime.Config, m.cfg) {
				diffTag = m.th.badgeWarn.Render("⚠ 磁盘配置与运行中实例存在非热更差异（需重启生效）")
			} else {
				diffTag = m.th.badgeOk.Render("✔ 磁盘配置与运行实例基本设置一致")
			}
		}
		lines = append(lines,
			diffTag,
			"",
			fmt.Sprintf("监听地址：         %s", m.cfg.Listen),
			fmt.Sprintf("存储根目录：       %s", m.cfg.StorageRoot),
			fmt.Sprintf("并发限制：         最大连接=%d · 并发握手=%d · 单IP未认证=%d · 单用户连接=%d",
				m.cfg.Resources.MaxConnections, m.cfg.Resources.MaxHandshakes,
				m.cfg.Resources.MaxUnauthenticatedPerIP, m.cfg.Resources.MaxConnectionsPerUser),
			fmt.Sprintf("流控参数：         每用户最大流=%d · 单连接最大流=%d · 分块=%s · 窗口=%s",
				m.cfg.Resources.MaxStreamsPerUser, m.cfg.Limits.MaxStreams,
				progress.FormatBytes(uint64(m.cfg.Limits.ChunkSize)), progress.FormatBytes(uint64(m.cfg.Limits.WindowSize))),
			fmt.Sprintf("默认单用户带宽：   ↑ %s  ·  ↓ %s  ·  Burst %s",
				progress.FormatRate(float64(m.cfg.Bandwidth.UploadBytesPerSecond)),
				progress.FormatRate(float64(m.cfg.Bandwidth.DownloadBytesPerSecond)),
				progress.FormatBytes(m.cfg.Bandwidth.BurstBytes)),
			fmt.Sprintf("日志文件上限：     %s", progress.FormatBytes(m.cfg.Logging.MaxFileBytes)),
		)
	}
	return renderPanelBox(m.th, m.th.hintKey.Render("服务端安全参数与策略配置"), lines, true, m.width, bodyH)
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
			"  概览页：  [I] 首次一键初始化证书与配置  [Enter] 连接详情",
			"  用户页：  [n] 新建  [t] 启用/吊销  [p] 编辑权限与带宽  [w] 续期  [e] 导出包",
			"  证书页：  [K] 续期CA(365d)  [S] 轮换服务端证书  [C] 签发客户端CSR",
			"  设置页：  [m] 编辑基础网络与默认带宽设置",
			"  日志页：  [p] 暂停/跟随  [l] 切换日志等级过滤  [Enter] 查看完整事件",
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

	// 动态渲染表单类弹窗
	var title string
	switch m.modal {
	case srvModalNewUser:
		title = "新建授权用户与证书"
	case srvModalCSR:
		title = "签发客户端 CSR 请求"
	case srvModalEditPolicy:
		title = "编辑用户权限与带宽策略"
	case srvModalExportUser:
		title = "导出客户端安全分发包"
	case srvModalRotateServer:
		title = "轮换服务端证书"
	case srvModalEditSettings:
		title = "修改服务端基础设置"
	}

	var lines []string
	for i, lbl := range m.formLabels {
		prefix := "  "
		if i == m.formFocus {
			prefix = "▶ "
		}
		lines = append(lines, fmt.Sprintf("%s%s", prefix, m.th.hintKey.Render(lbl)))
		lines = append(lines, "  "+m.formInputs[i].View())
	}
	for i, lbl := range m.formToggleLbl {
		idx := len(m.formInputs) + i
		prefix := "  "
		st := m.th.rowNormal
		if idx == m.formFocus {
			prefix = "▶ "
			st = m.th.rowCursor
		}
		box := "[ ]"
		if m.formToggles[i] {
			box = "[✓]"
		}
		lines = append(lines, st.Render(fmt.Sprintf("%s%s %s", prefix, box, lbl)))
	}
	return renderModalBox(m.width, m.height, m.th, title, lines, "Tab/↑↓ 切项 · Space 切换开关 · Enter 确认提交 · Esc 取消")
}
