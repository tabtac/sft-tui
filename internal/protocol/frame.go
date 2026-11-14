package protocol

import (
	"encoding/binary"
	"io"
)

// ValidateFrame 在分配 Payload 缓冲区前对帧头及字段长度进行严格校验。
// 防止恶意或损坏的 Length 字段引发异常大内存分配。
func ValidateFrame(h Header, negotiatedChunk uint32) error {
	if string(h.Magic[:]) != Magic {
		return &RemoteError{Code: CodeBadMagic, Message: "非法协议魔数 (Bad Magic)"}
	}
	if h.Version != Version {
		return &RemoteError{Code: CodeUnsupportedVersion, Message: "不支持的协议版本 (Unsupported Version)"}
	}
	if h.Flags != 0 {
		return &RemoteError{Code: CodeBadFrame, Message: "保留 Flags 字段必须为 0"}
	}
	if negotiatedChunk < MinChunkSize || negotiatedChunk > MaxChunkSize {
		return &RemoteError{Code: CodeBadFrame, Message: "协商数据块上限超出合法范围"}
	}

	var minLen, maxLen uint32
	connectionMsg := false

	switch h.Type {
	case TypePing, TypePong:
		minLen, maxLen, connectionMsg = 8, 8, true
	case TypeHello:
		minLen, maxLen, connectionMsg = 10, 10, true
	case TypeHelloAck:
		minLen, maxLen, connectionMsg = 12, 12, true
	case TypePutReq:
		minLen, maxLen = 59, 58+MaxFileNameBytes
	case TypePutResp:
		minLen, maxLen = 25, 25
	case TypeGetReq:
		minLen, maxLen = 19, 18+MaxFileNameBytes
	case TypeGetResp:
		minLen, maxLen = 65, 65
	case TypeListReq:
		minLen, maxLen = 2, 2+MaxFileNameBytes
	case TypeListResp:
		minLen, maxLen = 3, MaxControlPayload
	case TypeListNext:
		minLen, maxLen = 0, 0
	case TypeData:
		minLen, maxLen = 9, DataOffsetSize+negotiatedChunk
	case TypeAck:
		minLen, maxLen = 8, 8
	case TypeCommit:
		minLen, maxLen = 40, 40
	case TypeFinish:
		minLen, maxLen = 56, 56
	case TypeResumeReq:
		minLen, maxLen = 57, 57
	case TypeResumeResp:
		minLen, maxLen = 50, 50
	case TypeError:
		minLen, maxLen = 4, 4+MaxErrorMessageBytes
	case TypeCancel:
		minLen, maxLen = 1, 1
	case TypeCancelAck:
		minLen, maxLen = 9, 9
	case TypeGoAway:
		minLen, maxLen, connectionMsg = 8, 8, true
	default:
		return &RemoteError{Code: CodeUnsupportedType, Message: "未知的消息类型 (Unsupported Message Type)"}
	}

	if h.Length < minLen || h.Length > maxLen || (h.Type != TypeData && h.Length > MaxControlPayload) {
		return &RemoteError{Code: CodeBadFrame, Message: "Payload 长度越界或与消息类型不符"}
	}

	// 校验 StreamID：TypeError 可出现在连接级或流级；其他消息必须遵守连接/流约束
	if h.Type != TypeError {
		if connectionMsg && h.StreamID != 0 {
			return &RemoteError{Code: CodeBadStream, Message: "连接级控制消息必须使用 StreamID 0"}
		}
		if !connectionMsg && h.StreamID == 0 {
			return &RemoteError{Code: CodeBadStream, Message: "传输流消息禁止使用 StreamID 0"}
		}
	}

	// 客户端创建的文件传输 StreamID 必须为奇数
	if h.StreamID != 0 && h.StreamID%2 == 0 {
		return &RemoteError{Code: CodeBadStream, Message: "文件流 StreamID 必须为奇数"}
	}

	return nil
}

// ReadFrame 从输入流中读取一个完整的 SFT/1 帧。
// 必须先解析并校验 16 字节头部，确认 Length 合法后再读取 Payload。
func ReadFrame(r io.Reader, negotiatedChunk uint32) (Frame, error) {
	var raw [HeaderSize]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return Frame{}, err
	}

	h := Header{
		Version:  raw[4],
		Type:     Type(raw[5]),
		Flags:    binary.BigEndian.Uint16(raw[6:8]),
		StreamID: StreamID(binary.BigEndian.Uint32(raw[8:12])),
		Length:   binary.BigEndian.Uint32(raw[12:16]),
	}
	copy(h.Magic[:], raw[:4])

	if err := ValidateFrame(h, negotiatedChunk); err != nil {
		return Frame{}, err
	}

	var payload []byte
	if h.Length > 0 {
		payload = make([]byte, h.Length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return Frame{}, err
		}
	}

	return Frame{Header: h, Payload: payload}, nil
}

// WriteFrame 将消息帧编码并完整写入输出流。
func WriteFrame(w io.Writer, typ Type, stream StreamID, payload []byte, negotiatedChunk uint32) error {
	if len(payload) > MaxDataPayload {
		return &RemoteError{Code: CodeBadFrame, Message: "Payload 超过最大允许数据限制"}
	}

	h := Header{
		Version:  Version,
		Type:     typ,
		Flags:    0,
		StreamID: stream,
		Length:   uint32(len(payload)),
	}
	copy(h.Magic[:], Magic)

	if err := ValidateFrame(h, negotiatedChunk); err != nil {
		return err
	}

	buf := make([]byte, HeaderSize+len(payload))
	copy(buf[:4], Magic)
	buf[4] = Version
	buf[5] = byte(typ)
	binary.BigEndian.PutUint16(buf[6:8], 0)
	binary.BigEndian.PutUint32(buf[8:12], uint32(stream))
	binary.BigEndian.PutUint32(buf[12:16], uint32(len(payload)))
	copy(buf[HeaderSize:], payload)

	for len(buf) > 0 {
		n, err := w.Write(buf)
		if n < 0 || n > len(buf) {
			return io.ErrShortWrite
		}
		buf = buf[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}

	return nil
}
