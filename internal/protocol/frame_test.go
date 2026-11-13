package protocol

import (
	"bytes"
	"errors"
	"testing"
)

func TestValidateFrame_MagicAndVersion(t *testing.T) {
	chunk := uint32(65536)

	// 正确的头部
	h := Header{
		Magic:    [4]byte{'S', 'F', 'T', '1'},
		Version:  0x01,
		Type:     TypePing,
		Flags:    0,
		StreamID: 0,
		Length:   8,
	}
	if err := ValidateFrame(h, chunk); err != nil {
		t.Fatalf("expected valid frame, got: %v", err)
	}

	// 错误 Magic
	badMagic := h
	badMagic.Magic = [4]byte{'S', 'F', 'T', '2'}
	var remErr *RemoteError
	if err := ValidateFrame(badMagic, chunk); !errors.As(err, &remErr) || remErr.Code != CodeBadMagic {
		t.Errorf("expected CodeBadMagic, got: %v", err)
	}

	// 错误 Version
	badVer := h
	badVer.Version = 0x02
	if err := ValidateFrame(badVer, chunk); !errors.As(err, &remErr) || remErr.Code != CodeUnsupportedVersion {
		t.Errorf("expected CodeUnsupportedVersion, got: %v", err)
	}

	// 非零 Flags
	badFlags := h
	badFlags.Flags = 0x0001
	if err := ValidateFrame(badFlags, chunk); !errors.As(err, &remErr) || remErr.Code != CodeBadFrame {
		t.Errorf("expected CodeBadFrame for non-zero flags, got: %v", err)
	}
}

func TestValidateFrame_StreamIDConstraints(t *testing.T) {
	chunk := uint32(65536)

	// 连接级消息（PING）使用非零 StreamID 必须报错
	hPing := Header{
		Magic:    [4]byte{'S', 'F', 'T', '1'},
		Version:  0x01,
		Type:     TypePing,
		Flags:    0,
		StreamID: 1,
		Length:   8,
	}
	var remErr *RemoteError
	if err := ValidateFrame(hPing, chunk); !errors.As(err, &remErr) || remErr.Code != CodeBadStream {
		t.Errorf("expected CodeBadStream for connection msg on stream 1, got: %v", err)
	}

	// 流级消息（DATA）使用 StreamID 0 必须报错
	hData := Header{
		Magic:    [4]byte{'S', 'F', 'T', '1'},
		Version:  0x01,
		Type:     TypeData,
		Flags:    0,
		StreamID: 0,
		Length:   9,
	}
	if err := ValidateFrame(hData, chunk); !errors.As(err, &remErr) || remErr.Code != CodeBadStream {
		t.Errorf("expected CodeBadStream for stream msg on stream 0, got: %v", err)
	}

	// 偶数 StreamID (非零) 必须报错（客户端发起的 StreamID 必须为奇数）
	hEven := Header{
		Magic:    [4]byte{'S', 'F', 'T', '1'},
		Version:  0x01,
		Type:     TypeData,
		Flags:    0,
		StreamID: 2,
		Length:   9,
	}
	if err := ValidateFrame(hEven, chunk); !errors.As(err, &remErr) || remErr.Code != CodeBadStream {
		t.Errorf("expected CodeBadStream for even stream ID, got: %v", err)
	}

	// 奇数 StreamID 合法
	hOdd := Header{
		Magic:    [4]byte{'S', 'F', 'T', '1'},
		Version:  0x01,
		Type:     TypeData,
		Flags:    0,
		StreamID: 3,
		Length:   9,
	}
	if err := ValidateFrame(hOdd, chunk); err != nil {
		t.Errorf("expected valid frame for odd stream ID, got: %v", err)
	}
}

func TestReadWriteFrameRoundtrip(t *testing.T) {
	chunk := uint32(65536)
	var buf bytes.Buffer

	payload := []byte("12345678") // 8 bytes for PING
	err := WriteFrame(&buf, TypePing, 0, payload, chunk)
	if err != nil {
		t.Fatalf("WriteFrame failed: %v", err)
	}

	frame, err := ReadFrame(&buf, chunk)
	if err != nil {
		t.Fatalf("ReadFrame failed: %v", err)
	}

	if frame.Header.Type != TypePing {
		t.Errorf("expected TypePing, got: %v", frame.Header.Type)
	}
	if frame.Header.StreamID != 0 {
		t.Errorf("expected StreamID 0, got: %v", frame.Header.StreamID)
	}
	if !bytes.Equal(frame.Payload, payload) {
		t.Errorf("payload mismatch: expected %v, got %v", payload, frame.Payload)
	}
}
