package protocol

import "fmt"

// StreamID 用于单条 TLS 连接内的逻辑流多路复用。
// StreamID = 0 为连接级控制消息；客户端发起的文件流使用奇数 StreamID (1, 3, 5...)。
type StreamID uint32

// RequestID 客户端生成的 16 字节随机标识符，用于文件请求幂等性。
type RequestID [16]byte

// TransferID 服务端生成的 16 字节随机任务标识符，用于持久化与跨连接断点续传。
type TransferID [16]byte

// Digest 32 字节 SHA-256 哈希值。
type Digest [32]byte

// Header 对应 16 字节固定帧头：
// Offset 0x00 (4B): Magic "SFT1"
// Offset 0x04 (1B): Version 0x01
// Offset 0x05 (1B): Type
// Offset 0x06 (2B): Flags (保留，必须为 0)
// Offset 0x08 (4B): StreamID
// Offset 0x0C (4B): Length (Payload 长度)
type Header struct {
	Magic    [4]byte
	Version  uint8
	Type     Type
	Flags    uint16
	StreamID StreamID
	Length   uint32
}

// Frame 包含完整解析后的帧头与负载字节切片。
type Frame struct {
	Header  Header
	Payload []byte
}

// Direction 传输方向
type Direction uint8

const (
	DirectionUpload   Direction = 0x01
	DirectionDownload Direction = 0x02
)

// ResponseState 服务端对任务的响应状态
type ResponseState uint8

const (
	ResponseActive    ResponseState = 0x01
	ResponseCompleted ResponseState = 0x02
)

// CancelMode 取消模式
type CancelMode uint8

const (
	CancelPause CancelMode = 0x01
	CancelAbort CancelMode = 0x02
)

// RemoteError SFT/1 协议层错误
type RemoteError struct {
	Code    ErrorCode
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("SFT error (0x%04X): %s", uint16(e.Code), e.Message)
}
