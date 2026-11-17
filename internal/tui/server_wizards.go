package tui

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/control"
	"sft-tui/internal/progress"
	"sft-tui/internal/service"
	"sft-tui/internal/setup"
)

type deployInitDoneMsg struct {
	d       setup.Deployment
	started bool
	notice  string
	err     error
}

type deployExportDoneMsg struct {
	outDir string
	msgs   []string
	err    error
}

func (m *serverModel) beginDeployWizard() {
	if m.cfgErr == nil && m.loaded {
		m.statusToast = "当前部署配置已存在，可在 [5 设置] 或 [2 用户] 中维护"
		return
	}
	if m.deploy == nil {
		base := m.opts.ExecutableDir
		if base == "" {
			base = "."
		}
		d := setup.DefaultDeployment(m.opts.ConfigPath, base)
		d.Storage = filepath.Join("var", "storage")
		def := config.DefaultServer()
		m.deploy = &deployWizardDraft{
			d:          d,
			base:       base,
			step:       1,
			addresses:  setup.LocalAddresses(),
			globalUp:   def.Bandwidth.UploadBytesPerSecond * 2,
			globalDown: def.Bandwidth.DownloadBytesPerSecond * 2,
			globalBur:  def.Bandwidth.BurstBytes * 2,
			exportDir:  d.User + "-bundle",
		}
	} else {
		m.deploy.addresses = setup.LocalAddresses()
		if m.deploy.step < 1 || m.deploy.step > 5 {
			m.deploy.step = 1
		}
	}
	m.openDeployStep(m.deploy.step)
}

func (m *serverModel) openDeployStep(step int) {
	w := m.deploy
	if w == nil {
		return
	}
	w.step = step
	m.resetForm()
	m.modal = srvModalDeployWizard

	switch step {
	case 1:
		m.addNoteRow([]string{
			"第 1 步 / 4：本机演示使用回环地址；局域网部署选择网卡 IP 或填写实际 DNS，",
			"监听地址、证书 SAN 和客户端连接地址会自动联动。目标配置：" + truncCell(w.d.Config, 50),
		}, false)
		nicOpts := []string{"手动输入 DNS/IP"}
		nicOpts = append(nicOpts, w.addresses...)
		var sceneIdx int
		sceneIdx = m.addSelectRow("部署场景", []string{"本机演示 (仅本机 127.0.0.1)", "局域网 / 跨主机部署 (0.0.0.0)"}, w.scene, nil)
		addrIdx := m.addTextRow("客户端连接地址 (host:port)", w.d.Address)
		listenIdx := m.addTextRow("服务端监听地址 (listen host:port)", w.d.Listen)

		m.formSelects[sceneIdx].onChange = func(_ string, i int) {
			w.scene = i
			if i == 0 {
				m.formInputs[addrIdx].SetValue("127.0.0.1:7443")
				m.formInputs[listenIdx].SetValue("127.0.0.1:7443")
			} else {
				target := "127.0.0.1:7443"
				if len(w.addresses) > 0 {
					target = net.JoinHostPort(w.addresses[0], "7443")
				}
				m.formInputs[addrIdx].SetValue(target)
				m.formInputs[listenIdx].SetValue("0.0.0.0:7443")
			}
		}
		nicSelected := 0
		hostOnly, _, _ := net.SplitHostPort(w.d.Address)
		for i, ip := range w.addresses {
			if ip == hostOnly {
				nicSelected = i + 1
				break
			}
		}
		m.addSelectRow("本机网卡地址快速填充", nicOpts, nicSelected, func(_ string, i int) {
			if i > 0 && i-1 < len(w.addresses) {
				ip := w.addresses[i-1]
				w.scene = 1
				m.formSelects[sceneIdx].selected = 1
				_, port, _ := net.SplitHostPort(m.formInputs[addrIdx].Value())
				if port == "" {
					port = "7443"
				}
				m.formInputs[addrIdx].SetValue(net.JoinHostPort(ip, port))
				if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() == nil {
					m.formInputs[listenIdx].SetValue(net.JoinHostPort("::", port))
				} else {
					m.formInputs[listenIdx].SetValue(net.JoinHostPort("0.0.0.0", port))
				}
			}
		})

		capture1 := func() {
			w.scene = m.formSelects[sceneIdx].selected
			w.d.Address = strings.TrimSpace(m.formInputs[addrIdx].Value())
			w.d.Listen = strings.TrimSpace(m.formInputs[listenIdx].Value())
		}
		m.formButtons = []formButton{
			{label: "返回面板", onClick: func() (tea.Model, tea.Cmd) {
				capture1()
				m.modal = srvModalNone
				m.statusToast = "部署向导输入已暂存，按 [I] 可继续"
				return m, nil
			}},
			{label: "下一步 ▶", primary: true, onClick: func() (tea.Model, tea.Cmd) {
				capture1()
				host, port, err := setup.ValidateAddress(w.d.Address)
				if err != nil {
					m.statusToast = err.Error()
					return m, nil
				}
				if w.scene == 0 && host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
					m.statusToast = "跨主机地址请将部署场景切换为「局域网 / 跨主机部署」"
					return m, nil
				}
				if _, lport, err := net.SplitHostPort(w.d.Listen); err != nil || lport == "" {
					m.statusToast = "监听地址格式无效，须为 IP:端口（如 0.0.0.0:7443）"
					return m, nil
				}
				baseKey := fmt.Sprintf("%d:%s", w.scene, w.d.Address)
				if w.addrBase != baseKey {
					w.d.ServerName = host
					w.d.Hosts = host + ",localhost,127.0.0.1,::1"
				}
				w.addrBase = baseKey
				if w.d.Listen == "" {
					listenHost := "127.0.0.1"
					if host == "::1" {
						listenHost = "::1"
					}
					if w.scene == 1 {
						listenHost = "0.0.0.0"
						if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
							listenHost = "::"
						}
					}
					w.d.Listen = net.JoinHostPort(listenHost, port)
				}
				m.openDeployStep(2)
				return m, textinput.Blink
			}},
		}
		m.finalizeFormFocus(true)

	case 2:
		m.addNoteRow([]string{
			"第 2 步 / 4：配置服务端文件根目录；默认使用独立子目录，确认时转为绝对路径。",
			"目标配置：" + truncCell(w.d.Config, 56),
		}, false)
		stIdx := m.addTextRow("文件存储根目录", w.d.Storage)
		capture2 := func() {
			w.d.Storage = strings.TrimSpace(m.formInputs[stIdx].Value())
		}
		m.formButtons = []formButton{
			{label: "◀ 上一步", onClick: func() (tea.Model, tea.Cmd) {
				capture2()
				m.openDeployStep(1)
				return m, textinput.Blink
			}},
			{label: "下一步 ▶", primary: true, onClick: func() (tea.Model, tea.Cmd) {
				capture2()
				if w.d.Storage == "" {
					m.statusToast = "存储目录不能为空"
					return m, nil
				}
				if !filepath.IsAbs(w.d.Storage) {
					w.d.Storage = filepath.Join(w.base, w.d.Storage)
				}
				abs, err := filepath.Abs(w.d.Storage)
				if err != nil {
					m.statusToast = err.Error()
					return m, nil
				}
				w.d.Storage = abs
				m.openDeployStep(3)
				return m, textinput.Blink
			}},
		}
		m.finalizeFormFocus(true)

	case 3:
		m.addNoteRow([]string{
			"第 3 步 / 4：配置初始授权用户、默认速率与配额；速率/大小可按数字加右侧下拉单位选择。",
		}, false)
		userIdx := m.addTextRow("初始授权用户名", w.d.User)
		upIdx := m.addSizeUnitRow("默认上传带宽", w.d.Upload, true)
		dnIdx := m.addSizeUnitRow("默认下载带宽", w.d.Download, true)
		maxIdx := m.addSizeUnitRow("单文件大小上限", w.d.MaxFile, false)
		quotaIdx := m.addSizeUnitRow("每用户存储上限", w.d.UserStorage, false)

		var certIdx, cliIdx, snameIdx, sanIdx, daysIdx, caDaysIdx int
		if w.advanced {
			certIdx = m.addTextRow("证书保存目录", w.d.CertDir)
			cliIdx = m.addTextRow("用户配置输出目录", w.d.ClientDir)
			snameIdx = m.addTextRow("客户端校验名称 (SAN)", w.d.ServerName)
			sanIdx = m.addTextRow("服务端 SAN 列表 (DNS/IP，逗号分隔)", w.d.Hosts)
			daysIdx = m.addTextRow("服务端/用户证书有效天数 (1–3650)", strconv.Itoa(w.d.Days))
			caDaysIdx = m.addTextRow("CA 根证书有效天数 (1–3650)", strconv.Itoa(w.d.CADays))
		}

		capture3 := func(validateNums bool) error {
			w.d.User = strings.TrimSpace(m.formInputs[userIdx].Value())
			if w.advanced {
				w.d.CertDir = strings.TrimSpace(m.formInputs[certIdx].Value())
				w.d.ClientDir = strings.TrimSpace(m.formInputs[cliIdx].Value())
				w.d.ServerName = strings.TrimSpace(m.formInputs[snameIdx].Value())
				w.d.Hosts = strings.TrimSpace(m.formInputs[sanIdx].Value())
				if d, err := strconv.Atoi(strings.TrimSpace(m.formInputs[daysIdx].Value())); err == nil {
					w.d.Days = d
				} else if validateNums {
					return fmt.Errorf("证书有效天数须为整数")
				}
				if cd, err := strconv.Atoi(strings.TrimSpace(m.formInputs[caDaysIdx].Value())); err == nil {
					w.d.CADays = cd
				} else if validateNums {
					return fmt.Errorf("CA 有效天数须为整数")
				}
			}
			var err error
			pairs := []struct {
				idx    int
				label  string
				target *uint64
			}{
				{upIdx, "默认上传带宽", &w.d.Upload},
				{dnIdx, "默认下载带宽", &w.d.Download},
				{maxIdx, "单文件大小上限", &w.d.MaxFile},
				{quotaIdx, "每用户存储上限", &w.d.UserStorage},
			}
			for _, p := range pairs {
				v, e := m.parseSizeInput(p.idx, p.label)
				if e != nil {
					if validateNums {
						return e
					}
					err = e
				} else {
					*p.target = v
				}
			}
			_ = err
			return nil
		}

		advToggle := m.addToggleRow("展开/调整高级证书、SAN 与目录设置", w.advanced, func(v bool) {
			_ = capture3(false)
			w.advanced = v
			focusRow := m.formFocus
			m.openDeployStep(3)
			if focusRow < len(m.formRows) {
				m.setRowFocus(min(focusRow, len(m.formRows)-1), 0)
			}
		})
		_ = advToggle
		tuneToggle := m.addToggleRow("下一步继续配置全局带宽、突发量、流控与超时高级参数", w.tuneLimits, func(v bool) {
			w.tuneLimits = v
		})
		_ = tuneToggle

		m.formButtons = []formButton{
			{label: "◀ 上一步", onClick: func() (tea.Model, tea.Cmd) {
				_ = capture3(false)
				m.openDeployStep(2)
				return m, textinput.Blink
			}},
			{label: "下一步 ▶", primary: true, onClick: func() (tea.Model, tea.Cmd) {
				if err := capture3(true); err != nil {
					m.statusToast = err.Error()
					return m, nil
				}
				for _, p := range []*string{&w.d.CertDir, &w.d.ClientDir} {
					if !filepath.IsAbs(*p) {
						*p = filepath.Join(w.base, *p)
					}
					*p, _ = filepath.Abs(*p)
				}
				if w.tuneLimits {
					m.openDeployStep(35)
				} else {
					w.summaryErr = ""
					m.openDeployStep(4)
				}
				return m, textinput.Blink
			}},
		}
		m.finalizeFormFocus(true)

	case 35:
		// 3.5 步：协议流控、全局总带宽与资源并发高级参数（向导内完整涵盖每项配置）
		m.addNoteRow([]string{
			"高级协议与资源参数：可调整全局总带宽、日志上限、分块/窗口、超时与并发限额。",
		}, false)
		def := config.DefaultServer()
		if w.d.Limits == nil {
			l := def.Limits
			w.d.Limits = &l
		}
		if w.d.Timeouts == nil {
			t := def.Timeouts
			w.d.Timeouts = &t
		}
		if w.d.Resources == nil {
			r := def.Resources
			w.d.Resources = &r
		}
		if w.d.Burst == 0 {
			w.d.Burst = def.Bandwidth.BurstBytes
		}
		if w.d.LogMaxBytes == 0 {
			w.d.LogMaxBytes = def.Logging.MaxFileBytes
		}
		burstIdx := m.addSizeUnitRow("默认单用户带宽突发量", w.d.Burst, false)
		logIdx := m.addSizeUnitRow("日志单文件上限 (64 KiB–1 GiB)", w.d.LogMaxBytes, false)
		var gUpIdx, gDnIdx, gBurIdx int
		if w.useGlobal {
			gUpIdx = m.addSizeUnitRow("全局总上传带宽", w.globalUp, true)
			gDnIdx = m.addSizeUnitRow("全局总下载带宽", w.globalDown, true)
			gBurIdx = m.addSizeUnitRow("全局总带宽突发量", w.globalBur, false)
		}

		draft := config.DefaultServer()
		draft.Limits = *w.d.Limits
		draft.Timeouts = *w.d.Timeouts
		draft.Resources = *w.d.Resources
		advFields := configAdvancedFields(&draft)
		advInputIdx := make([]int, len(advFields))
		advIsSize := make([]bool, len(advFields))
		for i, f := range advFields {
			_, key, _ := strings.Cut(f.Path, ".")
			if key == "chunk_size" || key == "window_size" {
				advIsSize[i] = true
				advInputIdx[i] = m.addSizeUnitRow(settingLabel(key), f.Uint(), false)
			} else {
				advInputIdx[i] = m.addTextRow(settingLabel(key), strconv.FormatUint(f.Uint(), 10))
			}
		}

		capture35 := func(strict bool) error {
			if b, err := m.parseSizeInput(burstIdx, "默认突发量"); err == nil {
				w.d.Burst = b
			} else if strict {
				return err
			}
			if lb, err := m.parseSizeInput(logIdx, "日志单文件上限"); err == nil {
				w.d.LogMaxBytes = lb
			} else if strict {
				return err
			}
			if w.useGlobal {
				if u, err := m.parseSizeInput(gUpIdx, "全局上传带宽"); err == nil {
					w.globalUp = u
				} else if strict {
					return err
				}
				if dn, err := m.parseSizeInput(gDnIdx, "全局下载带宽"); err == nil {
					w.globalDown = dn
				} else if strict {
					return err
				}
				if bu, err := m.parseSizeInput(gBurIdx, "全局突发量"); err == nil {
					w.globalBur = bu
				} else if strict {
					return err
				}
				gb := config.Bandwidth{UploadBytesPerSecond: w.globalUp, DownloadBytesPerSecond: w.globalDown, BurstBytes: w.globalBur}
				w.d.GlobalBandwidth = &gb
			} else {
				w.d.GlobalBandwidth = nil
			}
			for i, f := range advFields {
				if advIsSize[i] {
					v, err := m.parseSizeInput(advInputIdx[i], f.Path)
					if err != nil {
						if strict {
							return err
						}
						continue
					}
					if err := f.Set(strconv.FormatUint(v, 10)); err != nil && strict {
						return err
					}
				} else {
					if err := f.Set(m.formInputs[advInputIdx[i]].Value()); err != nil && strict {
						return err
					}
				}
			}
			l, t, r := draft.Limits, draft.Timeouts, draft.Resources
			w.d.Limits, w.d.Timeouts, w.d.Resources = &l, &t, &r
			return nil
		}

		m.addToggleRow("启用全局总带宽限速 (global_bandwidth)", w.useGlobal, func(v bool) {
			_ = capture35(false)
			w.useGlobal = v
			focusRow := m.formFocus
			m.openDeployStep(35)
			m.setRowFocus(min(focusRow, len(m.formRows)-1), 0)
		})

		m.formButtons = []formButton{
			{label: "◀ 返回第 3 步", onClick: func() (tea.Model, tea.Cmd) {
				_ = capture35(false)
				m.openDeployStep(3)
				return m, textinput.Blink
			}},
			{label: "下一步：部署摘要 ▶", primary: true, onClick: func() (tea.Model, tea.Cmd) {
				if err := capture35(true); err != nil {
					m.statusToast = err.Error()
					return m, nil
				}
				w.summaryErr = ""
				m.openDeployStep(4)
				return m, nil
			}},
		}
		m.finalizeFormFocus(true)

	case 4:
		d := w.d
		summary := []string{
			fmt.Sprintf("配置文件：%s", d.Config),
			fmt.Sprintf("监听地址：%s   ·   客户端连接：%s", d.Listen, d.Address),
			fmt.Sprintf("证书 SAN：%s   (校验名称：%s)", d.Hosts, d.ServerName),
			fmt.Sprintf("存储根目录：%s", d.Storage),
			fmt.Sprintf("初始用户：%s   ·   证书 %d 天 / CA %d 天", d.User, d.Days, d.CADays),
			fmt.Sprintf("默认带宽：↑ %s  ↓ %s   ·   文件上限 %s / 用户配额 %s",
				progress.FormatRate(float64(d.Upload)), progress.FormatRate(float64(d.Download)),
				progress.FormatBytes(d.MaxFile), progress.FormatBytes(d.UserStorage)),
			"确认后统一原子生成配置与证书；已有配置或 CA 不会被覆盖。",
		}
		if d.GlobalBandwidth != nil {
			summary = append(summary, fmt.Sprintf("全局总带宽：↑ %s  ↓ %s  Burst %s",
				progress.FormatRate(float64(d.GlobalBandwidth.UploadBytesPerSecond)),
				progress.FormatRate(float64(d.GlobalBandwidth.DownloadBytesPerSecond)),
				progress.FormatBytes(d.GlobalBandwidth.BurstBytes)))
		}
		if w.summaryErr != "" {
			m.addNoteRow([]string{"保存失败：" + w.summaryErr}, true)
		}
		m.addNoteRow(summary, false)
		runSave := func(start bool) (tea.Model, tea.Cmd) {
			if err := d.Validate(); err != nil {
				w.summaryErr = err.Error()
				m.openDeployStep(4)
				return m, nil
			}
			m.busy = true
			m.modal = srvModalNone
			m.statusToast = "正在生成证书与初始化部署…"
			cfgPath := m.opts.ConfigPath
			return m, func() tea.Msg {
				if _, err := setup.InitializeDeployment(d); err != nil {
					return deployInitDoneMsg{d: d, err: err}
				}
				msg := "部署配置与证书已保存到 " + d.Config
				started := false
				if start {
					exe, err := os.Executable()
					if err == nil {
						ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
						_, err = service.Start(ctx, exe, cfgPath)
						cancel()
					}
					if err != nil {
						msg += "\n服务启动未完成：" + err.Error()
					} else {
						started = true
						msg += "\n后台 sftd 服务已启动就绪。"
					}
				}
				return deployInitDoneMsg{d: d, started: started, notice: msg}
			}
		}
		m.formButtons = []formButton{
			{label: "◀ 上一步", onClick: func() (tea.Model, tea.Cmd) {
				if w.tuneLimits {
					m.openDeployStep(35)
				} else {
					m.openDeployStep(3)
				}
				return m, textinput.Blink
			}},
			{label: "仅保存配置", onClick: func() (tea.Model, tea.Cmd) { return runSave(false) }},
			{label: "保存并启动 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) { return runSave(true) }},
		}
		m.finalizeFormFocus(false)

	case 5:
		lines := strings.Split(w.finishMsg, "\n")
		lines = append(lines, "可将以下目录导出发给对应用户（含 CA 与用户私钥，不含 CA/服务端私钥），客户端按 [i] 导入。")
		m.addNoteRow(lines, false)
		expIdx := m.addTextRow("导出用户分发包目录", w.exportDir)
		m.formButtons = []formButton{
			{label: "完成返回", primary: true, onClick: func() (tea.Model, tea.Cmd) {
				w.exportDir = strings.TrimSpace(m.formInputs[expIdx].Value())
				m.modal = srvModalNone
				m.deploy = nil
				return m, tea.Batch(m.loadConfigCmd(), m.pollRuntimeCmd())
			}},
			{label: "启动/检查服务", onClick: func() (tea.Model, tea.Cmd) {
				w.exportDir = strings.TrimSpace(m.formInputs[expIdx].Value())
				m.modal = srvModalNone
				return m, m.startServiceCmd()
			}},
			{label: "立即导出用户包", onClick: func() (tea.Model, tea.Cmd) {
				out := strings.TrimSpace(m.formInputs[expIdx].Value())
				if out == "" {
					m.statusToast = "请填写导出目标目录"
					return m, nil
				}
				w.exportDir = out
				d := w.d
				m.busy = true
				return m, func() tea.Msg {
					res, err := certmanage.Execute("export", certmanage.Options{
						Dir:             d.CertDir,
						ServerConfig:    d.Config,
						ClientConfigDir: d.ClientDir,
						Name:            d.User,
						Out:             out,
						Days:            d.Days,
					})
					return deployExportDoneMsg{outDir: out, msgs: res.Messages, err: err}
				}
			}},
		}
		m.finalizeFormFocus(true)
	}
}

func (m *serverModel) openEditSettingsModal() {
	if m.cfgErr != nil {
		m.statusToast = "请先加载或初始化有效的服务端配置"
		return
	}
	draft := m.cfg
	m.settingsExpected = m.cfg
	m.settingsDraft = &draft
	m.rebuildBasicSettingsForm()
}

func (m *serverModel) rebuildBasicSettingsForm() {
	c := m.settingsDraft
	if c == nil {
		return
	}
	m.resetForm()
	m.modal = srvModalEditSettings
	m.addNoteRow([]string{
		"说明：除全局总带宽可通过 [A] 热加载外，其余设置修改保存后需重启 sftd 生效。",
	}, false)
	listenIdx := m.addTextRow("监听地址 (listen)", c.Listen)
	storageIdx := m.addTextRow("存储根目录 (storage_root)", c.StorageRoot)
	upIdx := m.addSizeUnitRow("默认上传带宽", c.Bandwidth.UploadBytesPerSecond, true)
	dnIdx := m.addSizeUnitRow("默认下载带宽", c.Bandwidth.DownloadBytesPerSecond, true)
	burIdx := m.addSizeUnitRow("默认突发配额", c.Bandwidth.BurstBytes, false)
	maxIdx := m.addSizeUnitRow("单文件大小上限", c.Resources.MaxFileBytes, false)
	quotaIdx := m.addSizeUnitRow("每用户存储上限", c.Resources.UserStorageBytes, false)
	logIdx := m.addSizeUnitRow("日志单文件上限 (64 KiB–1 GiB)", c.Logging.MaxFileBytes, false)

	useGlobal := c.GlobalBandwidth != nil
	gb := config.Bandwidth{
		UploadBytesPerSecond:   c.Bandwidth.UploadBytesPerSecond * 2,
		DownloadBytesPerSecond: c.Bandwidth.DownloadBytesPerSecond * 2,
		BurstBytes:             c.Bandwidth.BurstBytes * 2,
	}
	if c.GlobalBandwidth != nil {
		gb = *c.GlobalBandwidth
	}
	var gUpIdx, gDnIdx, gBurIdx int
	if useGlobal {
		gUpIdx = m.addSizeUnitRow("全局总上传带宽", gb.UploadBytesPerSecond, true)
		gDnIdx = m.addSizeUnitRow("全局总下载带宽", gb.DownloadBytesPerSecond, true)
		gBurIdx = m.addSizeUnitRow("全局总带宽突发量", gb.BurstBytes, false)
	}

	captureBasic := func(strict bool) error {
		c.Listen = strings.TrimSpace(m.formInputs[listenIdx].Value())
		c.StorageRoot = strings.TrimSpace(m.formInputs[storageIdx].Value())
		if strict {
			if _, _, err := net.SplitHostPort(c.Listen); err != nil {
				return fmt.Errorf("监听地址须为 IP:端口")
			}
			if c.StorageRoot == "" {
				return fmt.Errorf("存储根目录不能为空")
			}
		}
		pairs := []struct {
			idx    int
			label  string
			target *uint64
		}{
			{upIdx, "默认上传带宽", &c.Bandwidth.UploadBytesPerSecond},
			{dnIdx, "默认下载带宽", &c.Bandwidth.DownloadBytesPerSecond},
			{burIdx, "默认突发配额", &c.Bandwidth.BurstBytes},
			{maxIdx, "单文件上限", &c.Resources.MaxFileBytes},
			{quotaIdx, "用户存储上限", &c.Resources.UserStorageBytes},
			{logIdx, "日志上限", &c.Logging.MaxFileBytes},
		}
		for _, p := range pairs {
			v, err := m.parseSizeInput(p.idx, p.label)
			if err != nil {
				if strict {
					return err
				}
			} else {
				*p.target = v
			}
		}
		if useGlobal {
			for _, gp := range []struct {
				idx    int
				label  string
				target *uint64
			}{
				{gUpIdx, "全局上传带宽", &gb.UploadBytesPerSecond},
				{gDnIdx, "全局下载带宽", &gb.DownloadBytesPerSecond},
				{gBurIdx, "全局突发配额", &gb.BurstBytes},
			} {
				v, err := m.parseSizeInput(gp.idx, gp.label)
				if err != nil {
					if strict {
						return err
					}
				} else {
					*gp.target = v
				}
			}
			copyGB := gb
			c.GlobalBandwidth = &copyGB
		} else {
			c.GlobalBandwidth = nil
		}
		return nil
	}

	m.addToggleRow("启用全局总带宽限速 (global_bandwidth)", useGlobal, func(v bool) {
		_ = captureBasic(false)
		if v {
			copyGB := gb
			c.GlobalBandwidth = &copyGB
		} else {
			c.GlobalBandwidth = nil
		}
		focus := m.formFocus
		m.rebuildBasicSettingsForm()
		m.setRowFocus(min(focus, len(m.formRows)-1), 0)
	})

	m.formButtons = []formButton{
		{label: "取消", onClick: func() (tea.Model, tea.Cmd) {
			m.modal = srvModalNone
			m.settingsDraft = nil
			return m, nil
		}},
		{label: "下一步：协议与资源限额 ▶", onClick: func() (tea.Model, tea.Cmd) {
			if err := captureBasic(true); err != nil {
				m.statusToast = err.Error()
				return m, nil
			}
			m.openAdvancedSettingsModal()
			return m, textinput.Blink
		}},
		{label: "直接保存 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) {
			if err := captureBasic(true); err != nil {
				m.statusToast = err.Error()
				return m, nil
			}
			return m.saveServerSettingsCmd(*c)
		}},
	}
	m.finalizeFormFocus(true)
}

func (m *serverModel) openAdvancedSettingsModal() {
	if m.settingsDraft == nil {
		if m.cfgErr != nil {
			m.statusToast = "请先加载有效的服务端配置"
			return
		}
		draft := m.cfg
		m.settingsExpected = m.cfg
		m.settingsDraft = &draft
	}
	c := m.settingsDraft
	m.resetForm()
	m.modal = srvModalEditAdvancedSettings
	m.addNoteRow([]string{
		"高级协议、超时与资源限额（↑↓/Tab/鼠标浏览完整 18 项配置，全部项保存后重启服务生效）：",
	}, false)

	fields := configAdvancedFields(c)
	idxMap := make([]int, len(fields))
	isSize := make([]bool, len(fields))
	for i, f := range fields {
		_, key, _ := strings.Cut(f.Path, ".")
		if key == "chunk_size" || key == "window_size" {
			isSize[i] = true
			idxMap[i] = m.addSizeUnitRow(settingLabel(key), f.Uint(), false)
		} else {
			idxMap[i] = m.addTextRow(settingLabel(key), strconv.FormatUint(f.Uint(), 10))
		}
	}

	applyAdv := func() error {
		for i, f := range fields {
			if isSize[i] {
				v, err := m.parseSizeInput(idxMap[i], f.Path)
				if err != nil {
					return err
				}
				if err := f.Set(strconv.FormatUint(v, 10)); err != nil {
					return err
				}
			} else {
				if err := f.Set(m.formInputs[idxMap[i]].Value()); err != nil {
					return err
				}
			}
		}
		return c.ValidateDraft()
	}

	m.formButtons = []formButton{
		{label: "◀ 返回基础设置", onClick: func() (tea.Model, tea.Cmd) {
			_ = applyAdv()
			m.rebuildBasicSettingsForm()
			return m, textinput.Blink
		}},
		{label: "保存全部设置 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) {
			if err := applyAdv(); err != nil {
				m.statusToast = err.Error()
				return m, nil
			}
			return m.saveServerSettingsCmd(*c)
		}},
	}
	m.finalizeFormFocus(true)
}

func (m *serverModel) openLogLimitModal() {
	if m.cfgErr != nil {
		m.statusToast = "请先加载有效配置"
		return
	}
	m.resetForm()
	m.modal = srvModalEditLogLimit
	m.addNoteRow([]string{
		"日志单文件大小上限 (64 KiB–1 GiB)；写满后自动清空旧内容重用当前文件，保存后需重启生效。",
	}, false)
	logIdx := m.addSizeUnitRow("日志文件大小上限", m.cfg.Logging.MaxFileBytes, false)
	m.formButtons = []formButton{
		{label: "取消", onClick: func() (tea.Model, tea.Cmd) {
			m.modal = srvModalNone
			return m, nil
		}},
		{label: "保存上限 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) {
			v, err := m.parseSizeInput(logIdx, "日志文件大小上限")
			if err != nil {
				m.statusToast = err.Error()
				return m, nil
			}
			next := m.cfg
			next.Logging.MaxFileBytes = v
			m.settingsExpected = m.cfg
			return m.saveServerSettingsCmd(next)
		}},
	}
	m.finalizeFormFocus(true)
}

func (m *serverModel) saveServerSettingsCmd(proposed config.Server) (tea.Model, tea.Cmd) {
	if err := proposed.ValidateDraft(); err != nil {
		m.statusToast = err.Error()
		return m, nil
	}
	expected := m.settingsExpected
	certDir := filepath.Dir(expected.TLS.CACertificate)
	cfgPath := m.opts.ConfigPath
	gbChanged := (expected.GlobalBandwidth == nil) != (proposed.GlobalBandwidth == nil) ||
		(expected.GlobalBandwidth != nil && proposed.GlobalBandwidth != nil && *expected.GlobalBandwidth != *proposed.GlobalBandwidth)
	m.modal = srvModalNone
	m.settingsDraft = nil
	m.busy = true
	return m, func() tea.Msg {
		saved, err := certmanage.SaveSettings(cfgPath, certDir, expected, proposed)
		if err != nil {
			return serverActionResultMsg{err: err, refresh: true}
		}
		notice := "配置已保存到磁盘"
		if gbChanged {
			if applied, rErr := control.Reload(cfgPath, saved.StorageRoot); rErr == nil && applied {
				notice += "（全局总带宽已热重载生效）"
			}
		}
		if config.RestartRequired(expected, saved) {
			notice += "（含需重启生效的配置，请择机重启服务）"
		}
		return serverActionResultMsg{notice: notice, refresh: true, poll: true}
	}
}
