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
