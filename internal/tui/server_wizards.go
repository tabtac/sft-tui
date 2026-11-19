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
		m.setToast("当前部署配置已存在，可在 [5 设置] 或 [2 用户] 中维护")
		return
	}
	if m.deploy == nil {
		base := m.opts.ExecutableDir
		if base == "" {
			base = "."
		}
		cfgPath := m.opts.ConfigPath
		if cfgPath != "" && !filepath.IsAbs(cfgPath) {
			if abs, err := filepath.Abs(cfgPath); err == nil {
				cfgPath = abs
			}
		}
		d := setup.DefaultDeployment(cfgPath, base)
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
	m.clearToast()
	m.modal = srvModalDeployWizard

	switch step {
	case 1:
		m.addNoteRow([]string{
			"第 1 步 / 4：本机演示使用回环地址；局域网部署选择网卡 IP 或填写实际 DNS，",
			"监听地址、证书 SAN 和客户端连接地址会自动联动。目标配置：" + truncPlainCell(w.d.Config, 50),
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
				m.setToast("部署向导输入已暂存，按 [I] 可继续")
				return m, nil
			}},
			{label: "下一步 ▶", primary: true, onClick: func() (tea.Model, tea.Cmd) {
				capture1()
				host, port, err := setup.ValidateAddress(w.d.Address)
				if err != nil {
					m.setToastSticky(err.Error())
					return m, m.focusInputRow(addrIdx)
				}
				if w.scene == 0 && host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
					m.setToastSticky("跨主机地址请将部署场景切换为「局域网 / 跨主机部署」")
					return m, m.focusSelectRow(sceneIdx)
				}
				if _, lport, err := net.SplitHostPort(w.d.Listen); err != nil || lport == "" {
					m.setToastSticky("监听地址格式无效，须为 IP:端口（如 0.0.0.0:7443）")
					return m, m.focusInputRow(listenIdx)
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
			"目标配置：" + truncPlainCell(w.d.Config, 56),
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
					m.setToastSticky("存储目录不能为空")
					return m, m.focusInputRow(stIdx)
				}
				if !filepath.IsAbs(w.d.Storage) {
					w.d.Storage = filepath.Join(w.base, w.d.Storage)
				}
				abs, err := filepath.Abs(w.d.Storage)
				if err != nil {
					m.setToastSticky(err.Error())
					return m, m.focusInputRow(stIdx)
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

		capture3 := func(validateNums bool) (int, error) {
			w.d.User = strings.TrimSpace(m.formInputs[userIdx].Value())
			if validateNums && !config.ValidUserPath(w.d.User) {
				return userIdx, fmt.Errorf("初始授权用户名无效（须为字母/数字/._-，且不能为系统保留名）")
			}
			if w.advanced {
				w.d.CertDir = strings.TrimSpace(m.formInputs[certIdx].Value())
				w.d.ClientDir = strings.TrimSpace(m.formInputs[cliIdx].Value())
				w.d.ServerName = strings.TrimSpace(m.formInputs[snameIdx].Value())
				w.d.Hosts = strings.TrimSpace(m.formInputs[sanIdx].Value())
				if validateNums {
					if w.d.CertDir == "" {
						return certIdx, fmt.Errorf("证书保存目录不能为空")
					}
					if w.d.ClientDir == "" {
						return cliIdx, fmt.Errorf("用户配置输出目录不能为空")
					}
					if w.d.ServerName == "" {
						return snameIdx, fmt.Errorf("客户端校验名称 (SAN) 不能为空")
					}
				}
				if d, err := parseStrictCertDays(m.formInputs[daysIdx].Value()); err == nil {
					w.d.Days = d
				} else if validateNums {
					return daysIdx, err
				}
				if cd, err := parseStrictCertDays(m.formInputs[caDaysIdx].Value()); err == nil {
					w.d.CADays = cd
				} else if validateNums {
					return caDaysIdx, fmt.Errorf("CA 证书%w", err)
				}
				if validateNums && w.d.CADays < w.d.Days {
					return caDaysIdx, fmt.Errorf("CA 有效天数 (%d) 不得小于服务端/用户证书有效天数 (%d)", w.d.CADays, w.d.Days)
				}
			}
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
						return p.idx, e
					}
				} else {
					*p.target = v
				}
			}
			return -1, nil
		}

		advToggle := m.addToggleRow("展开/调整高级证书、SAN 与目录设置", w.advanced, func(v bool) {
			_, _ = capture3(false)
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
				_, _ = capture3(false)
				m.openDeployStep(2)
				return m, textinput.Blink
			}},
			{label: "下一步 ▶", primary: true, onClick: func() (tea.Model, tea.Cmd) {
				if badIdx, err := capture3(true); err != nil {
					m.setToastSticky(err.Error())
					return m, m.focusInputRow(badIdx)
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
		burstIdx := m.addKeyedSizeUnitRow("bandwidth.burst_bytes", "默认单用户带宽突发量", w.d.Burst, false)
		logIdx := m.addKeyedSizeUnitRow("logging.max_file_bytes", "日志单文件上限 (64 KiB–1 GiB)", w.d.LogMaxBytes, false)
		var gUpIdx, gDnIdx, gBurIdx int
		if w.useGlobal {
			gUpIdx = m.addKeyedSizeUnitRow("global_bandwidth.upload", "全局总上传带宽", w.globalUp, true)
			gDnIdx = m.addKeyedSizeUnitRow("global_bandwidth.download", "全局总下载带宽", w.globalDown, true)
			gBurIdx = m.addKeyedSizeUnitRow("global_bandwidth.burst", "全局总带宽突发量", w.globalBur, false)
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
				advInputIdx[i] = m.addKeyedSizeUnitRow(f.Path, settingLabel(key), f.Uint(), false)
			} else {
				advInputIdx[i] = m.addKeyedTextRow(f.Path, settingLabel(key), strconv.FormatUint(f.Uint(), 10))
			}
		}

		capture35 := func(strict bool) (int, error) {
			if b, err := m.parseSizeInput(burstIdx, "默认突发量"); err == nil {
				w.d.Burst = b
			} else if strict {
				return burstIdx, err
			}
			if lb, err := m.parseSizeInput(logIdx, "日志单文件上限"); err == nil {
				w.d.LogMaxBytes = lb
			} else if strict {
				return logIdx, err
			}
			if w.useGlobal {
				if u, err := m.parseSizeInput(gUpIdx, "全局上传带宽"); err == nil {
					w.globalUp = u
				} else if strict {
					return gUpIdx, err
				}
				if dn, err := m.parseSizeInput(gDnIdx, "全局下载带宽"); err == nil {
					w.globalDown = dn
				} else if strict {
					return gDnIdx, err
				}
				if bu, err := m.parseSizeInput(gBurIdx, "全局突发量"); err == nil {
					w.globalBur = bu
				} else if strict {
					return gBurIdx, err
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
							return advInputIdx[i], err
						}
						continue
					}
					if err := f.Set(strconv.FormatUint(v, 10)); err != nil && strict {
						return advInputIdx[i], err
					}
				} else {
					if err := f.Set(m.formInputs[advInputIdx[i]].Value()); err != nil && strict {
						return advInputIdx[i], err
					}
				}
			}
			l, t, r := draft.Limits, draft.Timeouts, draft.Resources
			w.d.Limits, w.d.Timeouts, w.d.Resources = &l, &t, &r
			if strict {
				full := config.DefaultServer()
				full.Bandwidth = config.Bandwidth{
					UploadBytesPerSecond:   w.d.Upload,
					DownloadBytesPerSecond: w.d.Download,
					BurstBytes:             w.d.Burst,
				}
				full.GlobalBandwidth = w.d.GlobalBandwidth
				full.Logging.MaxFileBytes = w.d.LogMaxBytes
				full.Limits = draft.Limits
				full.Timeouts = draft.Timeouts
				full.Resources = draft.Resources
				full.Resources.MaxFileBytes = w.d.MaxFile
				full.Resources.UserStorageBytes = w.d.UserStorage
				full.TLS = config.TLSFiles{CACertificate: "ca.pem", Certificate: "server.pem", PrivateKey: "server-key.pem"}
				full.AuthorizedClients = []config.AuthorizedClient{{UserID: "user", Certificate: "user.pem"}}
				if err := full.ValidateDraft(); err != nil {
					return m.settingsValidationFocus(full, err, burstIdx), err
				}
			}
			return -1, nil
		}

		m.addToggleRow("启用全局总带宽限速 (global_bandwidth)", w.useGlobal, func(v bool) {
			_, _ = capture35(false)
			w.useGlobal = v
			focusRow := m.formFocus
			m.openDeployStep(35)
			m.setRowFocus(min(focusRow, len(m.formRows)-1), 0)
		})

		m.formButtons = []formButton{
			{label: "◀ 返回第 3 步", onClick: func() (tea.Model, tea.Cmd) {
				_, _ = capture35(false)
				m.openDeployStep(3)
				return m, textinput.Blink
			}},
			{label: "下一步：部署摘要 ▶", primary: true, onClick: func() (tea.Model, tea.Cmd) {
				if badIdx, err := capture35(true); err != nil {
					m.setToastSticky(err.Error())
					return m, m.focusInputRow(badIdx)
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
		if d.Limits != nil && d.Timeouts != nil && d.Resources != nil {
			summary = append(summary,
				fmt.Sprintf("高级流控与资源：每连接流=%d · 分块=%s · 窗口=%s · 最大连接=%d · 并发握手=%d",
					d.Limits.MaxStreams, progress.FormatBytes(uint64(d.Limits.ChunkSize)),
					progress.FormatBytes(uint64(d.Limits.WindowSize)), d.Resources.MaxConnections, d.Resources.MaxHandshakes),
				fmt.Sprintf("突发/日志/超时：默认突发 %s · 日志上限 %s · 握手=%ds / 空闲=%ds",
					progress.FormatBytes(d.Burst), progress.FormatBytes(d.LogMaxBytes),
					d.Timeouts.HandshakeSeconds, d.Timeouts.IdleSeconds),
			)
		}
		if w.summaryErr != "" {
			m.addNoteRow([]string{"保存失败：" + w.summaryErr}, true)
		}
		m.addNoteRow(summary, false)
		runSave := func(start bool) (tea.Model, tea.Cmd) {
			if m.rejectIfBusy() {
				return m, nil
			}
			if err := d.Validate(); err != nil {
				w.summaryErr = err.Error()
				m.openDeployStep(4)
				return m, nil
			}
			m.startBusy("初始化部署与证书")
			m.modal = srvModalNone
			m.setToastSticky("正在生成证书与初始化部署…")
			cfgPath := d.Config
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
				if m.statusToast != "" {
					m.setToast(m.statusToast)
				}
				return m, tea.Batch(m.loadConfigCmd(), m.pollRuntimeCmd())
			}},
			{label: "启动/检查服务", onClick: func() (tea.Model, tea.Cmd) {
				w.exportDir = strings.TrimSpace(m.formInputs[expIdx].Value())
				m.modal = srvModalNone
				return m, m.startServiceCmd()
			}},
			{label: "立即导出用户包", onClick: func() (tea.Model, tea.Cmd) {
				if m.rejectIfBusy() {
					return m, nil
				}
				out := strings.TrimSpace(m.formInputs[expIdx].Value())
				if out == "" {
					m.setToastSticky("请填写导出目标目录")
					return m, nil
				}
				w.exportDir = out
				d := w.d
				m.startBusy("导出用户分发包")
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
		m.setToast("请先加载或初始化有效的服务端配置")
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
	m.clearToast()
	m.modal = srvModalEditSettings
	m.addNoteRow([]string{
		"提示：在此修改基础网络、存储、带宽与日志单文件上限；保存后将告知生效方式。",
	}, false)
	listenIdx := m.addKeyedTextRow("listen", "监听地址", c.Listen)
	storageIdx := m.addKeyedTextRow("storage_root", "存储根目录", c.StorageRoot)
	upIdx := m.addKeyedSizeUnitRow("bandwidth.upload_bytes_per_second", "默认上传带宽", c.Bandwidth.UploadBytesPerSecond, true)
	dnIdx := m.addKeyedSizeUnitRow("bandwidth.download_bytes_per_second", "默认下载带宽", c.Bandwidth.DownloadBytesPerSecond, true)
	burIdx := m.addKeyedSizeUnitRow("bandwidth.burst_bytes", "默认突发配额", c.Bandwidth.BurstBytes, false)
	maxIdx := m.addKeyedSizeUnitRow("resources.max_file_bytes", "单文件大小上限", c.Resources.MaxFileBytes, false)
	quotaIdx := m.addKeyedSizeUnitRow("resources.user_storage_bytes", "每用户存储上限", c.Resources.UserStorageBytes, false)
	logIdx := m.addKeyedSizeUnitRow("logging.max_file_bytes", "日志单文件上限 (64 KiB–1 GiB)", c.Logging.MaxFileBytes, false)

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
		gUpIdx = m.addKeyedSizeUnitRow("global_bandwidth.upload", "全局总上传带宽", gb.UploadBytesPerSecond, true)
		gDnIdx = m.addKeyedSizeUnitRow("global_bandwidth.download", "全局总下载带宽", gb.DownloadBytesPerSecond, true)
		gBurIdx = m.addKeyedSizeUnitRow("global_bandwidth.burst", "全局总带宽突发量", gb.BurstBytes, false)
	}

	captureBasic := func(strict bool) (int, error) {
		c.Listen = strings.TrimSpace(m.formInputs[listenIdx].Value())
		c.StorageRoot = strings.TrimSpace(m.formInputs[storageIdx].Value())
		if strict {
			if _, _, err := net.SplitHostPort(c.Listen); err != nil {
				return listenIdx, fmt.Errorf("监听地址须为 IP:端口")
			}
			if c.StorageRoot == "" {
				return storageIdx, fmt.Errorf("存储根目录不能为空")
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
					return p.idx, err
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
						return gp.idx, err
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
		if strict {
			if c.Logging.MaxFileBytes < 64<<10 || c.Logging.MaxFileBytes > 1<<30 {
				return logIdx, fmt.Errorf("日志单文件上限须在 64 KiB–1 GiB 之间")
			}
			if err := c.ValidateDraft(); err != nil {
				return m.settingsValidationFocus(*c, err, upIdx), err
			}
		}
		return -1, nil
	}

	m.addToggleRow("启用全局总带宽限速", useGlobal, func(v bool) {
		_, _ = captureBasic(false)
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
			m.clearToast()
			return m, nil
		}},
		{label: "下一步：协议与资源限额 ▶", onClick: func() (tea.Model, tea.Cmd) {
			if badIdx, err := captureBasic(true); err != nil {
				m.setToastSticky(err.Error())
				return m, m.focusInputRow(badIdx)
			}
			m.openAdvancedSettingsModal()
			return m, textinput.Blink
		}},
		{label: "直接保存 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) {
			if badIdx, err := captureBasic(true); err != nil {
				m.setToastSticky(err.Error())
				return m, m.focusInputRow(badIdx)
			}
			return m.saveServerSettingsCmd(*c)
		}},
	}
	m.finalizeFormFocus(true)
}

func (m *serverModel) openAdvancedSettingsModal() {
	if m.settingsDraft == nil {
		if m.cfgErr != nil {
			m.setToast("请先加载有效的服务端配置")
			return
		}
		draft := m.cfg
		m.settingsExpected = m.cfg
		m.settingsDraft = &draft
	}
	c := m.settingsDraft
	m.resetForm()
	m.clearToast()
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
			idxMap[i] = m.addKeyedSizeUnitRow(f.Path, settingLabel(key), f.Uint(), false)
		} else {
			idxMap[i] = m.addKeyedTextRow(f.Path, settingLabel(key), strconv.FormatUint(f.Uint(), 10))
		}
	}

	applyAdv := func() (int, error) {
		for i, f := range fields {
			if isSize[i] {
				v, err := m.parseSizeInput(idxMap[i], f.Path)
				if err != nil {
					return idxMap[i], err
				}
				if err := f.Set(strconv.FormatUint(v, 10)); err != nil {
					return idxMap[i], err
				}
			} else {
				if err := f.Set(m.formInputs[idxMap[i]].Value()); err != nil {
					return idxMap[i], err
				}
			}
		}
		if err := c.ValidateDraft(); err != nil {
			return m.settingsValidationFocus(*c, err, idxMap[0]), err
		}
		return -1, nil
	}

	m.formButtons = []formButton{
		{label: "◀ 返回基础设置", onClick: func() (tea.Model, tea.Cmd) {
			_, _ = applyAdv()
			m.rebuildBasicSettingsForm()
			return m, textinput.Blink
		}},
		{label: "保存全部设置 ★", primary: true, onClick: func() (tea.Model, tea.Cmd) {
			if badIdx, err := applyAdv(); err != nil {
				m.setToastSticky(err.Error())
				return m, m.focusInputRow(badIdx)
			}
			return m.saveServerSettingsCmd(*c)
		}},
	}
	m.finalizeFormFocus(true)
}

func (m *serverModel) saveServerSettingsCmd(proposed config.Server) (tea.Model, tea.Cmd) {
	if m.rejectIfBusy() {
		return m, nil
	}
	if err := proposed.ValidateDraft(); err != nil {
		m.setToastSticky(err.Error())
		return m, nil
	}
	expected := m.settingsExpected
	certDir := filepath.Dir(expected.TLS.CACertificate)
	cfgPath := m.opts.ConfigPath
	gbChanged := (expected.GlobalBandwidth == nil) != (proposed.GlobalBandwidth == nil) ||
		(expected.GlobalBandwidth != nil && proposed.GlobalBandwidth != nil && *expected.GlobalBandwidth != *proposed.GlobalBandwidth)
	m.clearToast()
	m.modal = srvModalNone
	m.settingsDraft = nil
	m.startBusy("保存服务端配置")
	return m, func() tea.Msg {
		saved, err := certmanage.SaveSettings(cfgPath, certDir, expected, proposed)
		if err != nil {
			return serverActionResultMsg{err: err, refresh: true}
		}
		notice := "配置已保存到磁盘"
		if gbChanged {
			applied, rErr := control.Reload(cfgPath, saved.StorageRoot)
			switch {
			case rErr != nil:
				notice += "；但热重载运行中服务失败：" + rErr.Error()
			case applied:
				notice += "（全局总带宽已热重载生效）"
			}
		}
		if config.RestartRequired(expected, saved) {
			notice += "（含需重启生效的配置，请择机重启服务）"
		}
		return serverActionResultMsg{notice: notice, refresh: true, poll: true}
	}
}

func (m *serverModel) settingsValidationFocus(c config.Server, err error, fallbackIdx int) int {
	msg := err.Error()
	pick := func(key string) (int, bool) {
		return m.inputByKey(key)
	}
	switch {
	case strings.Contains(msg, "全局带宽"):
		if c.GlobalBandwidth != nil {
			switch {
			case c.GlobalBandwidth.UploadBytesPerSecond == 0:
				if idx, ok := pick("global_bandwidth.upload"); ok {
					return idx
				}
			case c.GlobalBandwidth.DownloadBytesPerSecond == 0:
				if idx, ok := pick("global_bandwidth.download"); ok {
					return idx
				}
			default:
				if idx, ok := pick("global_bandwidth.burst"); ok {
					return idx
				}
			}
		}
	case strings.Contains(msg, "突发量"), strings.Contains(msg, "带宽必须为正"):
		switch {
		case c.Bandwidth.UploadBytesPerSecond == 0:
			if idx, ok := pick("bandwidth.upload_bytes_per_second"); ok {
				return idx
			}
		case c.Bandwidth.DownloadBytesPerSecond == 0:
			if idx, ok := pick("bandwidth.download_bytes_per_second"); ok {
				return idx
			}
		default:
			if idx, ok := pick("bandwidth.burst_bytes"); ok {
				return idx
			}
		}
	case strings.Contains(msg, "日志文件大小"):
		if idx, ok := pick("logging.max_file_bytes"); ok {
			return idx
		}
	case strings.Contains(msg, "分块"), strings.Contains(msg, "窗口"):
		if c.Limits.MaxStreams <= 0 {
			if idx, ok := pick("limits.max_streams"); ok {
				return idx
			}
		}
		if c.Limits.WindowSize < c.Limits.ChunkSize || c.Limits.WindowSize <= 0 {
			if idx, ok := pick("limits.window_size"); ok {
				return idx
			}
		}
		if idx, ok := pick("limits.chunk_size"); ok {
			return idx
		}
	case strings.Contains(msg, "超时"):
		checks := []struct {
			bad bool
			key string
		}{
			{c.Timeouts.HandshakeSeconds <= 0, "timeouts.handshake_seconds"},
			{c.Timeouts.HelloSeconds <= 0, "timeouts.hello_seconds"},
			{c.Timeouts.FrameReadSeconds <= 0, "timeouts.frame_read_seconds"},
			{c.Timeouts.WriteSeconds <= 0, "timeouts.write_seconds"},
			{c.Timeouts.IdleSeconds <= 0, "timeouts.idle_seconds"},
		}
		for _, ch := range checks {
			if ch.bad {
				if idx, ok := pick(ch.key); ok {
					return idx
				}
			}
		}
	default:
		checks := []struct {
			bad bool
			key string
		}{
			{strings.TrimSpace(c.Listen) == "", "listen"},
			{strings.TrimSpace(c.StorageRoot) == "", "storage_root"},
			{c.Resources.MaxFileBytes == 0, "resources.max_file_bytes"},
			{c.Resources.UserStorageBytes < c.Resources.MaxFileBytes || c.Resources.UserStorageBytes == 0, "resources.user_storage_bytes"},
			{c.Resources.MaxConnections <= 0, "resources.max_connections"},
			{c.Resources.MaxHandshakes <= 0 || c.Resources.MaxHandshakes > c.Resources.MaxConnections, "resources.max_handshakes"},
			{c.Resources.MaxUnauthenticatedPerIP <= 0, "resources.max_unauthenticated_per_ip"},
			{c.Resources.MaxConnectionsPerUser <= 0, "resources.max_connections_per_user"},
			{c.Resources.MaxStreamsPerUser <= 0, "resources.max_streams_per_user"},
			{c.Resources.MaxPendingTransfersPerUser <= 0, "resources.max_pending_transfers_per_user"},
			{c.Resources.MaxFilesPerUser <= 0, "resources.max_files_per_user"},
			{c.Resources.MaxNewTasksPerMinute <= 0, "resources.max_new_tasks_per_minute"},
			{c.Resources.MaxUploadCheckpointsPerSecond <= 0, "resources.max_upload_checkpoints_per_second"},
			{c.Resources.TransferRetentionSeconds <= 0, "resources.transfer_retention_seconds"},
		}
		for _, ch := range checks {
			if ch.bad {
				if idx, ok := pick(ch.key); ok {
					return idx
				}
			}
		}
	}
	return fallbackIdx
}
