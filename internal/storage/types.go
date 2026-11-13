package storage

import (
	"encoding/hex"
	"errors"

	"sft-tui/internal/protocol"
)

// TaskStatus 任务状态枚举
type TaskStatus string

const (
	TaskActive    TaskStatus = "ACTIVE"
	TaskPaused    TaskStatus = "PAUSED"
	TaskCompleted TaskStatus = "COMPLETED"
	TaskFailed    TaskStatus = "FAILED"
	TaskCancelled TaskStatus = "CANCELLED"
	TaskExpired   TaskStatus = "EXPIRED"
)

// IsTerminal 终态检查
func (s TaskStatus) IsTerminal() bool {
	return s == TaskCompleted || s == TaskFailed || s == TaskCancelled || s == TaskExpired
}

// Task 持久化传输任务结构
type Task struct {
	ID         protocol.TransferID
	RequestID  protocol.RequestID
	UserID     string
	Direction  protocol.Direction
	FileName   string
	FileSize   uint64
	FileSHA256 protocol.Digest
	ObjectID   string
	Offset     uint64
	Status     TaskStatus
	CreatedAt  int64
	UpdatedAt  int64
	ExpiresAt  int64
}

// PublishedFile 已发布不可变文件的元数据
type PublishedFile struct {
	ObjectID  string
	UserID    string
	FileName  string
	FileSize  uint64
	SHA256    protocol.Digest
	CreatedAt int64
}

// 错误定义
var (
	ErrTaskNotFound     = errors.New("task not found")
	ErrTaskExpired      = errors.New("task expired")
	ErrTaskConflict     = errors.New("task conflict or already exists")
	ErrFileNotFound     = errors.New("file not found")
	ErrFileExists       = errors.New("file already exists")
	ErrOffsetMismatch   = errors.New("offset mismatch")
	ErrHashMismatch     = errors.New("file sha256 hash mismatch")
	ErrQuotaExceeded    = errors.New("storage quota exceeded")
	ErrStoreLocked      = errors.New("storage root is locked by another instance")
)

// IDToHex 将 16 字节 ID 转为 Hex 字符串
func IDToHex(id [16]byte) string {
	return hex.EncodeToString(id[:])
}

// HexToID 将 Hex 字符串还原为 16 字节 ID
func HexToID(s string) ([16]byte, error) {
	var out [16]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return out, errors.New("invalid 16-byte hex ID")
	}
	copy(out[:], b)
	return out, nil
}
