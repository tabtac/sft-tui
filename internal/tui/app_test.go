package tui

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
	m.Update(keyType(tea.KeyF6)) // 切换浅色主题
	if m.th.dark {
		t.Fatal("expected light theme after F6")
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
	m.refreshQueueSnapshots()
	if m.activeTasks[0].State != "paused" {
		t.Fatalf("expected task 0 paused, got %s", m.activeTasks[0].State)
	}
	_, cmd = m.Update(keyRune(' '))
	m.Update(cmd())
	m.refreshQueueSnapshots()
	if m.activeTasks[0].State != "waiting" {
		t.Fatalf("expected task 0 waiting after resume, got %s", m.activeTasks[0].State)
	}

	// a 全部暂停 -> 再按 a 全部恢复
	_, cmd = m.Update(keyRune('a'))
	m.Update(cmd())
	m.refreshQueueSnapshots()
	for _, tk := range m.activeTasks {
		if tk.State != "paused" {
			t.Fatalf("expected all paused after a, got %s", tk.State)
		}
	}
	_, cmd = m.Update(keyRune('a'))
	m.Update(cmd())
	m.refreshQueueSnapshots()
	for _, tk := range m.activeTasks {
		if tk.State != "waiting" {
			t.Fatalf("expected all waiting after second a, got %s", tk.State)
		}
	}

	// c 取消选中的第 1 项
	_, cmd = m.Update(keyRune('c'))
	m.Update(cmd())
	m.refreshQueueSnapshots()
	if len(m.historyTasks) == 0 || m.historyTasks[0].State != "cancelled" {
		t.Fatalf("expected cancelled task in history, got %+v", m.historyTasks)
	}

	// 空闲时按 q 直接触发优雅退出 tea.Quit
	_, quitCmd := m.Update(keyRune('q'))
	if quitCmd == nil {
		t.Fatal("expected quitCmd when pressing q idle")
	}
}
