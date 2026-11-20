package tui

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/control"
	"sft-tui/internal/identity"
	"sft-tui/internal/monitor"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
	"sft-tui/internal/testcert"
)

// These regressions assert the intended behavior, rather than accepting the
// current bugs. Fixtures use real certificates, files and queue databases;
// none of these tests needs a listening TCP/IPC endpoint or a background daemon.
func regressionServer(t *testing.T, width, height int) *serverModel {
	t.Helper()
	root := t.TempDir()
	cfgPath := filepath.Join(root, "configs", "server.json")
	_, err := certmanage.Execute("init", certmanage.Options{
		ServerConfig:    cfgPath,
		Dir:             filepath.Join(root, "certs"),
		ClientConfigDir: filepath.Dir(cfgPath),
		Name:            "alice",
		Hosts:           "localhost,127.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadServer(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.StorageRoot = filepath.Join(root, "storage")
	if err := config.SaveJSON(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}
	m := newServerModel(ServerOptions{ExecutableDir: root, ConfigPath: cfgPath})
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m.Update(m.loadConfigCmd()())
	if m.cfgErr != nil {
		t.Fatal(m.cfgErr)
	}
	return m
}

func TestRegressionDeploySavedConfigurationCanBeReloaded(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprintf("relative=%v", relative), func(t *testing.T) {
			root := t.TempDir()
			workingDir := filepath.Join(root, "working")
			executableDir := filepath.Join(root, "program")
			for _, dir := range []string{workingDir, executableDir} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(workingDir)
			cfgPath := filepath.Join("configs", "server.json")
			if !relative {
				cfgPath = filepath.Join(workingDir, cfgPath)
			}
			m := newServerModel(ServerOptions{ExecutableDir: executableDir, ConfigPath: cfgPath})
			m.Update(tea.WindowSizeMsg{Width: 110, Height: 32})
			m.Update(m.loadConfigCmd()())
			if m.deploy == nil || m.deploy.step != 1 {
				t.Fatal("missing configuration did not open the deployment wizard")
			}
			// Exercise each step's validation/capture callback, without depending
			// on whether Enter advances fields or activates a primary button.
			for _, step := range []int{1, 2, 3} {
				if m.deploy.step != step {
					t.Fatalf("expected step %d, got %d: %s", step, m.deploy.step, m.statusToast)
				}
				m.activateFormButton(1)
			}
			if m.deploy.step != 4 {
				t.Fatalf("did not reach deployment summary: %s", m.statusToast)
			}
			_, cmd := m.activateFormButton(1) // Only save; never start sftd.
			if cmd == nil {
				t.Fatal("save did not return a command")
			}
			done, ok := cmd().(deployInitDoneMsg)
			if !ok || done.err != nil {
				t.Fatalf("deployment failed: %v", done.err)
			}
			m.Update(done)
			saved, err := config.LoadServer(done.d.Config)
			if err != nil {
				t.Fatalf("wizard did not save a valid configuration: %v", err)
			}
			loaded, ok := m.loadConfigCmd()().(serverConfigLoadedMsg)
			if !ok || loaded.err != nil {
				t.Fatalf("wizard saved %q, but panel cannot reload its configuration %q: %v", done.d.Config, m.opts.ConfigPath, loaded.err)
			}
			if loaded.cfg.TLS != saved.TLS {
				t.Fatal("panel loaded different certificate material from the saved deployment")
			}
		})
	}
}

// Ignore framing and wrapping when checking whether an error is readable.
func regressionVisibleText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '║' || r == '│' {
			return -1
		}
		return r
	}, ansi.Strip(s))
}

func TestRegressionFormValidationErrorIsVisibleInsideModal(t *testing.T) {
	for _, scenario := range []string{"new_user", "settings", "deployment"} {
		t.Run(scenario, func(t *testing.T) {
			m := regressionServer(t, 80, 24)
			switch scenario {
			case "new_user":
				m.openNewUserModal()
				m.formInputs[0].SetValue("bob")
				m.formInputs[1].SetValue("invalid-days")
			case "settings":
				m.openEditSettingsModal()
				m.formInputs[1].SetValue("")
			case "deployment":
				m = newServerModel(ServerOptions{ExecutableDir: t.TempDir(), ConfigPath: filepath.Join(t.TempDir(), "server.json")})
				m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
				m.Update(m.loadConfigCmd()())
				m.formInputs[0].SetValue("invalid-address")
			}
			modal := m.modal
			m.triggerPrimaryButton()
			if m.modal != modal || m.statusToast == "" {
				t.Fatalf("expected validation failure with modal retained: modal=%v error=%q", m.modal, m.statusToast)
			}
			if !strings.Contains(regressionVisibleText(m.View()), regressionVisibleText(m.statusToast)) {
				t.Errorf("validation error is hidden behind the modal: %q", m.statusToast)
			}
		})
	}
}

func TestRegressionExpandedUnitDropdownFitsTerminal(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {96, 28}, {120, 32}} {
		for _, scenario := range []string{"deployment", "settings"} {
			t.Run(fmt.Sprintf("%s/%dx%d", scenario, size[0], size[1]), func(t *testing.T) {
				m := regressionServer(t, size[0], size[1])
				if scenario == "settings" {
					m.openEditSettingsModal()
				} else {
					m = newServerModel(ServerOptions{ExecutableDir: t.TempDir(), ConfigPath: filepath.Join(t.TempDir(), "server.json")})
					m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
					m.Update(m.loadConfigCmd()())
					m.openDeployStep(3)
				}
				for row, field := range m.formRows {
					if field.kind == formRowSizeUnit {
						m.setRowFocus(row, 1)
						break
					}
				}
				m.Update(keyType(tea.KeyEnter))
				if !m.dropdownOpen {
					t.Fatal("unit selector did not open")
				}
				view := m.View()
				if got := lipgloss.Height(view); got > size[1] {
					t.Errorf("expanded unit selector pushes modal outside terminal: got %d rows, available %d", got, size[1])
				}
				for _, line := range strings.Split(ansi.Strip(view), "\n") {
					if got := lipgloss.Width(line); got > size[0] {
						t.Errorf("expanded selector line exceeds terminal width: %d > %d", got, size[0])
						break
					}
				}
			})
		}
	}
}

func TestRegressionMouseClickTargetsRenderedInput(t *testing.T) {
	for _, inputIdx := range []int{0, 1} {
		t.Run(fmt.Sprintf("input=%d", inputIdx), func(t *testing.T) {
			m := regressionServer(t, 80, 24)
			m.openEditSettingsModal()
			// Keep the input fully visible even when Go's temporary path is
			// longer than the text field; no configuration is saved here.
			m.formInputs[1].SetValue("review-storage")
			value := m.formInputs[inputIdx].Value()
			// Locate the displayed input, not the form's internal hit boxes.
			for y, line := range strings.Split(ansi.Strip(m.View()), "\n") {
				if index := strings.Index(line, value); index >= 0 {
					m.Update(tea.MouseMsg{
						X: lipgloss.Width(line[:index]) + 1, Y: y,
						Button: tea.MouseButtonLeft, Action: tea.MouseActionPress,
					})
					if !m.formInputs[inputIdx].Focused() {
						t.Errorf("click on visible input[%d] focused another field (form row %d)", inputIdx, m.formFocus)
					}
					return
				}
			}
			t.Fatalf("could not find input[%d] in rendered modal", inputIdx)
		})
	}
}

func TestRegressionBandwidthReloadFailureIsReported(t *testing.T) {
	m := regressionServer(t, 120, 32)
	before := m.cfg
	proposed := before
	bandwidth := before.Bandwidth
	proposed.GlobalBandwidth = &bandwidth
	m.settingsExpected = before

	// Start a managed local IPC control service that deterministically rejects Reload.
	svc, err := control.StartManaged(m.opts.ConfigPath, before.StorageRoot, control.Handlers{
		Reload: func() error { return fmt.Errorf("simulated reload failure") },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if _, err := control.Reload(m.opts.ConfigPath, before.StorageRoot); err == nil {
		t.Fatal("fixture did not cause a reload failure")
	}
	_, cmd := m.saveServerSettingsCmd(proposed)
	if cmd == nil {
		t.Fatal("save did not return a command")
	}
	result, ok := cmd().(serverActionResultMsg)
	if !ok {
		t.Fatal("save returned an unexpected result")
	}
	saved, err := config.LoadServer(m.opts.ConfigPath)
	if err != nil || saved.GlobalBandwidth == nil || *saved.GlobalBandwidth != bandwidth {
		t.Fatalf("settings were not saved before reload: %v", err)
	}
	if result.err == nil && !strings.Contains(result.notice, "失败") {
		t.Errorf("successful disk save hid the reload failure: notice=%q", result.notice)
	}
}

func TestRegressionUnappliedBandwidthDoesNotClaimRuntimeMatches(t *testing.T) {
	m := regressionServer(t, 120, 32)
	running := m.cfg
	bandwidth := running.Bandwidth
	m.cfg.GlobalBandwidth = &bandwidth
	m.runtime = &monitor.Snapshot{Config: running}
	m.page = 4
	if strings.Contains(ansi.Strip(m.View()), "磁盘配置与运行实例基本设置一致") {
		t.Error("settings claim disk/runtime match even though the new global bandwidth has not been applied")
	}
}

func TestRegressionLateIdentitySwitchFailureKeepsQueueWritable(t *testing.T) {
	root := t.TempDir()
	certDir := filepath.Join(root, "certs")
	if err := testcert.Generate(certDir); err != nil {
		t.Fatal(err)
	}
	if err := testcert.IssueClient(certDir, "alice"); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultClient()
	cfg.StateDir = filepath.Join(root, "state")
	cfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certDir, "ca.pem"),
		Certificate:   filepath.Join(certDir, "alice.pem"),
		PrivateKey:    filepath.Join(certDir, "alice-key.pem"),
	}
	configPath := filepath.Join(root, "client.json")
	if err := config.SaveJSON(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	exeDir := t.TempDir()
	profile, err := identity.PrepareImport(exeDir, root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(Options{ExecutableDir: exeDir, Directory: root})
	t.Cleanup(m.cleanup)
	opened := openIdentityStack(cfg, "alice")
	if opened.err != nil {
		t.Fatal(opened.err)
	}
	m.Update(opened)         // Do not execute the returned remote-list command.
	m.manager.SetRunner(nil) // Queue writes must not start network transfers.
	// TLS and profile validation succeed; activation fails later while reading
	// the saved labels. This reaches the path missed by the missing-file test.
	if err := os.WriteFile(filepath.Join(exeDir, ".sft", "profile-labels.json"), []byte("{broken-labels"), 0600); err != nil {
		t.Fatal(err)
	}
	result, ok := m.activateChoiceCmd(identityChoice{profile: profile, imported: true}, "")().(identityActivatedMsg)
	if !ok || result.err == nil {
		t.Fatal("corrupt identity registry did not produce an activation failure")
	}
	m.Update(result)
	path := filepath.Join(root, "source.bin")
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.manager.Enqueue([]queue.Spec{{Direction: protocol.DirectionUpload, Remote: "source.bin", Path: path}}); err != nil {
		t.Errorf("late identity-switch failure left the original queue unusable: %v", err)
	}
}

func TestRegressionFileLogTailHandlesFilesAbove64KiB(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprintf("large=%v", large), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sftd.log")
			data := []byte("initial log event\n")
			if large {
				data = bytes.Repeat([]byte("older event\n"), 8000)
			}
			const last = "latest log event: upload completed\n"
			data = append(data, last...)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			got := readTailText(path)
			if strings.HasPrefix(got, "无法读取日志文件") || !strings.HasSuffix(got, last) {
				t.Fatalf("cannot view the latest events of a %d-byte log: %.160s", len(data), got)
			}
			if len(got) > 64*1024 {
				t.Errorf("log tail exceeded its 64 KiB read limit: %d bytes", len(got))
			}
		})
	}
}
