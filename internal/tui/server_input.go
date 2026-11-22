package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"sft-tui/internal/config"
	"sft-tui/internal/monitor"
	"sft-tui/internal/progress"
	"sft-tui/internal/security"
)

func (m *serverModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m, tea.Quit
	}

	if m.editingQuery {
		switch k {
		case "esc":
			mode := m.queryMode
			m.editingQuery = false
			m.queryMode = ""
			m.queryInput.Blur()
			m.queryInput.SetValue("")
			switch mode {
			case "log-user":
				m.logQuery.User = ""
				return m, m.resetLogsAndPollCmd()
			case "log-file":
				m.logQuery.File = ""
				return m, m.resetLogsAndPollCmd()
			default:
				m.updatePageFilter("")
				if m.page == 5 {
					return m, m.resetLogsAndPollCmd()
				}
			}
			return m, nil
		case "enter":
			mode := m.queryMode
			val := strings.TrimSpace(m.queryInput.Value())
			m.editingQuery = false
			m.queryMode = ""
			m.queryInput.Blur()
			switch mode {
			case "log-user":
				m.logQuery.User = val
				return m, m.resetLogsAndPollCmd()
			case "log-file":
				m.logQuery.File = val
				return m, m.resetLogsAndPollCmd()
			default:
				if m.page == 5 {
					m.logQuery.Text = val
					return m, m.resetLogsAndPollCmd()
				}
			}
			return m, nil
		}
		var cmd tea.Cmd
		m.queryInput, cmd = m.queryInput.Update(msg)
		if m.queryMode == "" {
			m.updatePageFilter(m.queryInput.Value())
		}
		return m, cmd
	}

	if m.modal != srvModalNone {
		return m.handleModalKey(msg)
	}

	switch k {
	case "esc":
		switch m.page {
		case 1:
			if m.userFilter != "" {
				m.userFilter = ""
				m.clampCursors()
				m.setToast("已清空用户筛选")
			}
		case 2:
			if m.txFilter != "" {
				m.txFilter = ""
				m.clampCursors()
				m.setToast("已清空传输筛选")
			}
		case 5:
			if m.hasLogFilters() {
				m.logQuery = monitor.LogQuery{}
				m.logMinLevel = ""
				m.logSinceIdx = 0
				m.setToast("已清空全部日志筛选条件")
				return m, m.resetLogsAndPollCmd()
			}
		}
		return m, nil
	case "q", "f10":
		if m.rejectIfBusy() {
			return m, nil
		}
		return m, tea.Quit
	case "?":
		m.infoScroll = 0
		m.modal = srvModalHelp
		return m, nil
	case "1", "2", "3", "4", "5", "6":
		idx := int(k[0] - '1')
		return m.switchPage(idx)
	case "tab":
		return m.switchPage((m.page + 1) % 6)
	case "shift+tab":
		return m.switchPage((m.page + 5) % 6)
	case "/":
		if m.page != 1 && m.page != 2 && m.page != 5 {
			return m, nil
		}
		m.editingQuery = true
		m.queryMode = ""
		switch m.page {
		case 1:
			m.queryInput.SetValue(m.userFilter)
		case 2:
			m.queryInput.SetValue(m.txFilter)
		case 5:
			m.queryInput.SetValue(m.logQuery.Text)
		default:
			m.queryInput.SetValue("")
		}
		m.queryInput.Focus()
		m.queryInput.CursorEnd()
		return m, textinput.Blink
	case "r":
		m.setToastSticky("正在刷新配置与运行状态…")
		m.refreshSeq++
		seq := m.refreshSeq
		m.refreshCfgSeq = seq
		m.refreshPollSeq = seq
		m.refreshCfgErr = nil
		m.refreshPollErr = nil
		return m, tea.Batch(m.loadConfigSeqCmd(seq), m.pollRuntimeSeqCmd(seq))
	case "R":
		return m, m.reloadServiceCmd()
	case "s":
		return m, m.startServiceCmd()
	case "x":
		if m.rejectIfBusy() {
			return m, nil
		}
		if m.runtime != nil {
			m.modal = srvModalStop
		} else {
			m.setToast("本机服务当前未运行")
		}
		return m, nil
	case "up", "k":
		if m.page == 5 {
			m.logPaused = true
		}
		if m.page == 4 {
			m.stepSettingsCursor(-1)
			return m, nil
		}
		if m.cursors[m.page] > 0 {
			m.cursors[m.page]--
		}
		return m, nil
	case "down", "j":
		if m.page == 4 {
			m.stepSettingsCursor(1)
			return m, nil
		}
		n := m.currentPageLen()
		if m.cursors[m.page] < n-1 {
			m.cursors[m.page]++
		} else if m.page == 5 && m.logPaused {
			m.logPaused = false
			return m, m.pollRuntimeCmd()
		}
		return m, nil
	case "pgup":
		if m.page == 5 {
			m.logPaused = true
		}
		m.cursors[m.page] = max(0, m.cursors[m.page]-8)
		if m.page == 4 {
			m.snapSettingsCursor(-1)
		}
		return m, nil
	case "pgdown":
		if n := m.currentPageLen(); n > 0 {
			m.cursors[m.page] = min(n-1, m.cursors[m.page]+8)
		}
		if m.page == 4 {
			m.snapSettingsCursor(1)
		}
		return m, nil
	case "home":
		if m.page == 5 {
			m.logPaused = true
		}
		m.cursors[m.page] = 0
		if m.page == 4 {
			m.snapSettingsCursor(1)
		}
		return m, nil
	case "end", "G":
		if n := m.currentPageLen(); n > 0 {
			m.cursors[m.page] = n - 1
		}
		if m.page == 4 {
			m.snapSettingsCursor(-1)
		}
		if m.page == 5 && m.logPaused {
			m.logPaused = false
			return m, m.pollRuntimeCmd()
		}
		return m, nil
	}
	if k == "g" && m.page != 1 && m.page != 2 {
		if m.page == 5 {
			m.logPaused = true
		}
		m.cursors[m.page] = 0
		if m.page == 4 {
			m.snapSettingsCursor(1)
		}
		return m, nil
	}

	// 页面专属操作键
	switch m.page {
	case 0: // 概览
		switch k {
		case "I":
			m.beginDeployWizard()
			return m, textinput.Blink
		case "enter":
			if m.runtime != nil && m.cursors[0] < len(m.runtime.Peers) {
				p := m.runtime.Peers[m.cursors[0]]
				m.detailTitle = "连接详情"
				m.detailLines = []string{
					fmt.Sprintf("连接 ID： %d", p.ID),
					fmt.Sprintf("授权用户：%s", sanitizeInline(p.User)),
					fmt.Sprintf("远端地址：%s", sanitizeInline(p.Remote)),
					fmt.Sprintf("建立时间：%s", p.Since.Local().Format("2006-01-02 15:04:05")),
				}
				m.infoScroll = 0
				m.modal = srvModalDetail
			}
		}
	case 1: // 用户管理
		users := m.filteredUsers()
		switch k {
		case "n":
			if m.cfgErr != nil {
				m.setToast("当前配置不可用，无法新建用户")
				return m, nil
			}
			if m.rejectIfBusy() {
				return m, nil
			}
			m.openNewUserModal()
			return m, textinput.Blink
		case "C":
			if m.cfgErr != nil {
				m.setToast("当前配置不可用，无法签发 CSR")
				return m, nil
			}
			if m.rejectIfBusy() {
				return m, nil
			}
			m.openCSRModal("")
			return m, textinput.Blink
		case "t":
			if m.cfgErr != nil {
				m.setToast("当前配置不可用")
				return m, nil
			}
			if m.rejectIfBusy() {
				return m, nil
			}
			if len(users) > 0 && m.cursors[1] < len(users) {
				u := users[m.cursors[1]]
				title := "确认吊销用户授权"
				lines := []string{
					m.th.badgeWarn.Render("即将吊销用户 " + sanitizeInline(u.UserID) + " 的访问权限。"),
					"",
					"  • 确认后将写入服务端配置，并立即热加载到运行中的 sftd 实例。",
					"  • 该用户将无法建立新传输连接；随时可再次按 [t] 重新启用。",
				}
				if u.Disabled {
					title = "确认重新启用用户"
					lines = []string{
						m.th.badgeOk.Render("即将重新启用用户 " + sanitizeInline(u.UserID) + " 的访问权限。"),
						"",
						"  • 确认后将写入服务端配置，并立即热加载到运行中的 sftd 实例。",
					}
				}
				m.openConfirmModal(title, lines, func() tea.Cmd {
					return m.toggleUserDisabledCmd(u)
				})
				return m, nil
			}
		case "p":
			if m.cfgErr != nil {
				m.setToast("当前配置不可用")
				return m, nil
			}
			if m.rejectIfBusy() {
				return m, nil
			}
			if len(users) > 0 && m.cursors[1] < len(users) {
				m.openEditPolicyModal(users[m.cursors[1]])
				return m, textinput.Blink
			}
		case "w":
			if m.cfgErr != nil {
				m.setToast("当前配置不可用")
				return m, nil
			}
			if m.rejectIfBusy() {
				return m, nil
			}
			if len(users) > 0 && m.cursors[1] < len(users) {
				u := users[m.cursors[1]]
				if u.ExternalKey {
					m.openCSRModalForRenew(u)
					return m, textinput.Blink
				}
				lines := []string{
					m.th.badgeWarn.Render("即将为用户 " + sanitizeInline(u.UserID) + " 签发新的客户端证书并热加载。"),
					"",
					"  • 新证书签发后，旧证书将失效；请按 [e] 重新导出分发包给客户端。",
					"  • 该操作不会改变用户当前的启停与限速策略。",
				}
				m.openConfirmModal("确认续期用户证书", lines, func() tea.Cmd {
					return m.renewUserCertCmd(u)
				})
				return m, nil
			}
		case "e":
			if m.cfgErr != nil {
				m.setToast("当前配置不可用")
				return m, nil
			}
			if m.rejectIfBusy() {
				return m, nil
			}
			if len(users) > 0 && m.cursors[1] < len(users) {
				m.openExportUserModal(users[m.cursors[1]])
				return m, textinput.Blink
			}
		case "g":
			if len(users) > 0 && m.cursors[1] < len(users) {
				u := users[m.cursors[1]]
				return m.jumpToLogs(monitor.LogQuery{User: u.UserID}, "已按用户过滤并跳转至相关日志："+u.UserID)
			}
		case "enter":
			if len(users) > 0 && m.cursors[1] < len(users) {
				m.showUserDetailModal(users[m.cursors[1]])
			}
		}
	case 2: // 实时传输
		tasks := m.filteredTasks()
		switch k {
		case "g":
			if len(tasks) > 0 && m.cursors[2] < len(tasks) {
				t := tasks[m.cursors[2]]
				return m.jumpToLogs(monitor.LogQuery{User: t.User, File: t.File}, fmt.Sprintf("已跳转至 %s / %s 的相关日志", t.User, t.File))
			}
		case "enter":
			if m.cursors[2] < len(tasks) {
				t := tasks[m.cursors[2]]
				m.detailTitle = "服务端传输任务详情"
				m.detailLines = []string{
					fmt.Sprintf("任务 / 连接：%d  (连接 #%d · 流 #%d)", t.ID, t.Connection, t.Stream),
					fmt.Sprintf("用户与方向：%s · %s", sanitizeInline(t.User), serverDirectionText(t.Direction)),
					fmt.Sprintf("文件名：    %s", sanitizeInline(t.File)),
					fmt.Sprintf("进度与速率：%s / %s (%d / %d B) · %s",
						progress.FormatBytes(t.Offset), progress.FormatBytes(t.Size), t.Offset, t.Size, progress.FormatRate(t.Rate)),
					fmt.Sprintf("当前状态：  %s", serverTaskStateText(t.State)),
				}
				if t.Error != "" {
					m.detailLines = append(m.detailLines, "", "错误信息：", "  "+sanitizeInline(t.Error))
				}
				m.infoScroll = 0
				m.modal = srvModalDetail
			}
		}
	case 3: // 证书状态
		switch k {
		case "K":
			if m.cfgErr != nil {
				m.setToast("当前配置不可用")
				return m, nil
			}
			if m.rejectIfBusy() {
				return m, nil
			}
			m.openConfirmModal("确认续期 CA 根证书", []string{
				m.th.badgeWarn.Render("即将沿用现有 CA 私钥将根证书有效期延长 365 天。"),
				"",
				"  • 续期完成后将自动热加载到运行中的服务端。",
				"  • 建议随后将更新后的 ca.pem 同步至各客户端。",
			}, func() tea.Cmd {
				return m.renewCACmd()
			})
			return m, nil
		case "S":
			if m.cfgErr != nil {
				m.setToast("当前配置不可用")
				return m, nil
			}
			if m.rejectIfBusy() {
				return m, nil
			}
			m.openRotateServerModal()
			return m, textinput.Blink
		case "C":
			if m.cfgErr != nil {
				m.setToast("当前配置不可用")
				return m, nil
			}
			if m.rejectIfBusy() {
				return m, nil
			}
			m.openCSRModal("")
			return m, textinput.Blink
		case "enter":
			if m.cursors[3] < len(m.certs) {
				c := m.certs[m.cursors[3]]
				m.detailTitle = "证书详情 · " + sanitizeInline(c.Name)
				m.detailLines = []string{
					"角色：    " + sanitizeInline(c.Role),
					"路径：    " + sanitizeInline(c.Path),
					"状态：    " + sanitizeInline(c.State),
					"到期/备注：" + sanitizeInline(c.Expiry),
					"SHA-256： " + sanitizeInline(c.Fingerprint),
				}
				m.infoScroll = 0
				m.modal = srvModalDetail
			}
		}
	case 4: // 设置
		switch k {
		case "m":
			if m.cfgErr == nil {
				m.openEditSettingsModal()
				return m, textinput.Blink
			}
		case "a":
			if m.cfgErr == nil {
				m.openAdvancedSettingsModal()
				return m, textinput.Blink
			}
		case "A":
			return m, m.reloadServiceCmd()
		case "enter":
			if m.cfgErr == nil {
				return m.activateSettingsRow()
			}
		}
	case 5: // 日志
		switch k {
		case "p":
			m.logPaused = !m.logPaused
			if !m.logPaused {
				return m, m.pollRuntimeCmd()
			}
		case "l":
			order := []string{"", "INFO", "WARN", "ERROR"}
			cur := 0
			for i, v := range order {
				if v == m.logMinLevel {
					cur = i
					break
				}
			}
			m.logMinLevel = order[(cur+1)%len(order)]
			return m, m.resetLogsAndPollCmd()
		case "U":
			m.editingQuery = true
			m.queryMode = "log-user"
			m.queryInput.SetValue(m.logQuery.User)
			m.queryInput.Focus()
			m.queryInput.CursorEnd()
			return m, textinput.Blink
		case "F":
			m.editingQuery = true
			m.queryMode = "log-file"
			m.queryInput.SetValue(m.logQuery.File)
			m.queryInput.Focus()
			m.queryInput.CursorEnd()
			return m, textinput.Blink
		case "T":
			m.logSinceIdx = (m.logSinceIdx + 1) % 4
			return m, m.resetLogsAndPollCmd()
		case "C":
			m.logQuery = monitor.LogQuery{}
			m.logMinLevel = ""
			m.logSinceIdx = 0
			m.setToast("已清空全部日志筛选条件")
			return m, m.resetLogsAndPollCmd()
		case "enter":
			if m.logFallback == "" && m.cursors[5] < len(m.logEvents) {
				e := m.logEvents[m.cursors[5]]
				m.logPaused = true
				m.detailTitle = fmt.Sprintf("日志事件 #%d [%s]", e.Sequence, sanitizeInline(e.Level))
				m.detailLines = []string{
					"时间：  " + e.Time.Local().Format("2006-01-02 15:04:05"),
					"消息：  " + sanitizeInline(e.Message),
					"用户：  " + sanitizeInline(e.User),
					"文件：  " + sanitizeInline(e.File),
					"详情：  " + sanitizeInline(e.Detail),
				}
				m.infoScroll = 0
				m.modal = srvModalDetail
			}
		}
	}
	return m, nil
}

func (m *serverModel) jumpToLogs(q monitor.LogQuery, toast string) (tea.Model, tea.Cmd) {
	m.logEpoch++
	m.logQuery = q
	m.logMinLevel = ""
	m.logSinceIdx = 0
	m.logCursor = 0
	m.logLost = false
	m.logEvents = nil
	m.logPaused = false
	m.setToast(toast)
	return m.switchPage(5)
}

func (m *serverModel) switchPage(idx int) (tea.Model, tea.Cmd) {
	m.editingQuery = false
	m.queryMode = ""
	m.queryInput.Blur()
	m.page = max(0, min(5, idx))
	m.clampCursors()
	if m.page == 5 {
		return m, m.pollRuntimeCmd()
	}
	return m, nil
}

func (m *serverModel) updatePageFilter(val string) {
	switch m.page {
	case 1:
		m.userFilter = val
	case 2:
		m.txFilter = val
	case 5:
		m.logQuery.Text = val
	}
	m.clampCursors()
}

func (m *serverModel) filteredUsers() []config.AuthorizedClient {
	q := strings.ToLower(strings.TrimSpace(m.userFilter))
	var out []config.AuthorizedClient
	for _, u := range m.cfg.AuthorizedClients {
		if q != "" && !strings.Contains(strings.ToLower(u.UserID), q) {
			continue
		}
		out = append(out, u)
	}
	return out
}

func (m *serverModel) filteredTasks() []monitor.Task {
	if m.runtime == nil {
		return nil
	}
	q := strings.ToLower(strings.TrimSpace(m.txFilter))
	all := make([]monitor.Task, 0, len(m.runtime.Tasks)+len(m.runtime.History))
	all = append(all, m.runtime.Tasks...)
	all = append(all, m.runtime.History...)
	if q == "" {
		return all
	}
	var out []monitor.Task
	for _, t := range all {
		haystack := strings.ToLower(t.User + " " + t.File + " " + t.State + " " + serverTaskStateText(t.State) + " " + t.Direction + " " + serverDirectionText(t.Direction))
		if strings.Contains(haystack, q) {
			out = append(out, t)
		}
	}
	return out
}

func (m *serverModel) currentPageLen() int {
	switch m.page {
	case 0:
		if m.runtime == nil {
			return 0
		}
		return len(m.runtime.Peers)
	case 1:
		return len(m.filteredUsers())
	case 2:
		return len(m.filteredTasks())
	case 3:
		return len(m.certs)
	case 4:
		if !m.loaded || m.cfgErr != nil {
			return 0
		}
		return len(m.settingsLines())
	case 5:
		if m.logFallback != "" {
			return len(m.filteredLogFallbackLines())
		}
		return len(m.logEvents)
	}
	return 0
}

func (m *serverModel) clampCursors() {
	for i := 0; i < 6; i++ {
		if i == m.page {
			n := m.currentPageLen()
			if m.cursors[i] >= n {
				m.cursors[i] = max(0, n-1)
			}
			if i == 4 && n > 0 {
				m.snapSettingsCursor(1)
			}
		}
	}
}

func (m *serverModel) openConfirmModal(title string, lines []string, onConfirm func() tea.Cmd) {
	m.confirm = &serverConfirmAction{
		title:     title,
		lines:     lines,
		onConfirm: onConfirm,
	}
	m.modal = srvModalConfirm
}

func (m *serverModel) stepSettingsCursor(dir int) {
	rows := m.settingsRows()
	if len(rows) == 0 {
		m.cursors[4] = 0
		return
	}
	cur := m.cursors[4]
	for next := cur + dir; next >= 0 && next < len(rows); next += dir {
		if rows[next].selectable {
			m.cursors[4] = next
			return
		}
	}
}

func (m *serverModel) snapSettingsCursor(preferDir int) {
	rows := m.settingsRows()
	if len(rows) == 0 {
		m.cursors[4] = 0
		return
	}
	cur := max(0, min(len(rows)-1, m.cursors[4]))
	if rows[cur].selectable {
		m.cursors[4] = cur
		return
	}
	for _, dir := range []int{preferDir, -preferDir} {
		if dir == 0 {
			dir = 1
		}
		for i := cur + dir; i >= 0 && i < len(rows); i += dir {
			if rows[i].selectable {
				m.cursors[4] = i
				return
			}
		}
	}
	m.cursors[4] = cur
}

func (m *serverModel) activateSettingsRow() (tea.Model, tea.Cmd) {
	rows := m.settingsRows()
	if m.cursors[4] < 0 || m.cursors[4] >= len(rows) {
		return m, nil
	}
	r := rows[m.cursors[4]]
	if r.jumpPage >= 0 {
		return m.switchPage(r.jumpPage)
	}
	switch r.targetModal {
	case srvModalEditSettings:
		m.openEditSettingsModal()
		if idx, ok := m.inputByKey(r.focusKey); ok {
			return m, m.focusInputRow(idx)
		}
		return m, textinput.Blink
	case srvModalEditAdvancedSettings:
		m.openAdvancedSettingsModal()
		if idx, ok := m.inputByKey(r.focusKey); ok {
			return m, m.focusInputRow(idx)
		}
		return m, textinput.Blink
	}
	return m, nil
}

func (m *serverModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || m.width <= 0 || m.height <= 0 {
		return m, nil
	}
	if m.modal != srvModalNone {
		return m.handleModalMouse(msg)
	}
	if msg.Y == 1 && msg.Button == tea.MouseButtonLeft {
		names := []string{"1 概览", "2 用户", "3 传输", "4 证书", "5 设置", "6 日志"}
		if idx := tabHitIndex(msg.X, 1, names, m.th, m.page); idx >= 0 {
			return m.switchPage(idx)
		}
		return m, nil
	}
	const bodyY = 2
	footerH := lipgloss.Height(m.renderFooter())
	bodyH := max(6, m.height-2-footerH)
	if msg.Y < bodyY || msg.Y >= bodyY+bodyH {
		return m, nil
	}
	total := m.currentPageLen()
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if m.page == 5 {
			m.logPaused = true
		}
		if m.page == 4 {
			m.stepSettingsCursor(-1)
		} else if m.cursors[m.page] > 0 {
			m.cursors[m.page]--
		}
	case tea.MouseButtonWheelDown:
		if m.page == 4 {
			m.stepSettingsCursor(1)
		} else if m.cursors[m.page] < total-1 {
			m.cursors[m.page]++
		} else if m.page == 5 && m.logPaused {
			m.logPaused = false
			return m, m.pollRuntimeCmd()
		}
	case tea.MouseButtonLeft:
		if total == 0 {
			return m, nil
		}
		rowStartY := bodyY + 2
		visRows := max(1, bodyH-3)
		if m.page == 0 {
			_, _, topTotalH, botH := overviewLayout(m.width, bodyH)
			rowStartY = bodyY + topTotalH + 2
			visRows = max(1, botH-3)
			if hasTableHeader(visRows, total) {
				rowStartY++
				visRows--
			}
		} else if m.page >= 1 && m.page <= 3 {
			if hasTableHeader(visRows, total) {
				rowStartY++
				visRows--
			}
		} else if m.page == 5 {
			m.logPaused = true
			if m.hasLogFilters() {
				rowStartY++
				visRows = max(1, visRows-1)
			}
		}
		relY := msg.Y - rowStartY
		if relY >= 0 && relY < visRows {
			idx := scrollOffset(m.cursors[m.page], total, visRows) + relY
			if idx >= 0 && idx < total {
				m.cursors[m.page] = idx
				if m.page == 4 {
					m.snapSettingsCursor(1)
				}
			}
		}
	}
	return m, nil
}

func (m *serverModel) showUserDetailModal(u config.AuthorizedClient) {
	status := "已启用"
	if u.Disabled {
		status = "已吊销"
	}
	bwText := "继承默认策略"
	if u.Bandwidth != nil {
		bwText = fmt.Sprintf("↑ %s · ↓ %s · 突发 %s",
			progress.FormatRate(float64(u.Bandwidth.UploadBytesPerSecond)),
			progress.FormatRate(float64(u.Bandwidth.DownloadBytesPerSecond)),
			progress.FormatBytes(u.Bandwidth.BurstBytes))
	}
	ins := security.InspectCertificate(u.Certificate, time.Now())
	m.detailTitle = "用户详情 · " + sanitizeInline(u.UserID)
	m.detailLines = []string{
		"授权状态：" + status,
		"操作权限：" + sanitizeInline(strings.Join(u.Permissions, ", ")),
		"带宽策略：" + bwText,
		"证书路径：" + sanitizeInline(u.Certificate),
		fmt.Sprintf("证书状态：%s · 指纹 %s", sanitizeInline(ins.State), sanitizeInline(ins.Fingerprint)),
	}
	m.infoScroll = 0
	m.modal = srvModalDetail
}

func scrollInfoModalStep(width, height int, th theme, title string, lines []string, footer string, scroll *int, k string) {
	_, maxScroll := renderModalScrollBox(width, height, th, title, lines, footer, *scroll)
	if next, ok := modalScrollStep(k, *scroll, maxScroll, modalScrollBoxPageRows(height, footer)); ok {
		*scroll = next
	}
}

func (m *serverModel) scrollServerInfoModal(k string) {
	title, lines, footer := m.infoModalData()
	scrollInfoModalStep(m.width, m.height, m.th, title, lines, footer, &m.infoScroll, k)
}

func (m *serverModel) handleModalKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "esc" && !m.dropdownOpen {
		onEsc := m.formOnEsc
		m.formOnEsc = nil
		if m.modal != srvModalHelp && m.modal != srvModalDetail && m.modal != srvModalStop {
			m.clearToast()
		}
		if onEsc != nil {
			onEsc()
		}
		m.modal = srvModalNone
		m.targetUser = nil
		m.userDraft = nil
		m.confirm = nil
		m.settingsDraft = nil
		return m, nil
	}
	if m.modal == srvModalConfirm {
		switch k {
		case "enter", "y", "Y":
			act := m.confirm
			m.confirm = nil
			m.modal = srvModalNone
			if act != nil && act.onConfirm != nil {
				return m, act.onConfirm()
			}
		case "n", "N":
			m.confirm = nil
			m.modal = srvModalNone
		}
		return m, nil
	}
	if m.modal == srvModalHelp || m.modal == srvModalDetail {
		if k == "enter" || k == "q" || (m.modal == srvModalHelp && k == "?") {
			m.modal = srvModalNone
			return m, nil
		}
		m.scrollServerInfoModal(k)
		return m, nil
	}
	if m.modal == srvModalStop {
		switch k {
		case "g", "enter":
			m.modal = srvModalNone
			return m, m.stopServiceCmd(false)
		case "f":
			m.modal = srvModalNone
			return m, m.stopServiceCmd(true)
		}
		return m, nil
	}
	return m.handleFormKey(msg)
}
