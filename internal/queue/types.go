package queue

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"sft-tui/internal/protocol"
)

// State 客户端传输任务状态枚举
type State string

const (
	Waiting      State = "waiting"
	Preparing    State = "preparing"
	Hashing      State = "hashing"
	Transferring State = "transferring"
	Verifying    State = "verifying"
	Pausing      State = "pausing"
	Paused       State = "paused"
	Reconnecting State = "reconnecting"
	Pending      State = "pending"
	Cancelling   State = "cancelling"
	Completed    State = "completed"
	Cancelled    State = "cancelled"
	Skipped      State = "skipped"
)

// Active 判断任务是否处于活跃/等待或控制中间态
func (s State) Active() bool {
	switch s {
	case Waiting, Preparing, Hashing, Transferring, Verifying, Reconnecting, Pausing, Cancelling:
		return true
	default:
		return false
	}
}

// Terminal 判断任务是否处于终态
func (s State) Terminal() bool {
	return s == Completed || s == Cancelled || s == Skipped
}

// Controlling 判断任务是否处于等待暂停或取消确认的中间态
func (s State) Controlling() bool {
	return s == Pausing || s == Cancelling
}

const (
	MaxBatchTasks      = 1000
	MaxUnfinishedTasks = 1000
	HistoryLimit       = 1000
	HistoryRetention   = 30 * 24 * time.Hour
	MaxRecordBytes     = 65536
)

// Spec 入队任务规格
type Spec struct {
	Skip         string
	Direction    protocol.Direction
	Remote, Path string
	Size         uint64
}

// Task 客户端持久化任务模型
type Task struct {
	ID             string             `json:"id"`
	Batch          string             `json:"batch"`
	Identity       string             `json:"identity"`
	StateKey       string             `json:"state_key"`
	Direction      protocol.Direction `json:"direction"`
	Remote         string             `json:"remote"`
	Path           string             `json:"path"`
	State          State              `json:"state"`
	Done           uint64             `json:"done"`
	Size           uint64             `json:"size"`
	Error          string             `json:"error,omitempty"`
	WantCancel     bool               `json:"want_cancel,omitempty"`
	SourceModified int64              `json:"source_modified,omitempty"`
	Created        time.Time          `json:"created"`
	Updated        time.Time          `json:"updated"`
	Speed          float64            `json:"-"`
	ETA            time.Duration      `json:"-"`
	SHA256         string             `json:"sha256,omitempty"`
	RequestID      string             `json:"request_id,omitempty"`
	TransferID     string             `json:"transfer_id,omitempty"`
	TempName       string             `json:"temp_name,omitempty"`
	TempDevice     uint64             `json:"temp_device,omitempty"`
	TempInode      uint64             `json:"temp_inode,omitempty"`
	Publishing     bool               `json:"publishing,omitempty"`
	Published      bool               `json:"published,omitempty"`
}

// HasCheckpoint 判断任务是否已绑定服务端传输或本地临时下载状态
func (t Task) HasCheckpoint() bool {
	return t.RequestID != "" || t.TransferID != "" || t.TempName != "" || t.TempInode != 0 || t.Publishing || t.Published
}

// Terminal 终态判断
func (t Task) Terminal() bool {
	return t.State.Terminal()
}

// TargetKey 跨平台目标标识键（Windows 下本地路径转换为小写，防止大小写冲突）
func TargetKey(direction protocol.Direction, remote, path string) string {
	target := remote
	if direction == protocol.DirectionDownload {
		target = filepath.Clean(path)
		if runtime.GOOS == "windows" {
			target = strings.ToLower(target)
		}
	}
	return fmt.Sprintf("%d:%s", direction, target)
}

// StateKeyForIdentity 生成特定身份和任务对应的检查点状态键
func StateKeyForIdentity(identity string, direction protocol.Direction, name, path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", identity, direction, name, absolute)))
	return hex.EncodeToString(h[:]), nil
}
