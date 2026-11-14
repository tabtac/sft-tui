package config

import (
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"runtime"
	"strings"

	"sft-tui/internal/protocol"
)

var userPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func ValidUserID(id string) bool {
	return userPattern.MatchString(id)
}

// ValidUserPath 校验用户 ID 合法性并拦截 Windows 保留设备名，避免路径冲突。
func ValidUserPath(id string) bool {
	if !ValidUserID(id) {
		return false
	}
	if runtime.GOOS != "windows" {
		return true
	}
	upper := strings.ToUpper(id)
	switch upper {
	case "CON", "PRN", "AUX", "NUL":
		return false
	}
	return !(len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && upper[3] >= '1' && upper[3] <= '9')
}

func validateCommon(files TLSFiles, limits Limits, t Timeouts) error {
	if files.CACertificate == "" || files.Certificate == "" || files.PrivateKey == "" {
		return fmt.Errorf("必须配置 CA、证书和私钥路径")
	}
	if err := protocol.ValidateHello(protocol.Hello{
		ClientMaxStreams: limits.MaxStreams,
		ClientMaxChunk:   limits.ChunkSize,
		ClientWindow:     limits.WindowSize,
	}); err != nil {
		return err
	}
	if limits.ChunkSize > limits.WindowSize {
		return fmt.Errorf("分块上限不能大于流接收窗口")
	}
	if t.HandshakeSeconds == 0 || t.HelloSeconds == 0 || t.FrameReadSeconds == 0 || t.WriteSeconds == 0 || t.IdleSeconds == 0 {
		return fmt.Errorf("超时参数必须大于 0")
	}
	return nil
}

func validateBandwidth(b Bandwidth, chunk uint32) error {
	if b.UploadBytesPerSecond == 0 || b.DownloadBytesPerSecond == 0 ||
		b.UploadBytesPerSecond > 1<<53 || b.DownloadBytesPerSecond > 1<<53 ||
		b.BurstBytes < uint64(chunk) || b.BurstBytes > 1<<30 {
		return fmt.Errorf("带宽必须为正，突发量至少容纳一个数据块且不超过 1 GiB")
	}
	return nil
}

func (c Server) ValidateDraft() error { return c.validate(false) }
func (c Server) Validate() error      { return c.validate(true) }

func (c Server) validate(requireAuthorization bool) error {
	if c.Logging.MaxFileBytes < 64*1024 || c.Logging.MaxFileBytes > 1<<30 {
		return fmt.Errorf("日志文件大小须为 64 KiB–1 GiB")
	}
	if err := validateCommon(c.TLS, c.Limits, c.Timeouts); err != nil {
		return err
	}
	r := c.Resources
	if r.MaxStreamsPerUser == 0 || r.MaxPendingTransfersPerUser == 0 || r.MaxFilesPerUser == 0 ||
		r.MaxNewTasksPerMinute == 0 || r.MaxUploadCheckpointsPerSecond == 0 || r.TransferRetentionSeconds == 0 {
		return fmt.Errorf("用户流、任务、文件数、初始化/检查点频率和保留时间限额必须大于 0")
	}
	if err := validateBandwidth(c.Bandwidth, c.Limits.ChunkSize); err != nil {
		return err
	}
	if c.GlobalBandwidth != nil {
		if err := validateBandwidth(*c.GlobalBandwidth, c.Limits.ChunkSize); err != nil {
			return fmt.Errorf("全局带宽配置非法：%w", err)
		}
	}
	if c.Listen == "" || c.StorageRoot == "" || r.MaxConnections == 0 || r.MaxHandshakes == 0 ||
		r.MaxHandshakes > r.MaxConnections || r.MaxUnauthenticatedPerIP == 0 || r.MaxConnectionsPerUser == 0 ||
		r.MaxFileBytes == 0 || r.MaxFileBytes > math.MaxInt64 || r.UserStorageBytes < r.MaxFileBytes || r.UserStorageBytes > math.MaxInt64 {
		return fmt.Errorf("监听地址、存储根目录或资源限额非法")
	}
	if requireAuthorization && len(c.AuthorizedClients) == 0 {
		return fmt.Errorf("必须显式授权至少一张客户端证书")
	}
	users := make(map[string]bool)
	for _, u := range c.AuthorizedClients {
		key := u.UserID
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if !ValidUserPath(u.UserID) || u.Certificate == "" || users[key] {
			return fmt.Errorf("授权用户 ID 或证书配置非法/重复")
		}
		users[key] = true
		for _, p := range u.Permissions {
			if p != "upload" && p != "download" {
				return fmt.Errorf("未知操作权限：%s", p)
			}
		}
		if u.Bandwidth != nil {
			if err := validateBandwidth(*u.Bandwidth, c.Limits.ChunkSize); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c Client) Validate() error {
	if err := validateCommon(c.TLS, c.Limits, c.Timeouts); err != nil {
		return err
	}
	if c.ServerAddress == "" || c.ServerName == "" || c.MaxFileBytes == 0 || c.MaxFileBytes > math.MaxInt64 {
		return fmt.Errorf("必须配置服务端地址、证书名称和合法文件上限")
	}
	if c.StateDir == "" {
		return fmt.Errorf("必须配置客户端状态目录")
	}
	if c.ServerFingerprint != "" {
		b, err := hex.DecodeString(strings.TrimSpace(c.ServerFingerprint))
		if err != nil || len(b) != 32 {
			return fmt.Errorf("服务端证书指纹必须为 64 位十六进制 SHA-256")
		}
	}
	return nil
}
