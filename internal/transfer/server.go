package transfer

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"log/slog"

	"sft-tui/internal/config"
	"sft-tui/internal/protocol"
	"sft-tui/internal/security"
	"sft-tui/internal/session"
	"sft-tui/internal/storage"
)

// ServerHooks 供阶段 4 挂载限速器与运行时监控快照，零值时使用安全的无操作默认实现。
type ServerHooks struct {
	Authorize        func(user, operation string) (security.Principal, error)
	AcquireStream    func(user string, bw config.Bandwidth) (release func(), err error)
	AcquireList      func(user string, bw config.Bandwidth) (release func(), err error)
	WaitBandwidth    func(ctx context.Context, user string, bw config.Bandwidth, dir protocol.Direction, n int) error
	WaitCheckpoint   func(ctx context.Context, user string, bw config.Bandwidth) error
	OnConnect        func(user, remote string) (connID uint64, ok bool)
	OnDisconnect     func(connID uint64)
	OnStreamBegin    func(connID uint64, streamID uint32, user, dir string) (taskID uint64, accepted bool)
	OnStreamDescribe func(taskID uint64, fileName string, size, offset uint64)
	OnStreamProgress func(taskID uint64, offset uint64, phase string)
	OnStreamEnd      func(taskID uint64, outcome string, err error)
}

// Server 实现 SFT/1 安全传输服务端协议处理器，复用 storage.Store 与 security.Authorizer。
type Server struct {
	cfg        config.Server
	store      *storage.Store
	auth       *security.Authorizer
	hooks      ServerHooks
	logger     *slog.Logger
	tlsBase    *tls.Config
	currentTLS atomic.Pointer[tls.Config]
}

func NewServer(cfg config.Server, store *storage.Store, auth *security.Authorizer, tlsCfg *tls.Config, hooks ServerHooks, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if store == nil || auth == nil || tlsCfg == nil {
		return nil, errors.New("server requires non-nil store, authorizer and tls config")
	}
	store.Configure(storage.Limits{
		MaxFileBytes:               cfg.Resources.MaxFileBytes,
		UserStorageBytes:           cfg.Resources.UserStorageBytes,
		MaxPendingTransfersPerUser: cfg.Resources.MaxPendingTransfersPerUser,
		MaxFilesPerUser:            cfg.Resources.MaxFilesPerUser,
		TransferRetention:          time.Duration(cfg.Resources.TransferRetentionSeconds) * time.Second,
	})
	s := &Server{
		cfg:    cfg,
		store:  store,
		auth:   auth,
		hooks:  hooks,
		logger: logger,
	}
	s.currentTLS.Store(tlsCfg)
	s.tlsBase = &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			return s.currentTLS.Load(), nil
		},
	}
	return s, nil
}

// UpdateTLS 原子切换热重载后的 TLS 配置快照。
func (s *Server) UpdateTLS(tlsCfg *tls.Config) {
	if tlsCfg != nil {
		s.currentTLS.Store(tlsCfg)
	}
}

type connGate struct {
	acquireHandshake func() (release func(), accepted bool)
	onAuthenticate   func(raw net.Conn, user string) (connID uint64, ok bool)
	onDisconnect     func(raw net.Conn, connID uint64)
	onMuxOpen        func(m *session.Mux)
	onMuxClose       func(m *session.Mux)
}

// HandleConn 对单条已接受的 TCP 连接执行 mTLS 1.3 握手、身份绑定与多路复用流调度。
func (s *Server) HandleConn(ctx context.Context, raw net.Conn) {
	s.handleConn(ctx, raw, nil)
}

func (s *Server) handleConn(ctx context.Context, raw net.Conn, gate *connGate) {
	defer raw.Close()

	var relHS func()
	if gate != nil && gate.acquireHandshake != nil {
		var ok bool
		relHS, ok = gate.acquireHandshake()
		if !ok {
			return
		}
	}

	conn := tls.Server(raw, s.tlsBase)
	timeout := time.Duration(s.cfg.Timeouts.HandshakeSeconds) * time.Second
	_ = conn.SetDeadline(time.Now().Add(timeout))
	handshakeCtx, cancel := context.WithTimeout(ctx, timeout)
	err := conn.HandshakeContext(handshakeCtx)
	cancel()
	if relHS != nil {
		relHS()
	}
	if err != nil {
		s.logger.Debug("TLS 握手失败", "remote", raw.RemoteAddr(), "error", err)
		return
	}
	_ = conn.SetDeadline(time.Time{})

	peer := conn.ConnectionState().PeerCertificates
	if len(peer) == 0 {
		return
	}
	principal, err := s.auth.Register(peer[0], raw)
	if err != nil {
		s.logger.Warn("客户端证书授权拒绝", "remote", raw.RemoteAddr(), "error", err)
		return
	}
	defer s.auth.Unregister(raw)

	user := principal.UserID
	var connID uint64
	if gate != nil && gate.onAuthenticate != nil {
		var ok bool
		connID, ok = gate.onAuthenticate(raw, user)
		if !ok {
			return
		}
		if gate.onDisconnect != nil {
			defer gate.onDisconnect(raw, connID)
		}
	} else if s.hooks.OnConnect != nil {
		remote := ""
		if addr := raw.RemoteAddr(); addr != nil {
			remote = addr.String()
		}
		var ok bool
		connID, ok = s.hooks.OnConnect(user, remote)
		if !ok {
			return
		}
		defer func() {
			if s.hooks.OnDisconnect != nil {
				s.hooks.OnDisconnect(connID)
			}
		}()
	}

	c := &session.Conn{
		Net:      conn,
		Chunk:    s.cfg.Limits.ChunkSize,
		Timeouts: s.cfg.Timeouts,
	}
	f, err := c.Read(s.cfg.Timeouts.HelloSeconds)
	if err != nil {
		s.sendInitError(c, err)
		return
	}
	if f.Header.Type != protocol.TypeHello {
		s.sendInitError(c, &protocol.RemoteError{Code: protocol.CodeBadState, Message: "连接必须先发送 HELLO"})
		return
	}
	var h protocol.Hello
	if err = protocol.Unmarshal(f.Payload, &h); err == nil {
		err = protocol.ValidateHello(h)
	}
	if err != nil {
		s.sendInitError(c, err)
		return
	}

	ack := protocol.HelloAck{
		MaxStreams:     min(h.ClientMaxStreams, s.cfg.Limits.MaxStreams),
		ChunkSize:      min(h.ClientMaxChunk, s.cfg.Limits.ChunkSize),
		WindowSize:     min(h.ClientWindow, s.cfg.Limits.WindowSize),
		IdleTimeoutSec: s.cfg.Timeouts.IdleSeconds,
	}
	c.Chunk = ack.ChunkSize
	if err = c.Send(protocol.TypeHelloAck, 0, ack); err != nil {
		return
	}

	var workers sync.WaitGroup
	mux := session.NewMux(c, ack.MaxStreams, ack.WindowSize, func(st *session.Stream, initial protocol.Frame) {
		dirLabel := classifyStreamDirection(initial)
		var monID uint64
		accepted := true
		if s.hooks.OnStreamBegin != nil {
			monID, accepted = s.hooks.OnStreamBegin(connID, uint32(st.ID), user, dirLabel)
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer st.Release()
			if !accepted {
				if s.hooks.OnStreamEnd != nil {
					s.hooks.OnStreamEnd(monID, "rejected", &protocol.RemoteError{Code: protocol.CodeTempUnavailable, Message: "服务正在排空，不再接收新任务"})
				}
				st.Report(&protocol.RemoteError{Code: protocol.CodeTempUnavailable, Message: "服务正在排空，不再接收新任务"})
				return
			}
			if err := s.runStream(user, st, initial, monID); err != nil {
				if cause := st.Cause(); cause != nil {
					err = cause
				}
				st.Report(storage.AsRemote(err))
			}
		}()
	})

	if gate != nil && gate.onMuxOpen != nil {
		gate.onMuxOpen(mux)
		if gate.onMuxClose != nil {
			defer gate.onMuxClose(mux)
		}
	}

	stopConn := context.AfterFunc(ctx, func() {
		mux.Close(ctx.Err())
	})
	defer stopConn()

	<-mux.Context().Done()
	mux.Wait()
	workers.Wait()
}

func classifyStreamDirection(f protocol.Frame) string {
	switch f.Header.Type {
	case protocol.TypeGetReq:
		return "download"
	case protocol.TypeListReq:
		return "list"
	case protocol.TypeResumeReq:
		var req protocol.ResumeRequest
		if protocol.Unmarshal(f.Payload, &req) == nil && req.Direction == protocol.DirectionDownload {
			return "download"
		}
	}
	return "upload"
}

func (s *Server) sendInitError(c *session.Conn, err error) {
	var r *protocol.RemoteError
	if errors.As(err, &r) && r.Code == protocol.CodeBadMagic {
		return
	}
	_ = c.Send(protocol.TypeError, 0, *storage.AsRemote(err))
}

func (s *Server) authorize(user, op string) (security.Principal, error) {
	if s.hooks.Authorize != nil {
		return s.hooks.Authorize(user, op)
	}
	return s.auth.Check(user, op)
}

func (s *Server) acquireStream(user string, bw config.Bandwidth) (func(), error) {
	if s.hooks.AcquireStream != nil {
		return s.hooks.AcquireStream(user, bw)
	}
	return func() {}, nil
}

func (s *Server) acquireList(user string, bw config.Bandwidth) (func(), error) {
	if s.hooks.AcquireList != nil {
		return s.hooks.AcquireList(user, bw)
	}
	return s.acquireStream(user, bw)
}

func (s *Server) waitBandwidth(ctx context.Context, user string, bw config.Bandwidth, dir protocol.Direction, n int) error {
	if s.hooks.WaitBandwidth != nil && n > 0 {
		return s.hooks.WaitBandwidth(ctx, user, bw, dir, n)
	}
	return nil
}

func (s *Server) waitCheckpoint(ctx context.Context, user string, bw config.Bandwidth) error {
	if s.hooks.WaitCheckpoint != nil {
		return s.hooks.WaitCheckpoint(ctx, user, bw)
	}
	return nil
}

func (s *Server) describe(taskID uint64, name string, size, offset uint64) {
	if s.hooks.OnStreamDescribe != nil {
		s.hooks.OnStreamDescribe(taskID, name, size, offset)
	}
}

func (s *Server) progress(taskID uint64, offset uint64, phase string) {
	if s.hooks.OnStreamProgress != nil {
		s.hooks.OnStreamProgress(taskID, offset, phase)
	}
}

type transferLogMeta struct {
	file       string
	transferID string
	size       uint64
	offset     uint64
}

func extractInitialFileName(initial protocol.Frame) string {
	switch initial.Header.Type {
	case protocol.TypePutReq:
		var req protocol.PutRequest
		if protocol.Unmarshal(initial.Payload, &req) == nil {
			return req.FileName
		}
	case protocol.TypeGetReq:
		var req protocol.GetRequest
		if protocol.Unmarshal(initial.Payload, &req) == nil {
			return req.FileName
		}
	}
	return ""
}

func (s *Server) runStream(user string, st *session.Stream, initial protocol.Frame, monID uint64) (resultErr error) {
	outcome := "completed"
	isTransfer := initial.Header.Type != protocol.TypeListReq
	opName := "upload"
	meta := &transferLogMeta{file: extractInitialFileName(initial)}
	defer func() {
		if resultErr != nil && outcome == "completed" {
			outcome = "failed"
		}
		if s.hooks.OnStreamEnd != nil {
			s.hooks.OnStreamEnd(monID, outcome, resultErr)
		}
		if !isTransfer {
			return
		}
		attrs := []any{
			"user", user,
			"file", meta.file,
			"direction", opName,
			"result", outcome,
			"size", meta.size,
			"offset", meta.offset,
		}
		if meta.transferID != "" {
			attrs = append(attrs, "transfer_id", meta.transferID)
		}
		if resultErr != nil {
			attrs = append(attrs, "error", resultErr.Error())
		}
		switch outcome {
		case "completed":
			s.logger.Info("传输完成", attrs...)
		case "paused", "cancelled":
			s.logger.Info("传输结束", attrs...)
		default:
			s.logger.Warn("传输失败", attrs...)
		}
	}()

	if initial.Header.Type == protocol.TypeListReq {
		s.describe(monID, "（文件列表）", 0, 0)
		s.progress(monID, 0, "浏览列表")
		return s.runList(user, st, initial)
	}

	dir := protocol.DirectionUpload
	var resume protocol.ResumeRequest
	if initial.Header.Type == protocol.TypeGetReq {
		dir = protocol.DirectionDownload
	} else if initial.Header.Type == protocol.TypeResumeReq {
		if err := protocol.Unmarshal(initial.Payload, &resume); err != nil {
			return err
		}
		if !resume.Direction.Valid() {
			return &protocol.RemoteError{Code: protocol.CodeBadFrame, Message: "非法恢复方向"}
		}
		dir = resume.Direction
	}

	if dir == protocol.DirectionDownload {
		opName = "download"
	}
	principal, err := s.authorize(user, opName)
	if err != nil {
		return err
	}
	var release func()
	if initial.Header.Type == protocol.TypeResumeReq {
		release, err = s.acquireList(user, principal.Bandwidth)
	} else {
		release, err = s.acquireStream(user, principal.Bandwidth)
	}
	if err != nil {
		return err
	}
	defer release()

	if dir == protocol.DirectionUpload {
		outcome, err = s.handleUpload(user, principal.Bandwidth, st, initial, resume, monID, release, meta)
		return err
	}
	outcome, err = s.handleDownload(user, principal.Bandwidth, st, initial, resume, monID, release, meta)
	return err
}

func (s *Server) runList(user string, st *session.Stream, initial protocol.Frame) error {
	var req protocol.ListRequest
	if err := protocol.Unmarshal(initial.Payload, &req); err != nil {
		return err
	}
	principal, err := s.authorize(user, "download")
	if err != nil {
		return err
	}
	release, err := s.acquireList(user, principal.Bandwidth)
	if err != nil {
		return err
	}
	defer release()

	after := req.After
	for {
		if _, err := s.authorize(user, "download"); err != nil {
			return err
		}
		files, more, err := s.store.ListPublishedFiles(st.Context(), user, after, protocol.ListPageSize)
		if err != nil {
			return err
		}
		if !more {
			release()
		}
		if err := st.Send(protocol.TypeListResp, protocol.ListResponse{More: more, Files: files}); err != nil {
			return err
		}
		if !more {
			return nil
		}
		after = files[len(files)-1].FileName
		if err := st.Expect(protocol.TypeListNext, &struct{}{}); err != nil {
			return err
		}
	}
}

func (s *Server) handleUpload(user string, bw config.Bandwidth, st *session.Stream, initial protocol.Frame, resume protocol.ResumeRequest, monID uint64, release func(), meta *transferLogMeta) (outcome string, resultErr error) {
	outcome = "completed"
	var task storage.Task
	var lease uint64
	var err error

	if initial.Header.Type == protocol.TypeResumeReq {
		task, lease, err = s.store.ResumeUploadSession(st.Context(), user, resume)
	} else {
		var req protocol.PutRequest
		if err = protocol.Unmarshal(initial.Payload, &req); err == nil {
			task, lease, err = s.store.BeginUploadSession(st.Context(), user, req)
		}
	}
	if err != nil {
		return "failed", err
	}

	meta.file = task.FileName
	meta.transferID = hex.EncodeToString(task.ID[:])
	meta.size = task.FileSize
	meta.offset = task.Offset
	s.describe(monID, task.FileName, task.FileSize, task.Offset)
	s.logger.Info("传输开始",
		"user", user,
		"file", task.FileName,
		"direction", "upload",
		"transfer_id", meta.transferID,
		"size", task.FileSize,
		"offset", task.Offset,
		"resumed", initial.Header.Type == protocol.TypeResumeReq,
	)

	settled := false
	defer func() {
		if settled || lease == 0 {
			return
		}
		finalStatus := storage.TaskPaused
		if isHardServerFailure(resultErr) || isHardServerFailure(st.Cause()) {
			finalStatus = storage.TaskFailed
		}
		_ = s.store.ReleaseTask(context.Background(), task.ID, lease, finalStatus)
	}()

	state := protocol.ResponseActive
	if task.Status == storage.TaskCompleted {
		state = protocol.ResponseCompleted
		settled = true
		release()
	}
	st.EnableReceive(task.Offset)

	if initial.Header.Type == protocol.TypeResumeReq {
		err = st.Send(protocol.TypeResumeResp, protocol.ResumeResponse{
			State:          state,
			Direction:      protocol.DirectionUpload,
			FileSize:       task.FileSize,
			FileSHA256:     task.FileSHA256,
			AcceptedOffset: task.Offset,
		})
	} else {
		err = st.Send(protocol.TypePutResp, protocol.PutResponse{
			TransferID: task.ID,
			State:      state,
			NextOffset: task.Offset,
		})
	}
	if err != nil || state == protocol.ResponseCompleted {
		return outcome, err
	}

	offset := task.Offset
	committedOffset := task.Offset
	lastFlush := time.Now()
	flushBatch := uint64(max(st.ChunkSize(), min(st.WindowSize()/2, 512*1024)))
	flushUpload := func(waitCtx context.Context) error {
		if offset <= committedOffset {
			return nil
		}
		if err := s.waitCheckpoint(waitCtx, user, bw); err != nil {
			return err
		}
		committed, err := s.store.FlushUploadCheckpoint(st.Context(), task.ID, lease)
		if err != nil {
			return err
		}
		committedOffset = committed
		meta.offset = committedOffset
		lastFlush = time.Now()
		s.progress(monID, committedOffset, "传输")
		return st.Send(protocol.TypeAck, protocol.Ack{NextOffset: committedOffset})
	}
	if offset == task.FileSize {
		if err = st.Send(protocol.TypeAck, protocol.Ack{NextOffset: offset}); err != nil {
			return "failed", err
		}
	}

	for {
		var frame protocol.Frame
		var err error
		if offset > committedOffset && !st.HasBuffered() {
			waitCtx, cancelWait := context.WithTimeout(st.Context(), 15*time.Millisecond)
			frame, err = st.ReceiveContext(waitCtx)
			timedOut := errors.Is(err, context.DeadlineExceeded) && waitCtx.Err() == context.DeadlineExceeded && st.Context().Err() == nil
			cancelWait()
			if timedOut {
				if fErr := flushUpload(st.PayloadContext()); fErr != nil {
					if st.PayloadContext().Err() != nil && st.Context().Err() == nil {
						continue
					}
					return "paused", fErr
				}
				continue
			}
		} else {
			frame, err = st.Receive()
		}
		if err != nil {
			return "paused", err
		}
		switch frame.Header.Type {
		case protocol.TypeData:
			var d protocol.Data
			if err = protocol.Unmarshal(frame.Payload, &d); err != nil {
				return "failed", err
			}
			if _, err = s.authorize(user, "upload"); err != nil {
				return "failed", err
			}
			if st.PayloadContext().Err() != nil && st.Context().Err() == nil {
				continue
			}
			if err = s.waitBandwidth(st.PayloadContext(), user, bw, protocol.DirectionUpload, len(d.Bytes)); err != nil {
				if st.PayloadContext().Err() != nil && st.Context().Err() == nil {
					continue
				}
				return "paused", err
			}
			if _, err = s.authorize(user, "upload"); err != nil {
				return "failed", err
			}
			newOffset, err := s.store.StageUploadChunk(st.Context(), task.ID, lease, d.FileOffset, d.Bytes)
			if err != nil {
				return "failed", err
			}
			offset = newOffset
			s.progress(monID, offset, "传输")
			if offset == task.FileSize || offset-committedOffset >= flushBatch || time.Since(lastFlush) >= 50*time.Millisecond {
				if err = flushUpload(st.PayloadContext()); err != nil {
					if st.PayloadContext().Err() != nil && st.Context().Err() == nil {
						continue
					}
					return "paused", err
				}
			}

		case protocol.TypeCommit:
			if offset > committedOffset {
				if err = flushUpload(st.Context()); err != nil {
					return "failed", err
				}
			}
			s.progress(monID, offset, "校验提交")
			var c protocol.Commit
			if err = protocol.Unmarshal(frame.Payload, &c); err != nil {
				return "failed", err
			}
			if _, err = s.authorize(user, "upload"); err != nil {
				return "failed", err
			}
			if err = s.store.CommitUploadSession(st.Context(), task.ID, lease, c.FileSize, c.FileSHA256); err != nil {
				return "failed", err
			}
			settled = true
			meta.offset = task.FileSize
			release()
			return "completed", st.Send(protocol.TypeFinish, protocol.Finish{
				TransferID: task.ID,
				FileSize:   task.FileSize,
				FileSHA256: task.FileSHA256,
			})

		case protocol.TypeCancel:
			var c protocol.Cancel
			if err = protocol.Unmarshal(frame.Payload, &c); err != nil {
				return "failed", err
			}
			if !c.Mode.Valid() {
				return "failed", &protocol.RemoteError{Code: protocol.CodeBadFrame, Message: "非法取消模式"}
			}
			if c.Mode == protocol.CancelPause && offset > committedOffset {
				if committed, fErr := s.store.FlushUploadCheckpoint(st.Context(), task.ID, lease); fErr == nil {
					committedOffset = committed
				}
			}
			targetStatus := storage.TaskPaused
			outcome = "paused"
			if c.Mode == protocol.CancelAbort {
				targetStatus = storage.TaskCancelled
				outcome = "cancelled"
			}
			if err = s.store.ReleaseTask(st.Context(), task.ID, lease, targetStatus); err != nil {
				return "failed", err
			}
			settled = true
			meta.offset = committedOffset
			release()
			return outcome, st.Send(protocol.TypeCancelAck, protocol.CancelAck{
				Mode:           c.Mode,
				AcceptedOffset: committedOffset,
			})

		default:
			return "failed", &protocol.RemoteError{Code: protocol.CodeBadState, Message: "上传消息状态非法"}
		}
	}
}

func (s *Server) handleDownload(user string, bw config.Bandwidth, st *session.Stream, initial protocol.Frame, resume protocol.ResumeRequest, monID uint64, release func(), meta *transferLogMeta) (outcome string, resultErr error) {
	outcome = "completed"
	var task storage.Task
	var file interface {
		io.Reader
		io.Seeker
		io.Closer
	}
	var lease uint64
	var err error

	if initial.Header.Type == protocol.TypeResumeReq {
		task, file, lease, err = s.store.ResumeDownloadSession(st.Context(), user, resume)
	} else {
		var req protocol.GetRequest
		if err = protocol.Unmarshal(initial.Payload, &req); err == nil {
			task, file, lease, err = s.store.BeginDownloadSession(st.Context(), user, req)
		}
	}
	if err != nil {
		return "failed", err
	}
	if file != nil {
		defer file.Close()
	}

	meta.file = task.FileName
	meta.transferID = hex.EncodeToString(task.ID[:])
	meta.size = task.FileSize
	meta.offset = task.Offset
	s.describe(monID, task.FileName, task.FileSize, task.Offset)
	s.logger.Info("传输开始",
		"user", user,
		"file", task.FileName,
		"direction", "download",
		"transfer_id", meta.transferID,
		"size", task.FileSize,
		"offset", task.Offset,
		"resumed", initial.Header.Type == protocol.TypeResumeReq,
	)

	var ackAtomic atomic.Uint64
	ackAtomic.Store(task.Offset)
	settled := false
	defer func() {
		if settled || lease == 0 {
			return
		}
		curAck := ackAtomic.Load()
		meta.offset = curAck
		if curAck > task.Offset {
			_ = s.store.CheckpointDownloadSession(context.Background(), task.ID, lease, curAck)
		}
		finalStatus := storage.TaskPaused
		if isHardServerFailure(resultErr) || isHardServerFailure(st.Cause()) {
			finalStatus = storage.TaskFailed
		}
		_ = s.store.ReleaseTask(context.Background(), task.ID, lease, finalStatus)
	}()

	state := protocol.ResponseActive
	if task.Status == storage.TaskCompleted {
		state = protocol.ResponseCompleted
		settled = true
		release()
	}

	if initial.Header.Type == protocol.TypeResumeReq {
		err = st.Send(protocol.TypeResumeResp, protocol.ResumeResponse{
			State:          state,
			Direction:      protocol.DirectionDownload,
			FileSize:       task.FileSize,
			FileSHA256:     task.FileSHA256,
			AcceptedOffset: task.Offset,
		})
	} else {
		err = st.Send(protocol.TypeGetResp, protocol.GetResponse{
			TransferID:  task.ID,
			State:       state,
			FileSize:    task.FileSize,
			FileSHA256:  task.FileSHA256,
			StartOffset: task.Offset,
		})
	}
	if err != nil || state == protocol.ResponseCompleted {
		return outcome, err
	}

	offset := task.Offset
	startOffset := offset
	if _, err = file.Seek(int64(offset), io.SeekStart); err != nil {
		return "failed", err
	}

	cancelDownload := func(payload []byte) (string, error) {
		var c protocol.Cancel
		if err := protocol.Unmarshal(payload, &c); err != nil {
			return "failed", err
		}
		if !c.Mode.Valid() {
			return "failed", &protocol.RemoteError{Code: protocol.CodeBadFrame, Message: "非法取消模式"}
		}
		curAck := ackAtomic.Load()
		meta.offset = curAck
		if curAck > task.Offset {
			if err := s.store.CheckpointDownloadSession(st.Context(), task.ID, lease, curAck); err != nil {
				return "failed", err
			}
		}
		status := storage.TaskPaused
		out := "paused"
		if c.Mode == protocol.CancelAbort {
			status = storage.TaskCancelled
			out = "cancelled"
		}
		if err := s.store.ReleaseTask(st.Context(), task.ID, lease, status); err != nil {
			return "failed", err
		}
		settled = true
		meta.offset = curAck
		release()
		return out, st.Send(protocol.TypeCancelAck, protocol.CancelAck{
			Mode:           c.Mode,
			AcceptedOffset: curAck,
		})
	}

	if startOffset == task.FileSize {
		for {
			frame, err := st.Receive()
			if err != nil {
				return "paused", err
			}
			if frame.Header.Type == protocol.TypeCancel {
				return cancelDownload(frame.Payload)
			}
			if frame.Header.Type != protocol.TypeAck {
				return "failed", &protocol.RemoteError{Code: protocol.CodeBadState, Message: "下载等待 ACK"}
			}
			var ack protocol.Ack
			if err = protocol.Unmarshal(frame.Payload, &ack); err != nil {
				return "failed", err
			}
			if ack.NextOffset != startOffset {
				return "failed", &protocol.RemoteError{Code: protocol.CodeOffsetMismatch, Message: "下载 ACK 偏移非法"}
			}
			break
		}
	} else {
		win := uint64(max(st.WindowSize(), st.ChunkSize()))
		ckptBatch := uint64(max(st.ChunkSize(), min(st.WindowSize()/2, 512*1024)))
		dlCtx, stopDL := context.WithCancel(st.Context())
		defer stopDL()

		var sentAtomic atomic.Uint64
		sentAtomic.Store(startOffset)
		ackSignal := make(chan struct{}, 1)

		type dlAckOutcome struct {
			cancelled bool
			out       string
			err       error
		}
		ackDone := make(chan dlAckOutcome, 1)
		go func() {
			curAck := startOffset
			persisted := startOffset
			for curAck < task.FileSize {
				frame, rErr := st.ReceiveContext(dlCtx)
				if rErr != nil {
					stopDL()
					ackDone <- dlAckOutcome{out: "paused", err: rErr}
					return
				}
				if frame.Header.Type == protocol.TypeCancel {
					stopDL()
					out, cErr := cancelDownload(frame.Payload)
					ackDone <- dlAckOutcome{cancelled: true, out: out, err: cErr}
					return
				}
				if frame.Header.Type != protocol.TypeAck {
					stopDL()
					ackDone <- dlAckOutcome{out: "failed", err: &protocol.RemoteError{Code: protocol.CodeBadState, Message: "下载等待 ACK"}}
					return
				}
				var ack protocol.Ack
				if uErr := protocol.Unmarshal(frame.Payload, &ack); uErr != nil {
					stopDL()
					ackDone <- dlAckOutcome{out: "failed", err: uErr}
					return
				}
				maxSent := sentAtomic.Load()
				if ack.NextOffset < curAck || ack.NextOffset > maxSent {
					stopDL()
					ackDone <- dlAckOutcome{out: "failed", err: &protocol.RemoteError{Code: protocol.CodeOffsetMismatch, Message: "下载 ACK 偏移非法"}}
					return
				}
				if ack.NextOffset == curAck {
					continue
				}
				curAck = ack.NextOffset
				ackAtomic.Store(curAck)
				select {
				case ackSignal <- struct{}{}:
				default:
				}
				if curAck == task.FileSize || curAck-persisted >= ckptBatch {
					if cErr := s.store.CheckpointDownloadSession(st.Context(), task.ID, lease, curAck); cErr != nil {
						stopDL()
						ackDone <- dlAckOutcome{out: "failed", err: cErr}
						return
					}
					persisted = curAck
				}
				s.progress(monID, curAck, "传输")
			}
			if curAck > persisted {
				if cErr := s.store.CheckpointDownloadSession(st.Context(), task.ID, lease, curAck); cErr != nil {
					ackDone <- dlAckOutcome{out: "failed", err: cErr}
					return
				}
			}
			ackDone <- dlAckOutcome{}
		}()

		buf := make([]byte, st.ChunkSize())
		sendAborted := false
		var sendOut string
		var sendErr error
	sendLoop:
		for offset < task.FileSize {
			for offset-ackAtomic.Load() >= win {
				select {
				case <-dlCtx.Done():
					sendAborted = true
					break sendLoop
				case <-st.PayloadContext().Done():
					sendAborted = true
					break sendLoop
				case <-ackSignal:
				}
			}
			if dlCtx.Err() != nil || st.PayloadContext().Err() != nil {
				sendAborted = true
				break
			}
			n := int(min(uint64(len(buf)), task.FileSize-offset))
			if winAvail := win - (offset - ackAtomic.Load()); uint64(n) > winAvail {
				n = int(winAvail)
			}
			if n <= 0 {
				continue
			}
			if _, err = io.ReadFull(file, buf[:n]); err != nil {
				sendAborted, sendOut, sendErr = true, "failed", err
				break
			}
			if _, err = s.authorize(user, "download"); err != nil {
				sendAborted, sendOut, sendErr = true, "failed", err
				break
			}
			if err = s.waitBandwidth(st.PayloadContext(), user, bw, protocol.DirectionDownload, n); err != nil {
				sendAborted = true
				if st.PayloadContext().Err() == nil || st.Context().Err() != nil {
					sendOut, sendErr = "paused", err
				}
				break
			}
			if st.PayloadContext().Err() != nil || dlCtx.Err() != nil {
				sendAborted = true
				break
			}
			sentAtomic.Store(offset + uint64(n))
			if err = st.SendContext(st.PayloadContext(), protocol.TypeData, protocol.Data{FileOffset: offset, Bytes: buf[:n]}); err != nil {
				sendAborted = true
				if st.PayloadContext().Err() == nil || st.Context().Err() != nil {
					sendOut, sendErr = "paused", err
				}
				break
			}
			offset += uint64(n)
		}

		if sendErr != nil {
			stopDL()
		}
		res := <-ackDone
		if res.cancelled {
			return res.out, res.err
		}
		if sendErr != nil {
			return sendOut, sendErr
		}
		if res.err != nil {
			return res.out, res.err
		}
		if sendAborted && st.Context().Err() != nil {
			return "paused", st.Context().Err()
		}
	}

	s.progress(monID, ackAtomic.Load(), "等待客户端校验")
	for {
		frame, err := st.Receive()
		if err != nil {
			return "paused", err
		}
		switch frame.Header.Type {
		case protocol.TypeCancel:
			return cancelDownload(frame.Payload)
		case protocol.TypeAck:
			var ack protocol.Ack
			if err = protocol.Unmarshal(frame.Payload, &ack); err != nil {
				return "failed", err
			}
			if ack.NextOffset != ackAtomic.Load() {
				return "failed", &protocol.RemoteError{Code: protocol.CodeOffsetMismatch, Message: "最终 ACK 偏移非法"}
			}
		case protocol.TypeCommit:
			var commit protocol.Commit
			if err = protocol.Unmarshal(frame.Payload, &commit); err != nil {
				return "failed", err
			}
			if commit.FileSize != task.FileSize || commit.FileSHA256 != task.FileSHA256 {
				return "failed", &protocol.RemoteError{Code: protocol.CodeMetadataMismatch, Message: "COMMIT 元数据不符"}
			}
			if err = s.store.FinishDownloadSession(st.Context(), task.ID, lease); err != nil {
				return "failed", err
			}
			settled = true
			meta.offset = task.FileSize
			release()
			return "completed", st.Send(protocol.TypeFinish, protocol.Finish{
				TransferID: task.ID,
				FileSize:   task.FileSize,
				FileSHA256: task.FileSHA256,
			})
		default:
			return "failed", &protocol.RemoteError{Code: protocol.CodeBadState, Message: "下载等待 COMMIT 或 CANCEL"}
		}
	}
}
