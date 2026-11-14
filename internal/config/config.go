// Package config 提供客户端与服务端配置结构、默认值、严格 JSON 解析与验证。
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"reflect"
	"slices"
	"strconv"

	"sft-tui/internal/pal"
	"sft-tui/internal/protocol"
)

const (
	DefaultMaxFilesPerUser uint32 = 10000
	MaxConfigBytes         int64  = 64 * 1024
)

type TLSFiles struct {
	CACertificate string `json:"ca_certificate"`
	Certificate   string `json:"certificate"`
	PrivateKey    string `json:"private_key"`
}

type Limits struct {
	MaxStreams uint16 `json:"max_streams"`
	ChunkSize  uint32 `json:"chunk_size"`
	WindowSize uint32 `json:"window_size"`
}

type Timeouts struct {
	HandshakeSeconds uint16 `json:"handshake_seconds"`
	HelloSeconds     uint16 `json:"hello_seconds"`
	FrameReadSeconds uint16 `json:"frame_read_seconds"`
	WriteSeconds     uint16 `json:"write_seconds"`
	IdleSeconds      uint16 `json:"idle_seconds"`
}

type Resources struct {
	MaxConnections                uint32 `json:"max_connections"`
	MaxHandshakes                 uint32 `json:"max_handshakes"`
	MaxUnauthenticatedPerIP       uint32 `json:"max_unauthenticated_per_ip"`
	MaxConnectionsPerUser         uint32 `json:"max_connections_per_user"`
	MaxStreamsPerUser             uint32 `json:"max_streams_per_user"`
	MaxPendingTransfersPerUser    uint32 `json:"max_pending_transfers_per_user"`
	MaxFilesPerUser               uint32 `json:"max_files_per_user"`
	MaxNewTasksPerMinute          uint32 `json:"max_new_tasks_per_minute"`
	MaxUploadCheckpointsPerSecond uint32 `json:"max_upload_checkpoints_per_second"`
	MaxFileBytes                  uint64 `json:"max_file_bytes"`
	UserStorageBytes              uint64 `json:"user_storage_bytes"`
	TransferRetentionSeconds      uint32 `json:"transfer_retention_seconds"`
}

// Bandwidth 由用户策略选择，在该用户所有连接间共享。
type Bandwidth struct {
	UploadBytesPerSecond   uint64 `json:"upload_bytes_per_second"`
	DownloadBytesPerSecond uint64 `json:"download_bytes_per_second"`
	BurstBytes             uint64 `json:"burst_bytes"`
}

type Logging struct {
	MaxFileBytes uint64 `json:"max_file_bytes"`
}

type AuthorizedClient struct {
	UserID      string     `json:"user_id"`
	Certificate string     `json:"certificate"`
	Disabled    bool       `json:"disabled"`
	Permissions []string   `json:"permissions"`
	Bandwidth   *Bandwidth `json:"bandwidth,omitempty"`
	ExternalKey bool       `json:"external_key,omitempty"`
}

type Server struct {
	Listen            string             `json:"listen"`
	StorageRoot       string             `json:"storage_root"`
	TLS               TLSFiles           `json:"tls"`
	Limits            Limits             `json:"limits"`
	Timeouts          Timeouts           `json:"timeouts"`
	Resources         Resources          `json:"resources"`
	Bandwidth         Bandwidth          `json:"bandwidth"`
	Logging           Logging            `json:"logging"`
	AuthorizedClients []AuthorizedClient `json:"authorized_clients"`
}

type Client struct {
	StateDir          string   `json:"state_dir"`
	MaxFileBytes      uint64   `json:"max_file_bytes"`
	ServerAddress     string   `json:"server_address"`
	ServerName        string   `json:"server_name"`
	ServerFingerprint string   `json:"server_fingerprint,omitempty"`
	TLS               TLSFiles `json:"tls"`
	Limits            Limits   `json:"limits"`
	Timeouts          Timeouts `json:"timeouts"`
}

func defaultLimits() Limits {
	return Limits{
		MaxStreams: protocol.TargetMaxStreams,
		ChunkSize:  protocol.MaxChunkSize,
		WindowSize: protocol.DefaultWindowSize,
	}
}

func defaultTimeouts() Timeouts {
	return Timeouts{
		HandshakeSeconds: 5,
		HelloSeconds:     5,
		FrameReadSeconds: 10,
		WriteSeconds:     30,
		IdleSeconds:      120,
	}
}

// DefaultServer 返回默认服务端配置（授权客户端列表需显式配置）。
func DefaultServer() Server {
	return Server{
		Listen:      "127.0.0.1:7443",
		StorageRoot: "var/storage",
		Logging:     Logging{MaxFileBytes: 10 * 1024 * 1024},
		TLS: TLSFiles{
			CACertificate: "certs/ca.pem",
			Certificate:   "certs/server.pem",
			PrivateKey:    "certs/server-key.pem",
		},
		Limits:   defaultLimits(),
		Timeouts: defaultTimeouts(),
		Resources: Resources{
			MaxConnections:                128,
			MaxHandshakes:                 16,
			MaxUnauthenticatedPerIP:       8,
			MaxConnectionsPerUser:         4,
			MaxStreamsPerUser:             protocol.TargetMaxStreams,
			MaxPendingTransfersPerUser:    16,
			MaxFilesPerUser:               DefaultMaxFilesPerUser,
			MaxNewTasksPerMinute:          30,
			MaxUploadCheckpointsPerSecond: 64,
			MaxFileBytes:                  10 * 1024 * 1024 * 1024,
			UserStorageBytes:              20 * 1024 * 1024 * 1024,
			TransferRetentionSeconds:      24 * 60 * 60,
		},
		Bandwidth: Bandwidth{
			UploadBytesPerSecond:   2 * 1024 * 1024,
			DownloadBytesPerSecond: 2 * 1024 * 1024,
			BurstBytes:             1024 * 1024,
		},
	}
}

// DefaultClient 返回默认客户端配置。
func DefaultClient() Client {
	return Client{
		StateDir:      "var/client-state",
		MaxFileBytes:  10 * 1024 * 1024 * 1024,
		ServerAddress: "127.0.0.1:7443",
		ServerName:    "localhost",
		TLS: TLSFiles{
			CACertificate: "certs/ca.pem",
			Certificate:   "certs/client.pem",
			PrivateKey:    "certs/client-key.pem",
		},
		Limits:   defaultLimits(),
		Timeouts: defaultTimeouts(),
	}
}

func loadJSON(path string, out any) error {
	data, err := pal.ReadSmall(path, MaxConfigBytes)
	if err != nil {
		return err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return fmt.Errorf("配置必须是最多 64 KiB 的 JSON 对象")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("读取配置：%w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("配置必须仅包含一个 JSON 对象")
	}
	return nil
}

// SaveJSON 将配置格式化为缩进 JSON 并原子落盘。
func SaveJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return pal.StageAndReplace(path, data)
}

func LoadServer(path string) (Server, error) {
	c := DefaultServer()
	if err := loadJSON(path, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

func LoadServerDraft(path string) (Server, error) {
	c := DefaultServer()
	if err := loadJSON(path, &c); err != nil {
		return c, err
	}
	return c, c.ValidateDraft()
}

func LoadClient(path string) (Client, error) {
	c := DefaultClient()
	if err := loadJSON(path, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

// ClientAddressFromListen 从服务端监听地址推导本地客户端连接地址。
func ClientAddressFromListen(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("无法从监听地址 %q 生成客户端连接地址：%w", listen, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("监听地址 %q 的端口须为 1–65535", listen)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return net.JoinHostPort(host, port), nil
}

// RestartConfig 排除支持热重载的字段（TLS 与 AuthorizedClients）。
func RestartConfig(c Server) Server {
	c.TLS = TLSFiles{}
	c.AuthorizedClients = nil
	return c
}

func RestartRequired(a, b Server) bool {
	return !reflect.DeepEqual(RestartConfig(a), RestartConfig(b))
}

// MergeHot 保留运行中需重启生效的字段，仅合并热更新字段。
func MergeHot(base, updated Server) Server {
	base.TLS, base.AuthorizedClients = updated.TLS, updated.AuthorizedClients
	return base
}

// HotEqual 比较热更新配置是否一致，忽略权限切片顺序。
func HotEqual(a, b Server) bool {
	normalize := func(c Server) Server {
		c = MergeHot(Server{}, c)
		c.AuthorizedClients = slices.Clone(c.AuthorizedClients)
		for i := range c.AuthorizedClients {
			p := &c.AuthorizedClients[i].Permissions
			*p = slices.Clone(*p)
			slices.Sort(*p)
		}
		return c
	}
	return reflect.DeepEqual(normalize(a), normalize(b))
}
