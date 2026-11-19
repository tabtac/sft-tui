package tui

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"sft-tui/internal/config"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
	"sft-tui/internal/testcert"
	"sft-tui/internal/transfer"
)

func setupTestClientEnv(t *testing.T) (config.Client, *transfer.Daemon, func()) {
	t.Helper()
	dir := t.TempDir()
	certsDir := filepath.Join(dir, "certs")
	if err := testcert.Generate(certsDir); err != nil {
		t.Fatal(err)
	}
	if err := testcert.IssueClient(certsDir, "alice"); err != nil {
		t.Fatal(err)
	}

	srvCfg := config.DefaultServer()
	srvCfg.Listen = "127.0.0.1:0"
	srvCfg.StorageRoot = filepath.Join(dir, "storage")
	srvCfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "server.pem"),
		PrivateKey:    filepath.Join(certsDir, "server-key.pem"),
	}
	srvCfg.AuthorizedClients = []config.AuthorizedClient{
		{UserID: "alice", Certificate: filepath.Join(certsDir, "alice.pem"), Permissions: []string{"upload", "download"}},
	}

	d, err := transfer.NewDaemon(srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Serve(ctx, ln)
	}()

	cliCfg := config.DefaultClient()
	cliCfg.StateDir = filepath.Join(dir, "cli-state")
	cliCfg.ServerAddress = ln.Addr().String()
	cliCfg.ServerName = "localhost"
	cliCfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "alice.pem"),
		PrivateKey:    filepath.Join(certsDir, "alice-key.pem"),
	}

	cleanup := func() {
		d.Stop(true)
		cancel()
		<-done
		_ = d.Close()
	}
	return cliCfg, d, cleanup
}

func keyRune(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func keyType(k tea.KeyType) tea.KeyMsg {
	return tea.KeyMsg{Type: k}
}

func awaitClientQueueSnapshot(t *testing.T, m *model, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m.refreshQueueSnapshots()
		if ready() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("queue snapshot did not converge: active=%+v history=%+v", m.activeTasks, m.historyTasks)
}

func TestBubbleTeaLayoutNavigationAndInputIsolation(t *testing.T) {
	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, "hello.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}

	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: localDir})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 34})
	resMsg := m.loadLocalCmd(localDir)()
	m.Update(resMsg)

	// 验证宽屏视图包含三栏与标题
	wideView := m.View()
	if !strings.Contains(wideView, "SFT-TUI") || !strings.Contains(wideView, "本地文件") || !strings.Contains(wideView, "远端存储") {
		t.Fatalf("unexpected wide view:\n%s", wideView)
	}

	// 进入 / 搜索状态后输入 q 不得触发退出！
	m.Update(keyRune('/'))
	if !m.editingSearch {
		t.Fatal("expected editingSearch to activate")
	}
	_, cmd := m.Update(keyRune('q'))
	_ = cmd
	if m.query != "q" {
		t.Fatalf("expected 'q' captured into search query, got %q", m.query)
	}
	m.Update(keyType(tea.KeyEsc))
	if m.editingSearch || m.query != "" {
		t.Fatal("expected Esc to exit search and clear query")
	}

	// 窄屏响应式标签栏 + Tab 循环视图（本地 -> 远端 -> 活动队列 -> 历史归档 -> 本地）
	m.Update(tea.WindowSizeMsg{Width: 78, Height: 24})
	narrowView := m.View()
	if !strings.Contains(narrowView, "1 本地文件") {
		t.Fatalf("expected narrow tab bar, got:\n%s", narrowView)
	}
	m.Update(keyType(tea.KeyTab)) // -> 远端
	if m.focus != areaRemote {
		t.Fatalf("expected focus=areaRemote, got %v", m.focus)
	}
	m.Update(keyType(tea.KeyTab)) // -> 活动队列
	if m.focus != areaQueue || m.historyView {
		t.Fatalf("expected active queue view, got focus=%v hist=%v", m.focus, m.historyView)
	}
	m.Update(keyType(tea.KeyTab)) // -> 历史归档
	if m.focus != areaQueue || !m.historyView {
		t.Fatalf("expected history queue view, got focus=%v hist=%v", m.focus, m.historyView)
	}
	if _, cmd := m.Update(keyType(tea.KeyF6)); cmd != nil || m.statusToast != "" {
		t.Fatalf("expected F6 theme toggle removed, got cmd=%v toast=%q", cmd, m.statusToast)
	}
}

func TestBubbleTeaLiveUploadQueueControlAndDownload(t *testing.T) {
	cliCfg, _, cleanup := setupTestClientEnv(t)
	defer cleanup()

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "client.alice.json")
	if err := config.SaveJSON(cfgPath, cliCfg); err != nil {
		t.Fatal(err)
	}

	localDir := t.TempDir()
	payload := bytes.Repeat([]byte("SFT-TUI-DATA-"), 2048)
	if err := os.WriteFile(filepath.Join(localDir, "demo.bin"), payload, 0600); err != nil {
		t.Fatal(err)
	}

	m := newModel(Options{ExecutableDir: cfgDir, Directory: localDir, ConfigPath: cfgPath})
	defer m.cleanup()
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 34})
	m.Update(m.loadLocalCmd(localDir)())
	m.Update(m.initIdentityCmd()())
	m.Update(m.loadRemoteCmd()())

	// 找到 demo.bin 并按 Space 选中后按 u 入队上传
	foundIdx := -1
	for i, e := range m.visibleLocal {
		if e.name == "demo.bin" {
			foundIdx = i
			break
		}
	}
	if foundIdx < 0 {
		t.Fatalf("demo.bin missing in visibleLocal: %+v", m.visibleLocal)
	}
	m.localCursor = foundIdx
	m.Update(keyRune(' '))
	_, cmd := m.Update(keyRune('u'))
	if cmd == nil {
		t.Fatal("expected enqueue command after pressing u")
	}
	m.Update(cmd())

	// 等待上传任务出现在已完成视图
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		m.refreshQueueSnapshots()
		if len(m.historyTasks) > 0 && m.historyTasks[0].State == "completed" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if len(m.historyTasks) == 0 || m.historyTasks[0].State != "completed" {
		t.Fatalf("expected upload task to complete; active=%+v hist=%+v", m.activeTasks, m.historyTasks)
	}

	// 刷新远端文件并用 Tab + Space/a 验证状态视图与下载
	m.Update(m.loadRemoteCmd()())
	if len(m.visibleRemote) != 1 || m.visibleRemote[0].FileName != "demo.bin" {
		t.Fatalf("expected demo.bin in remote list, got %+v", m.visibleRemote)
	}
	dlDir := filepath.Join(t.TempDir(), "dl")
	if err := os.MkdirAll(dlDir, 0700); err != nil {
		t.Fatal(err)
	}
	m.localDir = dlDir
	m.Update(m.loadLocalCmd(dlDir)())
	m.focus = areaRemote
	m.remoteCursor = 0
	_, dlCmd := m.Update(keyRune('d'))
	if dlCmd == nil {
		t.Fatal("expected download enqueue cmd")
	}
	m.Update(dlCmd())

	dlDeadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(dlDeadline) {
		m.refreshQueueSnapshots()
		if len(m.historyTasks) >= 2 && m.historyTasks[0].State == "completed" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	got, err := os.ReadFile(filepath.Join(dlDir, "demo.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("downloaded file mismatch: err=%v len=%d", err, len(got))
	}

	// 验证队列区 a (全部暂停/恢复) 和 c (取消) 对空闲已完成列表无崩溃且视图正常渲染
	m.focus = areaQueue
	m.historyView = true
	view := m.View()
	if !strings.Contains(view, "demo.bin") || !strings.Contains(view, "已完成") {
		t.Fatalf("expected history row in rendered TUI view:\n%s", view)
	}
}

func TestBubbleTeaQueueKeysSpacePauseResumeCancelAllAndQuitModal(t *testing.T) {
	dir := t.TempDir()
	localDir := filepath.Join(dir, "src")
	_ = os.MkdirAll(localDir, 0700)
	f1 := filepath.Join(localDir, "p1.bin")
	f2 := filepath.Join(localDir, "p2.bin")
	_ = os.WriteFile(f1, bytes.Repeat([]byte("1"), 1024), 0600)
	_ = os.WriteFile(f2, bytes.Repeat([]byte("2"), 1024), 0600)

	// 打开不绑定 runner 的持久化队列以便精确确定地测试 Space/c/a 状态流转
	m := newModel(Options{ExecutableDir: dir, Directory: localDir})
	mgr, err := queue.Open(queue.Config{
		StateDir:     filepath.Join(dir, "state"),
		Identity:     "test-alice",
		MaxFileBytes: 1 << 20,
		MaxStreams:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.manager = mgr
	defer m.cleanup()

	if _, err := mgr.Enqueue([]queue.Spec{
		{Direction: protocol.DirectionUpload, Remote: "p1.bin", Path: f1},
		{Direction: protocol.DirectionUpload, Remote: "p2.bin", Path: f2},
	}); err != nil {
		t.Fatal(err)
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	m.focus = areaQueue
	m.historyView = false
	m.refreshQueueSnapshots()
	if len(m.activeTasks) != 2 {
		t.Fatalf("expected 2 waiting tasks, got %d", len(m.activeTasks))
	}

	// Space 暂停第 1 项 -> 再按 Space 恢复第 1 项
	_, cmd := m.Update(keyRune(' '))
	if cmd == nil {
		t.Fatal("expected Space toggle cmd in queue")
	}
	m.Update(cmd())
	// Snapshot 在队列锁被占用时可返回旧视图，等待下一次刷新可见，避免时序依赖。
	awaitClientQueueSnapshot(t, m, func() bool { return len(m.activeTasks) == 2 && m.activeTasks[0].State == queue.Paused })
	if m.activeTasks[0].State != "paused" {
		t.Fatalf("expected task 0 paused, got %s", m.activeTasks[0].State)
	}
	_, cmd = m.Update(keyRune(' '))
	m.Update(cmd())
	awaitClientQueueSnapshot(t, m, func() bool { return len(m.activeTasks) == 2 && m.activeTasks[0].State == queue.Waiting })
	if m.activeTasks[0].State != "waiting" {
		t.Fatalf("expected task 0 waiting after resume, got %s", m.activeTasks[0].State)
	}

	// a 全部暂停 -> 再按 a 全部恢复
	_, cmd = m.Update(keyRune('a'))
	m.Update(cmd())
	awaitClientQueueSnapshot(t, m, func() bool {
		if len(m.activeTasks) != 2 {
			return false
		}
		for _, task := range m.activeTasks {
			if task.State != queue.Paused {
				return false
			}
		}
		return true
	})
	for _, tk := range m.activeTasks {
		if tk.State != "paused" {
			t.Fatalf("expected all paused after a, got %s", tk.State)
		}
	}
	_, cmd = m.Update(keyRune('a'))
	m.Update(cmd())
	awaitClientQueueSnapshot(t, m, func() bool {
		if len(m.activeTasks) != 2 {
			return false
		}
		for _, task := range m.activeTasks {
			if task.State != queue.Waiting {
				return false
			}
		}
		return true
	})
	for _, tk := range m.activeTasks {
		if tk.State != "waiting" {
			t.Fatalf("expected all waiting after second a, got %s", tk.State)
		}
	}

	// c 取消选中的第 1 项
	_, cmd = m.Update(keyRune('c'))
	m.Update(cmd())
	awaitClientQueueSnapshot(t, m, func() bool {
		return len(m.historyTasks) > 0 && m.historyTasks[0].State == queue.Cancelled
	})
	if len(m.historyTasks) == 0 || m.historyTasks[0].State != "cancelled" {
		t.Fatalf("expected cancelled task in history, got %+v", m.historyTasks)
	}

	// 空闲时按 q 直接触发优雅退出 tea.Quit
	_, quitCmd := m.Update(keyRune('q'))
	if quitCmd == nil {
		t.Fatal("expected quitCmd when pressing q idle")
	}
}

func TestClientIgnoresOutOfOrderDirectoryAndRemoteResponses(t *testing.T) {
	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir()})
	m.localEpoch = 2
	m.Update(localLoadedMsg{epoch: 2, dir: "new", entries: []localEntry{{name: "new.txt", regular: true}}})
	m.Update(localLoadedMsg{epoch: 1, dir: "old", entries: []localEntry{{name: "old.txt", regular: true}}})
	if m.localDir != "new" || len(m.localEntries) != 1 || m.localEntries[0].name != "new.txt" {
		t.Error("stale local listing replaced newer navigation")
	}
	m.remoteEpoch = 3
	m.Update(remoteLoadedMsg{epoch: 3, entries: []protocol.FileEntry{{FileName: "new.txt"}}})
	m.Update(remoteLoadedMsg{epoch: 2, entries: []protocol.FileEntry{{FileName: "old.txt"}}})
	if len(m.remoteEntries) != 1 || m.remoteEntries[0].FileName != "new.txt" {
		t.Error("stale remote listing replaced current identity data")
	}
}

func TestClientIdentitySwitchFailureKeepsExistingQueueUsable(t *testing.T) {
	cfg, _, cleanup := setupTestClientEnv(t)
	defer cleanup()
	local := t.TempDir()
	path := filepath.Join(local, "source.bin")
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: local})
	defer m.cleanup()
	opened := openIdentityStack(cfg, "alice")
	if opened.err != nil {
		t.Fatal(opened.err)
	}
	m.Update(opened)
	result := m.activateChoiceCmd(identityChoice{}, filepath.Join(t.TempDir(), "missing-client.json"))()
	msg, ok := result.(identityActivatedMsg)
	if !ok || msg.err == nil {
		t.Fatalf("invalid identity did not report failure: %T", result)
	}
	m.Update(result)
	// BUG: activateChoiceCmd 在验证新身份前关闭旧 manager/runner，失败后界面仍引用已关闭队列。
	if _, err := m.manager.Enqueue([]queue.Spec{{Direction: protocol.DirectionUpload, Remote: "source.bin", Path: path}}); err != nil {
		t.Errorf("BUG: failed identity switch disabled the existing queue: %v", err)
	}
}

func TestClientViewHandlesSmallWindowsAndWideUnicode(t *testing.T) {
	for _, size := range [][2]int{{0, 0}, {1, 1}, {40, 10}, {79, 23}, {80, 24}, {120, 34}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir()})
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.localEntries = []localEntry{{name: strings.Repeat("中文", 100), regular: true}}
			m.rebuildLocalVisible()
			_ = m.View() // 小窗口、空列表和超宽名称均不得 panic。
		})
	}
}

func TestAutoRenamePreservesProtocolFilenameLimit(t *testing.T) {
	for _, original := range []string{"a.bin", strings.Repeat("a", 251) + ".bin", strings.Repeat("界", 83) + "ab.bin"} {
		t.Run(fmt.Sprintf("bytes_%d", len(original)), func(t *testing.T) {
			if err := protocol.ValidateName(original); err != nil {
				t.Fatalf("fixture is not a valid filename: %v", err)
			}
			name := uniqueName(original, map[string]bool{original: true})
			if name == original {
				t.Error("auto-rename reused occupied name")
			}
			// BUG: 给 255 字节文件名直接追加 (1)，生成超出协议限制的目标名。
			if err := protocol.ValidateName(name); err != nil {
				t.Errorf("BUG: auto-rename generated invalid filename (%d bytes): %v", len(name), err)
			}
		})
	}
}

func TestDownloadConflictResolutionPreservesDirectoryAndSource(t *testing.T) {
	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir()})
	dir := t.TempDir()
	m.pendingBatchDir = protocol.DirectionDownload
	m.localEntries = []localEntry{{name: "a.bin", regular: true}, {name: "a (1).bin", regular: true}}
	m.pendingSpecs = []queue.Spec{{Direction: protocol.DirectionDownload, Remote: "a.bin", Path: filepath.Join(dir, "a.bin")}}
	m.conflictNames = []string{"a.bin"}
	renamed := m.resolveConflictsAutoRename()
	if len(renamed) != 1 || renamed[0].Remote != "a.bin" || renamed[0].Path != filepath.Join(dir, "a (2).bin") {
		t.Errorf("bad download conflict resolution: %+v", renamed)
	}
	skipped := m.resolveConflictsSkip()
	if len(skipped) != 1 || skipped[0].Skip == "" || skipped[0].Path != m.pendingSpecs[0].Path {
		t.Errorf("skip changed destination or failed to mark task: %+v", skipped)
	}
	if m.pendingSpecs[0].Skip != "" || m.pendingSpecs[0].Path != filepath.Join(dir, "a.bin") {
		t.Error("resolution mutated original batch")
	}
}

func TestClientAdaptiveThemeEmitsNoF6Action(t *testing.T) {
	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir()})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyF6})
	if cmd != nil || m.statusToast != "" {
		t.Fatalf("F6 should be a no-op after removing theme switcher: cmd=%v toast=%q", cmd, m.statusToast)
	}
}

func TestClientMouseClickAndWheelInteraction(t *testing.T) {
	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir()})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	m.visibleLocal = []localEntry{{name: "a.bin", regular: true}, {name: "b.bin", regular: true}, {name: "c.bin", regular: true}}
	m.visibleRemote = []protocol.FileEntry{{FileName: "r1.bin"}, {FileName: "r2.bin"}}
	m.activeTasks = []queue.Task{{ID: "t1", Remote: "a.bin", State: queue.Waiting}}
	m.historyTasks = []queue.Task{{ID: "h1", Remote: "old.bin", State: queue.Completed}}

	// 1. 点击远端面板第 2 行 (bodyY=2, topH=14, rowStart=4 => row1 at Y=5, X>=55)
	m.Update(tea.MouseMsg{X: 70, Y: 5, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.focus != areaRemote || m.remoteCursor != 1 {
		t.Fatalf("remote click failed: focus=%v cursor=%d", m.focus, m.remoteCursor)
	}
	// 2. 滚轮上滚远端列表
	m.Update(tea.MouseMsg{X: 70, Y: 5, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	if m.remoteCursor != 0 {
		t.Fatalf("mouse wheel up failed: cursor=%d", m.remoteCursor)
	}
	// 3. 点击底部队列面板标题切换至历史归档 (bodyY=2 + topH=14 => queueY=16, title row Y=17)
	titlePrefix := lipgloss.Width(m.th.tabActive.Render("活动队列 (1)"))
	m.Update(tea.MouseMsg{X: 2 + titlePrefix + 2, Y: 17, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.focus != areaQueue || !m.historyView {
		t.Fatalf("queue history tab click failed: focus=%v historyView=%v", m.focus, m.historyView)
	}
}

func TestClientShortcutScopesKeyAAndF5ToActiveArea(t *testing.T) {
	cliCfg, _, cleanup := setupTestClientEnv(t)
	defer cleanup()

	mgr, err := queue.Open(queue.Config{
		Identity: "alice-test-id",
		StateDir: cliCfg.StateDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()

	workDir := t.TempDir()
	localFile := filepath.Join(workDir, "local.bin")
	if err := os.WriteFile(localFile, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}

	tasks, err := mgr.Enqueue([]queue.Spec{{
		Direction: protocol.DirectionUpload,
		Remote:    "queued.bin",
		Path:      localFile,
		Size:      5,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Pause(tasks[0].ID); err != nil {
		t.Fatal(err)
	}

	m := newModel(Options{ExecutableDir: workDir, Directory: workDir})
	m.cfg = &cliCfg
	m.manager = mgr
	m.localDir = workDir
	m.visibleLocal = []localEntry{{name: "local.bin", path: localFile, size: 5, regular: true}}
	m.localSelected[localFile] = true
	m.visibleRemote = []protocol.FileEntry{{FileName: "queued.bin", Size: 5}}
	m.refreshQueueSnapshots()

	// 1. 文件区域按 a 不得暂停/恢复全部队列
	for _, area := range []focusArea{areaLocal, areaRemote} {
		m.focus = area
		_, cmd := m.Update(keyRune('a'))
		if cmd != nil {
			m.Update(cmd())
		}
		m.refreshQueueSnapshots()
		if got := mgr.Task(tasks[0].ID); got == nil || got.State != queue.Paused {
			t.Fatalf("pressing a in area %v unexpectedly altered queue state: %+v", area, got)
		}
	}

	// 2. 队列区域按 F5 不得进入上传逻辑，而应恢复选中的暂停任务
	m.focus = areaQueue
	m.historyView = false
	m.queueCursor = 0
	_, f5Cmd := m.Update(keyType(tea.KeyF5))
	if f5Cmd == nil {
		t.Fatal("F5 in queue on paused task did not emit resume command")
	}
	res := f5Cmd()
	m.Update(res)
	m.refreshQueueSnapshots()
	if m.modal != modalNone {
		t.Fatalf("F5 in queue opened upload conflict modal=%v", m.modal)
	}
	if got := len(m.activeTasks); got != 1 {
		t.Fatalf("F5 in queue unexpectedly uploaded another file: active=%d", got)
	}
	if got := mgr.Task(tasks[0].ID); got == nil || got.State != queue.Waiting {
		t.Fatalf("F5 in queue did not resume paused task: %+v", got)
	}

	// 3. 队列区域选中执行中任务按 F5 也不触发上传
	_, activeF5 := m.Update(keyType(tea.KeyF5))
	if activeF5 != nil {
		t.Fatal("F5 on running/waiting task in queue should not trigger upload cmd")
	}
	if !strings.Contains(m.statusToast, "无需恢复或重试") || len(m.activeTasks) != 1 {
		t.Fatalf("unexpected toast or task count after F5 on active task: toast=%q active=%d", m.statusToast, len(m.activeTasks))
	}

	// 4. 队列区域按 a 正常触发暂停全部
	_, qACmd := m.Update(keyRune('a'))
	if qACmd == nil {
		t.Fatal("pressing a in active queue did not trigger toggleAllCmd")
	}
	m.Update(qACmd())
	m.refreshQueueSnapshots()
	if got := mgr.Task(tasks[0].ID); got == nil || got.State != queue.Paused {
		t.Fatalf("pressing a in queue did not pause queue: %+v", got)
	}

	// 5. 队列中 Cancelled/历史任务按 F5 应重新入队（重传）而非触发本地选中上传
	if err := mgr.Cancel(tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	m.refreshQueueSnapshots()
	m.historyView = true
	m.queueCursor = 0
	_, retryCmd := m.Update(keyType(tea.KeyF5))
	if retryCmd == nil {
		t.Fatal("F5 on history task did not emit re-enqueue command")
	}
	m.Update(retryCmd())
	m.refreshQueueSnapshots()
	requeued := false
	for _, tk := range m.activeTasks {
		if tk.ID != tasks[0].ID && tk.Remote == "queued.bin" && tk.State == queue.Waiting {
			requeued = true
			break
		}
	}
	if !requeued {
		t.Fatalf("F5 on history task failed to re-enqueue target task: active=%+v", m.activeTasks)
	}
}

func TestClientFileListColumns80x24NeverWrapSizeUnit(t *testing.T) {
	workDir := t.TempDir()
	m := newModel(Options{ExecutableDir: workDir, Directory: workDir})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.localDir = workDir
	m.visibleLocal = []localEntry{
		{name: "..", path: filepath.Dir(workDir), dir: true},
		{name: strings.Repeat("long_local_archive_", 4) + ".tar.gz", path: filepath.Join(workDir, "a"), size: 25 * 1024 * 1024, regular: true},
		{name: "中文名称超长测试文件数据包-2026-生产备份.bin", path: filepath.Join(workDir, "b"), size: 2 * 1024 * 1024 * 1024, regular: true},
		{name: "small.txt", path: filepath.Join(workDir, "c"), size: 1536, regular: true},
	}
	cfg := config.DefaultClient()
	m.cfg = &cfg
	m.visibleRemote = []protocol.FileEntry{
		{FileName: strings.Repeat("remote_dataset_part_", 4) + ".zst", Size: 25 * 1024 * 1024},
		{FileName: "中文远端同步大数据镜像归档文件.img", Size: 2 * 1024 * 1024 * 1024},
		{FileName: "tiny.log", Size: 512},
	}

	assertSingleLineRows := func(name, panel string, wants []string) {
		t.Helper()
		plain := ansi.Strip(panel)
		lines := strings.Split(plain, "\n")
		for _, ln := range lines {
			trimmed := strings.TrimSpace(strings.Trim(ln, "│╭╮╰╯─ "))
			switch trimmed {
			case "B", "KiB", "MiB", "GiB", "TiB":
				t.Fatalf("%s wrapped size unit onto separate line:\n%s", name, plain)
			}
		}
		for _, want := range wants {
			foundSameLine := false
			for _, ln := range lines {
				if strings.Contains(ln, want) {
					foundSameLine = true
					break
				}
			}
			if !foundSameLine {
				t.Fatalf("%s missing single-line token %q:\n%s", name, want, plain)
			}
		}
	}

	m.focus = areaLocal
	localView := m.View()
	if h := lipgloss.Height(localView); h > 24 {
		t.Fatalf("80x24 local view overflowed height: got %d\n%s", h, ansi.Strip(localView))
	}
	assertSingleLineRows("local-80x24", localView, []string{"<DIR>", "25.0 MiB", "2.0 GiB", "1.5 KiB"})

	m.focus = areaRemote
	remoteView := m.View()
	if h := lipgloss.Height(remoteView); h > 24 {
		t.Fatalf("80x24 remote view overflowed height: got %d\n%s", h, ansi.Strip(remoteView))
	}
	assertSingleLineRows("remote-80x24", remoteView, []string{"25.0 MiB", "2.0 GiB", "512 B"})
}

func TestClientRenameModalAndScrollableInfoModals(t *testing.T) {
	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir()})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 14})

	// 1. 改名弹窗校验失败时保持打开并保留用户输入
	m.modal = modalRename
	m.renameTask = &queue.Task{ID: "t1", Direction: protocol.DirectionUpload, Remote: "a.bin", Path: "/tmp/a.bin", State: queue.Paused}
	m.renameInput.SetValue("bad/name")
	m.renameInput.Focus()
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalRename || m.renameTask == nil || m.renameInput.Value() != "bad/name" {
		t.Fatalf("invalid rename closed modal or lost input: modal=%v task=%v val=%q toast=%q",
			m.modal, m.renameTask, m.renameInput.Value(), m.statusToast)
	}
	m.modal = modalNone
	m.renameTask = nil

	// 2. 帮助弹窗与详情弹窗在 80x14 矮屏下不得溢出，标题与关闭提示始终可见并可滚动
	m.Update(keyRune('?'))
	v0 := ansi.Strip(m.View())
	if h := lipgloss.Height(v0); h > 14 {
		t.Fatalf("help modal exceeded 14 lines: got %d\n%s", h, v0)
	}
	if !strings.Contains(v0, "键盘快捷键说明") || !strings.Contains(v0, "关闭") {
		t.Fatalf("help modal clipped title or footer:\n%s", v0)
	}
	m.Update(keyType(tea.KeyDown))
	if m.infoScroll == 0 {
		t.Fatal("down key did not scroll overflowing help modal")
	}
	v1 := ansi.Strip(m.View())
	if lipgloss.Height(v1) > 14 || !strings.Contains(v1, "键盘快捷键说明") || !strings.Contains(v1, "关闭") {
		t.Fatalf("scrolled help modal clipped title/footer or overflowed:\n%s", v1)
	}

	m.detailTitle = "长内容详情标题"
	m.detailLines = make([]string, 30)
	for i := range m.detailLines {
		m.detailLines[i] = fmt.Sprintf("detail-row-%02d: %s", i, strings.Repeat("x", 85))
	}
	m.infoScroll = 0
	m.modal = modalDetail
	dv0 := ansi.Strip(m.View())
	if lipgloss.Height(dv0) > 14 || !strings.Contains(dv0, "长内容详情标题") || !strings.Contains(dv0, "关闭") {
		t.Fatalf("detail modal clipped or overflowed at 80x14:\n%s", dv0)
	}
	m.Update(tea.MouseMsg{X: 40, Y: 7, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if m.infoScroll == 0 {
		t.Fatal("wheel down did not scroll detail modal")
	}
	m.Update(keyType(tea.KeyEnd))
	dvEnd := ansi.Strip(m.View())
	if lipgloss.Height(dvEnd) > 14 || !strings.Contains(dvEnd, "长内容详情标题") || !strings.Contains(dvEnd, "关闭") || !strings.Contains(dvEnd, "detail-row-29") {
		t.Fatalf("detail modal scrolled to end missing last line or chrome:\n%s", dvEnd)
	}
}
