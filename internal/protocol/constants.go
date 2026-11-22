package protocol

// SFT/1 协议常量规范 (符合 SFT-DRAFT-001 规范)
const (
	Magic                      = "SFT1"
	Version              uint8 = 0x01
	HeaderSize                 = 16
	DataOffsetSize             = 8
	MaxControlPayload          = 4096
	MinChunkSize               = 4096
	MaxChunkSize               = 65536
	MaxDataPayload             = DataOffsetSize + MaxChunkSize
	MaxFrameSize               = HeaderSize + MaxDataPayload
	MinStreams                 = 1
	MaxStreams                 = 64
	MinWindowSize              = 65536
	MaxWindowSize              = 4194304
	DefaultWindowSize          = 1048576
	TargetMaxStreams           = 8
	MaxFileNameBytes           = 255
	MaxErrorMessageBytes       = 256
	ListPageSize               = 12
)

// Type 标识 SFT/1 帧消息类型
type Type uint8

const (
	TypePing       Type = 0x01
	TypePong       Type = 0x02
	TypeHello      Type = 0x03
	TypeHelloAck   Type = 0x04
	TypePutReq     Type = 0x10
	TypePutResp    Type = 0x11
	TypeGetReq     Type = 0x12
	TypeGetResp    Type = 0x13
	TypeListReq    Type = 0x14
	TypeListResp   Type = 0x15
	TypeListNext   Type = 0x16
	TypeDeleteReq  Type = 0x17
	TypeDeleteResp Type = 0x18
	TypeData       Type = 0x20
	TypeAck        Type = 0x21
	TypeCommit     Type = 0x22
	TypeFinish     Type = 0x23
	TypeResumeReq  Type = 0x30
	TypeResumeResp Type = 0x31
	TypeError      Type = 0x40
	TypeCancel     Type = 0x41
	TypeCancelAck  Type = 0x42
	TypeGoAway     Type = 0x43
)

// ErrorCode 对应 SFT/1 规范定义的错误码
type ErrorCode uint16

const (
	CodeBadMagic           ErrorCode = 0x0001
	CodeUnsupportedVersion ErrorCode = 0x0002
	CodeBadFrame           ErrorCode = 0x0003
	CodeUnsupportedType    ErrorCode = 0x0004
	CodeBadState           ErrorCode = 0x0005
	CodeBadStream          ErrorCode = 0x0006
	CodeWindowViolation    ErrorCode = 0x0007
	CodeUnauthorized       ErrorCode = 0x0010
	CodeNotFound           ErrorCode = 0x0011
	CodeAlreadyExists      ErrorCode = 0x0012
	CodeQuotaExceeded      ErrorCode = 0x0013
	CodeBusy               ErrorCode = 0x0014
	CodeOffsetMismatch     ErrorCode = 0x0015
	CodeHashMismatch       ErrorCode = 0x0016
	CodeTransferExpired    ErrorCode = 0x0017
	CodeMetadataMismatch   ErrorCode = 0x0018
	CodeInvalidName        ErrorCode = 0x0019
	CodeTransferCancelled  ErrorCode = 0x001A
	CodeInternalError      ErrorCode = 0x0020
	CodeTempUnavailable    ErrorCode = 0x0021
)
