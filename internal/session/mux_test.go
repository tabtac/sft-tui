package session

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/protocol"
)

func testTimeouts() config.Timeouts {
	return config.Timeouts{
		HandshakeSeconds: 2,
		HelloSeconds:     2,
		FrameReadSeconds: 2,
		WriteSeconds:     2,
		IdleSeconds:      5,
	}
}

func TestMuxMultiplexingHeartbeatAndGoAway(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	srvConn := &Conn{Net: c1, Chunk: 4096, Timeouts: testTimeouts()}
	cliConn := &Conn{Net: c2, Chunk: 4096, Timeouts: testTimeouts()}

	var wg sync.WaitGroup
	srvMux := NewMux(srvConn, 4, protocol.MinWindowSize, func(st *Stream, initial protocol.Frame) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer st.Release()
			var req protocol.PutRequest
			if err := protocol.Unmarshal(initial.Payload, &req); err != nil {
				st.Report(err)
				return
			}
			st.EnableReceive(0)
			_ = st.Send(protocol.TypePutResp, protocol.PutResponse{
				TransferID: protocol.TransferID{byte(st.ID)},
				State:      protocol.ResponseActive,
				NextOffset: 0,
			})
			var d protocol.Data
			if err := st.Expect(protocol.TypeData, &d); err != nil {
				st.Report(err)
				return
			}
			_ = st.Send(protocol.TypeAck, protocol.Ack{NextOffset: uint64(len(d.Bytes))})
		}()
	})
	defer srvMux.Close(io.EOF)

	cliMux := NewMux(cliConn, 4, protocol.MinWindowSize, nil)
	defer cliMux.Close(io.EOF)

	// 1. 控制连接 PING 测试
	var pingOpaque [8]byte
	binary.BigEndian.PutUint64(pingOpaque[:], 42)
	if err := cliMux.Control(protocol.TypePing, pingOpaque); err != nil {
		t.Fatalf("Control PING: %v", err)
	}

	// 2. 并发打开两条奇数流交叉传输
	s1, err := cliMux.Open(protocol.DirectionUpload)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := cliMux.Open(protocol.DirectionUpload)
	if err != nil {
		t.Fatal(err)
	}
	if s1.ID != 1 || s2.ID != 3 {
		t.Fatalf("expected odd stream IDs 1, 3, got %d, %d", s1.ID, s2.ID)
	}

	for _, st := range []*Stream{s1, s2} {
		if err := st.Send(protocol.TypePutReq, protocol.PutRequest{
			RequestID: protocol.RequestID{byte(st.ID)},
			FileName:  "f.bin",
			FileSize:  4,
		}); err != nil {
			t.Fatal(err)
		}
		var resp protocol.PutResponse
		if err := st.Expect(protocol.TypePutResp, &resp); err != nil {
			t.Fatal(err)
		}
		if err := st.Send(protocol.TypeData, protocol.Data{FileOffset: 0, Bytes: []byte("abcd")}); err != nil {
			t.Fatal(err)
		}
		var ack protocol.Ack
		if err := st.Expect(protocol.TypeAck, &ack); err != nil {
			t.Fatal(err)
		}
		if ack.NextOffset != 4 {
			t.Fatalf("expected ack 4, got %d", ack.NextOffset)
		}
		st.Release()
	}
	wg.Wait()

	// 3. GOAWAY 优雅排空，客户端不得再开启新流
	if err := srvMux.GoAway(protocol.CodeTempUnavailable, 5); err != nil {
		t.Fatalf("GoAway: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := cliMux.Open(protocol.DirectionUpload); err != nil {
			var rem *protocol.RemoteError
			if !errors.As(err, &rem) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("unexpected error after GoAway: %v", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected Open to fail after GoAway")
}

func TestMuxCancelSignalsPayloadContext(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	srvConn := &Conn{Net: c1, Chunk: 4096, Timeouts: testTimeouts()}
	cliConn := &Conn{Net: c2, Chunk: 4096, Timeouts: testTimeouts()}

	payloadCancelled := make(chan struct{})
	srvMux := NewMux(srvConn, 2, protocol.MinWindowSize, func(st *Stream, _ protocol.Frame) {
		go func() {
			defer st.Release()
			<-st.PayloadContext().Done()
			close(payloadCancelled)
			var cancelMsg protocol.Cancel
			_ = st.Expect(protocol.TypeCancel, &cancelMsg)
			_ = st.Send(protocol.TypeCancelAck, protocol.CancelAck{Mode: cancelMsg.Mode, AcceptedOffset: 0})
		}()
	})
	defer srvMux.Close(io.EOF)

	cliMux := NewMux(cliConn, 2, protocol.MinWindowSize, nil)
	defer cliMux.Close(io.EOF)

	st, err := cliMux.Open(protocol.DirectionUpload)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Release()

	if err := st.Send(protocol.TypePutReq, protocol.PutRequest{RequestID: protocol.RequestID{1}, FileName: "a.bin"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Send(protocol.TypeCancel, protocol.Cancel{Mode: protocol.CancelPause}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-payloadCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("PayloadContext was not cancelled when TypeCancel arrived")
	}
	var ack protocol.CancelAck
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := st.ExpectContext(ctx, protocol.TypeCancelAck, &ack); err != nil {
		t.Fatalf("ExpectContext CancelAck: %v", err)
	}
	if ack.Mode != protocol.CancelPause {
		t.Fatalf("expected CancelPause, got %v", ack.Mode)
	}
}

func newIdleTestMux(t *testing.T, limit uint16) (*Mux, net.Conn) {
	t.Helper()
	local, peer := net.Pipe()
	mux := NewMux(&Conn{Net: local, Chunk: 4096, Timeouts: testTimeouts()}, limit, protocol.MinWindowSize, nil)
	t.Cleanup(func() { mux.Close(io.EOF); peer.Close(); mux.Wait() })
	return mux, peer
}

func TestMuxCapacityReleaseAndStreamIDExhaustion(t *testing.T) {
	mux, _ := newIdleTestMux(t, 2)
	first, err := mux.Open(protocol.DirectionUpload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := mux.Open(protocol.DirectionDownload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mux.Open(protocol.DirectionUpload); err == nil {
		t.Error("stream capacity exceeded")
	}
	first.Release()
	third, err := mux.Open(protocol.DirectionUpload)
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != 5 {
		t.Errorf("released StreamID was reused: %d", third.ID)
	}
	second.Release()
	third.Release()
	mux.mu.Lock()
	mux.last = protocol.StreamID(math.MaxUint32 - 2)
	mux.mu.Unlock()
	last, err := mux.Open(protocol.DirectionUpload)
	if err != nil {
		t.Fatal(err)
	}
	if uint32(last.ID) != math.MaxUint32 {
		t.Errorf("last stream ID=%d", last.ID)
	}
	last.Release()
	if _, err := mux.Open(protocol.DirectionUpload); err == nil {
		t.Error("exhausted stream IDs wrapped around")
	}
}

func TestStreamReceiveCancellationAndMuxFailure(t *testing.T) {
	mux, _ := newIdleTestMux(t, 1)
	stream, err := mux.Open(protocol.DirectionDownload)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stream.ReceiveContext(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled receive=%v", err)
	}
	failure := errors.New("connection failed")
	mux.Close(failure)
	if _, err := stream.Receive(); !errors.Is(err, failure) {
		t.Errorf("mux failure not returned to stream: %v", err)
	}
	if _, err := mux.Open(protocol.DirectionUpload); !errors.Is(err, failure) {
		t.Errorf("closed mux accepted stream or lost cause: %v", err)
	}
}

func TestStreamInboxCoalescesDataAndBoundsControls(t *testing.T) {
	mux, _ := newIdleTestMux(t, 1)
	stream, err := mux.Open(protocol.DirectionDownload)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Release()
	for _, part := range []struct {
		offset uint64
		data   string
	}{{0, "ab"}, {2, "cd"}} {
		payload, err := protocol.Marshal(protocol.Data{FileOffset: part.offset, Bytes: []byte(part.data)})
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.enqueue(protocol.Frame{Header: protocol.Header{Type: protocol.TypeData, Length: uint32(len(payload))}, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	frame, ok := stream.dequeue()
	if !ok {
		t.Fatal("missing DATA")
	}
	var data protocol.Data
	if err := protocol.Unmarshal(frame.Payload, &data); err != nil || data.FileOffset != 0 || string(data.Bytes) != "abcd" {
		t.Errorf("DATA coalescing corrupted payload: %+v %v", data, err)
	}
	if _, ok := stream.dequeue(); ok {
		t.Error("coalesced DATA left duplicate frame")
	}
	ack, err := protocol.Marshal(protocol.Ack{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		if err := stream.enqueue(protocol.Frame{Header: protocol.Header{Type: protocol.TypeAck}, Payload: ack}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.enqueue(protocol.Frame{Header: protocol.Header{Type: protocol.TypeAck}, Payload: ack}); err == nil {
		t.Error("control inbox exceeded bound")
	}
}
