// Package session 实现基于单一 TLS 连接的 SFT/1 双向多路复用、流控窗口、心跳保活与优雅轮换。
package session

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/time/rate"

	"sft-tui/internal/protocol"
)

type outbound struct {
	typ     protocol.Type
	id      protocol.StreamID
	payload []byte
	done    chan error
	ctx     context.Context
}

type Mux struct {
	conn         *Conn
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	streams      map[protocol.StreamID]*Stream
	last         protocol.StreamID
	accepted     protocol.StreamID
	seen         map[protocol.StreamID]struct{}
	limit        uint16
	window       uint32
	incoming     func(*Stream, protocol.Frame)
	draining     bool
	err          error
	writeQueue   chan outbound
	controlQueue chan outbound
	activity     atomic.Int64
	wg           sync.WaitGroup
}

type Stream struct {
	ID           protocol.StreamID
	mux          *Mux
	ctx          context.Context
	cancel       context.CancelFunc
	payloadCtx   context.Context
	stopPayload  context.CancelFunc
	inbox        []protocol.Frame
	inboxReady   chan struct{}
	controls     int
	mu           sync.Mutex
	rxAllowed    bool
	rxEnd, rxAck uint64
	err          error
	peerError    bool
}

func NewMux(c *Conn, limit uint16, window uint32, incoming func(*Stream, protocol.Frame)) *Mux {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Mux{
		conn:         c,
		ctx:          ctx,
		cancel:       cancel,
		streams:      make(map[protocol.StreamID]*Stream),
		limit:        limit,
		window:       window,
		incoming:     incoming,
		writeQueue:   make(chan outbound, max(2, int(limit)*2)),
		controlQueue: make(chan outbound, 16),
	}
	m.activity.Store(time.Now().UnixNano())
	m.wg.Add(3)
	go m.reader()
	go m.writer()
	go m.heartbeat()
	return m
}

func (m *Mux) Context() context.Context { return m.ctx }

func (m *Mux) Error() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	return io.ErrClosedPipe
}

func (m *Mux) Close(err error) {
	m.mu.Lock()
	if m.err == nil {
		if err == nil {
			err = io.EOF
		}
		m.err = err
	}
	m.mu.Unlock()
	m.cancel()
	_ = m.conn.Net.Close()
}

func (m *Mux) Wait() { m.wg.Wait() }

func (m *Mux) newStream(id protocol.StreamID) *Stream {
	ctx, cancel := context.WithCancel(m.ctx)
	payloadCtx, stopPayload := context.WithCancel(ctx)
	s := &Stream{
		ID:          id,
		mux:         m,
		ctx:         ctx,
		cancel:      cancel,
		payloadCtx:  payloadCtx,
		stopPayload: stopPayload,
		inboxReady:  make(chan struct{}, 1),
	}
	m.streams[id] = s
	return s
}

func (m *Mux) Open(direction protocol.Direction) (*Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return nil, m.err
	}
	if m.draining {
		return nil, &protocol.RemoteError{Code: protocol.CodeTempUnavailable, Message: "连接正在关闭"}
	}
	if len(m.streams) >= int(m.limit) {
		return nil, &protocol.RemoteError{Code: protocol.CodeQuotaExceeded, Message: "连接活跃流超限"}
	}
	next := uint64(m.last) + 2
	if m.last == 0 {
		next = 1
	}
	if next > math.MaxUint32 {
		return nil, &protocol.RemoteError{Code: protocol.CodeBadStream, Message: "StreamID 耗尽"}
	}
	m.last = protocol.StreamID(next)
	s := m.newStream(m.last)
	s.rxAllowed = direction == protocol.DirectionDownload
	return s, nil
}

func (s *Stream) Context() context.Context        { return s.ctx }
func (s *Stream) PayloadContext() context.Context { return s.payloadCtx }
func (s *Stream) ChunkSize() uint32               { return s.mux.conn.Chunk }
func (s *Stream) WindowSize() uint32              { return s.mux.window }

func (s *Stream) HasBuffered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inbox) > 0
}

func (s *Stream) EnableReceive(offset uint64) {
	s.mu.Lock()
	s.rxAllowed = true
	s.rxEnd, s.rxAck = offset, offset
	s.mu.Unlock()
}

func (s *Stream) Release() {
	m := s.mux
	m.mu.Lock()
	if m.streams[s.ID] == s {
		delete(m.streams, s.ID)
	}
	closeConn := m.draining && m.incoming != nil && len(m.streams) == 0
	m.mu.Unlock()
	s.cancel()
	if closeConn {
		m.Close(io.EOF)
	}
}

func (s *Stream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.cancel()
}

func (s *Stream) Cause() error     { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *Stream) PeerFailed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.peerError }

func (s *Stream) Report(err error) {
	if s.PeerFailed() || s.mux.ctx.Err() != nil {
		return
	}
	var remote *protocol.RemoteError
	if !errors.As(err, &remote) {
		remote = &protocol.RemoteError{Code: protocol.CodeTempUnavailable, Message: "对端未能完成文件操作"}
	}
	s.mux.reject(s.ID, remote.Code, remote.Message)
}

func (s *Stream) enqueue(f protocol.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil
	}
	if f.Header.Type == protocol.TypeCancel && s.mux.incoming != nil {
		s.stopPayload()
	}
	if f.Header.Type == protocol.TypeData {
		// 合并相邻小 DATA 帧以降低消息缓冲开销，保证缓冲始终受接收窗口约束
		if n := len(s.inbox); n > 0 {
			tail := &s.inbox[n-1]
			if tail.Header.Type == protocol.TypeData && len(tail.Payload)+len(f.Payload)-8 <= int(s.ChunkSize())+8 {
				tail.Payload = append(tail.Payload, f.Payload[8:]...)
				tail.Header.Length = uint32(len(tail.Payload))
				return nil
			}
		}
		if cap(f.Payload) < int(s.ChunkSize())+8 {
			p := make([]byte, len(f.Payload), int(s.ChunkSize())+8)
			copy(p, f.Payload)
			f.Payload = p
		}
	} else {
		if s.controls >= 16 {
			return &protocol.RemoteError{Code: protocol.CodeTempUnavailable, Message: "流控制队列已满"}
		}
		s.controls++
	}
	s.inbox = append(s.inbox, f)
	select {
	case s.inboxReady <- struct{}{}:
	default:
	}
	return nil
}

func (s *Stream) dequeue() (protocol.Frame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inbox) == 0 {
		return protocol.Frame{}, false
	}
	f := s.inbox[0]
	s.inbox[0] = protocol.Frame{}
	s.inbox = s.inbox[1:]
	if len(s.inbox) == 0 {
		s.inbox = nil
	}
	if f.Header.Type != protocol.TypeData {
		s.controls--
	}
	return f, true
}

func (s *Stream) Receive() (protocol.Frame, error) {
	return s.ReceiveContext(context.Background())
}

func decodeStreamError(f protocol.Frame) (protocol.Frame, error) {
	var e protocol.RemoteError
	if err := protocol.Unmarshal(f.Payload, &e); err != nil {
		return f, err
	}
	return f, &e
}

func (m *Mux) idleSeconds() uint16 {
	if m.conn != nil && m.conn.Timeouts.IdleSeconds > 0 {
		return m.conn.Timeouts.IdleSeconds
	}
	return 120
}

func (s *Stream) ReceiveContext(ctx context.Context) (protocol.Frame, error) {
	interval := time.Duration(s.mux.idleSeconds()) * time.Second
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil && s.ctx.Err() == nil {
			return protocol.Frame{}, err
		}
		if f, ok := s.dequeue(); ok {
			if f.Header.Type == protocol.TypeError {
				return decodeStreamError(f)
			}
			return f, nil
		}
		if ctx.Err() != nil {
			if cause := s.Cause(); cause != nil {
				return protocol.Frame{}, cause
			}
			return protocol.Frame{}, s.mux.Error()
		}
		select {
		case <-ctx.Done():
			if s.ctx.Err() != nil {
				continue
			}
			return protocol.Frame{}, ctx.Err()
		case <-s.inboxReady:
			continue
		case <-s.ctx.Done():
			if f, ok := s.dequeue(); ok {
				if f.Header.Type == protocol.TypeError {
					return decodeStreamError(f)
				}
				return f, nil
			}
			if cause := s.Cause(); cause != nil {
				return protocol.Frame{}, cause
			}
			return protocol.Frame{}, s.mux.Error()
		case <-timer.C:
			if time.Since(time.Unix(0, s.mux.activity.Load())) >= interval {
				s.mux.Close(&protocol.RemoteError{Code: protocol.CodeTempUnavailable, Message: "连接空闲超时"})
				return protocol.Frame{}, s.mux.Error()
			}
			timer.Reset(interval)
		}
	}
}

func (s *Stream) Expect(typ protocol.Type, out any) error {
	return s.ExpectContext(context.Background(), typ, out)
}

func (s *Stream) ExpectContext(ctx context.Context, typ protocol.Type, out any) error {
	f, err := s.ReceiveContext(ctx)
	if err != nil {
		return err
	}
	if f.Header.Type != typ {
		return &protocol.RemoteError{Code: protocol.CodeBadState, Message: "消息不符合当前传输状态"}
	}
	return protocol.Unmarshal(f.Payload, out)
}

func (s *Stream) Send(typ protocol.Type, v any) error {
	return s.SendContext(s.ctx, typ, v)
}

func (s *Stream) SendContext(ctx context.Context, typ protocol.Type, v any) error {
	payload, err := protocol.Marshal(v)
	if err != nil {
		return err
	}
	err = s.mux.send(ctx, typ, s.ID, payload, false)
	if err != nil {
		if cause := s.Cause(); cause != nil {
			return cause
		}
	}
	return err
}

func (m *Mux) Control(typ protocol.Type, v any) error {
	payload, err := protocol.Marshal(v)
	if err != nil {
		return err
	}
	return m.send(m.ctx, typ, 0, payload, true)
}

func (m *Mux) enqueueControlNonBlocking(typ protocol.Type, id protocol.StreamID, p []byte) bool {
	if m.ctx.Err() != nil {
		return false
	}
	payloadCopy := append([]byte(nil), p...)
	job := outbound{typ: typ, id: id, payload: payloadCopy, ctx: m.ctx}
	select {
	case m.controlQueue <- job:
		return true
	default:
		return false
	}
}

func (m *Mux) send(ctx context.Context, typ protocol.Type, id protocol.StreamID, p []byte, control bool) error {
	if ctx.Err() != nil {
		if m.ctx.Err() != nil {
			return m.Error()
		}
		return ctx.Err()
	}
	job := outbound{typ: typ, id: id, payload: p, done: make(chan error, 1), ctx: ctx}
	q := m.writeQueue
	if control {
		q = m.controlQueue
	}
	select {
	case q <- job:
	case <-m.ctx.Done():
		return m.Error()
	case <-ctx.Done():
		if m.ctx.Err() != nil {
			return m.Error()
		}
		return ctx.Err()
	}
	select {
	case err := <-job.done:
		if err != nil && ctx.Err() != nil && m.ctx.Err() == nil {
			return ctx.Err()
		}
		return err
	case <-m.ctx.Done():
		return m.Error()
	case <-ctx.Done():
		if m.ctx.Err() != nil {
			return m.Error()
		}
		return ctx.Err()
	}
}

func isTerminalResponse(typ protocol.Type, payload []byte) bool {
	if typ == protocol.TypeListResp && len(payload) >= 3 && payload[0] == 0 {
		return true
	}
	if (typ == protocol.TypePutResp || typ == protocol.TypeGetResp) && len(payload) > 16 && payload[16] == byte(protocol.ResponseCompleted) {
		return true
	}
	if typ == protocol.TypeResumeResp && len(payload) > 0 && payload[0] == byte(protocol.ResponseCompleted) {
		return true
	}
	return false
}

func (m *Mux) writer() {
	defer m.wg.Done()
	for {
		var job outbound
		select {
		case <-m.ctx.Done():
			return
		case job = <-m.controlQueue:
		default:
			select {
			case <-m.ctx.Done():
				return
			case job = <-m.controlQueue:
			case job = <-m.writeQueue:
			}
		}
		if job.ctx.Err() != nil {
			if job.done != nil {
				job.done <- io.ErrClosedPipe
			}
			continue
		}
		terminal := job.typ == protocol.TypeFinish || job.typ == protocol.TypeCancelAck ||
			(job.typ == protocol.TypeError && job.id != 0) || isTerminalResponse(job.typ, job.payload)
		if terminal && m.incoming != nil {
			m.mu.Lock()
			delete(m.streams, job.id)
			m.mu.Unlock()
		}
		if job.typ == protocol.TypeAck && len(job.payload) == 8 {
			m.mu.Lock()
			s := m.streams[job.id]
			m.mu.Unlock()
			if s != nil {
				s.mu.Lock()
				ack := binary.BigEndian.Uint64(job.payload)
				if ack >= s.rxAck && ack <= s.rxEnd {
					s.rxAck = ack
				}
				s.mu.Unlock()
			}
		}
		err := m.conn.SendPayload(job.typ, job.id, job.payload)
		if job.done != nil {
			job.done <- err
		}
		if err != nil {
			m.Close(err)
			return
		}
	}
}

func truncateErrorText(s string, maxBytes int) string {
	s = strings.ToValidUTF8(s, "?")
	if len(s) <= maxBytes {
		return s
	}
	const ellipsis = "…"
	if maxBytes <= len(ellipsis) {
		limit := maxBytes
		for limit > 0 && !utf8.RuneStart(s[limit]) {
			limit--
		}
		return s[:limit]
	}
	limit := maxBytes - len(ellipsis)
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + ellipsis
}

func (m *Mux) reject(id protocol.StreamID, code protocol.ErrorCode, message string) {
	msg := truncateErrorText(message, protocol.MaxErrorMessageBytes)
	p, err := protocol.Marshal(protocol.RemoteError{Code: code, Message: msg})
	if err != nil {
		return
	}
	_ = m.send(m.ctx, protocol.TypeError, id, p, true)
}

func (m *Mux) reader() {
	defer m.wg.Done()
	controls := rate.NewLimiter(32, 64)
	for {
		f, err := m.conn.Read(m.idleSeconds())
		if err != nil {
			var p *protocol.RemoteError
			if errors.As(err, &p) && p.Code != protocol.CodeBadMagic {
				m.reject(0, p.Code, p.Message)
			}
			m.Close(err)
			return
		}
		m.activity.Store(time.Now().UnixNano())
		initial := f.Header.Type == protocol.TypePutReq || f.Header.Type == protocol.TypeGetReq ||
			f.Header.Type == protocol.TypeResumeReq || f.Header.Type == protocol.TypeListReq
		if (f.Header.StreamID == 0 || f.Header.Type == protocol.TypeError || initial) && !controls.Allow() {
			m.Close(&protocol.RemoteError{Code: protocol.CodeTempUnavailable, Message: "连接控制消息频率超限"})
			return
		}
		if f.Header.StreamID == 0 {
			switch f.Header.Type {
			case protocol.TypePing:
				if !m.enqueueControlNonBlocking(protocol.TypePong, 0, f.Payload) {
					m.Close(&protocol.RemoteError{Code: protocol.CodeTempUnavailable, Message: "控制发送队列拥塞"})
					return
				}
				continue
			case protocol.TypePong:
				continue
			case protocol.TypeGoAway:
				if m.incoming != nil {
					problem := &protocol.RemoteError{Code: protocol.CodeBadState, Message: "客户端不得发送 GOAWAY"}
					m.reject(0, problem.Code, problem.Message)
					m.Close(problem)
					return
				}
				var g protocol.GoAway
				if err = protocol.Unmarshal(f.Payload, &g); err != nil {
					m.Close(err)
					return
				}
				m.mu.Lock()
				m.draining = true
				for id, s := range m.streams {
					if id > g.LastAcceptedStreamID {
						s.fail(&protocol.RemoteError{Code: protocol.CodeTempUnavailable, Message: "新流未被服务端接受"})
					}
				}
				m.mu.Unlock()
				continue
			case protocol.TypeError:
				var e protocol.RemoteError
				if err = protocol.Unmarshal(f.Payload, &e); err != nil {
					m.Close(err)
				} else {
					m.Close(&e)
				}
				return
			default:
				m.reject(0, protocol.CodeBadState, "重复初始化或非法连接消息")
				m.Close(io.ErrUnexpectedEOF)
				return
			}
		}

		id := f.Header.StreamID
		m.mu.Lock()
		s := m.streams[id]
		if m.incoming != nil && initial {
			if s != nil {
				m.mu.Unlock()
				s.fail(&protocol.RemoteError{Code: protocol.CodeBadState, Message: "流已经初始化"})
				continue
			}
			if _, used := m.seen[id]; used {
				m.mu.Unlock()
				m.reject(id, protocol.CodeBadStream, "StreamID 不得复用")
				continue
			}
			if len(m.seen) >= 65536 {
				alreadyDraining := m.draining
				m.mu.Unlock()
				m.reject(id, protocol.CodeTempUnavailable, "连接流历史已满，请重新连接")
				if !alreadyDraining {
					_ = m.GoAway(protocol.CodeTempUnavailable, 0)
				}
				continue
			}
			if m.seen == nil {
				m.seen = make(map[protocol.StreamID]struct{})
			}
			m.seen[id] = struct{}{}
			if m.draining {
				m.mu.Unlock()
				m.reject(id, protocol.CodeTempUnavailable, "服务端正在关闭")
				continue
			}
			if len(m.streams) >= int(m.limit) {
				m.mu.Unlock()
				m.reject(id, protocol.CodeQuotaExceeded, "连接活跃流超限")
				continue
			}
			s = m.newStream(id)
			m.accepted = max(m.accepted, id)
			m.mu.Unlock()
			m.incoming(s, f)
			continue
		}
		m.mu.Unlock()

		if s == nil {
			if f.Header.Type != protocol.TypeError {
				if !controls.Allow() {
					m.Close(&protocol.RemoteError{Code: protocol.CodeTempUnavailable, Message: "非法流消息频率超限"})
					return
				}
				m.reject(id, protocol.CodeBadState, "流不存在或已经结束")
			}
			continue
		}
		if f.Header.Type == protocol.TypeError {
			var remote protocol.RemoteError
			if err := protocol.Unmarshal(f.Payload, &remote); err != nil {
				s.fail(err)
			} else {
				s.mu.Lock()
				s.peerError = true
				s.mu.Unlock()
				s.fail(&remote)
			}
			continue
		}
		if m.incoming == nil {
			if f.Header.Type == protocol.TypeGetResp {
				var r protocol.GetResponse
				if err = protocol.Unmarshal(f.Payload, &r); err == nil && r.State == protocol.ResponseActive {
					s.EnableReceive(r.StartOffset)
				}
			}
			if f.Header.Type == protocol.TypeResumeResp {
				var r protocol.ResumeResponse
				if err = protocol.Unmarshal(f.Payload, &r); err == nil && r.Direction == protocol.DirectionDownload && r.State == protocol.ResponseActive {
					s.EnableReceive(r.AcceptedOffset)
				}
			}
		}
		if f.Header.Type == protocol.TypeData {
			s.mu.Lock()
			offset := binary.BigEndian.Uint64(f.Payload[:8])
			n := uint64(len(f.Payload) - 8)
			code := protocol.CodeWindowViolation
			if !s.rxAllowed {
				code = protocol.CodeBadState
			} else if offset != s.rxEnd {
				code = protocol.CodeOffsetMismatch
			}
			valid := s.rxAllowed && offset == s.rxEnd && s.rxEnd >= s.rxAck &&
				s.rxEnd-s.rxAck <= uint64(m.window) && n <= uint64(m.window)-(s.rxEnd-s.rxAck) && offset <= math.MaxUint64-n
			if valid {
				s.rxEnd += n
			}
			s.mu.Unlock()
			if !valid {
				problem := &protocol.RemoteError{Code: code, Message: "DATA 偏移、方向或窗口非法"}
				if m.incoming == nil {
					m.reject(id, problem.Code, problem.Message)
				}
				s.fail(problem)
				continue
			}
		}
		if err := s.enqueue(f); err != nil {
			if m.incoming == nil {
				var problem *protocol.RemoteError
				if errors.As(err, &problem) {
					m.reject(id, problem.Code, problem.Message)
				}
			}
			s.fail(err)
		}
	}
}

func heartbeatInterval(idle uint16) time.Duration {
	if idle == 0 {
		idle = 1
	}
	interval := min(30*time.Second, time.Duration(idle)*time.Second/3)
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	return interval
}

func (m *Mux) heartbeat() {
	defer m.wg.Done()
	interval := heartbeatInterval(m.idleSeconds())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			var data [8]byte
			binary.BigEndian.PutUint64(data[:], uint64(now.UnixNano()))
			if err := m.Control(protocol.TypePing, data); err != nil {
				return
			}
		}
	}
}

func (m *Mux) GoAway(reason protocol.ErrorCode, retry uint16) error {
	m.mu.Lock()
	m.draining = true
	last := m.accepted
	m.mu.Unlock()
	err := m.Control(protocol.TypeGoAway, protocol.GoAway{
		ReasonCode:           reason,
		LastAcceptedStreamID: last,
		RetryAfterSec:        retry,
	})
	m.mu.Lock()
	empty := len(m.streams) == 0
	m.mu.Unlock()
	if empty {
		m.Close(io.EOF)
	}
	return err
}
