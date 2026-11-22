package transfer

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/pal"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
	"sft-tui/internal/security"
	"sft-tui/internal/session"
)

const controlTimeout = 30 * time.Second

// Result 描述单次文件传输的最终结果摘要。
type Result struct {
	FileName    string
	Size        uint64
	SHA256      protocol.Digest
	ResumedFrom uint64
}

// CheckpointUpdater 将运行中的检查点变更原子同步到持久化层。
type CheckpointUpdater func(func(*queue.Task)) error

// Client 封装客户端单条 mTLS 多路复用连接及其传输操作。
type Client struct {
	conn     *session.Conn
	cfg      config.Client
	mu       sync.Mutex
	mux      *session.Mux
	slots    chan struct{}
	identity string
	closed   bool
}

// Dial 建立到服务端的 TLS 1.3 mTLS 连接并完成 SFT/1 HELLO 握手。
func Dial(ctx context.Context, cfg config.Client) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	tlsCfg, err := security.ClientTLS(cfg)
	if err != nil {
		return nil, err
	}
	idKey, err := security.ClientIdentityKey(cfg)
	if err != nil {
		return nil, err
	}

	timeout := time.Duration(cfg.Timeouts.HandshakeSeconds) * time.Second
	raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", cfg.ServerAddress)
	if err != nil {
		return nil, err
	}
	stopCaller := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stopCaller()

	conn := tls.Client(raw, tlsCfg)
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()

	_ = conn.SetDeadline(time.Now().Add(timeout))
	handshakeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := conn.HandshakeContext(handshakeCtx); err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})

	c := &session.Conn{
		Net:      conn,
		Chunk:    cfg.Limits.ChunkSize,
		Timeouts: cfg.Timeouts,
	}
	if err := c.Send(protocol.TypeHello, 0, protocol.Hello{
		ClientMaxStreams: cfg.Limits.MaxStreams,
		ClientMaxChunk:   cfg.Limits.ChunkSize,
		ClientWindow:     cfg.Limits.WindowSize,
	}); err != nil {
		return nil, err
	}

	f, err := c.Read(cfg.Timeouts.HelloSeconds)
	if err != nil {
		return nil, err
	}
	if f.Header.Type == protocol.TypeError {
		var e protocol.RemoteError
		if err := protocol.Unmarshal(f.Payload, &e); err != nil {
			return nil, err
		}
		return nil, &e
	}
	if f.Header.Type != protocol.TypeHelloAck || f.Header.StreamID != 0 {
		return nil, fmt.Errorf("服务端未返回合法的 HELLO_ACK")
	}

	var ack protocol.HelloAck
	if err := protocol.Unmarshal(f.Payload, &ack); err != nil {
		return nil, err
	}
	if ack.MaxStreams < 1 || ack.MaxStreams > cfg.Limits.MaxStreams ||
		ack.ChunkSize < protocol.MinChunkSize || ack.ChunkSize > cfg.Limits.ChunkSize ||
		ack.WindowSize < protocol.MinWindowSize || ack.WindowSize > cfg.Limits.WindowSize ||
		ack.ChunkSize > ack.WindowSize || ack.IdleTimeoutSec == 0 {
		return nil, fmt.Errorf("服务端协商流控参数非法")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.Chunk = ack.ChunkSize
	c.Timeouts.IdleSeconds = ack.IdleTimeoutSec
	ok = true

	client := &Client{
		conn:     c,
		cfg:      cfg,
		slots:    make(chan struct{}, ack.MaxStreams),
		identity: idKey,
	}
	client.mux = session.NewMux(c, ack.MaxStreams, ack.WindowSize, nil)
	return client, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	m := c.mux
	c.mu.Unlock()
	if m != nil {
		m.Close(io.EOF)
		m.Wait()
	}
	return c.conn.Net.Close()
}

func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && c.mux != nil && c.mux.Context().Err() == nil && !c.mux.Draining()
}

func (c *Client) Draining() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mux != nil && c.mux.Draining()
}

func (c *Client) Identity() string { return c.identity }

func (c *Client) operationContext() (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, io.ErrClosedPipe
	}
	if c.mux.Context().Err() != nil {
		return nil, c.mux.Error()
	}
	return c.mux.Context(), nil
}

func (c *Client) openStream(ctx context.Context, direction protocol.Direction) (*session.Stream, func(), error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, nil, io.ErrClosedPipe
	}
	m := c.mux
	c.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case c.slots <- struct{}{}:
	case <-m.Context().Done():
		return nil, nil, m.Error()
	}
	st, err := m.Open(direction)
	if err != nil {
		<-c.slots
		return nil, nil, err
	}
	return st, func() { st.Release(); <-c.slots }, nil
}

// ListFiles 流式拉取当前用户远端已发布文件列表。
func (c *Client) ListFiles(ctx context.Context, visit func(protocol.FileEntry) error) (err error) {
	if visit == nil {
		return fmt.Errorf("文件列表回调不能为空")
	}
	opCtx, err := c.operationContext()
	if err != nil {
		return err
	}
	mergedCtx, cancel := context.WithCancel(opCtx)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()

	st, release, err := c.openStream(mergedCtx, protocol.DirectionDownload)
	if err != nil {
		return err
	}
	defer release()
	defer func() {
		if err != nil {
			st.Report(err)
		}
	}()

	if err = st.SendContext(mergedCtx, protocol.TypeListReq, protocol.ListRequest{}); err != nil {
		return err
	}
	previous := ""
	for {
		var page protocol.ListResponse
		if err = st.ExpectContext(mergedCtx, protocol.TypeListResp, &page); err != nil {
			return err
		}
		for _, file := range page.Files {
			if file.FileName <= previous {
				return &protocol.RemoteError{Code: protocol.CodeBadFrame, Message: "文件列表游标未递增"}
			}
			if err = visit(file); err != nil {
				return err
			}
			previous = file.FileName
		}
		if !page.More {
			return nil
		}
		if err = st.SendContext(mergedCtx, protocol.TypeListNext, struct{}{}); err != nil {
			return err
		}
	}
}

// Execute 执行单个队列任务（支持上传、下载、断点续传、暂停/取消确认及清理）。
func (c *Client) Execute(ctx context.Context, task queue.Task, op *queue.Operation, progress func(transferred, total uint64, phase queue.State), update CheckpointUpdater) (Result, error) {
	if update == nil {
		local := task
		update = func(fn func(*queue.Task)) error {
			fn(&local)
			task = local
			return nil
		}
	}
	if task.WantCancel {
		return Result{}, c.abortCheckpoint(ctx, &task, update)
	}
	if task.Direction == protocol.DirectionUpload {
		return c.executeUpload(ctx, &task, op, progress, update)
	}
	return c.executeDownload(ctx, &task, op, progress, update)
}

// UploadFile 便捷方法：独立上传本地文件。
func (c *Client) UploadFile(ctx context.Context, localPath, remoteName string, progress func(transferred, total uint64, phase queue.State)) (Result, error) {
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return Result{}, err
	}
	key, err := queue.StateKeyForIdentity(c.identity, protocol.DirectionUpload, remoteName, abs)
	if err != nil {
		return Result{}, err
	}
	task := queue.Task{
		ID:        key,
		Identity:  c.identity,
		StateKey:  key,
		Direction: protocol.DirectionUpload,
		Remote:    remoteName,
		Path:      abs,
	}
	return c.Execute(ctx, task, queue.NewOperation(ctx), progress, nil)
}

// DownloadFile 便捷方法：独立下载文件到本地路径（不覆盖已有目标）。
func (c *Client) DownloadFile(ctx context.Context, remoteName, dstPath string, progress func(transferred, total uint64, phase queue.State)) (Result, error) {
	abs, err := filepath.Abs(dstPath)
	if err != nil {
		return Result{}, err
	}
	key, err := queue.StateKeyForIdentity(c.identity, protocol.DirectionDownload, remoteName, abs)
	if err != nil {
		return Result{}, err
	}
	task := queue.Task{
		ID:        key,
		Identity:  c.identity,
		StateKey:  key,
		Direction: protocol.DirectionDownload,
		Remote:    remoteName,
		Path:      abs,
	}
	return c.Execute(ctx, task, queue.NewOperation(ctx), progress, nil)
}

func (c *Client) bindControlContext(ctx context.Context, op *queue.Operation) (context.Context, func(), error) {
	base, err := c.operationContext()
	if err != nil {
		return nil, func() {}, err
	}
	runCtx, cancel := context.WithCancel(base)
	stopParent := context.AfterFunc(ctx, cancel)
	var stopOp func() bool
	if op != nil {
		stopOp = context.AfterFunc(op.Context(), cancel)
		if op.Context().Err() != nil {
			cancel()
		}
	}
	if ctx.Err() != nil {
		cancel()
	}
	return runCtx, func() {
		stopParent()
		if stopOp != nil {
			stopOp()
		}
		cancel()
	}, nil
}

func reportProgress(fn func(uint64, uint64, queue.State), done, total uint64, phase queue.State) {
	if fn != nil {
		fn(done, total, phase)
	}
}

func clearTaskCheckpoint(t *queue.Task) {
	t.RequestID = ""
	t.TransferID = ""
	t.TempName = ""
	t.TempDevice = 0
	t.TempInode = 0
	t.Publishing = false
	t.Published = false
}

func decodeHex16(s string) ([16]byte, error) {
	var out [16]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return out, fmt.Errorf("非法 16 字节 Hex ID")
	}
	copy(out[:], b)
	return out, nil
}

func decodeDigestHex(s string) (protocol.Digest, error) {
	var out protocol.Digest
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return out, fmt.Errorf("非法 SHA-256 Hex")
	}
	copy(out[:], b)
	return out, nil
}

// ensureUploadInitCheckpoint 在发送 PUT 前一次性原子持久化 RequestID、文件大小与摘要，
// 避免在两次持久化之间中断留下仅有 RequestID 而缺少摘要的不可取消任务。
func ensureUploadInitCheckpoint(t *queue.Task, size uint64, hashHex string, update CheckpointUpdater) (protocol.RequestID, error) {
	var reqID protocol.RequestID
	var reqHex string
	if t.RequestID != "" {
		id, err := decodeHex16(t.RequestID)
		if err != nil {
			return reqID, fmt.Errorf("持久化请求 ID 无效：%w", err)
		}
		reqID = protocol.RequestID(id)
		reqHex = t.RequestID
	} else {
		if _, err := io.ReadFull(rand.Reader, reqID[:]); err != nil {
			return reqID, err
		}
		reqHex = hex.EncodeToString(reqID[:])
	}
	if err := update(func(cur *queue.Task) {
		cur.RequestID = reqHex
		cur.Size = size
		cur.SHA256 = hashHex
	}); err != nil {
		return reqID, err
	}
	t.RequestID = reqHex
	t.Size = size
	t.SHA256 = hashHex
	return reqID, nil
}

func ensureRequestID(t *queue.Task, update CheckpointUpdater) (protocol.RequestID, error) {
	if t.RequestID != "" {
		id, err := decodeHex16(t.RequestID)
		if err != nil {
			return protocol.RequestID{}, fmt.Errorf("持久化请求 ID 无效：%w", err)
		}
		return protocol.RequestID(id), nil
	}
	var reqID protocol.RequestID
	if _, err := io.ReadFull(rand.Reader, reqID[:]); err != nil {
		return reqID, err
	}
	reqHex := hex.EncodeToString(reqID[:])
	if err := update(func(cur *queue.Task) {
		cur.RequestID = reqHex
	}); err != nil {
		return reqID, err
	}
	t.RequestID = reqHex
	return reqID, nil
}

func validateWireResponse(state protocol.ResponseState, offset, size uint64) error {
	if !state.Valid() || offset > size || (state == protocol.ResponseCompleted && offset != size) {
		return fmt.Errorf("响应状态或偏移非法")
	}
	return nil
}

func validateFinish(f protocol.Finish, tid protocol.TransferID, size uint64, sha protocol.Digest) error {
	if f.TransferID != tid || f.FileSize != size || f.FileSHA256 != sha {
		return &protocol.RemoteError{Code: protocol.CodeMetadataMismatch, Message: "FINISH 与传输元数据不符"}
	}
	return nil
}

func (c *Client) executeUpload(ctx context.Context, task *queue.Task, op *queue.Operation, progress func(uint64, uint64, queue.State), update CheckpointUpdater) (result Result, err error) {
	runCtx, stop, err := c.bindControlContext(ctx, op)
	if err != nil {
		return result, err
	}
	defer stop()

	if err = protocol.ValidateName(task.Remote); err != nil {
		return result, err
	}
	file, err := os.Open(task.Path)
	if err != nil {
		return result, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || uint64(info.Size()) > c.cfg.MaxFileBytes {
		return result, fmt.Errorf("上传对象必须是限额内的普通文件：%s", task.Path)
	}
	if task.SourceModified != 0 && (uint64(info.Size()) != task.Size || info.ModTime().UnixNano() != task.SourceModified) {
		return result, fmt.Errorf("源文件已变化，请取消后重新加入队列")
	}

	size := uint64(info.Size())
	reportProgress(progress, task.Done, size, queue.Hashing)
	hash, _, err := pal.DigestReaderContext(runCtx, file)
	if err != nil {
		if op != nil && op.Mode() != 0 && errors.Is(err, context.Canceled) {
			return result, c.finishUnstartedControl(ctx, task, op.Mode(), update)
		}
		return result, err
	}
	nowInfo, err := file.Stat()
	if err != nil {
		return result, err
	}
	if nowInfo.Size() != info.Size() || !nowInfo.ModTime().Equal(info.ModTime()) {
		return result, fmt.Errorf("源文件在摘要计算期间发生变化")
	}

	hashHex := hex.EncodeToString(hash[:])
	if task.SHA256 != "" && task.SHA256 != hashHex {
		return result, &protocol.RemoteError{Code: protocol.CodeMetadataMismatch, Message: "源文件摘要与断点检查点不符"}
	}

	reqID, err := ensureUploadInitCheckpoint(task, size, hashHex, update)
	if err != nil {
		return result, err
	}
	result = Result{FileName: task.Remote, Size: size, SHA256: hash}

	st, release, err := c.openStream(runCtx, protocol.DirectionUpload)
	if err != nil {
		if op != nil && op.Mode() != 0 && errors.Is(err, context.Canceled) {
			return result, c.finishUnstartedControl(ctx, task, op.Mode(), update)
		}
		return result, err
	}
	defer release()

	wireCtx, cancelInit := context.WithTimeout(st.Context(), controlTimeout)
	defer cancelInit()

	initialized := false
	defer func() {
		if err != nil && initialized && op != nil && op.Mode() != 0 && errors.Is(err, context.Canceled) {
			err = c.cancelActiveStream(st, task, op.Mode(), nil, result, update)
		}
		if err != nil && !errors.Is(err, ErrPaused) && !errors.Is(err, ErrCancelled) {
			st.Report(err)
		}
		if isFatalTransfer(err) {
			_ = update(clearTaskCheckpoint)
		}
	}()

	var state protocol.ResponseState
	var offset uint64
	var tid protocol.TransferID

	if task.TransferID != "" {
		rawTID, e := decodeHex16(task.TransferID)
		if e != nil {
			return result, e
		}
		tid = protocol.TransferID(rawTID)
		if err = st.SendContext(wireCtx, protocol.TypeResumeReq, protocol.ResumeRequest{
			TransferID:   tid,
			Direction:    protocol.DirectionUpload,
			ClientOffset: 0,
			FileSHA256:   hash,
		}); err != nil {
			return result, err
		}
		var resp protocol.ResumeResponse
		if err = st.ExpectContext(wireCtx, protocol.TypeResumeResp, &resp); err != nil {
			return result, err
		}
		if resp.Direction != protocol.DirectionUpload || resp.FileSize != size || resp.FileSHA256 != hash ||
			validateWireResponse(resp.State, resp.AcceptedOffset, size) != nil {
			return result, &protocol.RemoteError{Code: protocol.CodeMetadataMismatch, Message: "上传恢复响应元数据不符"}
		}
		state, offset = resp.State, resp.AcceptedOffset
	} else {
		if err = st.SendContext(wireCtx, protocol.TypePutReq, protocol.PutRequest{
			RequestID:  reqID,
			FileName:   task.Remote,
			FileSize:   size,
			FileSHA256: hash,
		}); err != nil {
			return result, err
		}
		var resp protocol.PutResponse
		if err = st.ExpectContext(wireCtx, protocol.TypePutResp, &resp); err != nil {
			return result, err
		}
		if resp.TransferID == (protocol.TransferID{}) || validateWireResponse(resp.State, resp.NextOffset, size) != nil {
			return result, fmt.Errorf("上传响应参数非法")
		}
		tid = resp.TransferID
		tidHex := hex.EncodeToString(tid[:])
		if err = update(func(cur *queue.Task) {
			cur.TransferID = tidHex
			cur.Done = resp.NextOffset
		}); err != nil {
			return result, err
		}
		task.TransferID, task.Done = tidHex, resp.NextOffset
		state, offset = resp.State, resp.NextOffset
	}

	result.ResumedFrom = offset
	reportProgress(progress, offset, size, queue.Transferring)
	if state == protocol.ResponseCompleted {
		_ = update(func(cur *queue.Task) {
			cur.Done = size
			clearTaskCheckpoint(cur)
		})
		return result, nil
	}

	initialized = true
	cancelInit()
	if err = runCtx.Err(); err != nil {
		return result, err
	}
	if _, err = file.Seek(int64(offset), io.SeekStart); err != nil {
		return result, err
	}

	buf := make([]byte, st.ChunkSize())
	start := offset
	if start == size {
		var ack protocol.Ack
		if err = st.ExpectContext(runCtx, protocol.TypeAck, &ack); err != nil {
			return result, err
		}
		if ack.NextOffset != offset {
			return result, &protocol.RemoteError{Code: protocol.CodeOffsetMismatch, Message: "最终 ACK 偏移不符"}
		}
	} else {
		win := uint64(max(st.WindowSize(), st.ChunkSize()))
		uploadCtx, cancelUpload := context.WithCancel(runCtx)
		var sentAtomic, ackAtomic atomic.Uint64
		sentAtomic.Store(start)
		ackAtomic.Store(start)
		ackSignal := make(chan struct{}, 1)
		ackDone := make(chan error, 1)

		go func() {
			curAck := start
			for curAck < size {
				var ack protocol.Ack
				if aErr := st.ExpectContext(uploadCtx, protocol.TypeAck, &ack); aErr != nil {
					cancelUpload()
					ackDone <- aErr
					return
				}
				maxSent := sentAtomic.Load()
				if ack.NextOffset < curAck || ack.NextOffset > maxSent {
					cancelUpload()
					ackDone <- &protocol.RemoteError{Code: protocol.CodeOffsetMismatch, Message: "上传 ACK 偏移不符"}
					return
				}
				if ack.NextOffset == curAck {
					continue
				}
				curAck = ack.NextOffset
				ackAtomic.Store(curAck)
				reportProgress(progress, curAck, size, queue.Transferring)
				select {
				case ackSignal <- struct{}{}:
				default:
				}
			}
			ackDone <- nil
		}()

		for offset < size {
			for offset-ackAtomic.Load() >= win {
				select {
				case <-uploadCtx.Done():
					cancelUpload()
					if aErr := <-ackDone; aErr != nil {
						return result, aErr
					}
					return result, uploadCtx.Err()
				case <-ackSignal:
				}
			}
			n := int(min(uint64(len(buf)), size-offset))
			if winAvail := win - (offset - ackAtomic.Load()); uint64(n) > winAvail {
				n = int(winAvail)
			}
			if _, err = io.ReadFull(file, buf[:n]); err != nil {
				cancelUpload()
				<-ackDone
				return result, err
			}
			sentAtomic.Store(offset + uint64(n))
			if err = st.SendContext(uploadCtx, protocol.TypeData, protocol.Data{FileOffset: offset, Bytes: buf[:n]}); err != nil {
				cancelUpload()
				if aErr := <-ackDone; aErr != nil && !errors.Is(aErr, context.Canceled) {
					return result, aErr
				}
				return result, err
			}
			offset += uint64(n)
		}
		if err = <-ackDone; err != nil {
			cancelUpload()
			return result, err
		}
		cancelUpload()
	}

	nowInfo, err = file.Stat()
	if err != nil {
		return result, err
	}
	if nowInfo.Size() != info.Size() || !nowInfo.ModTime().Equal(info.ModTime()) {
		return result, &protocol.RemoteError{Code: protocol.CodeMetadataMismatch, Message: "源文件在传输期间发生变化"}
	}

	reportProgress(progress, size, size, queue.Verifying)
	if err = runCtx.Err(); err != nil {
		return result, err
	}

	if err = st.SendContext(st.Context(), protocol.TypeCommit, protocol.Commit{FileSize: size, FileSHA256: hash}); err != nil {
		return result, err
	}
	var finish protocol.Finish
	if err = st.ExpectContext(st.Context(), protocol.TypeFinish, &finish); err != nil {
		return result, err
	}
	if err = validateFinish(finish, tid, size, hash); err != nil {
		return result, err
	}
	_ = update(func(cur *queue.Task) {
		cur.Done = size
		clearTaskCheckpoint(cur)
	})
	return result, nil
}

func (c *Client) executeDownload(ctx context.Context, task *queue.Task, op *queue.Operation, progress func(uint64, uint64, queue.State), update CheckpointUpdater) (result Result, err error) {
	runCtx, stop, err := c.bindControlContext(ctx, op)
	if err != nil {
		return result, err
	}
	defer stop()

	if err = protocol.ValidateName(task.Remote); err != nil {
		return result, err
	}
	absPath, err := filepath.Abs(task.Path)
	if err != nil {
		return result, err
	}
	parent, base := filepath.Dir(absPath), filepath.Base(absPath)
	if err = pal.ValidateLocalFilename(base); err != nil {
		return result, err
	}
	root, err := pal.OpenUserRoot(parent)
	if err != nil {
		return result, err
	}
	defer root.Close()

	tempName := ".sft-" + task.StateKey + ".part"
	if task.TempName != "" && task.TempName != tempName {
		return result, fmt.Errorf("下载临时文件名与任务状态键不匹配")
	}

	// 检查最终发布目标是否存在（禁止覆盖未由本断点发布的已有文件；兼容 Link/Rename 后、SQLite 持久化前的崩溃恢复）
	if baseStat, statErr := root.Lstat(base); statErr == nil {
		if !task.Published {
			recovered, recErr := recoverCrashPublished(runCtx, root, base, baseStat, tempName, task, update)
			if recErr != nil {
				return result, recErr
			}
			if !recovered {
				return result, fmt.Errorf("下载目标已存在，禁止覆盖：%s", absPath)
			}
		}
	} else if !os.IsNotExist(statErr) {
		return result, statErr
	}

	var partFile *os.File
	closePart := func() {
		if partFile != nil {
			_ = partFile.Close()
			partFile = nil
		}
	}
	defer closePart()

	if task.Published {
		// 本地文件已在崩溃前完成原子发布，仅核对后补发 COMMIT/FINISH
		f, err := root.OpenFile(base, os.O_RDONLY, 0)
		if err != nil {
			return result, err
		}
		partFile = f
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || uint64(info.Size()) != task.Size {
			return result, fmt.Errorf("本地已发布文件与断点大小不符")
		}
		expected, err := decodeDigestHex(task.SHA256)
		if err != nil {
			return result, err
		}
		actual, _, err := pal.DigestReaderContext(runCtx, f)
		if err != nil || actual != expected {
			return result, fmt.Errorf("本地已发布文件摘要校验失败")
		}
		closePart()
	} else if task.TempName != "" {
		info, err := root.Lstat(tempName)
		if err != nil {
			return result, fmt.Errorf("下载临时文件不可用，请取消后重试：%w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return result, fmt.Errorf("安全校验失败：下载临时文件为符号链接或非常规文件")
		}
		f, err := root.OpenFile(tempName, os.O_RDWR, 0600)
		if err != nil {
			return result, err
		}
		partFile = f
		dev, ino, nlink, err := pal.FileIdentity(f)
		if err != nil {
			return result, err
		}
		if (task.TempInode != 0 && (dev != task.TempDevice || ino != task.TempInode)) || nlink != 1 {
			return result, fmt.Errorf("下载临时文件身份或链接数异常，请取消后重新下载")
		}
		if uint64(info.Size()) < task.Done {
			return result, fmt.Errorf("下载临时文件短于本地检查点，请取消后重新下载")
		}
		if err = f.Truncate(int64(task.Done)); err != nil {
			return result, err
		}
	} else {
		if info, err := root.Lstat(tempName); err == nil {
			_ = info
			return result, fmt.Errorf("存在残留临时文件 %s，请先清理或取消", tempName)
		} else if !os.IsNotExist(err) {
			return result, err
		}
		f, err := root.OpenFile(tempName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return result, err
		}
		partFile = f
		dev, ino, _, err := pal.FileIdentity(f)
		if err != nil {
			closePart()
			_ = root.Remove(tempName)
			return result, err
		}
		if err = f.Sync(); err != nil {
			closePart()
			_ = root.Remove(tempName)
			return result, err
		}
		_ = pal.SyncParentDir(filepath.Join(parent, tempName))
		if err = update(func(cur *queue.Task) {
			cur.TempName = tempName
			cur.TempDevice = dev
			cur.TempInode = ino
		}); err != nil {
			closePart()
			_ = root.Remove(tempName)
			return result, err
		}
		task.TempName, task.TempDevice, task.TempInode = tempName, dev, ino
	}

	reqID, err := ensureRequestID(task, update)
	if err != nil {
		return result, err
	}

	st, release, err := c.openStream(runCtx, protocol.DirectionDownload)
	if err != nil {
		if op != nil && op.Mode() != 0 && errors.Is(err, context.Canceled) {
			return result, c.finishUnstartedControl(ctx, task, op.Mode(), update)
		}
		return result, err
	}
	defer release()

	wireCtx, cancelInit := context.WithTimeout(st.Context(), controlTimeout)
	defer cancelInit()

	initialized := false
	defer func() {
		closePart()
		if err != nil && initialized && op != nil && op.Mode() != 0 && errors.Is(err, context.Canceled) {
			err = c.cancelActiveStream(st, task, op.Mode(), root, result, update)
		}
		if err != nil && !errors.Is(err, ErrPaused) && !errors.Is(err, ErrCancelled) {
			st.Report(err)
		}
		if isFatalTransfer(err) {
			_ = removeDownloadPart(root, *task)
			_ = update(clearTaskCheckpoint)
		}
	}()

	var state protocol.ResponseState
	var size, offset uint64
	var digest protocol.Digest
	var tid protocol.TransferID

	if task.TransferID != "" {
		rawTID, e := decodeHex16(task.TransferID)
		if e != nil {
			return result, e
		}
		expectedDigest, e := decodeDigestHex(task.SHA256)
		if e != nil {
			return result, e
		}
		tid = protocol.TransferID(rawTID)
		if err = st.SendContext(wireCtx, protocol.TypeResumeReq, protocol.ResumeRequest{
			TransferID:   tid,
			Direction:    protocol.DirectionDownload,
			ClientOffset: task.Done,
			FileSHA256:   expectedDigest,
		}); err != nil {
			return result, err
		}
		var resp protocol.ResumeResponse
		if err = st.ExpectContext(wireCtx, protocol.TypeResumeResp, &resp); err != nil {
			return result, err
		}
		if resp.Direction != protocol.DirectionDownload || resp.FileSize != task.Size || resp.FileSHA256 != expectedDigest ||
			validateWireResponse(resp.State, resp.AcceptedOffset, task.Size) != nil ||
			(resp.State == protocol.ResponseActive && resp.AcceptedOffset > task.Done) {
			return result, &protocol.RemoteError{Code: protocol.CodeMetadataMismatch, Message: "下载恢复响应元数据或偏移非法"}
		}
		state, size, offset, digest = resp.State, resp.FileSize, resp.AcceptedOffset, resp.FileSHA256
	} else {
		if err = st.SendContext(wireCtx, protocol.TypeGetReq, protocol.GetRequest{
			RequestID: reqID,
			FileName:  task.Remote,
		}); err != nil {
			return result, err
		}
		var resp protocol.GetResponse
		if err = st.ExpectContext(wireCtx, protocol.TypeGetResp, &resp); err != nil {
			return result, err
		}
		if resp.TransferID == (protocol.TransferID{}) || resp.FileSize > math.MaxInt64 ||
			validateWireResponse(resp.State, resp.StartOffset, resp.FileSize) != nil ||
			resp.StartOffset > task.Done {
			return result, fmt.Errorf("下载响应元数据或偏移非法")
		}
		tid = resp.TransferID
		state, size, offset, digest = resp.State, resp.FileSize, resp.StartOffset, resp.FileSHA256
	}

	if size > c.cfg.MaxFileBytes {
		return result, fmt.Errorf("远端文件大小超出客户端限额 (%d > %d)", size, c.cfg.MaxFileBytes)
	}

	tidHex := hex.EncodeToString(tid[:])
	shaHex := hex.EncodeToString(digest[:])
	if err = update(func(cur *queue.Task) {
		cur.TransferID = tidHex
		cur.Size = size
		cur.SHA256 = shaHex
		cur.Done = offset
	}); err != nil {
		return result, err
	}
	task.TransferID, task.Size, task.SHA256, task.Done = tidHex, size, shaHex, offset
	result = Result{FileName: task.Remote, Size: size, SHA256: digest, ResumedFrom: offset}
	reportProgress(progress, offset, size, queue.Transferring)

	if state == protocol.ResponseCompleted {
		if !task.Published {
			return result, fmt.Errorf("服务端任务已完成但本地目标尚未发布")
		}
		_ = removeDownloadPart(root, *task)
		_ = update(func(cur *queue.Task) {
			cur.Done = size
			clearTaskCheckpoint(cur)
		})
		return result, nil
	}

	initialized = true
	cancelInit()
	if err = runCtx.Err(); err != nil {
		return result, err
	}

	if !task.Published {
		if err = partFile.Truncate(int64(offset)); err != nil {
			return result, err
		}
		if _, err = partFile.Seek(int64(offset), io.SeekStart); err != nil {
			return result, err
		}
		persistedOffset := offset
		lastFlush := time.Now()
		flushBatch := checkpointBatchBytes(st.ChunkSize(), st.WindowSize())
		flushDownload := func(sendAck bool) error {
			if offset > persistedOffset {
				if fErr := partFile.Sync(); fErr != nil {
					return fErr
				}
				if uErr := update(func(cur *queue.Task) {
					cur.Done = offset
				}); uErr != nil {
					return uErr
				}
				persistedOffset = offset
				task.Done = offset
				lastFlush = time.Now()
			}
			if sendAck {
				return st.SendContext(runCtx, protocol.TypeAck, protocol.Ack{NextOffset: offset})
			}
			return nil
		}

		for offset < size {
			var data protocol.Data
			if offset > persistedOffset && !st.HasBuffered() {
				readCtx, cancelRead := context.WithTimeout(runCtx, 15*time.Millisecond)
				err = st.ExpectContext(readCtx, protocol.TypeData, &data)
				timedOut := errors.Is(err, context.DeadlineExceeded) && readCtx.Err() == context.DeadlineExceeded && runCtx.Err() == nil
				cancelRead()
				if timedOut {
					if err = flushDownload(offset < size); err != nil {
						return result, err
					}
					continue
				}
			} else {
				err = st.ExpectContext(runCtx, protocol.TypeData, &data)
			}
			if err != nil {
				if offset > persistedOffset && (op == nil || op.Mode() != protocol.CancelAbort) {
					_ = flushDownload(false)
				}
				return result, err
			}
			if data.FileOffset != offset || uint64(len(data.Bytes)) > size-offset {
				return result, &protocol.RemoteError{Code: protocol.CodeOffsetMismatch, Message: "下载数据块偏移或长度越界"}
			}
			n, writeErr := partFile.Write(data.Bytes)
			if writeErr != nil {
				return result, writeErr
			}
			if n != len(data.Bytes) {
				return result, io.ErrShortWrite
			}
			offset += uint64(n)
			reportProgress(progress, offset, size, queue.Transferring)
			if offset == size || offset-persistedOffset >= flushBatch || time.Since(lastFlush) >= checkpointFlushInterval {
				if err = flushDownload(offset < size); err != nil {
					return result, err
				}
			}
		}
	}

	reportProgress(progress, size, size, queue.Verifying)
	if !task.Published {
		if err = partFile.Sync(); err != nil {
			return result, err
		}
		if _, err = partFile.Seek(0, io.SeekStart); err != nil {
			return result, err
		}
		actual, actualSize, hashErr := pal.DigestReaderContext(runCtx, partFile)
		closePart()
		if hashErr != nil {
			return result, hashErr
		}
		if actualSize != size || actual != digest {
			return result, &protocol.RemoteError{Code: protocol.CodeHashMismatch, Message: "下载文件 SHA-256 校验失败"}
		}

		// 防覆盖发布：先确定目标不存在，记录 Publishing 标记，再在 pal.Root 内硬链接/重命名发布
		if _, err = root.Lstat(base); err == nil {
			return result, fmt.Errorf("下载目标已存在，禁止覆盖：%s", absPath)
		} else if !os.IsNotExist(err) {
			return result, err
		}
		if !task.Publishing {
			if err = update(func(cur *queue.Task) {
				cur.Publishing = true
			}); err != nil {
				return result, err
			}
			task.Publishing = true
		}
		if linkErr := root.Link(tempName, base); linkErr != nil {
			if errors.Is(linkErr, os.ErrExist) || os.IsExist(linkErr) {
				_ = update(func(cur *queue.Task) {
					cur.Publishing = false
				})
				task.Publishing = false
				return result, fmt.Errorf("下载目标已存在，禁止覆盖：%s", absPath)
			}
			if renameErr := root.Rename(tempName, base); renameErr != nil {
				_ = update(func(cur *queue.Task) {
					cur.Publishing = false
				})
				task.Publishing = false
				return result, fmt.Errorf("发布下载文件失败：%w", renameErr)
			}
		}
		_ = pal.SyncParentDir(absPath)

		if err = update(func(cur *queue.Task) {
			cur.Published = true
			cur.Publishing = false
		}); err != nil {
			return result, err
		}
		task.Published = true
		task.Publishing = false
	}

	if err = st.SendContext(st.Context(), protocol.TypeAck, protocol.Ack{NextOffset: size}); err != nil {
		return result, err
	}
	if err = st.SendContext(st.Context(), protocol.TypeCommit, protocol.Commit{FileSize: size, FileSHA256: digest}); err != nil {
		return result, err
	}
	var finish protocol.Finish
	if err = st.ExpectContext(st.Context(), protocol.TypeFinish, &finish); err != nil {
		return result, err
	}
	if err = validateFinish(finish, tid, size, digest); err != nil {
		return result, err
	}
	_ = removeDownloadPart(root, *task)
	_ = update(func(cur *queue.Task) {
		cur.Done = size
		clearTaskCheckpoint(cur)
	})
	return result, nil
}

func (c *Client) finishUnstartedControl(ctx context.Context, task *queue.Task, mode protocol.CancelMode, update CheckpointUpdater) error {
	if mode == protocol.CancelAbort && task.HasCheckpoint() {
		if err := c.abortCheckpoint(context.WithoutCancel(ctx), task, update); err != nil {
			return fmt.Errorf("取消尚未确认，保留检查点：%w", err)
		}
		return ErrCancelled
	}
	if mode == protocol.CancelAbort {
		return ErrCancelled
	}
	return ErrPaused
}

func (c *Client) cancelActiveStream(st *session.Stream, task *queue.Task, mode protocol.CancelMode, dlRoot *pal.Root, result Result, update CheckpointUpdater) error {
	ctx, cancel := context.WithTimeout(st.Context(), controlTimeout)
	defer cancel()
	if err := st.SendContext(ctx, protocol.TypeCancel, protocol.Cancel{Mode: mode}); err != nil {
		return fmt.Errorf("任务控制未确认：%w", err)
	}
	tid, _ := decodeHex16(task.TransferID)
	for {
		frame, err := st.ReceiveContext(ctx)
		if err != nil {
			return fmt.Errorf("任务控制未确认，保留检查点：%w", err)
		}
		switch frame.Header.Type {
		case protocol.TypeData, protocol.TypeAck:
			continue
		case protocol.TypeFinish:
			var finish protocol.Finish
			if err = protocol.Unmarshal(frame.Payload, &finish); err != nil {
				return err
			}
			if err = validateFinish(finish, protocol.TransferID(tid), result.Size, result.SHA256); err != nil {
				return err
			}
			if dlRoot != nil {
				_ = removeDownloadPart(dlRoot, *task)
			}
			_ = update(func(cur *queue.Task) {
				cur.Done = result.Size
				clearTaskCheckpoint(cur)
			})
			return nil
		case protocol.TypeCancelAck:
			return applyCancelAck(frame.Payload, mode, task, dlRoot, update)
		default:
			return fmt.Errorf("任务控制收到非预期帧类型 %d", frame.Header.Type)
		}
	}
}

func applyCancelAck(payload []byte, mode protocol.CancelMode, task *queue.Task, dlRoot *pal.Root, update CheckpointUpdater) error {
	var ack protocol.CancelAck
	if err := protocol.Unmarshal(payload, &ack); err != nil {
		return err
	}
	if ack.Mode != mode || ack.AcceptedOffset > task.Size ||
		(task.Direction == protocol.DirectionDownload && ack.AcceptedOffset > task.Done) {
		return fmt.Errorf("任务控制确认偏移非法")
	}
	if mode == protocol.CancelAbort {
		if dlRoot != nil {
			if err := removeDownloadPart(dlRoot, *task); err != nil {
				return err
			}
		}
		if err := update(clearTaskCheckpoint); err != nil {
			return err
		}
		return ErrCancelled
	}
	if task.Direction == protocol.DirectionUpload {
		if err := update(func(cur *queue.Task) {
			cur.Done = ack.AcceptedOffset
		}); err != nil {
			return err
		}
	}
	return ErrPaused
}

func (c *Client) abortCheckpoint(parent context.Context, task *queue.Task, update CheckpointUpdater) (err error) {
	if !task.HasCheckpoint() {
		return nil
	}
	var dlRoot *pal.Root
	if task.Direction == protocol.DirectionDownload && task.Path != "" {
		r, openErr := pal.OpenUserRoot(filepath.Dir(task.Path))
		if openErr == nil {
			dlRoot = r
			defer dlRoot.Close()
		} else if !os.IsNotExist(openErr) && task.TempName != "" {
			return openErr
		}
	}

	ctx, cancel := context.WithTimeout(parent, controlTimeout)
	defer cancel()

	discardLocal := func() error {
		if dlRoot != nil {
			if err := removeDownloadPart(dlRoot, *task); err != nil {
				return err
			}
		}
		return update(clearTaskCheckpoint)
	}

	if task.TransferID == "" && task.RequestID == "" {
		return discardLocal()
	}
	// 上传任务若仅有 RequestID 而尚未保存摘要，则 PUT 必然尚未发出，可直接安全放弃本地检查点。
	if task.Direction == protocol.DirectionUpload && task.TransferID == "" && task.SHA256 == "" {
		return discardLocal()
	}

	st, release, err := c.openStream(ctx, task.Direction)
	if err != nil {
		return err
	}
	defer release()

	requestSent := false
	settled := false
	defer func() {
		if err == nil || !requestSent || settled || st.PeerFailed() {
			return
		}
		cleanCtx, cleanCancel := context.WithTimeout(st.Context(), 2*time.Second)
		defer cleanCancel()
		if sendErr := st.SendContext(cleanCtx, protocol.TypeCancel, protocol.Cancel{Mode: protocol.CancelPause}); sendErr == nil {
			for {
				f, recvErr := st.ReceiveContext(cleanCtx)
				if recvErr != nil {
					st.Report(err)
					_ = c.Close()
					return
				}
				if f.Header.Type == protocol.TypeCancelAck {
					return
				}
			}
		}
		st.Report(err)
		_ = c.Close()
	}()

	var handleRemoteCleanup = func(e error) error {
		var remote *protocol.RemoteError
		if errors.As(e, &remote) {
			settled = true
			switch remote.Code {
			case protocol.CodeNotFound,
				protocol.CodeTransferExpired,
				protocol.CodeHashMismatch,
				protocol.CodeTransferCancelled:
				return discardLocal()
			case protocol.CodeUnauthorized,
				protocol.CodeQuotaExceeded,
				protocol.CodeAlreadyExists,
				protocol.CodeInvalidName,
				protocol.CodeMetadataMismatch,
				protocol.CodeBadState,
				protocol.CodeBadFrame:
				if task.TransferID == "" {
					return discardLocal()
				}
			}
		}
		return e
	}

	if task.TransferID != "" {
		tid, e := decodeHex16(task.TransferID)
		if e != nil {
			return e
		}
		sha, e := decodeDigestHex(task.SHA256)
		if e != nil {
			return e
		}
		offset := uint64(0)
		if task.Direction == protocol.DirectionDownload {
			offset = task.Done
		}
		requestSent = true
		if err = st.SendContext(ctx, protocol.TypeResumeReq, protocol.ResumeRequest{
			TransferID:   protocol.TransferID(tid),
			Direction:    task.Direction,
			ClientOffset: offset,
			FileSHA256:   sha,
		}); err != nil {
			return err
		}
		var resp protocol.ResumeResponse
		if err = st.ExpectContext(ctx, protocol.TypeResumeResp, &resp); err != nil {
			return handleRemoteCleanup(err)
		}
		if resp.State == protocol.ResponseCompleted {
			settled = true
			return discardLocal()
		}
	} else {
		reqID, e := decodeHex16(task.RequestID)
		if e != nil {
			return e
		}
		if task.Direction == protocol.DirectionUpload {
			sha, e := decodeDigestHex(task.SHA256)
			if e != nil {
				return e
			}
			requestSent = true
			if err = st.SendContext(ctx, protocol.TypePutReq, protocol.PutRequest{
				RequestID:  protocol.RequestID(reqID),
				FileName:   task.Remote,
				FileSize:   task.Size,
				FileSHA256: sha,
			}); err != nil {
				return err
			}
			var resp protocol.PutResponse
			if err = st.ExpectContext(ctx, protocol.TypePutResp, &resp); err != nil {
				return handleRemoteCleanup(err)
			}
			if resp.State == protocol.ResponseCompleted {
				settled = true
				return discardLocal()
			}
			tidHex := hex.EncodeToString(resp.TransferID[:])
			if err = update(func(cur *queue.Task) { cur.TransferID = tidHex }); err != nil {
				return err
			}
			task.TransferID = tidHex
		} else {
			requestSent = true
			if err = st.SendContext(ctx, protocol.TypeGetReq, protocol.GetRequest{
				RequestID: protocol.RequestID(reqID),
				FileName:  task.Remote,
			}); err != nil {
				return err
			}
			var resp protocol.GetResponse
			if err = st.ExpectContext(ctx, protocol.TypeGetResp, &resp); err != nil {
				return handleRemoteCleanup(err)
			}
			if resp.State == protocol.ResponseCompleted {
				settled = true
				return discardLocal()
			}
			tidHex := hex.EncodeToString(resp.TransferID[:])
			shaHex := hex.EncodeToString(resp.FileSHA256[:])
			if err = update(func(cur *queue.Task) {
				cur.TransferID = tidHex
				cur.Size = resp.FileSize
				cur.SHA256 = shaHex
			}); err != nil {
				return err
			}
			task.TransferID = tidHex
			task.Size = resp.FileSize
			task.SHA256 = shaHex
		}
	}

	if err = st.SendContext(ctx, protocol.TypeCancel, protocol.Cancel{Mode: protocol.CancelAbort}); err != nil {
		return err
	}
	for {
		frame, recvErr := st.ReceiveContext(ctx)
		if recvErr != nil {
			return handleRemoteCleanup(recvErr)
		}
		if frame.Header.Type == protocol.TypeData || frame.Header.Type == protocol.TypeAck {
			continue
		}
		if frame.Header.Type != protocol.TypeCancelAck {
			return fmt.Errorf("取消确认收到非预期帧类型 %d", frame.Header.Type)
		}
		settled = true
		err = applyCancelAck(frame.Payload, protocol.CancelAbort, task, dlRoot, update)
		if errors.Is(err, ErrCancelled) {
			return nil
		}
		return err
	}
}

func recoverCrashPublished(ctx context.Context, root *pal.Root, base string, baseStat os.FileInfo, tempName string, task *queue.Task, update CheckpointUpdater) (bool, error) {
	if task.TempName != tempName || task.TransferID == "" || task.SHA256 == "" {
		return false, nil
	}
	if baseStat.Mode()&os.ModeSymlink != 0 || !baseStat.Mode().IsRegular() || uint64(baseStat.Size()) != task.Size {
		return false, nil
	}
	expected, err := decodeDigestHex(task.SHA256)
	if err != nil {
		return false, nil
	}

	bf, err := root.OpenFile(base, os.O_RDONLY, 0)
	if err != nil {
		return false, err
	}
	devB, inoB, _, idBErr := pal.FileIdentity(bf)
	actual, actualSize, hashErr := pal.DigestReaderContext(ctx, bf)
	_ = bf.Close()
	if idBErr != nil || hashErr != nil || actualSize != task.Size || actual != expected {
		return false, nil
	}

	matchedTemp := false
	if tf, err := root.OpenFile(tempName, os.O_RDONLY, 0); err == nil {
		devT, inoT, _, idTErr := pal.FileIdentity(tf)
		_ = tf.Close()
		if idTErr == nil && devB == devT && inoB == inoT &&
			(task.TempInode == 0 || (devT == task.TempDevice && inoT == task.TempInode)) {
			matchedTemp = true
		}
	} else if task.Publishing && os.IsNotExist(err) {
		matchedTemp = true
	}
	if !matchedTemp {
		return false, nil
	}

	if err = update(func(cur *queue.Task) {
		cur.Done = task.Size
		cur.Published = true
		cur.Publishing = false
	}); err != nil {
		return false, err
	}
	task.Done = task.Size
	task.Published = true
	task.Publishing = false
	return true, nil
}

func removeDownloadPart(root *pal.Root, t queue.Task) error {
	if t.TempName == "" {
		return nil
	}
	info, err := root.Lstat(t.TempName)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("拒绝移除非常规下载临时文件：%s", t.TempName)
	}
	if t.TempInode != 0 {
		f, err := root.OpenFile(t.TempName, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		dev, ino, _, idErr := pal.FileIdentity(f)
		_ = f.Close()
		if idErr != nil {
			return idErr
		}
		if dev != t.TempDevice || ino != t.TempInode {
			return fmt.Errorf("临时文件已被替换，跳过移除：%s", t.TempName)
		}
	}
	if err := root.Remove(t.TempName); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
