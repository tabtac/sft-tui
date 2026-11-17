package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
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
