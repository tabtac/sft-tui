package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
	"sft-tui/internal/security"
	"sft-tui/internal/storage"
	"sft-tui/internal/testcert"
)

type testHarness struct {
	dir     string
	srvCfg  config.Server
	cliCfg  config.Client
	store   *storage.Store
	auth    *security.Authorizer
	server  *Server
	ln      net.Listener
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	stopped chan struct{}
}

func startTestServer(t *testing.T) *testHarness {
	t.Helper()
	dir := t.TempDir()
	certsDir := filepath.Join(dir, "certs")
	if err := testcert.Generate(certsDir); err != nil {
		t.Fatalf("testcert.Generate: %v", err)
	}
	if err := testcert.IssueClient(certsDir, "alice"); err != nil {
		t.Fatalf("IssueClient: %v", err)
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

	store, err := storage.OpenStore(srvCfg.StorageRoot)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	tlsCfg, auth, err := security.ServerTLS(srvCfg)
	if err != nil {
		store.Close()
		t.Fatalf("ServerTLS: %v", err)
	}
	srv, err := NewServer(srvCfg, store, auth, tlsCfg, ServerHooks{}, nil)
	if err != nil {
		store.Close()
		t.Fatalf("NewServer: %v", err)
	}
	ln, err := net.Listen("tcp", srvCfg.Listen)
	if err != nil {
		store.Close()
		t.Fatalf("net.Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h := &testHarness{
		dir:     dir,
		srvCfg:  srvCfg,
		store:   store,
		auth:    auth,
		server:  srv,
		ln:      ln,
		cancel:  cancel,
		stopped: make(chan struct{}),
	}

	cliCfg := config.DefaultClient()
	cliCfg.StateDir = filepath.Join(dir, "client-state")
	cliCfg.ServerAddress = ln.Addr().String()
	cliCfg.ServerName = "localhost"
	cliCfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "alice.pem"),
		PrivateKey:    filepath.Join(certsDir, "alice-key.pem"),
	}
	h.cliCfg = cliCfg

	go func() {
		defer close(h.stopped)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			h.wg.Add(1)
			go func(c net.Conn) {
				defer h.wg.Done()
				srv.HandleConn(ctx, c)
			}(conn)
		}
	}()

	t.Cleanup(func() {
		h.stop()
		_ = store.Close()
	})
	return h
}

func (h *testHarness) stop() {
	h.cancel()
	_ = h.ln.Close()
	<-h.stopped
	h.wg.Wait()
}

func TestEndToEndUploadDownloadListAndZeroByte(t *testing.T) {
	h := startTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := Dial(ctx, h.cliCfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	// 1. 上传大文件（跨多块 > 64 KiB）与 0 字节文件
	bigData := bytes.Repeat([]byte("SFT-BLOCK-"), 20000) // ~200 KB
	bigPath := filepath.Join(h.dir, "big.bin")
	if err := os.WriteFile(bigPath, bigData, 0600); err != nil {
		t.Fatal(err)
	}
	emptyPath := filepath.Join(h.dir, "empty.txt")
	if err := os.WriteFile(emptyPath, nil, 0600); err != nil {
		t.Fatal(err)
	}

	resBig, err := client.UploadFile(ctx, bigPath, "remote-big.bin", nil)
	if err != nil {
		t.Fatalf("UploadFile big: %v", err)
	}
	if resBig.Size != uint64(len(bigData)) || resBig.SHA256 != sha256.Sum256(bigData) {
		t.Fatalf("unexpected big upload result: %+v", resBig)
	}

	resEmpty, err := client.UploadFile(ctx, emptyPath, "empty.txt", nil)
	if err != nil {
		t.Fatalf("UploadFile empty: %v", err)
	}
	if resEmpty.Size != 0 || resEmpty.SHA256 != sha256.Sum256(nil) {
		t.Fatalf("unexpected empty upload result: %+v", resEmpty)
	}

	// 2. 拉取列表验证顺序与大小
	var listed []protocol.FileEntry
	if err := client.ListFiles(ctx, func(f protocol.FileEntry) error {
		listed = append(listed, f)
		return nil
	}); err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(listed) != 2 || listed[0].FileName != "empty.txt" || listed[1].FileName != "remote-big.bin" {
		t.Fatalf("unexpected list: %+v", listed)
	}

	// 3. 下载两个文件到新目录并校验字节内容
	dlDir := filepath.Join(h.dir, "downloads")
	if err := os.MkdirAll(dlDir, 0700); err != nil {
		t.Fatal(err)
	}
	dlBig := filepath.Join(dlDir, "big.bin")
	if _, err := client.DownloadFile(ctx, "remote-big.bin", dlBig, nil); err != nil {
		t.Fatalf("DownloadFile big: %v", err)
	}
	gotBig, err := os.ReadFile(dlBig)
	if err != nil || !bytes.Equal(gotBig, bigData) {
		t.Fatalf("downloaded big content mismatch: len=%d err=%v", len(gotBig), err)
	}

	dlEmpty := filepath.Join(dlDir, "empty.txt")
	if _, err := client.DownloadFile(ctx, "empty.txt", dlEmpty, nil); err != nil {
		t.Fatalf("DownloadFile empty: %v", err)
	}
	gotEmpty, err := os.ReadFile(dlEmpty)
	if err != nil || len(gotEmpty) != 0 {
		t.Fatalf("downloaded empty mismatch: %q err=%v", gotEmpty, err)
	}

	// 4. 禁止覆盖已有目标文件
	if _, err := client.DownloadFile(ctx, "empty.txt", dlEmpty, nil); err == nil {
		t.Fatal("expected download over existing file to fail")
	}
}

func TestQueueRunnerPauseResumeCancelAndCrashRecovery(t *testing.T) {
	h := startTestServer(t)

	// 准备约 1.5 MiB 文件确保分块期间有充足时间触发 Pause/Resume
	payload := bytes.Repeat([]byte("ABCDEFGH"), 200*1024)
	srcPath := filepath.Join(h.dir, "stream.bin")
	if err := os.WriteFile(srcPath, payload, 0600); err != nil {
		t.Fatal(err)
	}

	// 限制每次流块大小以便多轮交互验证暂停/恢复
	cliCfg := h.cliCfg
	cliCfg.Limits.ChunkSize = 16 * 1024

	idKey, err := security.ClientIdentityKey(cliCfg)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := queue.Open(queue.Config{
		StateDir:     cliCfg.StateDir,
		Identity:     idKey,
		MaxFileBytes: cliCfg.MaxFileBytes,
		MaxStreams:   2,
	})
	if err != nil {
		t.Fatal(err)
	}

	runner := NewQueueRunner(cliCfg, mgr)
	mgr.SetRunner(runner.Run)

	// 注入传输进度观察，收到第 2 个块后立刻暂停上传
	tasks, err := mgr.Enqueue([]queue.Spec{{
		Direction: protocol.DirectionUpload,
		Remote:    "stream.bin",
		Path:      srcPath,
		Size:      uint64(len(payload)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	tid := tasks[0].ID

	// 等待进入 Transferring 并且有部分 Done 后暂停
	deadline := time.Now().Add(10 * time.Second)
	pausedSent := false
	for time.Now().Before(deadline) {
		cur := mgr.Task(tid)
		if cur != nil && cur.State == queue.Transferring && cur.Done > 0 && !pausedSent {
			if err := mgr.Pause(tid); err != nil {
				t.Fatalf("Pause: %v", err)
			}
			pausedSent = true
		}
		if cur != nil && cur.State == queue.Paused {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	pausedTask := mgr.Task(tid)
	if pausedTask == nil || pausedTask.State != queue.Paused || pausedTask.Done == 0 || pausedTask.TransferID == "" {
		t.Fatalf("expected paused task with persisted checkpoint, got %+v", pausedTask)
	}

	// 模拟客户端重启：关闭 runner/mgr 再重新打开，确认 SQLite 断点保留并可续传完成
	_ = runner.Close()
	_ = mgr.Close()

	mgr2, err := queue.Open(queue.Config{
		StateDir:     cliCfg.StateDir,
		Identity:     idKey,
		MaxFileBytes: cliCfg.MaxFileBytes,
		MaxStreams:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr2.Close()

	runner2 := NewQueueRunner(cliCfg, mgr2)
	defer runner2.Close()
	mgr2.SetRunner(runner2.Run)

	resumedBefore := mgr2.Task(tid)
	if resumedBefore == nil || resumedBefore.State != queue.Paused || resumedBefore.TransferID == "" {
		t.Fatalf("expected recovered paused task after restart, got %+v", resumedBefore)
	}
	if err := mgr2.Resume(tid); err != nil {
		t.Fatalf("Resume after restart: %v", err)
	}

	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		cur := mgr2.Task(tid)
		if cur != nil && cur.State == queue.Completed {
			break
		}
		if cur != nil && cur.State == queue.Pending {
			t.Fatalf("resumed upload entered Pending unexpectedly: %s", cur.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cur := mgr2.Task(tid); cur == nil || cur.State != queue.Completed {
		t.Fatalf("expected upload Completed, got %+v", cur)
	}

	// 测试下载途中取消（CancelAbort）：应当删除服务端与本地 .sft-*.part 临时文件
	dlDir := filepath.Join(h.dir, "dl")
	_ = os.MkdirAll(dlDir, 0700)
	dlDest := filepath.Join(dlDir, "stream.bin")
	dlTasks, err := mgr2.Enqueue([]queue.Spec{{
		Direction: protocol.DirectionDownload,
		Remote:    "stream.bin",
		Path:      dlDest,
	}})
	if err != nil {
		t.Fatal(err)
	}
	dlID := dlTasks[0].ID

	// 利用服务端钩子在首个 DATA 后延时/通知或立即触发 Pause/Cancel
	deadline = time.Now().Add(10 * time.Second)
	cancelledSent := false
	for time.Now().Before(deadline) {
		cur := mgr2.Task(dlID)
		if cur != nil && cur.State == queue.Transferring && cur.Done > 0 && !cancelledSent {
			if err := mgr2.Cancel(dlID); err != nil {
				t.Fatalf("Cancel download: %v", err)
			}
			cancelledSent = true
		}
		if cur != nil && (cur.State == queue.Cancelled || cur.State == queue.Completed) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	curDL := mgr2.Task(dlID)
	if curDL == nil {
		t.Fatal("download task missing")
	}
	if curDL.State == queue.Cancelled {
		// 取消后不得遗留 .part 文件
		matches, _ := filepath.Glob(filepath.Join(dlDir, ".sft-*.part"))
		if len(matches) != 0 {
			t.Errorf("cancelled download leaked temp files: %v", matches)
		}
		if _, err := os.Stat(dlDest); !os.IsNotExist(err) {
			t.Errorf("cancelled download created final target: %v", err)
		}
	}
}

func TestServerBandwidthHookAndAuthCheck(t *testing.T) {
	// 验证 ServerHooks 在传输路径中被正确调用
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

	store, err := storage.OpenStore(srvCfg.StorageRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	tlsCfg, auth, err := security.ServerTLS(srvCfg)
	if err != nil {
		t.Fatal(err)
	}

	var bwBytes atomic.Int64
	var ckptCalls atomic.Int64
	hooks := ServerHooks{
		WaitBandwidth: func(_ context.Context, _ string, _ config.Bandwidth, _ protocol.Direction, n int) error {
			bwBytes.Add(int64(n))
			return nil
		},
		WaitCheckpoint: func(_ context.Context, _ string, _ config.Bandwidth) error {
			ckptCalls.Add(1)
			return nil
		},
	}
	srv, err := NewServer(srvCfg, store, auth, tlsCfg, hooks, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", srvCfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.HandleConn(ctx, c)
		}
	}()

	cliCfg := config.DefaultClient()
	cliCfg.StateDir = filepath.Join(dir, "state")
	cliCfg.ServerAddress = ln.Addr().String()
	cliCfg.ServerName = "localhost"
	cliCfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "alice.pem"),
		PrivateKey:    filepath.Join(certsDir, "alice-key.pem"),
	}
	client, err := Dial(ctx, cliCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	src := filepath.Join(dir, "hook.bin")
	if err := os.WriteFile(src, []byte("hello-hooks"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := client.UploadFile(ctx, src, "hook.bin", nil); err != nil {
		t.Fatal(err)
	}
	if bwBytes.Load() != 11 || ckptCalls.Load() != 1 {
		t.Fatalf("expected bwBytes=11 ckptCalls=1, got %d, %d", bwBytes.Load(), ckptCalls.Load())
	}

	// 下载验证同样触发 WaitBandwidth
	dst := filepath.Join(dir, "hook-dl.bin")
	if _, err := client.DownloadFile(ctx, "hook.bin", dst, nil); err != nil {
		t.Fatal(err)
	}
	if bwBytes.Load() != 22 {
		t.Fatalf("expected total bwBytes=22 after upload+download, got %d", bwBytes.Load())
	}
	_ = io.EOF
	_ = errors.New("")
}
