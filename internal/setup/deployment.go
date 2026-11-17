// Package setup 提供服务端首次部署校验、本机证书发现及交互式向导配置。
package setup

import (
	"crypto/x509"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/security"
)

// Deployment 描述服务端全新部署的完整安全参数与输出路径。
type Deployment struct {
	Config          string
	CertDir         string
	ClientDir       string
	Storage         string
	Listen          string
	Address         string
	ServerName      string
	Hosts           string
	User            string
	Upload          uint64
	Download        uint64
	Burst           uint64
	MaxFile         uint64
	UserStorage     uint64
	LogMaxBytes     uint64
	GlobalBandwidth *config.Bandwidth
	Limits          *config.Limits
	Timeouts        *config.Timeouts
	Resources       *config.Resources
	Days            int
	CADays          int
}

func DefaultDeployment(path, base string) Deployment {
	abs := func(s string) string {
		if !filepath.IsAbs(s) {
			s = filepath.Join(base, s)
		}
		p, _ := filepath.Abs(s)
		return p
	}
	c := config.DefaultServer()
	cfgAbs := abs(path)
	return Deployment{
		Config:      cfgAbs,
		CertDir:     abs("certs"),
		ClientDir:   filepath.Dir(cfgAbs),
		Storage:     abs(c.StorageRoot),
		Listen:      c.Listen,
		Address:     "127.0.0.1:7443",
		ServerName:  "localhost",
		Hosts:       "localhost,127.0.0.1,::1",
		User:        "alice",
		Upload:      c.Bandwidth.UploadBytesPerSecond,
		Download:    c.Bandwidth.DownloadBytesPerSecond,
		Burst:       c.Bandwidth.BurstBytes,
		MaxFile:     c.Resources.MaxFileBytes,
		UserStorage: c.Resources.UserStorageBytes,
		LogMaxBytes: c.Logging.MaxFileBytes,
		Days:        30,
		CADays:      365,
	}
}

func ValidateAddress(address string) (string, string, error) {
	return security.ValidateConnectAddress(address)
}

func (d Deployment) Validate() error {
	if _, _, err := ValidateAddress(d.Address); err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(d.Listen)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("监听端口无效")
	}
	dns, ips, err := security.ParseServerNames(d.Hosts)
	if err != nil {
		return err
	}
	leaf := x509.Certificate{DNSNames: dns, IPAddresses: ips}
	if err = leaf.VerifyHostname(d.ServerName); err != nil {
		return fmt.Errorf("客户端校验名称不在 SAN 中：%w", err)
	}
	if !config.ValidUserPath(d.User) || d.User == "ca" || d.User == "server" {
		return fmt.Errorf("用户名须为 1–64 个字母、数字、下划线或连字符，且不能为保留名称")
	}
	if d.Days < 1 || d.Days > 3650 || d.CADays < d.Days || d.CADays > 3650 {
		return fmt.Errorf("证书有效期须为 1–3650 天，CA 不短于用户证书")
	}
	if !filepath.IsAbs(d.Config) || !filepath.IsAbs(d.CertDir) || !filepath.IsAbs(d.ClientDir) || !filepath.IsAbs(d.Storage) {
		return fmt.Errorf("部署路径必须为绝对路径")
	}
	if _, err = os.Lstat(d.Config); !os.IsNotExist(err) {
		return fmt.Errorf("配置已存在或无法访问；新部署不会覆盖：%s", d.Config)
	}
	if _, err = os.Lstat(filepath.Join(d.CertDir, "ca.pem")); !os.IsNotExist(err) {
		return fmt.Errorf("证书目录已有 CA 或无法访问，请选择新的证书目录")
	}
	if info, err := os.Stat(d.Storage); err == nil && !info.IsDir() {
		return fmt.Errorf("存储位置必须为目录")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return d.serverConfig().ValidateDraft()
}

func (d Deployment) serverConfig() config.Server {
	c := config.DefaultServer()
	if d.Limits != nil {
		c.Limits = *d.Limits
	}
	if d.Timeouts != nil {
		c.Timeouts = *d.Timeouts
	}
	if d.Resources != nil {
		c.Resources = *d.Resources
	}
	c.Listen = d.Listen
	c.StorageRoot = d.Storage
	c.Bandwidth.UploadBytesPerSecond = d.Upload
	c.Bandwidth.DownloadBytesPerSecond = d.Download
	if d.Burst > 0 {
		c.Bandwidth.BurstBytes = d.Burst
	}
	c.Resources.MaxFileBytes = d.MaxFile
	c.Resources.UserStorageBytes = d.UserStorage
	if d.LogMaxBytes > 0 {
		c.Logging.MaxFileBytes = d.LogMaxBytes
	}
	if d.GlobalBandwidth != nil {
		gb := *d.GlobalBandwidth
		c.GlobalBandwidth = &gb
	}
	return c
}

func InitializeDeployment(d Deployment) (certmanage.Result, error) {
	if err := d.Validate(); err != nil {
		return certmanage.Result{}, err
	}
	c := d.serverConfig()
	return certmanage.Execute("init", certmanage.Options{
		Dir:              d.CertDir,
		ServerConfig:     d.Config,
		ClientConfigDir:  d.ClientDir,
		Name:             d.User,
		Hosts:            d.Hosts,
		Address:          d.Address,
		ServerName:       d.ServerName,
		Days:             d.Days,
		CADays:           d.CADays,
		CAName:           "SFT Local CA",
		ServerCommonName: "SFT server",
		InitialServer:    &c,
	})
}

// LocalAddresses 枚举本机非环回单播地址，用于生成跨机部署 SAN 提示。
func LocalAddresses() []string {
	seen := map[string]bool{}
	var out []string
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, i := range interfaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addresses {
			ip, _, err := net.ParseCIDR(a.String())
			if err == nil && ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				s := ip.String()
				if !seen[s] {
					out = append(out, s)
					seen[s] = true
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := net.ParseIP(out[i]).To4() != nil, net.ParseIP(out[j]).To4() != nil
		if a != b {
			return a
		}
		return out[i] < out[j]
	})
	return out
}

var quantityPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([a-zA-Z]*(?:/s)?)$`)

// ParseBytes 使用有理数精确解析容量与带宽单位（如 "10 MiB/s"、"5 GiB"），拒绝四舍五入截断或溢出。
func ParseBytes(text string) (uint64, error) {
	match := quantityPattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return 0, fmt.Errorf("请输入数值和单位，例如 10 MiB/s、5 GiB")
	}
	unit := strings.ToLower(strings.TrimSuffix(match[2], "/s"))
	multipliers := map[string]uint64{
		"": 1, "b": 1,
		"kb": 1000, "mb": 1000000, "gb": 1000000000, "tb": 1000000000000,
		"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
	}
	factor, ok := multipliers[unit]
	if !ok {
		return 0, fmt.Errorf("不支持的单位：%s", match[2])
	}
	value, ok := new(big.Rat).SetString(match[1])
	if !ok {
		return 0, fmt.Errorf("数值无效")
	}
	value.Mul(value, new(big.Rat).SetInt(new(big.Int).SetUint64(factor)))
	if !value.IsInt() || !value.Num().IsUint64() {
		return 0, fmt.Errorf("大小须为整数个字节且不能溢出")
	}
	n := value.Num().Uint64()
	if n == 0 || n > 1<<63-1 {
		return 0, fmt.Errorf("大小须大于 0 且不超过 2^63-1 字节")
	}
	return n, nil
}
