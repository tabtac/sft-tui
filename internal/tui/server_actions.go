package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/control"
	"sft-tui/internal/security"
	"sft-tui/internal/service"
)

func (m *serverModel) initForm(labels, defaults, toggleLabels []string, toggles []bool) {
	m.resetForm()
	for i, lbl := range labels {
		def := ""
		if i < len(defaults) {
			def = defaults[i]
		}
		m.addTextRow(lbl, def)
	}
	for i, lbl := range toggleLabels {
		checked := false
		if i < len(toggles) {
			checked = toggles[i]
		}
		m.addToggleRow(lbl, checked, nil)
	}
	m.formButtons = []formButton{
		{label: "取消", onClick: func() (tea.Model, tea.Cmd) {
			m.modal = srvModalNone
			m.targetUser = nil
			return m, nil
		}},
		{label: "确认提交 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) {
			return m.submitModalForm()
		}},
	}
	m.finalizeFormFocus(true)
}

func (m *serverModel) openNewUserModal() {
	bw := config.DefaultServer().Bandwidth
	if m.cfgErr == nil {
		bw = m.cfg.Bandwidth
	}
	upNum, _, upUnit := decomposeBytes(bw.UploadBytesPerSecond, true)
	dnNum, _, dnUnit := decomposeBytes(bw.DownloadBytesPerSecond, true)
	buNum, _, buUnit := decomposeBytes(bw.BurstBytes, false)
	m.targetUser = nil
	m.userDraft = &userFormDraft{
		days:      "30",
		enabled:   true,
		allowUp:   true,
		allowDown: true,
		inheritBW: true,
		upNum:     upNum,
		upUnit:    upUnit,
		dnNum:     dnNum,
		dnUnit:    dnUnit,
		burstNum:  buNum,
		burstUnit: buUnit,
	}
	m.rebuildNewUserForm()
}

func (m *serverModel) rebuildNewUserForm() {
	d := m.userDraft
	if d == nil {
		return
	}
	m.resetForm()
	m.clearToast()
	m.modal = srvModalNewUser
	m.addTextRow("用户名", d.name)
	m.addTextRow("有效期(天)", d.days)
	m.addToggleRow("允许上传", d.allowUp, func(v bool) { d.allowUp = v })
	m.addToggleRow("允许下载", d.allowDown, func(v bool) { d.allowDown = v })

	capture := func() {
		if len(m.formInputs) >= 2 {
			d.name = m.formInputs[0].Value()
			d.days = m.formInputs[1].Value()
		}
		if len(m.formToggles) >= 3 {
			d.allowUp = m.formToggles[0]
			d.allowDown = m.formToggles[1]
			d.inheritBW = m.formToggles[2]
		}
		if !d.inheritBW && len(m.formInputs) >= 5 {
			d.upNum = m.formInputs[2].Value()
			d.dnNum = m.formInputs[3].Value()
			d.burstNum = m.formInputs[4].Value()
			if m.formUnits[2] != nil {
				d.upUnit = m.formUnits[2].selected
			}
			if m.formUnits[3] != nil {
				d.dnUnit = m.formUnits[3].selected
			}
			if m.formUnits[4] != nil {
				d.burstUnit = m.formUnits[4].selected
			}
		}
	}

	m.addToggleRow("继承默认带宽（取消勾选可自定义限速）", d.inheritBW, func(v bool) {
		capture()
		d.inheritBW = v
		focusRow := m.formFocus
		m.rebuildNewUserForm()
		m.setRowFocus(min(focusRow, len(m.formRows)-1), 0)
	})
	if !d.inheritBW {
		upIdx := m.addSizeUnitRow("自定义上传速率", 0, true)
		m.formInputs[upIdx].SetValue(d.upNum)
		m.formUnits[upIdx].selected = d.upUnit
		dnIdx := m.addSizeUnitRow("自定义下载速率", 0, true)
		m.formInputs[dnIdx].SetValue(d.dnNum)
		m.formUnits[dnIdx].selected = d.dnUnit
		buIdx := m.addSizeUnitRow("自定义突发配额", 0, false)
		m.formInputs[buIdx].SetValue(d.burstNum)
		m.formUnits[buIdx].selected = d.burstUnit
	}
	m.formButtons = []formButton{
		{label: "取消", onClick: func() (tea.Model, tea.Cmd) { m.modal = srvModalNone; m.userDraft = nil; m.clearToast(); return m, nil }},
		{label: "创建用户 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) { return m.submitModalForm() }},
	}
	m.finalizeFormFocus(true)
}

func (m *serverModel) openCSRModal(user string) {
	m.clearToast()
	m.targetUser = nil
	m.initForm([]string{"用户名", "CSR 文件路径", "有效期(天)"}, []string{user, "", "30"}, nil, nil)
	m.modal = srvModalCSR
}

func (m *serverModel) openCSRModalForRenew(u config.AuthorizedClient) {
	m.openCSRModal(u.UserID)
	uCopy := u
	m.targetUser = &uCopy
}

func (m *serverModel) openEditPolicyModal(u config.AuthorizedClient) {
	hasUp, hasDown := u.Permissions == nil, u.Permissions == nil
	for _, p := range u.Permissions {
		if p == "upload" {
			hasUp = true
		} else if p == "download" {
			hasDown = true
		}
	}
	bw := m.cfg.Bandwidth
	inherit := u.Bandwidth == nil
	if u.Bandwidth != nil {
		bw = *u.Bandwidth
	}
	upNum, _, upUnit := decomposeBytes(bw.UploadBytesPerSecond, true)
	dnNum, _, dnUnit := decomposeBytes(bw.DownloadBytesPerSecond, true)
	buNum, _, buUnit := decomposeBytes(bw.BurstBytes, false)
	uCopy := u
	m.targetUser = &uCopy
	m.userDraft = &userFormDraft{
		enabled:   !u.Disabled,
		allowUp:   hasUp,
		allowDown: hasDown,
		inheritBW: inherit,
		upNum:     upNum,
		upUnit:    upUnit,
		dnNum:     dnNum,
		dnUnit:    dnUnit,
		burstNum:  buNum,
		burstUnit: buUnit,
	}
	m.rebuildEditPolicyForm()
}

func (m *serverModel) rebuildEditPolicyForm() {
	d := m.userDraft
	if d == nil {
		return
	}
	m.resetForm()
	m.clearToast()
	m.modal = srvModalEditPolicy
	m.addToggleRow("启用该用户", d.enabled, func(v bool) { d.enabled = v })
	m.addToggleRow("允许上传", d.allowUp, func(v bool) { d.allowUp = v })
	m.addToggleRow("允许下载", d.allowDown, func(v bool) { d.allowDown = v })

	capture := func() {
		if len(m.formToggles) >= 4 {
			d.enabled = m.formToggles[0]
			d.allowUp = m.formToggles[1]
			d.allowDown = m.formToggles[2]
			d.inheritBW = m.formToggles[3]
		}
		if !d.inheritBW && len(m.formInputs) >= 3 {
			d.upNum = m.formInputs[0].Value()
			d.dnNum = m.formInputs[1].Value()
			d.burstNum = m.formInputs[2].Value()
			if m.formUnits[0] != nil {
				d.upUnit = m.formUnits[0].selected
			}
			if m.formUnits[1] != nil {
				d.dnUnit = m.formUnits[1].selected
			}
			if m.formUnits[2] != nil {
				d.burstUnit = m.formUnits[2].selected
			}
		}
	}

	m.addToggleRow("继承默认带宽（取消勾选可自定义速率）", d.inheritBW, func(v bool) {
		capture()
		d.inheritBW = v
		focusRow := m.formFocus
		m.rebuildEditPolicyForm()
		m.setRowFocus(min(focusRow, len(m.formRows)-1), 0)
	})
	if !d.inheritBW {
		upIdx := m.addSizeUnitRow("上传速率", 0, true)
		m.formInputs[upIdx].SetValue(d.upNum)
		m.formUnits[upIdx].selected = d.upUnit
		dnIdx := m.addSizeUnitRow("下载速率", 0, true)
		m.formInputs[dnIdx].SetValue(d.dnNum)
		m.formUnits[dnIdx].selected = d.dnUnit
		buIdx := m.addSizeUnitRow("突发配额", 0, false)
		m.formInputs[buIdx].SetValue(d.burstNum)
		m.formUnits[buIdx].selected = d.burstUnit
	}
	m.formButtons = []formButton{
		{label: "取消", onClick: func() (tea.Model, tea.Cmd) {
			m.modal = srvModalNone
			m.targetUser = nil
			m.userDraft = nil
			m.clearToast()
			return m, nil
		}},
		{label: "保存策略 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) { return m.submitModalForm() }},
	}
	m.finalizeFormFocus(true)
}

func (m *serverModel) openExportUserModal(u config.AuthorizedClient) {
	uCopy := u
	m.targetUser = &uCopy
	m.clearToast()
	outDir := filepath.Join("bundles", u.UserID)
	m.initForm([]string{"导出目标目录"}, []string{outDir}, []string{"仅导出公开证书(-public-only，不含私钥)"}, []bool{u.ExternalKey})
	m.modal = srvModalExportUser
}

func (m *serverModel) openRotateServerModal() {
	m.clearToast()
	m.targetUser = nil
	m.initForm([]string{"服务端 SAN(逗号分隔 DNS/IP，留空沿用)", "有效期(天)"}, []string{"", "30"}, nil, nil)
	m.modal = srvModalRotateServer
}

func (m *serverModel) certBaseOptions(name string) certmanage.Options {
	dir := "certs"
	if m.cfgErr == nil && m.cfg.TLS.CACertificate != "" {
		dir = filepath.Dir(m.cfg.TLS.CACertificate)
	}
	return certmanage.Options{
		Dir:             dir,
		ServerConfig:    m.opts.ConfigPath,
		ClientConfigDir: filepath.Dir(m.opts.ConfigPath),
		Name:            name,
		Days:            30,
	}
}

func (m *serverModel) execCertAndReload(action string, o certmanage.Options, notice string) tea.Cmd {
	if m.rejectIfBusy() {
		return nil
	}
	opLabel := "执行证书与权限变更"
	switch action {
	case "user":
		if o.Renew {
			opLabel = "续期用户证书"
		} else {
			opLabel = "签发用户证书"
		}
	case "policy":
		opLabel = "保存用户策略"
	case "ca":
		opLabel = "续期 CA 证书"
	case "server":
		opLabel = "轮换服务端证书"
	}
	m.startBusy(opLabel)
	cfgPath := m.opts.ConfigPath
	return func() tea.Msg {
		res, err := certmanage.Execute(action, o)
		if err != nil {
			return serverActionResultMsg{err: err}
		}
		if res.Changed {
			if applied, rErr := control.Reload(cfgPath, res.Server.StorageRoot); rErr != nil {
				return serverActionResultMsg{err: fmt.Errorf("已保存，但热加载失败：%w", rErr), refresh: true}
			} else if applied {
				notice += "（已热加载到运行中的服务端）"
			}
		}
		return serverActionResultMsg{notice: notice, refresh: true, poll: true}
	}
}

func parseStrictCertDays(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("有效天数不能为空，请输入 1–3650 之间的整数")
	}
	days, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("有效天数 %q 无效：请输入 1–3650 之间的整数", s)
	}
	if days < 1 || days > 3650 {
		return 0, fmt.Errorf("有效天数 %d 超出范围，须在 1–3650 天之间", days)
	}
	return days, nil
}

func (m *serverModel) validateBandwidthInputs(upIdx, dnIdx, burstIdx int) (*config.Bandwidth, int, error) {
	fields := []struct {
		idx   int
		label string
	}{
		{upIdx, "上传速率"},
		{dnIdx, "下载速率"},
		{burstIdx, "突发配额"},
	}
	vals := make([]uint64, len(fields))
	for i, f := range fields {
		v, err := m.parseSizeInput(f.idx, f.label)
		if err != nil {
			return nil, f.idx, err
		}
		vals[i] = v
	}
	const maxSafeBandwidth = uint64(1) << 53
	if vals[0] == 0 || vals[0] > maxSafeBandwidth {
		return nil, upIdx, fmt.Errorf("上传速率必须大于 0 且不超过 %d B/s", maxSafeBandwidth)
	}
	if vals[1] == 0 || vals[1] > maxSafeBandwidth {
		return nil, dnIdx, fmt.Errorf("下载速率必须大于 0 且不超过 %d B/s", maxSafeBandwidth)
	}
	chunk := uint64(config.DefaultServer().Limits.ChunkSize)
	if m.cfgErr == nil && m.cfg.Limits.ChunkSize > 0 {
		chunk = uint64(m.cfg.Limits.ChunkSize)
	}
	if vals[2] < chunk || vals[2] > 1<<30 {
		return nil, burstIdx, fmt.Errorf("突发配额须在分块大小 (%d B) 与 1 GiB 之间", chunk)
	}
	return &config.Bandwidth{
		UploadBytesPerSecond:   vals[0],
		DownloadBytesPerSecond: vals[1],
		BurstBytes:             vals[2],
	}, -1, nil
}

func (m *serverModel) findAuthorizedClient(name string) (config.AuthorizedClient, bool) {
	for _, u := range m.cfg.AuthorizedClients {
		match := u.UserID == name
		if runtime.GOOS == "windows" {
			match = strings.EqualFold(u.UserID, name)
		}
		if match {
			return u, true
		}
	}
	return config.AuthorizedClient{}, false
}

func isReservedIdentityName(name string) bool {
	return strings.EqualFold(name, "ca") || strings.EqualFold(name, "server")
}

func (m *serverModel) submitModalForm() (tea.Model, tea.Cmd) {
	switch m.modal {
	case srvModalNewUser:
		name := strings.TrimSpace(m.formInputs[0].Value())
		if !config.ValidUserPath(name) || isReservedIdentityName(name) {
			m.setToastSticky("用户名无效（须为字母、数字、_ 或 -，且不能为系统保留名）")
			return m, m.focusInputRow(0)
		}
		if existing, ok := m.findAuthorizedClient(name); ok {
			m.setToastSticky(fmt.Sprintf("用户 %q 已存在", existing.UserID))
			return m, m.focusInputRow(0)
		}
		days, err := parseStrictCertDays(m.formInputs[1].Value())
		if err != nil {
			m.setToastSticky(err.Error())
			return m, m.focusInputRow(1)
		}
		perms := []string{}
		if m.formToggles[0] {
			perms = append(perms, "upload")
		}
		if m.formToggles[1] {
			perms = append(perms, "download")
		}
		var bw *config.Bandwidth
		if len(m.formToggles) > 2 && !m.formToggles[2] {
			var badIdx int
			bw, badIdx, err = m.validateBandwidthInputs(2, 3, 4)
			if err != nil {
				m.setToastSticky(err.Error())
				return m, m.focusInputRow(badIdx)
			}
		}
		m.clearToast()
		m.modal = srvModalNone
		m.userDraft = nil
		o := m.certBaseOptions(name)
		o.Days = days
		o.Policy = &certmanage.UserPolicy{Permissions: perms, Bandwidth: bw}
		return m, m.execCertAndReload("user", o, "已新建用户 "+name)

	case srvModalCSR:
		name := strings.TrimSpace(m.formInputs[0].Value())
		if !config.ValidUserPath(name) || isReservedIdentityName(name) {
			m.setToastSticky("用户名无效（须为字母、数字、_ 或 -，且不能为系统保留名）")
			return m, m.focusInputRow(0)
		}
		renewTarget := m.targetUser != nil && (m.targetUser.UserID == name || (runtime.GOOS == "windows" && strings.EqualFold(m.targetUser.UserID, name)))
		if !renewTarget {
			if existing, ok := m.findAuthorizedClient(name); ok {
				m.setToastSticky(fmt.Sprintf("用户 %q 已存在（如需续期请从用户列表按 [w]）", existing.UserID))
				return m, m.focusInputRow(0)
			}
		}
		csr := strings.TrimSpace(m.formInputs[1].Value())
		if csr == "" {
			m.setToastSticky("CSR 文件路径不能为空")
			return m, m.focusInputRow(1)
		}
		days, err := parseStrictCertDays(m.formInputs[2].Value())
		if err != nil {
			m.setToastSticky(err.Error())
			return m, m.focusInputRow(2)
		}
		parsedCSR, err := security.ReadCSR(csr)
		if err != nil {
			m.setToastSticky(err.Error())
			return m, m.focusInputRow(1)
		}
		csrSum := sha256.Sum256(parsedCSR.Raw)
		var expected *config.AuthorizedClient
		if renewTarget {
			uCopy := *m.targetUser
			expected = &uCopy
			name = uCopy.UserID
		}
		m.clearToast()
		m.modal = srvModalNone
		m.targetUser = nil
		o := m.certBaseOptions(name)
		o.CSR = csr
		o.CSRFingerprint = hex.EncodeToString(csrSum[:])
		o.Days = days
		o.Renew = renewTarget
		o.ExpectedUser = expected
		notice := "CSR 已签发给 " + name
		if renewTarget {
			notice = "已使用外部 CSR 续期用户 " + name
		}
		return m, m.execCertAndReload("user", o, notice)

	case srvModalEditPolicy:
		if m.targetUser == nil {
			m.modal = srvModalNone
			return m, nil
		}
		u := *m.targetUser
		perms := []string{}
		if m.formToggles[1] {
			perms = append(perms, "upload")
		}
		if m.formToggles[2] {
			perms = append(perms, "download")
		}
		var bw *config.Bandwidth
		if !m.formToggles[3] {
			var badIdx int
			var err error
			bw, badIdx, err = m.validateBandwidthInputs(0, 1, 2)
			if err != nil {
				m.setToastSticky(err.Error())
				return m, m.focusInputRow(badIdx)
			}
		}
		m.clearToast()
		m.modal = srvModalNone
		m.targetUser = nil
		m.userDraft = nil
		o := m.certBaseOptions(u.UserID)
		o.ExpectedUser = &u
		o.Policy = &certmanage.UserPolicy{
			Disabled:    !m.formToggles[0],
			Permissions: perms,
			Bandwidth:   bw,
		}
		return m, m.execCertAndReload("policy", o, "已更新用户策略 "+u.UserID)

	case srvModalExportUser:
		if m.targetUser == nil {
			m.modal = srvModalNone
			return m, nil
		}
		if m.rejectIfBusy() {
			return m, nil
		}
		out := strings.TrimSpace(m.formInputs[0].Value())
		if out == "" {
			m.setToastSticky("导出目标目录不能为空")
			return m, m.focusInputRow(0)
		}
		u := *m.targetUser
		m.clearToast()
		m.modal = srvModalNone
		m.targetUser = nil
		m.userDraft = nil
		o := m.certBaseOptions(u.UserID)
		o.Out = out
		o.CertificateOnly = m.formToggles[0]
		m.startBusy("导出用户分发包")
		return m, func() tea.Msg {
			if _, err := certmanage.Execute("export", o); err != nil {
				return serverActionResultMsg{err: err}
			}
			return serverActionResultMsg{notice: "已导出用户分发目录：" + o.Out}
		}

	case srvModalRotateServer:
		hosts := strings.TrimSpace(m.formInputs[0].Value())
		if hosts != "" {
			if _, _, err := security.ParseServerNames(hosts); err != nil {
				m.setToastSticky(err.Error())
				return m, m.focusInputRow(0)
			}
		}
		days, err := parseStrictCertDays(m.formInputs[1].Value())
		if err != nil {
			m.setToastSticky(err.Error())
			return m, m.focusInputRow(1)
		}
		m.clearToast()
		m.modal = srvModalNone
		o := m.certBaseOptions("")
		o.Hosts = hosts
		o.Days = days
		return m, m.execCertAndReload("server", o, "服务端证书已轮换")
	}
	m.modal = srvModalNone
	return m, nil
}

func (m *serverModel) toggleUserDisabledCmd(u config.AuthorizedClient) tea.Cmd {
	o := m.certBaseOptions(u.UserID)
	uCopy := u
	o.ExpectedUser = &uCopy
	o.Policy = &certmanage.UserPolicy{
		Disabled:    !u.Disabled,
		Permissions: u.Permissions,
		Bandwidth:   u.Bandwidth,
	}
	label := "已吊销用户 " + u.UserID
	if u.Disabled {
		label = "已重新启用用户 " + u.UserID
	}
	return m.execCertAndReload("policy", o, label)
}

func (m *serverModel) renewUserCertCmd(u config.AuthorizedClient) tea.Cmd {
	o := m.certBaseOptions(u.UserID)
	uCopy := u
	o.ExpectedUser = &uCopy
	o.Renew = true
	notice := "已刷新用户证书 " + u.UserID
	if u.Disabled {
		notice += "（保留原禁用状态）"
	}
	return m.execCertAndReload("user", o, notice)
}

func (m *serverModel) renewCACmd() tea.Cmd {
	o := m.certBaseOptions("")
	o.Renew = true
	o.Days = 365
	return m.execCertAndReload("ca", o, "CA 证书已续期 365 天")
}

func (m *serverModel) reloadServiceCmd() tea.Cmd {
	if m.rejectIfBusy() {
		return nil
	}
	cfgPath := m.opts.ConfigPath
	root := m.endpointRoot
	m.startBusy("热加载服务配置")
	return func() tea.Msg {
		ok, err := control.Reload(cfgPath, root)
		if err != nil {
			return serverActionResultMsg{err: err}
		}
		if !ok {
			return serverActionResultMsg{notice: "未检测到运行中的服务，启动后生效"}
		}
		return serverActionResultMsg{notice: "已触发运行中服务热加载证书与 ACL", refresh: true, poll: true}
	}
}

func (m *serverModel) startServiceCmd() tea.Cmd {
	if m.rejectIfBusy() {
		return nil
	}
	if m.runtime != nil && m.runtime.State == "running" {
		m.setToast("服务已在运行中")
		return nil
	}
	cfgPath := m.opts.ConfigPath
	exeDir := m.opts.ExecutableDir
	m.startBusy("后台启动 sftd 服务")
	m.setToastSticky("正在后台启动 sftd 服务…")
	return func() tea.Msg {
		exe, err := os.Executable()
		if err != nil {
			exe = filepath.Join(exeDir, "sftd")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
		defer cancel()
		st, err := service.Start(ctx, exe, cfgPath)
		if err != nil {
			return serverActionResultMsg{err: err}
		}
		if st.AlreadyRunning {
			return serverActionResultMsg{notice: "服务已在运行中", poll: true}
		}
		return serverActionResultMsg{notice: fmt.Sprintf("服务已后台启动（PID %d）", st.PID), poll: true}
	}
}

func (m *serverModel) stopServiceCmd(immediate bool) tea.Cmd {
	if m.rejectIfBusy() {
		return nil
	}
	cfgPath := m.opts.ConfigPath
	root := m.endpointRoot
	m.startBusy("停止 sftd 服务")
	return func() tea.Msg {
		ok, err := control.Stop(cfgPath, root, immediate)
		if err != nil {
			return serverActionResultMsg{err: err}
		}
		if !ok {
			return serverActionResultMsg{notice: "未检测到运行中的服务"}
		}
		msg := "已请求服务优雅排空后停机"
		if immediate {
			msg = "已发送立即停服指令"
		}
		return serverActionResultMsg{notice: msg, poll: true}
	}
}
