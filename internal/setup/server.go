package setup

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"sft-tui/internal/config"
	"sft-tui/internal/pal"
	"sft-tui/internal/prompt"
	"sft-tui/internal/security"
)

type certFile struct {
	path string
	data []byte
	leaf *x509.Certificate
}

func scanCertFiles(root string, maxDepth int) ([]string, error) {
	var out []string
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if depth == 0 {
				return err
			}
			return nil
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || e.Type()&os.ModeSymlink != 0 {
				continue
			}
			full := filepath.Join(dir, name)
			if e.IsDir() {
				if depth+1 <= maxDepth {
					if err := walk(full, depth+1); err != nil {
						return err
					}
				}
				continue
			}
			switch strings.ToLower(filepath.Ext(name)) {
			case ".pem", ".crt", ".cer", ".key":
				if e.Type().IsRegular() {
					out = append(out, full)
				}
			}
		}
		return nil
	}
	if err := walk(root, 0); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// ScanServerTLS 在指定目录树中自动匹配有效的 CA、服务端叶证书与私钥组合。
func ScanServerTLS(root string) ([]config.TLSFiles, error) {
	paths, err := scanCertFiles(root, 6)
	if err != nil {
		return nil, err
	}
	var certs, keys []certFile
	for _, path := range paths {
		data, err := pal.ReadSmall(path, security.MaxCertFileBytes)
		if err != nil {
			continue
		}
		if leaf, err := security.ReadCertificate(path); err == nil {
			certs = append(certs, certFile{path: path, data: data, leaf: leaf})
		}
		if bytes.Contains(data, []byte("PRIVATE KEY-----")) {
			keys = append(keys, certFile{path: path, data: data})
		}
		if len(certs)+len(keys) > 512 {
			return nil, fmt.Errorf("证书材料过多，请用 -dir 指定较小的扫描目录")
		}
	}
	sort.SliceStable(certs, func(i, j int) bool { return certs[i].leaf.NotAfter.After(certs[j].leaf.NotAfter) })
	var matches []config.TLSFiles
	for _, cert := range certs {
		if cert.leaf.IsCA {
			continue
		}
		for _, key := range keys {
			if _, err := tls.X509KeyPair(cert.data, key.data); err != nil {
				continue
			}
			for _, ca := range certs {
				if !ca.leaf.IsCA {
					continue
				}
				files := config.TLSFiles{CACertificate: ca.path, Certificate: cert.path, PrivateKey: key.path}
				c := config.DefaultServer()
				c.TLS = files
				if _, _, err := security.PrepareServerTLS(c); err == nil {
					matches = append(matches, files)
				}
			}
		}
	}
	return matches, nil
}

// Server 提供交互式服务端配置调整向导，并保持已有授权用户列表与原子落盘锁安全。
func Server(in io.Reader, out io.Writer, path, scanDir string) error {
	c, err := config.LoadServerDraft(path)
	fresh := os.IsNotExist(err)
	if err != nil && !fresh {
		return err
	}
	var before []byte
	if fresh {
		c = config.DefaultServer()
		c.AuthorizedClients = []config.AuthorizedClient{}
	} else {
		before, err = os.ReadFile(path)
		if err != nil {
			return err
		}
	}
	d := prompt.New(in, out)
	matches, err := ScanServerTLS(scanDir)
	if err != nil {
		return err
	}
	validCurrent := false
	if !fresh {
		_, _, err := security.PrepareServerTLS(c)
		validCurrent = err == nil
	}
	if !validCurrent && len(matches) > 0 {
		index := 0
		if len(matches) > 1 {
			choices := make([]string, len(matches))
			for i, match := range matches {
				choices[i] = match.Certificate + " (CA: " + match.CACertificate + ")"
			}
			index, err = d.Choose("发现多组服务端证书，选择一组", choices)
			if err != nil {
				return err
			}
		}
		c.TLS = matches[index]
	}
	for _, field := range []struct {
		label string
		value *string
	}{
		{"CA 证书路径", &c.TLS.CACertificate},
		{"服务端证书路径", &c.TLS.Certificate},
		{"服务端私钥路径", &c.TLS.PrivateKey},
	} {
		*field.value, err = d.Text(field.label, *field.value, func(s string) error {
			info, err := os.Stat(s)
			if err == nil && !info.Mode().IsRegular() {
				err = fmt.Errorf("必须是普通文件")
			}
			return err
		})
		if err != nil {
			return err
		}
		*field.value, err = filepath.Abs(*field.value)
		if err != nil {
			return err
		}
	}
	if _, _, err := security.PrepareServerTLS(c); err != nil {
		return fmt.Errorf("证书、私钥或 CA 不匹配：%w", err)
	}
	c.Listen, err = d.Text("监听地址（跨主机可填 0.0.0.0:7443）", c.Listen, func(s string) error {
		_, port, err := net.SplitHostPort(s)
		if err != nil {
			return err
		}
		n, err := net.LookupPort("tcp", port)
		if err != nil || n < 0 || n > 65535 {
			return fmt.Errorf("端口须在 0–65535 范围内")
		}
		return nil
	})
	if err != nil {
		return err
	}
	c.StorageRoot, err = d.Text("存储目录", c.StorageRoot, prompt.Nonempty)
	if err != nil {
		return err
	}
	c.StorageRoot, err = filepath.Abs(c.StorageRoot)
	if err != nil {
		return err
	}
	for _, field := range []struct {
		label     string
		value     *uint64
		low, high uint64
	}{
		{"默认上传带宽（支持单位如 10 MiB/s）", &c.Bandwidth.UploadBytesPerSecond, 1, 1 << 53},
		{"默认下载带宽（支持单位如 10 MiB/s）", &c.Bandwidth.DownloadBytesPerSecond, 1, 1 << 53},
		{"突发字节数（支持单位如 1 MiB）", &c.Bandwidth.BurstBytes, uint64(c.Limits.ChunkSize), 1 << 30},
		{"单文件最大字节数（支持单位如 10 GiB）", &c.Resources.MaxFileBytes, 1, 1<<63 - 1},
		{"每用户存储字节数（支持单位如 20 GiB）", &c.Resources.UserStorageBytes, c.Resources.MaxFileBytes, 1<<63 - 1},
	} {
		*field.value, err = promptBytes(d, field.label, *field.value, field.low, field.high)
		if err != nil {
			return err
		}
	}
	advanced, err := d.Bool("设置全部高级参数（全局总带宽、流、块、窗口、超时、资源限额）", false)
	if err != nil {
		return err
	}
	if advanced {
		useGlobal, err := d.Bool("启用全局总带宽限速 (global_bandwidth)", c.GlobalBandwidth != nil)
		if err != nil {
			return err
		}
		if useGlobal {
			gb := config.Bandwidth{
				UploadBytesPerSecond:   c.Bandwidth.UploadBytesPerSecond * 2,
				DownloadBytesPerSecond: c.Bandwidth.DownloadBytesPerSecond * 2,
				BurstBytes:             c.Bandwidth.BurstBytes * 2,
			}
			if c.GlobalBandwidth != nil {
				gb = *c.GlobalBandwidth
			}
			if gb.UploadBytesPerSecond, err = promptBytes(d, "全局总上传带宽（支持单位如 50 MiB/s）", gb.UploadBytesPerSecond, 1, 1<<53); err != nil {
				return err
			}
			if gb.DownloadBytesPerSecond, err = promptBytes(d, "全局总下载带宽（支持单位如 50 MiB/s）", gb.DownloadBytesPerSecond, 1, 1<<53); err != nil {
				return err
			}
			if gb.BurstBytes, err = promptBytes(d, "全局总带宽突发字节数（支持单位如 2 MiB）", gb.BurstBytes, uint64(c.Limits.ChunkSize), 1<<30); err != nil {
				return err
			}
			c.GlobalBandwidth = &gb
		} else {
			c.GlobalBandwidth = nil
		}

		for _, field := range config.ServerNumericFields(&c) {
			switch field.Path {
			case "bandwidth.upload_bytes_per_second", "bandwidth.download_bytes_per_second", "bandwidth.burst_bytes",
				"resources.max_file_bytes", "resources.user_storage_bytes":
				continue
			case "limits.chunk_size", "limits.window_size", "logging.max_file_bytes":
				n, err := promptBytes(d, field.Path+"（支持单位）", field.Uint(), 1, field.Max())
				if err != nil {
					return err
				}
				if err := field.Set(strconv.FormatUint(n, 10)); err != nil {
					return err
				}
			default:
				n, err := d.Uint(field.Path, field.Uint(), 1, field.Max())
				if err != nil {
					return err
				}
				if err := field.Set(fmt.Sprint(n)); err != nil {
					return err
				}
			}
		}
	}
	for i := range c.AuthorizedClients {
		c.AuthorizedClients[i].Certificate, err = filepath.Abs(c.AuthorizedClients[i].Certificate)
		if err != nil {
			return err
		}
	}
	if err := c.ValidateDraft(); err != nil {
		return err
	}
	if len(c.AuthorizedClients) > 0 {
		if _, _, err := security.ServerTLS(c); err != nil {
			return err
		}
	}

	certDir := filepath.Dir(c.TLS.CACertificate)
	if err := pal.EnsurePrivateDir(certDir); err != nil {
		return err
	}
	lock, err := pal.OpenLockFile(filepath.Join(certDir, ".certgen.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := pal.LockFile(lock); err != nil {
		return err
	}
	defer func() { _ = pal.UnlockFile(lock) }()

	if !fresh {
		current, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(current, before) {
			return fmt.Errorf("配置在交互期间发生变化，请重新运行 init")
		}
	}
	if err := pal.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := pal.StageAndReplaceMode(path, append(data, '\n'), 0600, fresh); err != nil {
		return err
	}
	fmt.Fprintln(out, "服务端配置已保存："+path+"；启动或重启 sftd 后生效")
	if len(c.AuthorizedClients) == 0 {
		fmt.Fprintln(out, "尚未授权用户；可运行 sftd cert users -server-config "+path+" 新建用户，再启动 sftd")
	}
	return nil
}

func promptBytes(d *prompt.Dialog, label string, fallback, low, high uint64) (uint64, error) {
	raw, err := d.Text(label, strconv.FormatUint(fallback, 10), func(s string) error {
		v, err := ParseBytes(s)
		if err != nil {
			return err
		}
		if v < low || v > high {
			return fmt.Errorf("须在 %d–%d 字节范围内", low, high)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return ParseBytes(raw)
}
