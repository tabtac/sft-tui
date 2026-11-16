package transfer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/pal"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
	"sft-tui/internal/security"
	"sft-tui/internal/session"
	"sft-tui/internal/testcert"
)

type e2eCountedListener struct {
	net.Listener
	accepted atomic.Int64
}

func (l *e2eCountedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepted.Add(1)
	}
	return c, err
}

type e2eFixture struct {
	dir      string
	certsDir string
	srvCfg   config.Server
	cliCfg   config.Client
	daemon   *Daemon
	listener *e2eCountedListener
	stop     func()
}

func startE2EFixture(t *testing.T, tweak func(*config.Server)) *e2eFixture {
	t.Helper()
	dir := t.TempDir()
	certsDir := filepath.Join(dir, "certs")
	if err := testcert.Generate(certsDir); err != nil {
		t.Fatal(err)
	}
	if err := testcert.IssueClient(certsDir, "alice"); err != nil {
		t.Fatal(err)
	}

	scfg := config.DefaultServer()
	scfg.Listen = "127.0.0.1:0"
	scfg.StorageRoot = filepath.Join(dir, "storage")
	scfg.Bandwidth = config.Bandwidth{
		UploadBytesPerSecond:   512 * 1024 * 1024,
		DownloadBytesPerSecond: 512 * 1024 * 1024,
		BurstBytes:             2 * 1024 * 1024,
	}
	scfg.Resources.MaxNewTasksPerMinute = 10000
	scfg.Resources.MaxUploadCheckpointsPerSecond = 4096
	scfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "server.pem"),
		PrivateKey:    filepath.Join(certsDir, "server-key.pem"),
	}
	scfg.AuthorizedClients = []config.AuthorizedClient{
		{UserID: "alice", Certificate: filepath.Join(certsDir, "alice.pem"), Permissions: []string{"upload", "download"}},
	}
	if tweak != nil {
		tweak(&scfg)
	}

	d, err := NewDaemon(scfg)
	if err != nil {
		t.Fatal(err)
	}
	rawLn, err := net.Listen("tcp", scfg.Listen)
	if err != nil {
		_ = d.Close()
		t.Fatal(err)
	}
	ln := &e2eCountedListener{Listener: rawLn}
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan error, 1)
	go func() { ended <- d.Serve(ctx, ln) }()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			d.Stop(true)
			cancel()
			select {
			case err := <-ended:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("daemon shutdown timeout")
			}
			_ = d.Close()
		})
	}
	t.Cleanup(stop)

	ccfg := config.DefaultClient()
	ccfg.StateDir = filepath.Join(dir, "client-state")
	ccfg.ServerAddress = ln.Addr().String()
	ccfg.ServerName = "localhost"
	ccfg.TLS = config.TLSFiles{
		CACertificate: filepath.Join(certsDir, "ca.pem"),
		Certificate:   filepath.Join(certsDir, "alice.pem"),
		PrivateKey:    filepath.Join(certsDir, "alice-key.pem"),
	}

	return &e2eFixture{
		dir:      dir,
		certsDir: certsDir,
		srvCfg:   scfg,
		cliCfg:   ccfg,
		daemon:   d,
		listener: ln,
		stop:     stop,
	}
}

func (f *e2eFixture) rawConn(t *testing.T) *session.Conn {
	t.Helper()
	tlsCfg, err := security.ClientTLS(f.cliCfg)
	if err != nil {
		t.Fatal(err)
	}
	netConn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", f.cliCfg.ServerAddress, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netConn.Close() })
	c := &session.Conn{Net: netConn, Chunk: f.cliCfg.Limits.ChunkSize, Timeouts: f.cliCfg.Timeouts}
	if err := c.Send(protocol.TypeHello, 0, protocol.Hello{
		ClientMaxStreams: f.cliCfg.Limits.MaxStreams,
		ClientMaxChunk:   f.cliCfg.Limits.ChunkSize,
		ClientWindow:     f.cliCfg.Limits.WindowSize,
	}); err != nil {
		t.Fatal(err)
	}
	frame, err := c.Read(f.cliCfg.Timeouts.HelloSeconds)
	if err != nil || frame.Header.Type != protocol.TypeHelloAck {
		t.Fatalf("HELLO_ACK failed: frame=%v err=%v", frame.Header.Type, err)
	}
	var ack protocol.HelloAck
	if err := protocol.Unmarshal(frame.Payload, &ack); err != nil {
		t.Fatal(err)
	}
	c.Chunk, c.Timeouts.IdleSeconds = ack.ChunkSize, ack.IdleTimeoutSec
	return c
}

func requireRemoteCode(t *testing.T, err error, code protocol.ErrorCode) {
	t.Helper()
	var rem *protocol.RemoteError
	if !errors.As(err, &rem) || rem.Code != code {
		t.Fatalf("expected RemoteError 0x%04x, got %v", code, err)
	}
}

func TestE2EConnectionReuseAndLargeFileIntegrity(t *testing.T) {
	f := startE2EFixture(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := Dial(ctx, f.cliCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// 1. Unicode 文件名、0 字节文件与多 MiB 大文件复用单一 mTLS 连接传输
	largeSize := 8 * 1024 * 1024
	if !testing.Short() {
		largeSize = 32 * 1024 * 1024
	}
	largePath := filepath.Join(f.dir, "验收-大文件.bin")
	lf, err := os.OpenFile(largePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	pattern := make([]byte, 64*1024)
	for i := range pattern {
		pattern[i] = byte((i*31 + 17) % 251)
	}
	hasher := sha256.New()
	for rem := largeSize; rem > 0; {
		n := min(rem, len(pattern))
		if _, err := io.MultiWriter(lf, hasher).Write(pattern[:n]); err != nil {
			lf.Close()
			t.Fatal(err)
		}
		rem -= n
	}
	_ = lf.Close()

	var wantDigest protocol.Digest
	copy(wantDigest[:], hasher.Sum(nil))

	upRes, err := c.UploadFile(ctx, largePath, "验收-大文件.bin", nil)
	if err != nil {
		t.Fatalf("UploadFile large: %v", err)
	}
	outPath := filepath.Join(f.dir, "dl", "验收-大文件.bin")
	_ = os.MkdirAll(filepath.Dir(outPath), 0700)
	dlRes, err := c.DownloadFile(ctx, "验收-大文件.bin", outPath, nil)
	if err != nil {
		t.Fatalf("DownloadFile large: %v", err)
	}
	gotFile, err := os.Open(outPath)
	if err != nil {
		t.Fatal(err)
	}
	gotDigest, gotLen, err := pal.DigestReaderContext(ctx, gotFile)
	_ = gotFile.Close()
	if err != nil || gotLen != uint64(largeSize) || upRes.SHA256 != wantDigest || dlRes.SHA256 != wantDigest || gotDigest != wantDigest {
		t.Fatalf("large file verification failed: gotLen=%d up=%x dl=%x file=%x want=%x err=%v",
			gotLen, upRes.SHA256, dlRes.SHA256, gotDigest, wantDigest, err)
	}

	if count := f.listener.accepted.Load(); count != 1 {
		t.Fatalf("expected single reused TLS connection (accepted=1), got %d", count)
	}
}

func TestE2ESecurityRejectsTLS12WrongSANUntrustedAndUnlisted(t *testing.T) {
	f := startE2EFixture(t, nil)

	// 1. SAN 不匹配
	badSAN := f.cliCfg
	badSAN.ServerName = "evil.invalid"
	if c, err := Dial(context.Background(), badSAN); err == nil {
		_ = c.Close()
		t.Fatal("accepted mismatched SAN")
	}

	// 2. 不受信任的 CA
	otherPKI := filepath.Join(f.dir, "other-pki")
	if err := testcert.Generate(otherPKI); err != nil {
		t.Fatal(err)
	}
	badCA := f.cliCfg
	badCA.TLS.CACertificate = filepath.Join(otherPKI, "ca.pem")
	if c, err := Dial(context.Background(), badCA); err == nil {
		_ = c.Close()
		t.Fatal("accepted untrusted CA")
	}

	// 3. TLS 1.2 降级及缺证书拒绝
	for _, mode := range []string{"tls12", "nocert"} {
		tlsCfg, err := security.ClientTLS(f.cliCfg)
		if err != nil {
			t.Fatal(err)
		}
		if mode == "tls12" {
			tlsCfg.MinVersion, tlsCfg.MaxVersion = tls.VersionTLS12, tls.VersionTLS12
		} else {
			tlsCfg.Certificates = nil
		}
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", f.cliCfg.ServerAddress, tlsCfg)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			p, _ := protocol.Marshal(protocol.Hello{ClientMaxStreams: 1, ClientMaxChunk: 65536, ClientWindow: 1048576})
			_ = protocol.WriteFrame(conn, protocol.TypeHello, 0, p, 65536)
			if _, readErr := protocol.ReadFrame(conn, 65536); readErr == nil {
				_ = conn.Close()
				t.Fatalf("%s unexpectedly completed frame exchange", mode)
			}
			_ = conn.Close()
		}
	}

	// 4. 受信任 CA 签发但不在 AuthorizedClients 列表的客户端证书必须被拒绝
	caData, _ := os.ReadFile(filepath.Join(f.certsDir, "ca.pem"))
	caBlk, _ := pem.Decode(caData)
	caCert, _ := x509.ParseCertificate(caBlk.Bytes)
	keyData, _ := os.ReadFile(filepath.Join(f.certsDir, "ca-key.pem"))
	keyBlk, _ := pem.Decode(keyData)
	caKey, _ := x509.ParsePKCS8PrivateKey(keyBlk.Bytes)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(9999),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, caCert, pub, caKey)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(priv)
	unlisted := f.cliCfg
	unlisted.TLS.Certificate = filepath.Join(f.dir, "unlisted.pem")
	unlisted.TLS.PrivateKey = filepath.Join(f.dir, "unlisted-key.pem")
	_ = os.WriteFile(unlisted.TLS.Certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	_ = os.WriteFile(unlisted.TLS.PrivateKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), 0600)
	if c, err := Dial(context.Background(), unlisted); err == nil {
		_ = c.Close()
		t.Fatal("accepted unlisted CA-signed client certificate")
	}
}

func TestE2EMaliciousTraversalTamperingAndProtocolAbuse(t *testing.T) {
	f := startE2EFixture(t, nil)

	// 1. 绕过客户端校验直接在报文层发送恶意路径穿透名称，服务端必须拦截并拒绝落地
	for _, badName := range []string{"../escape.txt", `sub\file.txt`, "/etc/passwd", "bad.", "a\tb.bin"} {
		wire := f.rawConn(t)
		h := sha256.Sum256([]byte("x"))
		var raw bytes.Buffer
		var reqID protocol.RequestID
		reqID[0] = 1
		raw.Write(reqID[:])
		_ = binary.Write(&raw, binary.BigEndian, uint16(len(badName)))
		raw.WriteString(badName)
		_ = binary.Write(&raw, binary.BigEndian, uint64(1))
		raw.Write(h[:])
		if err := wire.SendPayload(protocol.TypePutReq, 1, raw.Bytes()); err != nil {
			t.Fatal(err)
		}
		resp, err := wire.Read(5)
		if err != nil || resp.Header.Type != protocol.TypeError {
			t.Fatalf("expected TypeError for bad name %q, got frame=%v err=%v", badName, resp.Header.Type, err)
		}
		_ = wire.Net.Close()
	}

	// 2. 上传 Hash 篡改与偏移错位必须失败且清理 staging/
	for idx, tc := range []struct {
		name   string
		offset uint64
		body   string
		code   protocol.ErrorCode
	}{
		{"wrong_hash", 0, "XYZ", protocol.CodeHashMismatch},
		{"wrong_offset", 1, "ABC", protocol.CodeOffsetMismatch},
	} {
		wire := f.rawConn(t)
		h := sha256.Sum256([]byte("ABC"))
		_ = wire.Send(protocol.TypePutReq, 1, protocol.PutRequest{RequestID: protocol.RequestID{byte(10 + idx)}, FileName: tc.name + ".bin", FileSize: 3, FileSHA256: h})
		frame, err := wire.Read(5)
		if err != nil || frame.Header.Type != protocol.TypePutResp {
			t.Fatalf("%s PutResp failed: %v", tc.name, err)
		}
		_ = wire.Send(protocol.TypeData, 1, protocol.Data{FileOffset: tc.offset, Bytes: []byte(tc.body)})
		frame, err = wire.Read(5)
		if err != nil {
			t.Fatal(err)
		}
		if tc.offset == 0 {
			if frame.Header.Type != protocol.TypeAck {
				t.Fatalf("expected ACK before commit, got %v", frame.Header.Type)
			}
			_ = wire.Send(protocol.TypeCommit, 1, protocol.Commit{FileSize: 3, FileSHA256: h})
			frame, err = wire.Read(5)
			if err != nil {
				t.Fatal(err)
			}
		}
		if frame.Header.Type != protocol.TypeError {
			t.Fatalf("%s expected TypeError, got %v", tc.name, frame.Header.Type)
		}
		var rem protocol.RemoteError
		_ = protocol.Unmarshal(frame.Payload, &rem)
		if rem.Code != tc.code {
			t.Fatalf("%s code=%v want=%v", tc.name, rem.Code, tc.code)
		}
		stagingDeadline := time.Now().Add(time.Second)
		var staging []os.DirEntry
		for time.Now().Before(stagingDeadline) {
			staging, err = os.ReadDir(filepath.Join(f.srvCfg.StorageRoot, "staging"))
			if err == nil && len(staging) == 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil || len(staging) != 0 {
			t.Fatalf("staging not cleaned: %v %v", staging, err)
		}
		_ = wire.Net.Close()
	}

	// 3. 服务端对象篡改导致下载校验失败（CodeHashMismatch），不得保留目标与 .part 文件
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := Dial(ctx, f.cliCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	src := filepath.Join(f.dir, "good.bin")
	_ = os.WriteFile(src, []byte("ABC"), 0600)
	if _, err := c.UploadFile(ctx, src, "good.bin", nil); err != nil {
		t.Fatal(err)
	}
	// 禁止覆盖同名已上传文件
	if _, err := c.UploadFile(ctx, src, "good.bin", nil); err == nil {
		t.Fatal("expected overwrite upload to fail")
	} else {
		requireRemoteCode(t, err, protocol.CodeAlreadyExists)
	}
	// 禁止下载覆盖已存在本地文件
	if _, err := c.DownloadFile(ctx, "good.bin", src, nil); err == nil {
		t.Fatal("expected download overwrite to fail")
	}

	// 查找并篡改 objects/ 中的文件
	objFile, pubMeta, err := f.daemon.store.OpenPublishedFile(ctx, "alice", "good.bin")
	if err != nil {
		t.Fatal(err)
	}
	_ = objFile.Close()
	if err := os.WriteFile(filepath.Join(f.srvCfg.StorageRoot, "objects", pubMeta.ObjectID), []byte("XYZ"), 0600); err != nil {
		t.Fatal(err)
	}
	dlDir := filepath.Join(f.dir, "corrupt-dl")
	_ = os.MkdirAll(dlDir, 0700)
	_, err = c.DownloadFile(ctx, "good.bin", filepath.Join(dlDir, "good.bin"), nil)
	requireRemoteCode(t, err, protocol.CodeHashMismatch)
	if entries, err := os.ReadDir(dlDir); err != nil || len(entries) != 0 {
		t.Fatalf("corrupted download left files behind: %v %v", entries, err)
	}

	// 4. 错误 Magic 断连与超大帧返回 CodeBadFrame
	for _, badMagic := range []bool{true, false} {
		wire := f.rawConn(t)
		hdr := make([]byte, protocol.HeaderSize)
		copy(hdr, protocol.Magic)
		hdr[4], hdr[5], hdr[11] = 1, byte(protocol.TypeData), 1
		if badMagic {
			hdr[0] = 'X'
			binary.BigEndian.PutUint32(hdr[12:], 9)
		} else {
			binary.BigEndian.PutUint32(hdr[12:], ^uint32(0))
		}
		_, _ = wire.Net.Write(hdr)
		frame, rErr := wire.Read(5)
		if badMagic {
			if rErr == nil {
				t.Fatal("expected connection close on bad magic")
			}
		} else {
			if rErr != nil || frame.Header.Type != protocol.TypeError {
				t.Fatalf("expected TypeError on huge frame: frame=%v err=%v", frame.Header.Type, rErr)
			}
			var rem protocol.RemoteError
			_ = protocol.Unmarshal(frame.Payload, &rem)
			if rem.Code != protocol.CodeBadFrame {
				t.Fatalf("expected CodeBadFrame, got %v", rem.Code)
			}
		}
		_ = wire.Net.Close()
	}
}

type crashChildArgs struct {
	Client   config.Client `json:"client"`
	SrcPath  string        `json:"src_path"`
	DstPath  string        `json:"dst_path"`
	Remote   string        `json:"remote"`
	Mode     string        `json:"mode"` // "upload" | "download"
	DoneFile string        `json:"done_file"`
}

func TestE2ECrashKillMidTransferHelper(t *testing.T) {
	raw := os.Getenv("SFT_E2E_CRASH_SPEC")
	if raw == "" {
		return
	}
	var spec crashChildArgs
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatalf("unmarshal crash spec: %v", err)
	}
	idKey, err := security.ClientIdentityKey(spec.Client)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := queue.Open(queue.Config{
		StateDir:     spec.Client.StateDir,
		Identity:     idKey,
		MaxFileBytes: spec.Client.MaxFileBytes,
		MaxStreams:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := NewQueueRunner(spec.Client, mgr)
	mgr.SetRunner(runner.Run)

	dir := protocol.DirectionUpload
	localPath := spec.SrcPath
	if spec.Mode == "download" {
		dir = protocol.DirectionDownload
		localPath = spec.DstPath
	}
	tasks, err := mgr.Enqueue([]queue.Spec{{Direction: dir, Remote: spec.Remote, Path: localPath}})
	if err != nil {
		t.Fatal(err)
	}
	tid := tasks[0].ID

	// 等待产生已确认的非零传输断点后通知父进程执行强杀（不优雅关库）
	deadline := time.Now().Add(15 * time.Second)
	signaled := false
	for time.Now().Before(deadline) {
		cur := mgr.Task(tid)
		if cur != nil && cur.State == queue.Transferring && cur.Done >= 48*1024 && cur.HasCheckpoint() {
			if !signaled {
				signaled = true
				_ = os.WriteFile(spec.DoneFile, []byte("ready"), 0600)
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("child timed out waiting to be killed")
}

func TestE2EKillProcessMidTransferAndResumeCheckpoint(t *testing.T) {
	// 限速至 160 KiB/s + 16 KiB 分块，制造稳定的半途强杀窗口
	f := startE2EFixture(t, func(s *config.Server) {
		s.Limits.ChunkSize = 16 * 1024
		s.Bandwidth = config.Bandwidth{
			UploadBytesPerSecond:   160 * 1024,
			DownloadBytesPerSecond: 160 * 1024,
			BurstBytes:             32 * 1024,
		}
	})
	f.cliCfg.Limits.ChunkSize = 16 * 1024

	payload := bytes.Repeat([]byte("CRASH-RESUME-BLOCK-"), 35*1024) // ~665 KB
	srcPath := filepath.Join(f.dir, "crash.bin")
	if err := os.WriteFile(srcPath, payload, 0600); err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256(payload)
	readyFlag := filepath.Join(f.dir, "ready-upload")

	spec := crashChildArgs{
		Client:   f.cliCfg,
		SrcPath:  srcPath,
		Remote:   "crash.bin",
		Mode:     "upload",
		DoneFile: readyFlag,
	}
	specJSON, _ := json.Marshal(spec)

	cmd := exec.Command(os.Args[0], "-test.run=^TestE2ECrashKillMidTransferHelper$")
	cmd.Env = append(os.Environ(), "SFT_E2E_CRASH_SPEC="+string(specJSON))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitReady := time.Now().Add(15 * time.Second)
	for time.Now().Before(waitReady) {
		if _, err := os.Stat(readyFlag); err == nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if _, err := os.Stat(readyFlag); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("child upload process never reached checkpoint stage")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	// 重新打开同一 SQLite 队列：应自动将异常中断任务恢复为 Paused 且保留非零检查点偏移！
	idKey, err := security.ClientIdentityKey(f.cliCfg)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := queue.Open(queue.Config{
		StateDir:     f.cliCfg.StateDir,
		Identity:     idKey,
		MaxFileBytes: f.cliCfg.MaxFileBytes,
		MaxStreams:   1,
	})
	if err != nil {
		t.Fatalf("reopen SQLite queue after SIGKILL: %v", err)
	}
	defer mgr.Close()

	snap := mgr.Snapshot(false)
	if len(snap) != 1 || snap[0].State != queue.Paused || !snap[0].HasCheckpoint() || snap[0].TransferID == "" {
		t.Fatalf("unexpected post-kill recovered upload state: %+v", snap)
	}
	rawTID, err := storageHex16(snap[0].TransferID)
	if err != nil {
		t.Fatal(err)
	}
	srvTask, err := f.daemon.store.GetTask(context.Background(), protocol.TransferID(rawTID))
	if err != nil || srvTask.Offset == 0 {
		t.Fatalf("expected non-zero server upload checkpoint offset after SIGKILL, got %+v err=%v", srvTask, err)
	}
	resumedFrom := srvTask.Offset

	// 放开限速恢复完成上传
	unthrottled := f.srvCfg
	unthrottled.AuthorizedClients = []config.AuthorizedClient{
		{
			UserID:      "alice",
			Certificate: filepath.Join(f.certsDir, "alice.pem"),
			Permissions: []string{"upload", "download"},
			Bandwidth:   &config.Bandwidth{UploadBytesPerSecond: 128 * 1024 * 1024, DownloadBytesPerSecond: 128 * 1024 * 1024, BurstBytes: 1024 * 1024},
		},
	}
	if err := f.daemon.ApplySaved(unthrottled); err != nil {
		t.Fatal(err)
	}
	runner := NewQueueRunner(f.cliCfg, mgr)
	mgr.SetRunner(runner.Run)
	if err := mgr.Resume(snap[0].ID); err != nil {
		t.Fatal(err)
	}

	finishWait := time.Now().Add(15 * time.Second)
	uploadDone := false
	for time.Now().Before(finishWait) {
		hist := mgr.Snapshot(true)
		if len(hist) == 1 && hist[0].State == queue.Completed {
			if hist[0].Size != uint64(len(payload)) {
				t.Fatalf("resumed upload wrong final size: %d", hist[0].Size)
			}
			pubF, pubMeta, openErr := f.daemon.store.OpenPublishedFile(context.Background(), "alice", "crash.bin")
			if openErr != nil {
				t.Fatal(openErr)
			}
			_ = pubF.Close()
			if pubMeta.SHA256 != wantSum {
				t.Fatalf("resumed server digest mismatch: %x != %x", pubMeta.SHA256, wantSum)
			}
			uploadDone = true
			t.Logf("Upload SIGKILL crash-resume succeeded from server checkpoint offset %d/%d", resumedFrom, len(payload))
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = runner.Close()
	_ = mgr.Close()
	if !uploadDone {
		t.Fatalf("upload did not complete after SIGKILL resume")
	}

	// -------- 第二阶段：下载中途 SIGKILL 强杀并验证客户端本地 .part + SQLite Done > 0 断点恢复 --------
	if err := f.daemon.ApplySaved(f.srvCfg); err != nil { // 恢复 160 KiB/s 限速制造下载强杀窗口
		t.Fatal(err)
	}
	dlDir := filepath.Join(f.dir, "crash-dl")
	_ = os.MkdirAll(dlDir, 0700)
	dstPath := filepath.Join(dlDir, "crash.bin")
	dlReadyFlag := filepath.Join(f.dir, "ready-download")
	dlSpec := crashChildArgs{
		Client:   f.cliCfg,
		DstPath:  dstPath,
		Remote:   "crash.bin",
		Mode:     "download",
		DoneFile: dlReadyFlag,
	}
	dlSpecJSON, _ := json.Marshal(dlSpec)
	dlCmd := exec.Command(os.Args[0], "-test.run=^TestE2ECrashKillMidTransferHelper$")
	dlCmd.Env = append(os.Environ(), "SFT_E2E_CRASH_SPEC="+string(dlSpecJSON))
	if err := dlCmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitDL := time.Now().Add(15 * time.Second)
	for time.Now().Before(waitDL) {
		if _, err := os.Stat(dlReadyFlag); err == nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if _, err := os.Stat(dlReadyFlag); err != nil {
		_ = dlCmd.Process.Kill()
		_ = dlCmd.Wait()
		t.Fatal("child download process never reached checkpoint stage")
	}
	_ = dlCmd.Process.Kill()
	_ = dlCmd.Wait()

	mgr2, err := queue.Open(queue.Config{
		StateDir:     f.cliCfg.StateDir,
		Identity:     idKey,
		MaxFileBytes: f.cliCfg.MaxFileBytes,
		MaxStreams:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr2.Close()

	snap2 := mgr2.Snapshot(false)
	if len(snap2) != 1 || snap2[0].State != queue.Paused || snap2[0].Done == 0 || !snap2[0].HasCheckpoint() {
		t.Fatalf("unexpected post-kill recovered download state: %+v", snap2)
	}
	dlResumedFrom := snap2[0].Done
	if err := f.daemon.ApplySaved(unthrottled); err != nil {
		t.Fatal(err)
	}
	runner2 := NewQueueRunner(f.cliCfg, mgr2)
	defer runner2.Close()
	mgr2.SetRunner(runner2.Run)
	if err := mgr2.Resume(snap2[0].ID); err != nil {
		t.Fatal(err)
	}

	dlWait := time.Now().Add(15 * time.Second)
	for time.Now().Before(dlWait) {
		hist2 := mgr2.Snapshot(true)
		if len(hist2) >= 2 && hist2[0].State == queue.Completed {
			gotDL, err := os.ReadFile(dstPath)
			if err != nil || sha256.Sum256(gotDL) != wantSum {
				t.Fatalf("resumed download content/digest mismatch: %v", err)
			}
			// 检查下载目录仅含目标文件，无残留 .sft-*.part 文件
			entries, _ := os.ReadDir(dlDir)
			if len(entries) != 1 || entries[0].Name() != "crash.bin" {
				t.Fatalf("expected only crash.bin after resume completion, got %+v", entries)
			}
			t.Logf("Download SIGKILL crash-resume succeeded cleanly from SQLite checkpoint offset %d/%d", dlResumedFrom, len(payload))
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("download did not complete after SIGKILL resume; state=%+v", mgr2.Snapshot(false))
}

func storageHex16(s string) ([16]byte, error) {
	return decodeHex16(s)
}
