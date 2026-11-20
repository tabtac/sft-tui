package certmanage

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"sft-tui/internal/config"
	"sft-tui/internal/pal"
	"sft-tui/internal/security"
)

func prepareExportDir(outDir string, trackCertsSubdir bool) (func(bool), error) {
	_, outStatErr := os.Lstat(outDir)
	outExisted := !os.IsNotExist(outStatErr)
	var certsDir string
	var certsExisted bool
	if trackCertsSubdir {
		certsDir = filepath.Join(outDir, "certs")
		_, certDirStatErr := os.Lstat(certsDir)
		certsExisted = !os.IsNotExist(certDirStatErr)
	}
	if err := pal.EnsurePrivateDir(outDir); err != nil {
		return nil, err
	}
	return func(ok bool) {
		if ok {
			return
		}
		if !outExisted {
			_ = os.RemoveAll(outDir)
		} else if trackCertsSubdir && !certsExisted {
			_ = os.Remove(certsDir)
		}
	}, nil
}

func exportAction(o Options, server config.Server, index int) (Result, error) {
	result := Result{Server: server}
	if index < 0 || o.Out == "" {
		return result, fmt.Errorf("export 必须指定有效的 -name 和目标目录 -out")
	}
	u := server.AuthorizedClients[index]
	if u.Disabled {
		return result, fmt.Errorf("用户已吊销，不能导出")
	}
	if o.CertificateOnly {
		return exportPublicCertificates(o, server, u)
	}
	if u.ExternalKey {
		return result, fmt.Errorf("该用户自行保管私钥，请使用 -public-only 导出公开证书")
	}
	c, err := config.LoadClient(clientConfigPath(o, u.UserID))
	if err != nil {
		return result, err
	}
	configured, err := os.Stat(c.TLS.Certificate)
	if err != nil {
		return result, err
	}
	authorized, err := os.Stat(u.Certificate)
	if err != nil {
		return result, err
	}
	if !os.SameFile(configured, authorized) {
		return result, fmt.Errorf("客户端配置与当前授权证书不一致，请先执行 sync")
	}
	pair, err := tls.LoadX509KeyPair(c.TLS.Certificate, c.TLS.PrivateKey)
	if err != nil {
		return result, err
	}
	ca, err := extractCertificatesOnly(server.TLS.CACertificate)
	if err != nil {
		return result, err
	}
	cert, err := extractCertificatesOnly(c.TLS.Certificate)
	if err != nil {
		return result, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(pair.PrivateKey)
	if err != nil {
		return result, fmt.Errorf("编码客户端私钥失败：%w", err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	c.TLS = config.TLSFiles{
		CACertificate: "certs/ca.pem",
		Certificate:   "certs/" + u.UserID + ".pem",
		PrivateKey:    "certs/" + u.UserID + "-key.pem",
	}
	c.StateDir = "state"
	cfgFile, err := jsonChange(filepath.Join(o.Out, "client."+u.UserID+".local.json"), c)
	if err != nil {
		return result, err
	}
	cleanupDir, err := prepareExportDir(o.Out, true)
	if err != nil {
		return result, err
	}
	ok := false
	defer func() { cleanupDir(ok) }()
	err = applyChanges([]fileChange{
		{path: filepath.Join(o.Out, c.TLS.CACertificate), data: ca, mode: 0644, exclusive: true},
		{path: filepath.Join(o.Out, c.TLS.Certificate), data: cert, mode: 0644, exclusive: true},
		{path: filepath.Join(o.Out, c.TLS.PrivateKey), data: key, mode: 0600, exclusive: true},
		cfgFile,
	})
	if err != nil {
		return result, err
	}
	ok = true
	result.Messages = []string{"客户端分发目录已导出：" + o.Out}
	return result, nil
}

func exportPublicCertificates(o Options, server config.Server, user config.AuthorizedClient) (Result, error) {
	result := Result{Server: server}
	ca, err := readPublicCertificates(server.TLS.CACertificate)
	if err != nil {
		return result, err
	}
	cert, err := readPublicCertificates(user.Certificate)
	if err != nil {
		return result, err
	}
	cleanupDir, err := prepareExportDir(o.Out, false)
	if err != nil {
		return result, err
	}
	ok := false
	defer func() { cleanupDir(ok) }()
	note := "此目录仅含 CA 和用户公开证书，不含任何私钥。\n将 " + user.UserID + ".pem 与客户端本地保留的原私钥配套使用。\n"
	err = applyChanges([]fileChange{
		{path: filepath.Join(o.Out, "ca.pem"), data: ca, mode: 0644, exclusive: true},
		{path: filepath.Join(o.Out, user.UserID+".pem"), data: cert, mode: 0644, exclusive: true},
		{path: filepath.Join(o.Out, "README.txt"), data: []byte(note), mode: 0644, exclusive: true},
	})
	if err != nil {
		return result, err
	}
	ok = true
	result.Messages = []string{"公开证书已导出：" + o.Out}
	return result, nil
}

// extractCertificatesOnly 解析并仅重新编码 PEM 中的合法 CERTIFICATE 块，绝不带出任何混排私钥或杂项内容。
func extractCertificatesOnly(path string) ([]byte, error) {
	data, err := pal.ReadSmall(path, security.MaxCertFileBytes)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	remaining := data
	count := 0
	for len(bytes.TrimSpace(remaining)) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil {
			break
		}
		remaining = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, fmt.Errorf("无效证书 %s：%w", path, err)
		}
		out.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}))
		count++
	}
	if count == 0 {
		return nil, fmt.Errorf("证书文件未包含有效 CERTIFICATE 块：%s", path)
	}
	return out.Bytes(), nil
}

func readPublicCertificates(path string) ([]byte, error) {
	data, err := pal.ReadSmall(path, security.MaxCertFileBytes)
	if err != nil {
		return nil, err
	}
	remaining := bytes.TrimSpace(data)
	count := 0
	for len(remaining) > 0 {
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, fmt.Errorf("公开证书导出拒绝非证书或私钥内容：%s", path)
		}
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("无效的公开证书：%s", path)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return nil, err
		}
		count++
		remaining = bytes.TrimSpace(rest)
	}
	if count == 0 {
		return nil, fmt.Errorf("公开证书文件为空")
	}
	return data, nil
}
