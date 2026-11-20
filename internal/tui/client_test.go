package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"sft-tui/internal/config"
	"sft-tui/internal/identity"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
	"sft-tui/internal/testcert"
)

func TestClientConflictCancelPreservesSelectionAndHistoryRetryChecksConflict(t *testing.T) {
	dir := t.TempDir()
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(localDir, "dup.bin")
	if err := os.WriteFile(dst, bytes.Repeat([]byte("a"), 128), 0600); err != nil {
		t.Fatal(err)
	}

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
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})

	m.remoteSelected["dup.bin"] = true
	spec := queue.Spec{Direction: protocol.DirectionDownload, Remote: "dup.bin", Path: dst, Size: 128}
	if cmd := m.prepareBatch(protocol.DirectionDownload, []queue.Spec{spec}, true); cmd != nil {
		t.Fatalf("expected conflict modal instead of immediate enqueue")
	}
	if m.modal != modalConflict {
		t.Fatalf("expected modalConflict, got %v", m.modal)
	}
	if !m.remoteSelected["dup.bin"] {
		t.Fatalf("expected remoteSelected to remain intact before conflict decision")
	}

	// Esc 取消冲突弹窗必须保留原有勾选状态
	m.Update(keyType(tea.KeyEsc))
	if m.modal != modalNone {
		t.Fatalf("expected modalNone after Esc, got %v", m.modal)
	}
	if !m.remoteSelected["dup.bin"] {
		t.Fatalf("expected remoteSelected preserved after cancelling conflict modal")
	}

	// 验证确认自动改名会清理勾选，并生成不冲突的新路径
	if cmd := m.prepareBatch(protocol.DirectionDownload, []queue.Spec{spec}, true); cmd != nil {
		t.Fatalf("expected conflict modal before auto-rename")
	}
	_, cmd := m.Update(keyRune('r'))
	if cmd != nil {
		m.Update(cmd())
	}
	if m.remoteSelected["dup.bin"] {
		t.Fatalf("expected remoteSelected cleared after resolving conflict")
	}
	awaitClientQueueSnapshot(t, m, func() bool { return len(m.activeTasks) == 1 })

	// 取消任务进入历史区后，按 r/F5 重试若本地目标已存在同名文件也应触发冲突确认弹窗
	m.focus = areaQueue
	m.historyView = false
	_, cancelCmd := m.Update(keyRune('c'))
	if cancelCmd != nil {
		m.Update(cancelCmd())
	}
	awaitClientQueueSnapshot(t, m, func() bool { return len(m.historyTasks) == 1 })
	histTask := m.historyTasks[0]
	histDst := histTask.Path
	if err := os.WriteFile(histDst, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}

	m.focus = areaQueue
	m.historyView = true
	_, retryCmd := m.Update(keyType(tea.KeyF5))
	if retryCmd != nil {
		t.Fatalf("expected history F5 retry with existing destination to open modalConflict")
	}
	if m.modal != modalConflict {
		t.Fatalf("expected modalConflict on history F5 retry, got %v", m.modal)
	}
}

func TestClientRenameValidationAndStateRestriction(t *testing.T) {
	dir := t.TempDir()
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(localDir, 0755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(localDir, "taken.bin")
	if err := os.WriteFile(existing, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}

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
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 32})

	if _, err := mgr.Enqueue([]queue.Spec{{
		Direction: protocol.DirectionDownload,
		Remote:    "src.bin",
		Path:      filepath.Join(localDir, "src.bin"),
		Size:      64,
	}}); err != nil {
		t.Fatal(err)
	}
	awaitClientQueueSnapshot(t, m, func() bool { return len(m.activeTasks) == 1 })

	m.focus = areaQueue
	m.historyView = false

	// Waiting 状态不允许改名另存重传（仅 Paused/Pending 允许）
	m.Update(keyRune('n'))
	if m.modal != modalNone {
		t.Fatalf("expected Waiting task not to open modalRename, got %v", m.modal)
	}

	// 暂停后再按 n 应打开改名弹窗；输入已存在的目标名并回车应保持弹窗打开并在弹窗内报错
	_, pauseCmd := m.Update(keyRune(' '))
	if pauseCmd != nil {
		m.Update(pauseCmd())
	}
	awaitClientQueueSnapshot(t, m, func() bool { return len(m.activeTasks) == 1 && m.activeTasks[0].State == queue.Paused })

	m.Update(keyRune('n'))
	if m.modal != modalRename {
		t.Fatalf("expected Paused task to open modalRename, got %v", m.modal)
	}
	m.renameInput.SetValue("taken.bin")
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalRename {
		t.Fatalf("expected modalRename to remain open on duplicate filename error")
	}
	if !strings.Contains(m.View(), "taken.bin") || !strings.Contains(m.View(), "已存在") {
		t.Fatalf("expected duplicate filename error visible inside modalRename view:\n%s", m.View())
	}

	// 输入合法且不冲突的新文件名后应关闭弹窗并入队新任务
	m.renameInput.SetValue("renamed.bin")
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalNone {
		t.Fatalf("expected modalRename to close on valid rename, got %v (toast=%q)", m.modal, m.statusToast)
	}
	awaitClientQueueSnapshot(t, m, func() bool { return len(m.activeTasks) == 2 })
}

func TestClientPerPaneSortPersistenceAndFoldCollision(t *testing.T) {
	dir := t.TempDir()
	prefsPath := resolvePrefsPath("", dir, ".sft-prefs.json")

	m := newModel(Options{ExecutableDir: dir, Directory: dir})
	m.prefsPath = prefsPath
	m.focus = areaLocal
	_, cmd := m.Update(keyRune('o'))
	if cmd == nil {
		t.Fatal("expected saveUIPrefsCmd on local sort change")
	}
	m.Update(cmd())

	m.focus = areaRemote
	_, cmd = m.Update(keyRune('o'))
	if cmd == nil {
		t.Fatal("expected saveUIPrefsCmd on remote sort change")
	}
	m.Update(cmd())
	_, cmd = m.Update(keyRune('o'))
	if cmd == nil {
		t.Fatal("expected saveUIPrefsCmd on second remote sort change")
	}
	m.Update(cmd())

	if m.localSort != sortNameDesc || m.remoteSort != sortSizeDesc {
		t.Fatalf("unexpected split sort states: local=%v remote=%v", m.localSort, m.remoteSort)
	}

	reloaded := newModel(Options{ExecutableDir: dir, Directory: dir})
	if reloaded.localSort != sortNameDesc || reloaded.remoteSort != sortSizeDesc {
		t.Fatalf("expected persisted per-pane sorts across restarts, got local=%v remote=%v", reloaded.localSort, reloaded.remoteSort)
	}

	used := map[string]bool{"report.txt": true}
	if got := uniqueNameFold("Report.TXT", used, true); got != "Report (1).TXT" {
		t.Fatalf("expected case-insensitive rename Report (1).TXT, got %q", got)
	}
	if got := uniqueNameFold("Report.TXT", used, false); got != "Report.TXT" {
		t.Fatalf("expected case-sensitive mode to keep Report.TXT, got %q", got)
	}
}

func TestClientPreservesFileFocusOnSortRefreshFilterAndDirExit(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct {
		name string
		size int
	}{{"alpha.bin", 300}, {"beta.bin", 100}, {"gamma.bin", 200}} {
		if err := os.WriteFile(filepath.Join(dir, f.name), bytes.Repeat([]byte("x"), f.size), 0600); err != nil {
			t.Fatal(err)
		}
	}

	m := newModel(Options{ExecutableDir: dir, Directory: dir})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(m.loadLocalCmd(dir)())

	// 选中 beta.bin 后按 o 切换排序、按刷新重新加载，焦点必须始终跟随 beta.bin
	betaPath := filepath.Join(dir, "beta.bin")
	for i, e := range m.visibleLocal {
		if e.path == betaPath {
			m.localCursor = i
			break
		}
	}
	m.focus = areaLocal
	m.Update(keyRune('o')) // NameDesc
	if got := m.visibleLocal[m.localCursor].path; got != betaPath {
		t.Fatalf("expected cursor to stay on beta.bin after sort, got %q", got)
	}
	m.Update(m.loadLocalCmd(dir)())
	if got := m.visibleLocal[m.localCursor].path; got != betaPath {
		t.Fatalf("expected cursor to stay on beta.bin after refresh, got %q", got)
	}

	// 进入 sub 再按 Backspace 返回上级目录时，光标应回到 sub 目录项
	for i, e := range m.visibleLocal {
		if e.path == sub {
			m.localCursor = i
			break
		}
	}
	_, enterCmd := m.Update(keyType(tea.KeyEnter))
	m.Update(enterCmd())
	if m.localDir != sub {
		t.Fatalf("expected dir to change to sub, got %q", m.localDir)
	}
	_, backCmd := m.Update(keyType(tea.KeyBackspace))
	m.Update(backCmd())
	if m.localDir != dir || m.visibleLocal[m.localCursor].path != sub {
		t.Fatalf("expected Backspace to return to parent and focus sub, dir=%q focused=%q", m.localDir, m.visibleLocal[m.localCursor].path)
	}

	// 远端列表排序与刷新后也保持当前选中文件名
	m.focus = areaRemote
	m.remoteEntries = []protocol.FileEntry{
		{FileName: "a.dat", Size: 50},
		{FileName: "b.dat", Size: 500},
		{FileName: "c.dat", Size: 10},
	}
	m.rebuildRemoteVisible()
	m.remoteCursor = 1 // b.dat
	m.Update(keyRune('o'))
	if got := m.visibleRemote[m.remoteCursor].FileName; got != "b.dat" {
		t.Fatalf("expected remote cursor to stay on b.dat after sort, got %q", got)
	}
	m.Update(remoteLoadedMsg{epoch: m.remoteEpoch, entries: []protocol.FileEntry{
		{FileName: "z.dat", Size: 1000},
		{FileName: "a.dat", Size: 50},
		{FileName: "b.dat", Size: 500},
	}})
	if got := m.visibleRemote[m.remoteCursor].FileName; got != "b.dat" {
		t.Fatalf("expected remote cursor to stay on b.dat after refresh, got %q", got)
	}
}

func TestClientContextualFooterAndAsyncDoubleSubmitGuard(t *testing.T) {
	dir := t.TempDir()
	m := newModel(Options{ExecutableDir: dir, Directory: dir})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.focus = areaLocal

	// 80x24 下未选文件且无身份时，Footer 单行渲染并明示不可用原因
	footer := ansi.Strip(m.renderFooter())
	if lipgloss.Height(footer) != 1 || !strings.Contains(footer, "需先选身份") || !strings.Contains(footer, "仅队列区") {
		t.Fatalf("unexpected client contextual footer at 80x24:\n%s", footer)
	}

	// 验证身份切换/批量操作执行期间防重入保护
	m.modal = modalIdentity
	m.idChoices = []identityChoice{{profile: identity.Profile{Label: "alice@127.0.0.1:7443", Config: config.DefaultClient()}, imported: false}}
	m.activatingID = true
	_, dupCmd := m.Update(keyType(tea.KeyEnter))
	if dupCmd != nil {
		t.Fatal("expected duplicate identity activation submit to be blocked while activatingID=true")
	}
	m.activatingID = false

	m.modal = modalNone
	mgr, err := queue.Open(queue.Config{StateDir: filepath.Join(dir, "st"), Identity: "id1", MaxFileBytes: 1 << 20, MaxStreams: 2})
	if err != nil {
		t.Fatal(err)
	}
	m.manager = mgr
	defer m.cleanup()
	m.opBusy = true
	m.focus = areaQueue
	if cmd := m.toggleAllCmd(); cmd != nil {
		t.Fatal("expected toggleAllCmd to block concurrent submission when opBusy=true")
	}
}

func TestClientIdentityPreviewLabelEditConnTestAndMultiDiscovery(t *testing.T) {
	cliCfg, _, cleanup := setupTestClientEnv(t)
	defer cleanup()

	base := t.TempDir()
	certsDir := filepath.Join(base, "certs")
	if err := testcert.Generate(certsDir); err != nil {
		t.Fatal(err)
	}
	if err := testcert.IssueClient(certsDir, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := testcert.IssueClient(certsDir, "bob"); err != nil {
		t.Fatal(err)
	}

	multiDir := filepath.Join(base, "multi")
	if err := os.MkdirAll(multiDir, 0755); err != nil {
		t.Fatal(err)
	}
	c1 := cliCfg
	c1.TLS.Certificate = filepath.Join(filepath.Dir(cliCfg.TLS.Certificate), "alice.pem")
	c2 := cliCfg
	c2.StateDir = filepath.Join(base, "state-bob")
	c2.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "bob.pem"),
		PrivateKey:    filepath.Join(certsDir, "bob-key.pem"),
	}
	if err := config.SaveJSON(filepath.Join(multiDir, "client.alice.json"), c1); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveJSON(filepath.Join(multiDir, "client.bob.json"), c2); err != nil {
		t.Fatal(err)
	}

	m := newModel(Options{ExecutableDir: base, Directory: base})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.modal = modalIdentity

	// 1. 输入包含多个 client*.json 的目录：应展开为多个可选项而非报错拒绝
	m.idInputFocus = true
	m.idInput.SetValue(multiDir)
	_, discCmd := m.Update(keyType(tea.KeyEnter))
	if discCmd == nil {
		t.Fatal("expected discover cmd for multi-config directory")
	}
	m.Update(discCmd())
	if len(m.idChoices) < 2 || m.idInputFocus {
		t.Fatalf("expected >=2 discovered candidates and focus returned to list, len=%d inputFocus=%v toast=%q", len(m.idChoices), m.idInputFocus, m.statusToast)
	}

	// 2. 验证 80x24 下预览卡渲染不超终端高度，且显示预览信息
	view80 := ansi.Strip(m.View())
	if lipgloss.Height(view80) > 24 {
		t.Fatalf("identity preview modal overflowed 80x24: height=%d\n%s", lipgloss.Height(view80), view80)
	}
	if !strings.Contains(view80, "预览:") || !strings.Contains(view80, "指纹:") {
		t.Fatalf("expected identity preview details in modalIdentity:\n%s", view80)
	}

	// 3. 测试选中候选身份（指向真实存活 daemon 的 alice）按 t 测试连接
	aliceIdx := -1
	for i, ch := range m.idChoices {
		if strings.Contains(ch.profile.User, "alice") {
			aliceIdx = i
			break
		}
	}
	if aliceIdx < 0 {
		t.Fatalf("expected alice candidate in %+v", m.idChoices)
	}
	m.idCursor = aliceIdx
	_, testCmd := m.Update(keyRune('t'))
	if testCmd == nil {
		t.Fatal("expected testIdentityConnCmd on key t")
	}
	m.Update(testCmd())
	if !strings.Contains(ansi.Strip(m.View()), "mTLS 握手通过") {
		t.Fatalf("expected mTLS handshake success in preview card:\n%s", ansi.Strip(m.View()))
	}

	// 4. 先导入激活，再按 e 编辑标签并验证持久化
	_, actCmd := m.Update(keyType(tea.KeyEnter))
	if actCmd == nil {
		t.Fatal("expected activate cmd")
	}
	m.Update(actCmd())
	if m.cfg == nil {
		t.Fatalf("expected client identity activated, toast=%q", m.statusToast)
	}

	m.Update(keyRune('i'))
	m.Update(m.scanIdentitiesCmd()())
	m.Update(keyRune('e'))
	if !m.editingIDLabel {
		t.Fatal("expected e to enter editingIDLabel mode")
	}
	m.idLabelInput.SetValue("研发生产主节点")
	m.Update(keyType(tea.KeyEnter))
	if m.editingIDLabel || m.identityLabel != "研发生产主节点" {
		t.Fatalf("expected updated active label 研发生产主节点, editing=%v label=%q toast=%q", m.editingIDLabel, m.identityLabel, m.statusToast)
	}
	profs, err := identity.Profiles(base)
	if err != nil || len(profs) == 0 || profs[0].Label != "研发生产主节点" {
		t.Fatalf("expected updated label persisted to disk: %+v %v", profs, err)
	}
}

func TestClientIdentitySwitchWarnsAndConfirmsUnfinishedTasks(t *testing.T) {
	cliCfg, _, cleanup := setupTestClientEnv(t)
	defer cleanup()

	base := t.TempDir()
	srcFile := filepath.Join(base, "payload.bin")
	if err := os.WriteFile(srcFile, bytes.Repeat([]byte("z"), 512), 0600); err != nil {
		t.Fatal(err)
	}
	altCfg := cliCfg
	altCfg.StateDir = filepath.Join(base, "alt-state")
	altPath := filepath.Join(base, "client.alt.json")
	if err := config.SaveJSON(altPath, altCfg); err != nil {
		t.Fatal(err)
	}

	m := newModel(Options{ExecutableDir: base, Directory: base})
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 26})
	opened := openIdentityStack(cliCfg, "alice@main")
	if opened.err != nil {
		t.Fatal(opened.err)
	}
	m.Update(opened)
	m.manager.SetRunner(nil) // 保持入队任务处于 Waiting/Paused，不依赖远端完成
	defer m.cleanup()

	if _, err := m.manager.Enqueue([]queue.Spec{{
		Direction: protocol.DirectionUpload,
		Remote:    "payload.bin",
		Path:      srcFile,
		Size:      512,
	}}); err != nil {
		t.Fatal(err)
	}
	awaitClientQueueSnapshot(t, m, func() bool { return len(m.activeTasks) == 1 })

	// 在有 1 个未完成任务时选择切换新身份，必须先弹出未完成任务影响确认
	prof, err := identity.PrepareImport(base, base, altPath)
	if err != nil {
		t.Fatal(err)
	}
	m.modal = modalIdentity
	m.idChoices = []identityChoice{{profile: prof, imported: true}}
	m.idCursor = 0
	_, cmd := m.Update(keyType(tea.KeyEnter))
	if cmd != nil || m.pendingSwitch == nil {
		t.Fatalf("expected switch confirmation prompt when unfinished tasks exist, cmd=%v pending=%+v", cmd, m.pendingSwitch)
	}
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "未完成任务：共 1 个") || !strings.Contains(v, "安全暂停") {
		t.Fatalf("expected unfinished task count and pause impact warning in modal:\n%s", v)
	}

	// Esc 取消确认后保持原有身份且任务仍在队列
	m.Update(keyType(tea.KeyEsc))
	if m.pendingSwitch != nil || m.modal != modalIdentity || m.identityLabel != "alice@main" {
		t.Fatalf("expected Esc to return to list keeping original identity, pending=%v modal=%v label=%q", m.pendingSwitch, m.modal, m.identityLabel)
	}

	// 再次 Enter 并确认 'y' 后，真正执行切换
	_, cmd = m.Update(keyType(tea.KeyEnter))
	if m.pendingSwitch == nil || cmd != nil {
		t.Fatalf("expected pendingSwitch before confirmation")
	}
	_, confirmCmd := m.Update(keyRune('y'))
	if confirmCmd == nil {
		t.Fatal("expected activation cmd after confirming switch")
	}
	m.Update(confirmCmd())
	if m.activatingID {
		t.Fatal("expected activatingID cleared after identityActivatedMsg")
	}
}

func TestClientRecoveryNoticeBatchSummaryAndRetryFailed(t *testing.T) {
	cliCfg, _, cleanup := setupTestClientEnv(t)
	defer cleanup()

	dir := t.TempDir()
	okFile := filepath.Join(dir, "ok.bin")
	if err := os.WriteFile(okFile, bytes.Repeat([]byte("A"), 256), 0600); err != nil {
		t.Fatal(err)
	}
	// 先写入预设未完成断点任务，验证打开身份栈后的恢复提示
	preMgr, err := queue.Open(queue.Config{
		StateDir:     cliCfg.StateDir,
		Identity:     "alice-recovery",
		MaxFileBytes: cliCfg.MaxFileBytes,
		MaxStreams:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	preTasks, err := preMgr.Enqueue([]queue.Spec{{
		Direction: protocol.DirectionUpload,
		Remote:    "ok.bin",
		Path:      okFile,
		Size:      256,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := preMgr.Pause(preTasks[0].ID); err != nil {
		t.Fatal(err)
	}
	_ = preMgr.Close()

	m := newModel(Options{ExecutableDir: dir, Directory: dir})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	opened := openIdentityStack(cliCfg, "alice-recover")
	if opened.err != nil {
		t.Fatal(opened.err)
	}
	// 同 identityKey 若为首次打开（先手动打开匹配的 stateKey 任务，或直接打开真实 identityKey 任务）
	_ = opened.manager.Close()
	_ = opened.runner.Close()
	pre2, err := queue.Open(queue.Config{
		StateDir:     cliCfg.StateDir,
		Identity:     opened.idKey,
		MaxFileBytes: cliCfg.MaxFileBytes,
		MaxStreams:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t2, err := pre2.Enqueue([]queue.Spec{{
		Direction: protocol.DirectionUpload,
		Remote:    "ok.bin",
		Path:      okFile,
		Size:      256,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := pre2.Pause(t2[0].ID); err != nil {
		t.Fatal(err)
	}
	_ = pre2.Close()

	reopened := openIdentityStack(cliCfg, "alice-recover")
	if reopened.err != nil {
		t.Fatal(reopened.err)
	}
	m.Update(reopened)
	defer m.cleanup()

	if !strings.Contains(m.recoveryNotice, "检测到 1 个未完成断点任务") {
		t.Fatalf("expected unfinished recovery notice after loading identity, got %q", m.recoveryNotice)
	}
	if !strings.Contains(ansi.Strip(m.View()), "检测到 1 个未完成断点任务") {
		t.Fatalf("expected recovery banner visible in queue view:\n%s", ansi.Strip(m.View()))
	}

	// 取消旧任务并入队含 Skip + 失败(源文件被删除触发 preflight failure)的新批次，验证批次摘要与 R 重试
	if err := m.manager.Cancel(t2[0].ID); err != nil {
		t.Fatal(err)
	}
	m.refreshQueueSnapshots()

	missingFile := filepath.Join(dir, "will-fix.bin")
	cmd := m.enqueueSpecsCmd([]queue.Spec{
		{Direction: protocol.DirectionUpload, Remote: "skip.bin", Path: okFile, Skip: "用户跳过重名文件"},
		{Direction: protocol.DirectionUpload, Remote: "will-fix.bin", Path: missingFile, Size: 128},
	})
	m.Update(cmd())
	m.refreshQueueSnapshots()

	if m.lastBatch == nil || m.lastBatch.Total != 2 || m.lastBatch.Skipped != 1 || m.lastBatch.Failed != 1 {
		t.Fatalf("expected settled batch summary (total=2, skipped=1, failed=1), got %+v", m.lastBatch)
	}
	m.Update(keyRune('B'))
	if m.modal != modalBatchSummary {
		t.Fatalf("expected modalBatchSummary on key B, got %v", m.modal)
	}
	bv := ansi.Strip(m.View())
	if !strings.Contains(bv, "成功 0 · 失败 1 · 取消 0 · 跳过 1") || !strings.Contains(bv, "will-fix.bin") {
		t.Fatalf("unexpected batch summary modal content:\n%s", bv)
	}

	// 补齐缺失源文件后在摘要弹窗按 R 重试失败项，应成功恢复为等待/完成
	if err := os.WriteFile(missingFile, bytes.Repeat([]byte("R"), 128), 0600); err != nil {
		t.Fatal(err)
	}
	_, retryCmd := m.Update(keyRune('R'))
	if retryCmd == nil {
		t.Fatal("expected retry cmd on key R in batch summary")
	}
	m.Update(retryCmd())

	deadline := time.Now().Add(8 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		m.refreshQueueSnapshots()
		for _, t := range m.historyTasks {
			if t.Remote == "will-fix.bin" && t.State == queue.Completed {
				completed = true
				break
			}
		}
		if completed && m.failedTaskCount() == 0 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !completed || m.failedTaskCount() != 0 {
		t.Fatalf("expected retried failed task to complete, active=%+v hist=%+v", m.activeTasks, m.historyTasks)
	}
}

func TestClientToastAutoExpiryAndModalValidationPersistence(t *testing.T) {
	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir()})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})

	m.Update(keyRune('o'))
	if m.statusToast == "" {
		t.Fatal("expected sort toast after o")
	}
	m.Update(tickMsg(time.Now().Add(1 * time.Second)))
	if m.statusToast == "" {
		t.Fatal("client toast cleared too early")
	}
	m.Update(tickMsg(time.Now().Add(3 * time.Second)))
	if m.statusToast != "" {
		t.Fatalf("expected client toast to auto-dismiss after 2-3s, got %q", m.statusToast)
	}

	m.modal = modalRename
	m.renameTask = &queue.Task{ID: "t1", Direction: protocol.DirectionUpload, Remote: "a.bin", Path: "/tmp/a.bin", State: queue.Paused}
	m.renameInput.SetValue("bad/name")
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalRename || m.statusToast == "" {
		t.Fatalf("expected rename validation error inside modal, modal=%v toast=%q", m.modal, m.statusToast)
	}
	m.Update(tickMsg(time.Now().Add(10 * time.Second)))
	if m.statusToast == "" {
		t.Fatal("validation error inside open client modal expired prematurely")
	}
	m.Update(keyType(tea.KeyEsc))
	if m.modal != modalNone || m.statusToast != "" {
		t.Fatalf("expected Esc to close modal and clear validation toast, modal=%v toast=%q", m.modal, m.statusToast)
	}
}

func TestClientValidIdentityQueueInitFailurePreservesCause(t *testing.T) {
	dir := t.TempDir()
	certsDir := filepath.Join(dir, "certs")
	if err := testcert.Generate(certsDir); err != nil {
		t.Fatal(err)
	}
	if err := testcert.IssueClient(certsDir, "alice"); err != nil {
		t.Fatal(err)
	}
	blockedState := filepath.Join(dir, "blocked-state")
	if err := os.WriteFile(blockedState, []byte("ordinary file"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultClient()
	cfg.ServerAddress = "127.0.0.1:7443"
	cfg.ServerName = "localhost"
	cfg.StateDir = blockedState
	cfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "alice.pem"),
		PrivateKey:    filepath.Join(certsDir, "alice-key.pem"),
	}
	cfgPath := filepath.Join(dir, "client.json")
	if err := config.SaveJSON(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir(), ConfigPath: cfgPath})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})
	rawMsg := m.initIdentityCmd()()
	actMsg, ok := rawMsg.(identityActivatedMsg)
	if !ok || actMsg.err == nil {
		t.Fatalf("expected queue init error, got %#v", rawMsg)
	}
	if actMsg.stage != identityStageQueue || !actMsg.hasCfg || actMsg.label != "SFT alice@127.0.0.1:7443" {
		t.Fatalf("unexpected stage/context: %+v", actMsg)
	}
	m.Update(actMsg)
	if m.modal != modalIdentity {
		t.Fatalf("expected modalIdentity, got %v", m.modal)
	}
	view := ansi.Strip(m.View())
	if strings.Contains(view, "未找到已激活身份") {
		t.Fatalf("valid identity queue failure misreported as missing identity:\n%s", view)
	}
	if !strings.Contains(view, "队列初始化失败") || !strings.Contains(view, "not a directory") {
		t.Fatalf("expected queue failure and cause in modal view:\n%s", view)
	}
	if !strings.Contains(view, "SFT alice@127.0.0.1:7443") {
		t.Fatalf("expected resolved identity label retained in view:\n%s", view)
	}
}

func TestClientValidIdentityTLSFailurePreservesCause(t *testing.T) {
	dir := t.TempDir()
	certsDir := filepath.Join(dir, "certs")
	if err := testcert.Generate(certsDir); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultClient()
	cfg.ServerAddress = "127.0.0.1:7443"
	cfg.ServerName = "localhost"
	cfg.StateDir = filepath.Join(dir, "state")
	cfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "missing-alice.pem"),
		PrivateKey:    filepath.Join(certsDir, "missing-alice-key.pem"),
	}
	cfgPath := filepath.Join(dir, "client.json")
	if err := config.SaveJSON(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir(), ConfigPath: cfgPath})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})
	rawMsg := m.initIdentityCmd()()
	actMsg, ok := rawMsg.(identityActivatedMsg)
	if !ok || actMsg.err == nil {
		t.Fatalf("expected TLS init error, got %#v", rawMsg)
	}
	if actMsg.stage != identityStageTLS || !actMsg.hasCfg {
		t.Fatalf("unexpected stage/context: %+v", actMsg)
	}
	m.Update(actMsg)
	view := ansi.Strip(m.View())
	if strings.Contains(view, "未找到已激活身份") {
		t.Fatalf("valid identity TLS failure misreported as missing identity:\n%s", view)
	}
	if !strings.Contains(view, "TLS 证书初始化失败") {
		t.Fatalf("expected TLS initialization error in view:\n%s", view)
	}
}

func TestClientMissingConfigOpensIdentityPicker(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "missing-client.json")
	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir(), ConfigPath: missingPath})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})
	rawMsg := m.initIdentityCmd()()
	actMsg, ok := rawMsg.(identityActivatedMsg)
	if !ok || actMsg.err == nil {
		t.Fatalf("expected lookup error for missing config, got %#v", rawMsg)
	}
	if actMsg.stage != identityStageLookup || !isMissingIdentityErr(actMsg.err) {
		t.Fatalf("expected lookup missing-identity error, got stage=%v err=%v", actMsg.stage, actMsg.err)
	}
	m.Update(actMsg)
	view := ansi.Strip(m.View())
	if !strings.Contains(view, "身份管理") || !strings.Contains(view, "未找到已激活身份") || !strings.Contains(view, "选择") || !strings.Contains(view, "导入") {
		t.Fatalf("missing config view did not preserve onboarding guidance:\n%s", view)
	}
}

func TestClientStartupLoadsCustomLabelAndStagesUnimportedLabel(t *testing.T) {
	base := t.TempDir()
	certsDir := filepath.Join(base, "certs")
	if err := testcert.Generate(certsDir); err != nil {
		t.Fatal(err)
	}
	if err := testcert.IssueClient(certsDir, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := testcert.IssueClient(certsDir, "bob"); err != nil {
		t.Fatal(err)
	}
	srcDir := filepath.Join(base, "src")
	if err := os.MkdirAll(srcDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultClient()
	cfg.ServerAddress = "127.0.0.1:7443"
	cfg.ServerName = "localhost"
	cfg.StateDir = filepath.Join(base, "state")
	cfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "alice.pem"),
		PrivateKey:    filepath.Join(certsDir, "alice-key.pem"),
	}
	srcCfgPath := filepath.Join(srcDir, "client.alice.local.json")
	if err := config.SaveJSON(srcCfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	exeDir := filepath.Join(base, "exe")
	prof, err := identity.PrepareImport(exeDir, base, srcCfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identity.SelectPrepared(exeDir, prof, "自定义别名A"); err != nil {
		t.Fatal(err)
	}

	// 启动加载已保存身份时应带出自定义标签而非回退为 user@host。
	m := newModel(Options{ExecutableDir: exeDir, Directory: base})
	m.Init()
	actMsg, ok := m.initIdentityCmd()().(identityActivatedMsg)
	if !ok || actMsg.err != nil {
		t.Fatalf("expected active profile load success, got %#v", actMsg)
	}
	if actMsg.activeID != prof.ID || actMsg.label != "自定义别名A" {
		t.Fatalf("unexpected startup profile metadata: id=%q label=%q", actMsg.activeID, actMsg.label)
	}
	m.Update(actMsg)
	m.cleanup()
	if m.identityLabel != "自定义别名A" || m.activeID != prof.ID {
		t.Fatalf("expected model to adopt saved label, got id=%q label=%q", m.activeID, m.identityLabel)
	}

	// 未导入身份修改标签后立即激活，应保留暂存标签。
	unimported := filepath.Join(base, "client.new.json")
	cfg2 := cfg
	cfg2.StateDir = filepath.Join(base, "state2")
	if err := config.SaveJSON(unimported, cfg2); err != nil {
		t.Fatal(err)
	}
	m2 := newModel(Options{ExecutableDir: filepath.Join(base, "exe2"), Directory: base})
	m2.modal = modalIdentity
	m2.idChoices = []identityChoice{{
		profile: identity.Profile{Label: "orig-label", Path: unimported, Config: cfg2},
	}}
	m2.idCursor = 0
	m2.editingIDLabel = true
	m2.idLabelInput.SetValue("我的生产客户端")
	m2.handleModalKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m2.idChoices[0].profile.Label != "我的生产客户端" {
		t.Fatalf("staged label not applied to choice: %q", m2.idChoices[0].profile.Label)
	}
	raw := m2.activateChoiceCmd(m2.idChoices[0], "")()
	act2, ok := raw.(identityActivatedMsg)
	if !ok || act2.err != nil || act2.label != "我的生产客户端" {
		t.Fatalf("staged label lost during import activation: %#v", raw)
	}
	m2.Update(act2)
	m2.cleanup()
}

func TestClientOpBusyPreservedAcrossSingleOpAndCrossDirConflictScan(t *testing.T) {
	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: t.TempDir()})
	m.opBusy = true
	m.opLabel = "正在全部暂停"
	m.opStart = time.Now()
	m.Update(batchOpResultMsg{notice: "单条取消", isGlobalOp: false})
	if !m.opBusy {
		t.Fatal("non-global batchOpResultMsg prematurely cleared opBusy")
	}
	m.Update(batchOpResultMsg{notice: "已全部暂停", isGlobalOp: true})
	if m.opBusy || m.opLabel != "" {
		t.Fatalf("global batchOpResultMsg failed to clear opBusy/opLabel: busy=%v label=%q", m.opBusy, m.opLabel)
	}

	// 验证目标目录与当前视图目录不同（如历史任务重试）时，自动改名应扫描目标目录磁盘文件。
	targetDir := t.TempDir()
	for _, name := range []string{"report.bin", "report (1).bin"} {
		if err := os.WriteFile(filepath.Join(targetDir, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m.localEntries = nil // 模拟当前浏览目录与目标目录不同
	m.pendingBatchDir = protocol.DirectionDownload
	m.pendingSpecs = []queue.Spec{{
		Direction: protocol.DirectionDownload,
		Remote:    "report.bin",
		Path:      filepath.Join(targetDir, "report.bin"),
	}}
	m.conflictNames = []string{"report.bin"}
	renamed := m.resolveConflictsAutoRename()
	want := filepath.Join(targetDir, "report (2).bin")
	if len(renamed) != 1 || renamed[0].Path != want {
		t.Fatalf("expected target dir scan to pick %q, got %+v", want, renamed)
	}
}

func TestClientNarrowSubHeaderToastAndScrollableConflictModal(t *testing.T) {
	m := newModel(Options{ExecutableDir: t.TempDir(), Directory: "/very/long/nested/directory/path/for/narrow/terminal/testing"})
	m.Update(tea.WindowSizeMsg{Width: 64, Height: 18})
	m.setToast("已入队 3 个传输任务")

	sub := ansi.Strip(m.renderSubHeader())
	if lipgloss.Height(sub) != 1 || lipgloss.Width(sub) > 64 {
		t.Fatalf("narrow subheader overflowed 64 cols: h=%d w=%d %q", lipgloss.Height(sub), lipgloss.Width(sub), sub)
	}
	if !strings.Contains(sub, "已入队 3 个传输任务") {
		t.Fatalf("expected toast visible in narrow 64-col subheader, got %q", sub)
	}

	// 冲突弹窗含大量冲突文件时，在 50x14 极小窗口内支持滚动且不溢出高度/宽度
	m.Update(tea.WindowSizeMsg{Width: 50, Height: 14})
	m.pendingBatchDir = protocol.DirectionUpload
	for i := 0; i < 15; i++ {
		name := "conflict-" + string(rune('a'+i)) + ".bin"
		m.conflictNames = append(m.conflictNames, name)
		m.pendingSpecs = append(m.pendingSpecs, queue.Spec{Direction: protocol.DirectionUpload, Remote: name})
	}
	m.infoScroll = 0
	m.modal = modalConflict

	v0 := ansi.Strip(m.View())
	if lipgloss.Height(v0) > 14 {
		t.Fatalf("conflict modal exceeded 14 rows: got %d\n%s", lipgloss.Height(v0), v0)
	}
	for i, ln := range strings.Split(v0, "\n") {
		if lipgloss.Width(ln) > 50 {
			t.Fatalf("conflict modal line %d exceeded 50 cols: %q", i+1, ln)
		}
	}
	m.Update(keyType(tea.KeyDown))
	if m.infoScroll == 0 {
		t.Fatal("expected Down key to scroll long conflict modal")
	}
	m.Update(keyType(tea.KeyEnd))
	vEnd := ansi.Strip(m.View())
	if !strings.Contains(vEnd, "跳过冲突文件") {
		t.Fatalf("expected conflict action options reachable at bottom of scrollable modal:\n%s", vEnd)
	}
}

func TestClientEscClearSelectAllKeyScopesAndRemoteErrorDetail(t *testing.T) {
	dir := t.TempDir()
	m := newModel(Options{ExecutableDir: dir, Directory: dir})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 28})
	m.localEntries = []localEntry{
		{name: "sub", path: filepath.Join(dir, "sub"), dir: true},
		{name: "alpha.bin", path: filepath.Join(dir, "alpha.bin"), regular: true},
		{name: "beta.bin", path: filepath.Join(dir, "beta.bin"), regular: true},
	}
	m.rebuildLocalVisible()

	// 1. A 全选本地普通文件（不含目录），再按 A 取消全选
	m.focus = areaLocal
	m.Update(keyRune('A'))
	if len(m.localSelected) != 2 {
		t.Fatalf("expected A to select 2 regular files, got %d", len(m.localSelected))
	}
	// 普通模式下 Esc 清空筛选优先，其次清空当前区多选
	m.query = "alpha"
	m.searchInput.SetValue("alpha")
	m.rebuildLocalVisible()
	m.Update(keyType(tea.KeyEsc))
	if m.query != "" || len(m.localSelected) != 2 {
		t.Fatalf("first Esc should clear search query keeping selection: query=%q sel=%d", m.query, len(m.localSelected))
	}
	m.Update(keyType(tea.KeyEsc))
	if len(m.localSelected) != 0 {
		t.Fatalf("second Esc should clear local selection, got %d", len(m.localSelected))
	}

	// 2. 队列区按 o 不得修改本地排序；g/G 支持首尾跳转；h 从文件区直达历史归档
	m.activeTasks = []queue.Task{{ID: "t1"}, {ID: "t2"}, {ID: "t3"}}
	m.focus = areaQueue
	origLocalSort := m.localSort
	m.Update(keyRune('o'))
	if m.localSort != origLocalSort {
		t.Fatalf("o in queue unexpectedly mutated localSort: %v -> %v", origLocalSort, m.localSort)
	}
	m.Update(keyRune('G'))
	if m.queueCursor != 2 {
		t.Fatalf("G in queue did not jump to end: cursor=%d", m.queueCursor)
	}
	m.Update(keyRune('g'))
	if m.queueCursor != 0 {
		t.Fatalf("g in queue did not jump to start: cursor=%d", m.queueCursor)
	}

	m.focus = areaLocal
	m.historyView = true
	m.Update(keyRune('h'))
	if m.focus != areaQueue || !m.historyView {
		t.Fatalf("h from local pane should open history view, got focus=%v hist=%v", m.focus, m.historyView)
	}

	// 3. 远端离线报错时按 Enter 打开完整错误详情弹窗
	m.focus = areaRemote
	m.visibleRemote = nil
	m.remoteErr = "x509: certificate signed by unknown authority (detailed diagnostic)"
	m.Update(keyType(tea.KeyEnter))
	if m.modal != modalDetail || !strings.Contains(ansi.Strip(m.View()), "unknown authority") {
		t.Fatalf("expected Enter on remote error to open detail modal, modal=%v", m.modal)
	}
}
