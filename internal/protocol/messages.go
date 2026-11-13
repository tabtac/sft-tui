package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// ---------------- 消息体定义 ----------------

type Hello struct {
	ClientMaxStreams uint16
	ClientMaxChunk   uint32
	ClientWindow     uint32
}

type HelloAck struct {
	MaxStreams     uint16
	ChunkSize      uint32
	WindowSize     uint32
	IdleTimeoutSec uint16
}

type PutRequest struct {
	RequestID  RequestID
	FileName   string
	FileSize   uint64
	FileSHA256 Digest
}

type PutResponse struct {
	TransferID TransferID
	State      ResponseState
	NextOffset uint64
}

type GetRequest struct {
	RequestID RequestID
	FileName  string
}

type GetResponse struct {
	TransferID  TransferID
	State       ResponseState
	FileSize    uint64
	FileSHA256  Digest
	StartOffset uint64
}

type Data struct {
	FileOffset uint64
	Bytes      []byte
}

type Ack struct {
	NextOffset uint64
}

type Commit struct {
	FileSize   uint64
	FileSHA256 Digest
}

type Finish struct {
	TransferID TransferID
	FileSize   uint64
	FileSHA256 Digest
}

type ResumeRequest struct {
	TransferID   TransferID
	Direction    Direction
	ClientOffset uint64
	FileSHA256   Digest
}

type ResumeResponse struct {
	State          ResponseState
	Direction      Direction
	FileSize       uint64
	FileSHA256     Digest
	AcceptedOffset uint64
}

type Cancel struct {
	Mode CancelMode
}

type CancelAck struct {
	Mode           CancelMode
	AcceptedOffset uint64
}

type GoAway struct {
	ReasonCode           ErrorCode
	LastAcceptedStreamID StreamID
	RetryAfterSec        uint16
}

// ---------------- 校验规则 ----------------

// ValidateName 校验文件名必须为合法 UTF-8、NFC 形式、单级逻辑名，且不得包含路径穿越或控制字符。
func ValidateName(name string) error {
	if len(name) < 1 || len(name) > MaxFileNameBytes || !utf8.ValidString(name) || !norm.NFC.IsNormalString(name) ||
		name == "." || name == ".." || strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return &RemoteError{Code: CodeInvalidName, Message: "文件名必须为 1–255 字节的 NFC UTF-8 单级名称，不能以空格或点结尾"}
	}
	for _, r := range name {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return &RemoteError{Code: CodeInvalidName, Message: "文件名不得包含路径分隔符或控制字符"}
		}
	}
	return nil
}

// ValidateHello 校验客户端 HELLO 消息中的流控和并发参数范围。
func ValidateHello(h Hello) error {
	if h.ClientMaxStreams < MinStreams || h.ClientMaxStreams > MaxStreams ||
		h.ClientMaxChunk < MinChunkSize || h.ClientMaxChunk > MaxChunkSize ||
		h.ClientWindow < MinWindowSize || h.ClientWindow > MaxWindowSize {
		return &RemoteError{Code: CodeBadFrame, Message: "HELLO 参数超出协议允许范围"}
	}
	return nil
}

// ---------------- 序列化与反序列化 ----------------

// Marshal 将 SFT/1 协议消息体序列化为 Payload 二进制。
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	write := func(data any) error {
		return binary.Write(&buf, binary.BigEndian, data)
	}

	writeName := func(name string) error {
		if err := ValidateName(name); err != nil {
			return err
		}
		if err := write(uint16(len(name))); err != nil {
			return err
		}
		_, err := buf.WriteString(name)
		return err
	}

	switch msg := v.(type) {
	case ListRequest:
		return marshalListRequest(msg)
	case ListResponse:
		return marshalListResponse(msg)
	case PutRequest:
		if err := write(msg.RequestID); err != nil {
			return nil, err
		}
		if err := writeName(msg.FileName); err != nil {
			return nil, err
		}
		if err := write(msg.FileSize); err != nil {
			return nil, err
		}
		if err := write(msg.FileSHA256); err != nil {
			return nil, err
		}
	case GetRequest:
		if err := write(msg.RequestID); err != nil {
			return nil, err
		}
		if err := writeName(msg.FileName); err != nil {
			return nil, err
		}
	case Data:
		if len(msg.Bytes) < 1 || len(msg.Bytes) > MaxChunkSize {
			return nil, &RemoteError{Code: CodeBadFrame, Message: "DATA 数据块长度必须在 1 ~ MaxChunkSize 之间"}
		}
		if err := write(msg.FileOffset); err != nil {
			return nil, err
		}
		buf.Write(msg.Bytes)
	case RemoteError:
		if len(msg.Message) > MaxErrorMessageBytes || !utf8.ValidString(msg.Message) {
			return nil, fmt.Errorf("错误文本过长或编码非法")
		}
		if err := write(msg.Code); err != nil {
			return nil, err
		}
		if err := write(uint16(len(msg.Message))); err != nil {
			return nil, err
		}
		buf.WriteString(msg.Message)
	default:
		// 定长结构体直接通过 binary.Write 序列化
		if err := write(v); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

// Unmarshal 从二进制 Payload 中反序列化为指定的 SFT/1 消息体。
func Unmarshal(payload []byte, out any) error {
	r := bytes.NewReader(payload)
	read := func(data any) error {
		return binary.Read(r, binary.BigEndian, data)
	}

	readName := func(s *string) error {
		var strLen uint16
		if err := read(&strLen); err != nil {
			return err
		}
		if strLen < 1 || strLen > MaxFileNameBytes || int(strLen) > r.Len() {
			return &RemoteError{Code: CodeBadFrame, Message: "文件名长度前缀越界"}
		}
		nameBytes := make([]byte, strLen)
		if _, err := io.ReadFull(r, nameBytes); err != nil {
			return err
		}
		*s = string(nameBytes)
		return ValidateName(*s)
	}

	var err error
	switch target := out.(type) {
	case *ListRequest:
		return unmarshalListRequest(payload, target)
	case *ListResponse:
		return unmarshalListResponse(payload, target)
	case *PutRequest:
		if err = read(&target.RequestID); err == nil {
			err = readName(&target.FileName)
		}
		if err == nil {
			err = read(&target.FileSize)
		}
		if err == nil {
			err = read(&target.FileSHA256)
		}
	case *GetRequest:
		if err = read(&target.RequestID); err == nil {
			err = readName(&target.FileName)
		}
	case *Data:
		if err = read(&target.FileOffset); err == nil {
			rem := r.Len()
			if rem < 1 {
				return &RemoteError{Code: CodeBadFrame, Message: "DATA 负载不能为空"}
			}
			target.Bytes = make([]byte, rem)
			_, err = io.ReadFull(r, target.Bytes)
		}
	case *RemoteError:
		var msgLen uint16
		if err = read(&target.Code); err == nil {
			err = read(&msgLen)
		}
		if err == nil {
			if msgLen > MaxErrorMessageBytes || int(msgLen) != r.Len() {
				return &RemoteError{Code: CodeBadFrame, Message: "错误信息长度不符"}
			}
			msgBytes := make([]byte, msgLen)
			_, err = io.ReadFull(r, msgBytes)
			target.Message = string(msgBytes)
			if !utf8.ValidString(target.Message) {
				return &RemoteError{Code: CodeBadFrame, Message: "错误信息非合法 UTF-8"}
			}
		}
	default:
		// 定长结构体直接通过 binary.Read 反序列化
		err = read(out)
	}

	if err != nil {
		var remote *RemoteError
		if errors.As(err, &remote) {
			return remote
		}
		return &RemoteError{Code: CodeBadFrame, Message: "消息数据截断或格式不匹配"}
	}

	if r.Len() != 0 {
		return &RemoteError{Code: CodeBadFrame, Message: "消息包含多余未解析字节"}
	}

	return nil
}
