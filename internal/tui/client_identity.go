package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"sft-tui/internal/config"
	"sft-tui/internal/identity"
	"sft-tui/internal/queue"
	"sft-tui/internal/security"
	"sft-tui/internal/transfer"
)

func (m *model) initIdentityCmd() tea.Cmd {
	opts := m.opts
	return func() tea.Msg {
		if opts.ConfigPath == "" && opts.BundleDir == "" {
			if cfg, activeID, customLabel, err := identity.ActiveProfileInfo(opts.ExecutableDir); err == nil {
				res := openIdentityStack(cfg, customLabel)
				res.activeID = activeID
				return res
			} else if !os.IsNotExist(err) {
				return identityActivatedMsg{
					stage: identityStageLookup,
					err:   err,
				}
			}
		}
		wd, _ := os.Getwd()
		cfg, err := identity.Resolve(opts.ExecutableDir, wd, opts.BundleDir, opts.ConfigPath, false, nil)
		if err != nil {
			return identityActivatedMsg{
				stage: identityStageLookup,
				err:   err,
			}
		}
		return openIdentityStack(cfg, "")
	}
}

func identityDisplayLabel(cfg config.Client, customLabel string) string {
	if customLabel != "" {
		return customLabel
	}
	user := cfg.ServerName
	if cert, err := security.ReadCertificate(cfg.TLS.Certificate); err == nil && cert.Subject.CommonName != "" {
		user = cert.Subject.CommonName
	}
	if user == "" && cfg.ServerAddress == "" {
		return ""
	}
	return user + "@" + cfg.ServerAddress
}

func identityStageStatusText(stage identityInitStage) string {
	switch stage {
	case identityStageTLS:
		return "TLS 证书初始化失败"
	case identityStageQueue:
		return "队列初始化失败"
	default:
		return "身份配置加载失败"
	}
}

func isMissingIdentityErr(err error) bool {
	return errors.Is(err, identity.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

func openIdentityStack(cfg config.Client, customLabel string) identityActivatedMsg {
	label := identityDisplayLabel(cfg, customLabel)
	if _, err := security.ClientTLS(cfg); err != nil {
		return identityActivatedMsg{
			cfg:    cfg,
			hasCfg: true,
			label:  label,
			stage:  identityStageTLS,
			err:    fmt.Errorf("TLS 证书初始化失败：%w", err),
		}
	}
	idKey, err := security.ClientIdentityKey(cfg)
	if err != nil {
		return identityActivatedMsg{
			cfg:    cfg,
			hasCfg: true,
			label:  label,
			stage:  identityStageTLS,
			err:    fmt.Errorf("TLS 证书初始化失败：%w", err),
		}
	}
	legacyKey, _ := security.LegacyClientIdentityKey(cfg)
	mgr, err := queue.Open(queue.Config{
		StateDir:       cfg.StateDir,
		Identity:       idKey,
		LegacyIdentity: legacyKey,
		MaxFileBytes:   cfg.MaxFileBytes,
		MaxStreams:     cfg.Limits.MaxStreams,
	})
	if err != nil {
		return identityActivatedMsg{
			cfg:    cfg,
			hasCfg: true,
			label:  label,
			stage:  identityStageQueue,
			err:    fmt.Errorf("队列初始化失败：%w", err),
		}
	}
	runner := transfer.NewQueueRunner(cfg, mgr)
	mgr.SetRunner(runner.Run)

	return identityActivatedMsg{
		cfg:     cfg,
		hasCfg:  true,
		label:   label,
		idKey:   idKey,
		manager: mgr,
		runner:  runner,
	}
}

func (m *model) scanIdentitiesCmd() tea.Cmd {
	exeDir := m.opts.ExecutableDir
	return func() tea.Msg {
		wd, _ := os.Getwd()
		saved, _ := identity.Profiles(exeDir)
		discovered, err := identity.Discover(exeDir, wd, "")
		seenPath := make(map[string]bool)
		var out []identityChoice
		for _, p := range saved {
			seenPath[p.Path] = true
			out = append(out, identityChoice{profile: p, imported: true})
		}
		for _, path := range discovered {
			if seenPath[path] {
				continue
			}
			if prof, e := identity.Preview(path, wd); e == nil {
				out = append(out, identityChoice{profile: prof, imported: false})
			}
		}
		return identitiesScannedMsg{choices: out, err: err}
	}
}

func (m *model) unfinishedTaskCounts() (active, paused int) {
	if m.manager == nil {
		return 0, 0
	}
	m.refreshQueueSnapshots()
	for _, t := range m.activeTasks {
		if t.State.Active() {
			active++
		} else if !t.Terminal() {
			paused++
		}
	}
	return active, paused
}

func (m *model) beginIdentitySwitch(ch identityChoice, customPath string, bypassConfirm bool) tea.Cmd {
	if m.activatingID {
		m.setToastSticky("正在切换或导入身份，请稍候…")
		return nil
	}
	targetLabel := customPath
	if targetLabel == "" {
		targetLabel = ch.profile.Label
	}
	if !bypassConfirm {
		activeN, pausedN := m.unfinishedTaskCounts()
		if customPath != "" {
			if info, err := os.Stat(customPath); err == nil && info.IsDir() {
				// 目录可能含多个配置，先扫描（多配置或存在未完成任务时不直接静默切换）
				m.activatingID = true
				m.opLabel = "正在扫描身份目录"
				m.opStart = time.Now()
				m.setToastSticky("正在扫描目录中的身份配置…")
				return m.discoverOrActivatePathCmd(customPath, activeN+pausedN > 0)
			}
		}
		if activeN+pausedN > 0 {
			m.pendingSwitch = &identitySwitchConfirm{
				choice:      ch,
				customPath:  customPath,
				targetLabel: targetLabel,
				activeCount: activeN,
				pausedCount: pausedN,
			}
			m.clearToast()
			return nil
		}
	}
	m.pendingSwitch = nil
	m.activatingID = true
	if customPath != "" {
		m.opLabel = "正在导入身份"
		m.setToastSticky("正在校验并导入身份…")
	} else {
		m.opLabel = "正在切换身份"
		m.setToastSticky("正在切换身份…")
	}
	m.opStart = time.Now()
	return m.activateChoiceCmd(ch, customPath)
}

func (m *model) discoverOrActivatePathCmd(customPath string, confirmSingle bool) tea.Cmd {
	exeDir := m.opts.ExecutableDir
	actCustom := m.activateChoiceCmd(identityChoice{}, customPath)
	return func() tea.Msg {
		wd, _ := os.Getwd()
		candidates, discErr := identity.Discover(exeDir, wd, customPath)
		if discErr != nil {
			return identityActivatedMsg{stage: identityStageLookup, err: discErr}
		}
		if len(candidates) == 0 {
			return identityActivatedMsg{stage: identityStageLookup, err: fmt.Errorf("目录中未找到有效的客户端配置：%s", customPath)}
		}
		if len(candidates) > 1 || confirmSingle {
			var found []identityChoice
			for _, p := range candidates {
				if prof, e := identity.Preview(p, wd); e == nil {
					found = append(found, identityChoice{profile: prof, imported: false})
				}
			}
			if len(found) == 0 {
				return identityActivatedMsg{stage: identityStageLookup, err: fmt.Errorf("目录中的客户端配置均无法解析：%s", customPath)}
			}
			return identityDirDiscoveredMsg{dir: customPath, choices: found}
		}
		return actCustom()
	}
}

func (m *model) testIdentityConnCmd(ch identityChoice) tea.Cmd {
	path := ch.profile.Path
	cfg := ch.profile.Config
	return func() tea.Msg {
		if path != "" {
			wd, _ := os.Getwd()
			if prof, err := identity.Preview(path, wd); err == nil {
				cfg = prof.Config
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		cli, err := transfer.Dial(ctx, cfg)
		elapsed := time.Since(start)
		if cli != nil {
			_ = cli.Close()
		}
		return identityConnTestMsg{path: path, latency: elapsed, err: err}
	}
}

func sameClientDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil {
		absA = filepath.Clean(a)
	}
	if errB != nil {
		absB = filepath.Clean(b)
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(absA, absB)
	}
	return absA == absB
}

func (m *model) activateChoiceCmd(ch identityChoice, customPath string) tea.Cmd {
	oldMgr := m.manager
	oldRunner := m.runner
	oldIDKey := m.idKey
	oldStateDir := ""
	if m.cfg != nil {
		oldStateDir = m.cfg.StateDir
	}
	exeDir := m.opts.ExecutableDir
	return func() tea.Msg {
		wd, _ := os.Getwd()
		var (
			prof   identity.Profile
			useSel bool
			err    error
		)
		switch {
		case customPath != "":
			info, statErr := os.Stat(customPath)
			if statErr != nil {
				return identityActivatedMsg{stage: identityStageLookup, err: statErr}
			}
			target := customPath
			if info.IsDir() {
				candidates, discErr := identity.Discover(exeDir, wd, customPath)
				if discErr != nil {
					return identityActivatedMsg{stage: identityStageLookup, err: discErr}
				}
				if len(candidates) == 0 {
					return identityActivatedMsg{stage: identityStageLookup, err: fmt.Errorf("目录中未找到有效的客户端配置：%s", customPath)}
				}
				if len(candidates) > 1 {
					return identityActivatedMsg{stage: identityStageLookup, err: fmt.Errorf("目录中存在多个身份配置（%d 个），请直接指定配置文件路径", len(candidates))}
				}
				target = candidates[0]
			}
			prof, err = identity.PrepareImport(exeDir, wd, target)
			if err != nil {
				return identityActivatedMsg{stage: identityStageLookup, err: err}
			}
		case ch.imported:
			prof = ch.profile
			if _, err = identity.Preview(prof.Path, wd); err != nil {
				return identityActivatedMsg{cfg: prof.Config, hasCfg: prof.Label != "", label: prof.Label, stage: identityStageLookup, err: err}
			}
			useSel = true
		default:
			stagedLabel := strings.TrimSpace(ch.profile.Label)
			prof, err = identity.PrepareImport(exeDir, wd, ch.profile.Path)
			if err != nil {
				return identityActivatedMsg{stage: identityStageLookup, err: err}
			}
			if stagedLabel != "" {
				prof.Label = stagedLabel
			}
		}
		if _, err = security.ClientTLS(prof.Config); err != nil {
			return identityActivatedMsg{
				cfg:    prof.Config,
				hasCfg: true,
				label:  prof.Label,
				stage:  identityStageTLS,
				err:    fmt.Errorf("TLS 证书初始化失败：%w", err),
			}
		}

		// 先在不修改磁盘 active 的前提下校验目标身份、证书材料和标签元数据，
		// 避免后续打开新队列或静默旧队列失败时导致界面与磁盘 active 不一致。
		var cfg config.Client
		if useSel {
			cfg, err = identity.ValidateSelect(exeDir, prof.ID, prof.Label)
		} else {
			cfg, err = identity.ValidateSelectPrepared(exeDir, prof, prof.Label)
		}
		if err != nil {
			return identityActivatedMsg{
				cfg:    prof.Config,
				hasCfg: true,
				label:  prof.Label,
				stage:  identityStageLookup,
				err:    err,
			}
		}

		commitActive := func() error {
			if useSel {
				_, err := identity.Select(exeDir, prof.ID, prof.Label)
				return err
			}
			_, err := identity.SelectPrepared(exeDir, prof, prof.Label)
			return err
		}

		newIDKey, idErr := security.ClientIdentityKey(cfg)
		legacyKey, _ := security.LegacyClientIdentityKey(cfg)

		// 情况 1：新旧身份键不同（各自使用独立队列文件），先打开新引擎，确认可用且旧队列静默后再提交 active
		if idErr == nil && newIDKey != oldIDKey {
			if prof.SourceStateDir != "" && !sameClientDir(prof.SourceStateDir, cfg.StateDir) {
				if _, migErr := queue.MigrateUnfinished(prof.SourceStateDir, newIDKey, cfg.StateDir, newIDKey); migErr != nil {
					return identityActivatedMsg{
						cfg:    cfg,
						hasCfg: true,
						label:  prof.Label,
						stage:  identityStageQueue,
						err:    fmt.Errorf("迁移原状态目录队列失败：%w", migErr),
					}
				}
				if legacyKey != "" && legacyKey != newIDKey {
					if _, migErr := queue.MigrateUnfinished(prof.SourceStateDir, legacyKey, cfg.StateDir, newIDKey); migErr != nil {
						return identityActivatedMsg{
							cfg:    cfg,
							hasCfg: true,
							label:  prof.Label,
							stage:  identityStageQueue,
							err:    fmt.Errorf("迁移历史队列失败：%w", migErr),
						}
					}
				}
			}
			res := openIdentityStack(cfg, prof.Label)
			if res.err != nil {
				return res
			}
			if oldMgr != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err = oldMgr.Quiesce(ctx)
				cancel()
				if err != nil {
					if res.runner != nil {
						_ = res.runner.Close()
					}
					if res.manager != nil {
						_ = res.manager.Close()
					}
					return identityActivatedMsg{
						cfg:    cfg,
						hasCfg: true,
						label:  prof.Label,
						stage:  identityStageQueue,
						err:    fmt.Errorf("切换身份前暂停当前任务失败：%w", err),
					}
				}
			}
			if err := commitActive(); err != nil {
				if res.runner != nil {
					_ = res.runner.Close()
				}
				if res.manager != nil {
					_ = res.manager.Close()
				}
				if oldMgr != nil {
					oldMgr.ReleaseHold()
				}
				return identityActivatedMsg{
					cfg:    cfg,
					hasCfg: true,
					label:  prof.Label,
					stage:  identityStageLookup,
					err:    err,
				}
			}
			if oldMgr != nil {
				if oldRunner != nil {
					_ = oldRunner.Close()
				}
				_ = oldMgr.Close()
			}
			res.activeID = prof.ID
			return res
		}

		// 情况 2：同一身份但状态目录发生变化（例如将 -config 启动的身份导入便携 .sft 目录），
		// 先锁定新队列确认无冲突，再静默关闭旧队列并将未完成任务迁移到新目录，最后提交 active。
		if idErr == nil && newIDKey == oldIDKey && oldMgr != nil && oldStateDir != "" && !sameClientDir(oldStateDir, cfg.StateDir) {
			dstStore, err := queue.OpenStore(cfg.StateDir, newIDKey)
			if err != nil {
				return identityActivatedMsg{
					cfg:    cfg,
					hasCfg: true,
					label:  prof.Label,
					stage:  identityStageQueue,
					err:    fmt.Errorf("队列初始化失败：%w", err),
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = oldMgr.Quiesce(ctx)
			cancel()
			if err != nil {
				_ = dstStore.Close()
				return identityActivatedMsg{
					cfg:    cfg,
					hasCfg: true,
					label:  prof.Label,
					stage:  identityStageQueue,
					err:    fmt.Errorf("切换身份前暂停当前任务失败：%w", err),
				}
			}
			if oldRunner != nil {
				_ = oldRunner.Close()
			}
			_ = oldMgr.Close()

			_, migErr := dstStore.MigrateUnfinishedFrom(oldStateDir, oldIDKey, newIDKey)
			if migErr == nil && legacyKey != "" && legacyKey != newIDKey {
				_, migErr = dstStore.MigrateUnfinishedFrom(oldStateDir, legacyKey, newIDKey)
			}
			_ = dstStore.Close()
			if migErr != nil {
				return identityActivatedMsg{
					cfg:      cfg,
					hasCfg:   true,
					label:    prof.Label,
					stage:    identityStageQueue,
					resetOld: true,
					err:      fmt.Errorf("迁移原状态目录队列失败：%w", migErr),
				}
			}
			res := openIdentityStack(cfg, prof.Label)
			if res.err != nil {
				res.resetOld = true
				return res
			}
			if err := commitActive(); err != nil {
				if res.runner != nil {
					_ = res.runner.Close()
				}
				if res.manager != nil {
					_ = res.manager.Close()
				}
				return identityActivatedMsg{
					cfg:      cfg,
					hasCfg:   true,
					label:    prof.Label,
					stage:    identityStageLookup,
					resetOld: true,
					err:      err,
				}
			}
			res.activeID = prof.ID
			return res
		}

		// 情况 3：同一身份同一状态目录，或当前尚未加载旧身份
		if oldMgr != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err = oldMgr.Quiesce(ctx)
			cancel()
			if err != nil {
				return identityActivatedMsg{
					cfg:    cfg,
					hasCfg: true,
					label:  prof.Label,
					stage:  identityStageQueue,
					err:    fmt.Errorf("切换身份前暂停当前任务失败：%w", err),
				}
			}
			if oldRunner != nil {
				_ = oldRunner.Close()
			}
			_ = oldMgr.Close()
		}
		if idErr == nil && prof.SourceStateDir != "" && !sameClientDir(prof.SourceStateDir, cfg.StateDir) {
			if _, migErr := queue.MigrateUnfinished(prof.SourceStateDir, newIDKey, cfg.StateDir, newIDKey); migErr != nil {
				return identityActivatedMsg{
					cfg:      cfg,
					hasCfg:   true,
					label:    prof.Label,
					stage:    identityStageQueue,
					resetOld: oldMgr != nil,
					err:      fmt.Errorf("迁移原状态目录队列失败：%w", migErr),
				}
			}
			if legacyKey != "" && legacyKey != newIDKey {
				if _, migErr := queue.MigrateUnfinished(prof.SourceStateDir, legacyKey, cfg.StateDir, newIDKey); migErr != nil {
					return identityActivatedMsg{
						cfg:      cfg,
						hasCfg:   true,
						label:    prof.Label,
						stage:    identityStageQueue,
						resetOld: oldMgr != nil,
						err:      fmt.Errorf("迁移历史队列失败：%w", migErr),
					}
				}
			}
		}
		res := openIdentityStack(cfg, prof.Label)
		res.activeID = prof.ID
		if res.err != nil {
			if oldMgr != nil {
				res.resetOld = true
			}
			return res
		}
		if err := commitActive(); err != nil {
			if res.runner != nil {
				_ = res.runner.Close()
			}
			if res.manager != nil {
				_ = res.manager.Close()
			}
			return identityActivatedMsg{
				cfg:      cfg,
				hasCfg:   true,
				label:    prof.Label,
				stage:    identityStageLookup,
				resetOld: oldMgr != nil,
				err:      err,
			}
		}
		return res
	}
}
