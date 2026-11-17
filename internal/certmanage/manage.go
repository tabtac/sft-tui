// Package certmanage 提供证书签发、轮换、吊销、ACL 策略调整与客户端配置同步/导出。
package certmanage

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/pal"
	"sft-tui/internal/security"
)

type Options struct {
	InitialServer                      *config.Server
	Dir                                string
	ServerConfig                       string
	ClientConfigDir                    string
	Name, Hosts, Address, ServerName   string
	Out, CSR, CSRFingerprint           string
	CertificateOnly                    bool
	CAName, ServerCommonName           string
	CADays, Days                       int
	Renew, Enable                      bool
	Policy                             *UserPolicy
	ExpectedUser                       *config.AuthorizedClient
}

type UserPolicy struct {
	Disabled    bool
	Permissions []string
	Bandwidth   *config.Bandwidth
}

type Result struct {
	Messages []string
	Server   config.Server
	Changed  bool
}

func loadServerDraftOrDefault(path string) (config.Server, error) {
	c, err := config.LoadServerDraft(path)
	if !os.IsNotExist(err) {
		return c, err
	}
	c = config.DefaultServer()
	c.AuthorizedClients = []config.AuthorizedClient{}
	return c, nil
}

func jsonChange(path string, v any) (fileChange, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fileChange{}, err
	}
	data = append(data, '\n')
	if int64(len(data)) > config.MaxConfigBytes {
		return fileChange{}, fmt.Errorf("配置超过 64 KiB：%s", path)
	}
	return fileChange{path: path, data: data, mode: 0600}, nil
}

func clientConfigPath(o Options, user string) string {
	return filepath.Join(o.ClientConfigDir, "client."+user+".local.json")
}

func keyForCertificate(path string) string {
	if filepath.Base(path) == "cert.pem" {
		return filepath.Join(filepath.Dir(path), "key.pem")
	}
	return strings.TrimSuffix(path, filepath.Ext(path)) + "-key.pem"
}

func signingKey(certificate string) (string, error) {
	preferred := keyForCertificate(certificate)
	if _, err := tls.LoadX509KeyPair(certificate, preferred); err == nil {
		return preferred, nil
	}
	entries, err := os.ReadDir(filepath.Dir(certificate))
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if !entry.Type().IsRegular() || (ext != ".pem" && ext != ".key") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Size() > security.MaxCertFileBytes {
			continue
		}
		candidate := filepath.Join(filepath.Dir(certificate), entry.Name())
		if _, err := tls.LoadX509KeyPair(certificate, candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("未在 CA 同目录找到匹配的签发私钥：%s", certificate)
}

func pendingBytes(path string, changes []fileChange) ([]byte, error) {
	for _, c := range changes {
		if c.path == path {
			return c.data, nil
		}
	}
	return pal.ReadSmall(path, security.MaxCertFileBytes)
}

func clientKey(certificate, existingKey string, changes []fileChange) (string, error) {
	cert, err := pendingBytes(certificate, changes)
	if err != nil {
		return "", err
	}
	defaultKey := keyForCertificate(certificate)
	if existingKey != "" && existingKey != defaultKey {
		if key, err := pendingBytes(existingKey, changes); err == nil {
			if _, err = tls.X509KeyPair(cert, key); err == nil {
				return existingKey, nil
			}
		}
	}
	key, err := pendingBytes(defaultKey, changes)
	if err != nil {
		return "", err
	}
	if _, err = tls.X509KeyPair(cert, key); err != nil {
		return "", err
	}
	return defaultKey, nil
}

func buildClientConfig(o Options, server config.Server, user config.AuthorizedClient, leaf *x509.Certificate, changes []fileChange) (config.Client, error) {
	c, err := config.LoadClient(clientConfigPath(o, user.UserID))
	fresh := os.IsNotExist(err)
	if err != nil && !fresh {
		return c, err
	}
	if fresh {
		c = config.DefaultClient()
		c.StateDir = filepath.Join("var", "client-state", user.UserID)
		if o.Address == "" {
			c.ServerAddress, err = config.ClientAddressFromListen(server.Listen)
			if err != nil {
				return c, err
			}
		}
	}
	key, err := clientKey(user.Certificate, c.TLS.PrivateKey, changes)
	if err != nil {
		return c, fmt.Errorf("用户 %s 的证书与私钥不匹配或不可读取：%w", user.UserID, err)
	}
	c.TLS = config.TLSFiles{CACertificate: server.TLS.CACertificate, Certificate: user.Certificate, PrivateKey: key}
	if o.Address != "" {
		c.ServerAddress = o.Address
	}
	if o.ServerName != "" {
		c.ServerName = o.ServerName
	}
	if leaf != nil && leaf.VerifyHostname(c.ServerName) != nil {
		if o.ServerName != "" {
			return c, fmt.Errorf("server-name 不匹配服务端 SAN")
		}
		if len(leaf.DNSNames) > 0 && !strings.Contains(leaf.DNSNames[0], "*") {
			c.ServerName = leaf.DNSNames[0]
		} else if len(leaf.IPAddresses) > 0 {
			c.ServerName = leaf.IPAddresses[0].String()
		} else {
			return c, fmt.Errorf("请用 -server-name 指定匹配证书 SAN 的名称")
		}
	}
	return c, c.Validate()
}

// withCertLock 在证书管理独占锁下执行配置与证书文件读取/写入操作，防止并发覆盖。
func withCertLock(dir string, fn func() error) error {
	if err := pal.EnsurePrivateDir(dir); err != nil {
		return err
	}
	lock, err := pal.OpenLockFile(filepath.Join(dir, ".certgen.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := pal.LockFile(lock); err != nil {
		return fmt.Errorf("另一个证书管理操作正在执行：%w", err)
	}
	defer func() { _ = pal.UnlockFile(lock) }()
	return fn()
}

// SaveSettings 在证书锁下保留最新热更新证书/授权，原子保存服务端配置（含全局总带宽）。
func SaveSettings(path, dir string, expected, proposed config.Server) (config.Server, error) {
	var out config.Server
	err := withCertLock(dir, func() error {
		current, err := config.LoadServerDraft(path)
		if err != nil {
			return err
		}
		if config.RestartRequired(current, expected) || !reflect.DeepEqual(current.GlobalBandwidth, expected.GlobalBandwidth) {
			out = current
			return fmt.Errorf("配置在编辑期间已被修改，请刷新后重新编辑")
		}
		gb := proposed.GlobalBandwidth
		proposed = config.MergeHot(proposed, current)
		proposed.GlobalBandwidth = gb
		if err := proposed.ValidateDraft(); err != nil {
			out = current
			return err
		}
		pending, err := jsonChange(path, proposed)
		if err != nil {
			return err
		}
		if err := applyChanges([]fileChange{pending}); err != nil {
			return err
		}
		out = proposed
		return nil
	})
	return out, err
}

func Execute(action string, o Options) (Result, error) {
	if o.Dir == "" {
		o.Dir = "certs"
	}
	if o.ServerConfig == "" {
		o.ServerConfig = filepath.Join("configs", "server.local.json")
	}
	if o.ClientConfigDir == "" {
		o.ClientConfigDir = filepath.Dir(o.ServerConfig)
	}
	if o.Days == 0 {
		if action == "ca" {
			o.Days = 365
		} else {
			o.Days = 30
		}
	}
	if o.CADays == 0 {
		o.CADays = 365
	}
	if o.Hosts == "" && action == "init" {
		o.Hosts = "localhost,127.0.0.1,::1"
	}
	if o.InitialServer != nil {
		if action != "init" || len(o.InitialServer.AuthorizedClients) != 0 {
			return Result{}, fmt.Errorf("初始服务配置仅用于全新部署")
		}
		if err := o.InitialServer.ValidateDraft(); err != nil {
			return Result{}, err
		}
	}
	if o.CSR != "" && action != "user" {
		return Result{}, fmt.Errorf("-csr 仅用于签发用户证书")
	}
	if o.Name != "" && (!config.ValidUserPath(o.Name) || o.Name == "ca" || o.Name == "server") {
		return Result{}, fmt.Errorf("非法或保留的用户名")
	}
	if o.Address != "" {
		if _, _, err := net.SplitHostPort(o.Address); err != nil {
			return Result{}, fmt.Errorf("server-address 必须为主机:端口：%w", err)
		}
	}

	var result Result
	err := withCertLock(o.Dir, func() error {
		c, err := loadServerDraftOrDefault(o.ServerConfig)
		if err != nil {
			return err
		}
		result.Server = c
		index := -1
		for i, u := range c.AuthorizedClients {
			if u.UserID == o.Name {
				index = i
				break
			}
		}
		if o.ExpectedUser != nil {
			if (action != "policy" && !(action == "user" && o.Renew)) || index < 0 || !reflect.DeepEqual(c.AuthorizedClients[index], *o.ExpectedUser) {
				return fmt.Errorf("此用户在编辑期间已被修改；请刷新列表后重新提交")
			}
		}
		if action == "status" {
			result.Messages = statusReport(c, o.Name)
			return nil
		}
		if action == "export" {
			res, err := exportAction(o, c, index)
			result = res
			return err
		}
		plan := certificatePlan{
			options: o,
			server:  c,
			index:   index,
			caPath:  filepath.Join(o.Dir, "ca.pem"),
			caKey:   filepath.Join(o.Dir, "ca-key.pem"),
		}
		if leaf, e := security.ReadCertificate(c.TLS.Certificate); e == nil {
			plan.serverLeaf = leaf
		}
		if err := plan.build(action); err != nil {
			return err
		}
		c, changes, serverLeaf, syncUsers := plan.server, plan.changes, plan.serverLeaf, plan.syncUsers
		result.Messages = plan.messages
		if o.Policy != nil && (action == "user" || action == "init") {
			for i := range c.AuthorizedClients {
				if c.AuthorizedClients[i].UserID == o.Name {
					c.AuthorizedClients[i].Disabled = o.Policy.Disabled
					c.AuthorizedClients[i].Permissions = o.Policy.Permissions
					c.AuthorizedClients[i].Bandwidth = o.Policy.Bandwidth
				}
			}
		}
		if err := c.ValidateDraft(); err != nil {
			return err
		}
		if syncUsers {
			for _, u := range c.AuthorizedClients {
				if (action == "user" || action == "sync") && o.Name != "" && u.UserID != o.Name {
					continue
				}
				client, err := buildClientConfig(o, c, u, serverLeaf, changes)
				if err != nil {
					return err
				}
				file, err := jsonChange(clientConfigPath(o, u.UserID), client)
				if err != nil {
					return err
				}
				changes = append(changes, file)
				result.Messages = append(result.Messages, "客户端配置："+file.path)
			}
		}
		file, err := jsonChange(o.ServerConfig, c)
		if err != nil {
			return err
		}
		if action == "init" {
			file.exclusive = true
		}
		changes = append(changes, file)
		if err := applyChanges(changes); err != nil {
			return err
		}
		result.Server, result.Changed = c, true
		result.Messages = append(result.Messages, "服务端配置已同步："+o.ServerConfig)
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

type certificatePlan struct {
	options       Options
	server        config.Server
	index         int
	caPath, caKey string
	changes       []fileChange
	serverLeaf    *x509.Certificate
	syncUsers     bool
	messages      []string
}

func (p *certificatePlan) build(action string) error {
	o := p.options
	switch action {
	case "init":
		return p.initialize()
	case "ca":
		return p.authority()
	case "server", "user":
		return p.issue(action)
	case "revoke":
		if p.index < 0 {
			return fmt.Errorf("用户不存在，请指定 -name")
		}
		p.server.AuthorizedClients[p.index].Disabled = true
		p.messages = append(p.messages, "已撤销用户访问："+o.Name)
	case "policy":
		if p.index < 0 || o.Policy == nil {
			return fmt.Errorf("必须选择已有用户并提供策略")
		}
		u := &p.server.AuthorizedClients[p.index]
		u.Disabled, u.Permissions, u.Bandwidth = o.Policy.Disabled, o.Policy.Permissions, o.Policy.Bandwidth
		p.messages = append(p.messages, "用户权限、禁用状态和带宽已保存："+o.Name)
	case "sync":
		if o.Name != "" && p.index < 0 {
			return fmt.Errorf("用户不存在")
		}
		p.syncUsers = true
	default:
		return fmt.Errorf("未知操作：%s", action)
	}
	return nil
}

func (p *certificatePlan) generation(role string, m security.CertificateMaterial) (string, string) {
	dir := filepath.Join(p.options.Dir, role, m.Leaf.SerialNumber.Text(16))
	cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	p.changes = append(p.changes, fileChange{path: cert, data: m.Certificate, mode: 0644, exclusive: true})
	if len(m.PrivateKey) > 0 {
		p.changes = append(p.changes, fileChange{path: key, data: m.PrivateKey, mode: 0600, exclusive: true})
	}
	return cert, key
}

func (p *certificatePlan) initialize() error {
	o := p.options
	if _, err := os.Lstat(o.ServerConfig); !os.IsNotExist(err) {
		return fmt.Errorf("服务端配置已存在或不可访问：%s", o.ServerConfig)
	}
	if len(p.server.AuthorizedClients) != 0 {
		return fmt.Errorf("已有授权用户，不能重新初始化 CA")
	}
	if o.InitialServer != nil {
		p.server = *o.InitialServer
	}
	ca, err := security.NewCA(o.CADays, "", "", o.CAName)
	if err != nil {
		return err
	}
	leaf, err := security.NewLeafFromCA(ca, "", o.Hosts, o.Days, o.ServerCommonName)
	if err != nil {
		return err
	}
	p.changes = append(p.changes,
		fileChange{path: p.caPath, data: ca.Certificate, mode: 0644, exclusive: true},
		fileChange{path: p.caKey, data: ca.PrivateKey, mode: 0600, exclusive: true},
	)
	cert, key := p.generation("server", leaf)
	p.server.TLS = config.TLSFiles{CACertificate: p.caPath, Certificate: cert, PrivateKey: key}
	p.serverLeaf = leaf.Leaf
	if o.Name != "" {
		user, err := security.NewLeafFromCA(ca, o.Name, "", o.Days)
		if err != nil {
			return err
		}
		ucert, _ := p.generation(filepath.Join("users", o.Name), user)
		p.server.AuthorizedClients = []config.AuthorizedClient{{
			UserID:      o.Name,
			Certificate: ucert,
			Permissions: []string{"upload", "download"},
		}}
		p.syncUsers = true
	}
	p.messages = append(p.messages, "CA、服务端证书与初始配置已生成")
	return nil
}

func (p *certificatePlan) authority() error {
	o := p.options
	var err error
	oldCert, oldKey := "", ""
	if o.Renew {
		p.caPath = p.server.TLS.CACertificate
		p.caKey, err = signingKey(p.caPath)
		if err != nil {
			return err
		}
		oldCert, oldKey = p.caPath, p.caKey
	}
	m, err := security.NewCA(o.Days, oldCert, oldKey)
	if err != nil {
		return err
	}
	p.changes = append(p.changes, fileChange{path: p.caPath, data: m.Certificate, mode: 0644, exclusive: !o.Renew})
	if !o.Renew {
		p.changes = append(p.changes, fileChange{path: p.caKey, data: m.PrivateKey, mode: 0600, exclusive: true})
	}
	if !o.Renew && len(p.server.AuthorizedClients) > 0 {
		return fmt.Errorf("已有授权用户；更换 CA 需要单独的存储目录和服务端配置")
	}
	p.server.TLS.CACertificate = p.caPath
	p.messages = append(p.messages, fmt.Sprintf("CA 已生成，到期：%s", m.Leaf.NotAfter.Format(time.RFC3339)))
	return nil
}

func (p *certificatePlan) issue(action string) error {
	o := p.options
	var err error
	user := ""
	if action == "user" {
		if o.Name == "" {
			return fmt.Errorf("user 必须指定 -name")
		}
		if p.index >= 0 && !o.Renew {
			return fmt.Errorf("用户已存在；续期使用 -renew，修复配置使用 sync")
		}
		if p.index < 0 && o.Renew {
			return fmt.Errorf("无法续期不存在的用户")
		}
		if o.Enable && !o.Renew {
			return fmt.Errorf("-enable 必须与 -renew 一起使用")
		}
		user = o.Name
	}
	if _, err := os.Stat(p.server.TLS.CACertificate); err == nil {
		p.caPath = p.server.TLS.CACertificate
	}
	p.caKey, err = signingKey(p.caPath)
	if err != nil {
		return err
	}
	hosts := o.Hosts
	if action == "server" && hosts == "" {
		hosts = "localhost,127.0.0.1,::1"
		if p.serverLeaf != nil {
			names := append([]string{}, p.serverLeaf.DNSNames...)
			for _, ip := range p.serverLeaf.IPAddresses {
				names = append(names, ip.String())
			}
			if len(names) > 0 {
				hosts = strings.Join(names, ",")
			}
		}
	}
	var m security.CertificateMaterial
	if o.CSR != "" {
		m, err = security.SignCSRExpected(p.caPath, p.caKey, user, o.CSR, o.Days, o.CSRFingerprint)
	} else {
		cn := ""
		if action == "server" && p.serverLeaf != nil {
			cn = p.serverLeaf.Subject.CommonName
		}
		m, err = security.NewLeaf(p.caPath, p.caKey, user, hosts, o.Days, cn)
	}
	if err != nil {
		return err
	}
	role := "server"
	if user != "" {
		role = filepath.Join("users", user)
	}
	certPath, keyPath := p.generation(role, m)
	p.server.TLS.CACertificate = p.caPath
	if user == "" {
		p.server.TLS.Certificate, p.server.TLS.PrivateKey = certPath, keyPath
		p.serverLeaf = m.Leaf
	} else {
		if p.index < 0 {
			p.server.AuthorizedClients = append(p.server.AuthorizedClients, config.AuthorizedClient{
				UserID:      user,
				Certificate: certPath,
				Permissions: []string{"upload", "download"},
			})
			p.index = len(p.server.AuthorizedClients) - 1
		} else {
			p.server.AuthorizedClients[p.index].Certificate = certPath
		}
		if o.Enable {
			p.server.AuthorizedClients[p.index].Disabled = false
		}
		p.server.AuthorizedClients[p.index].ExternalKey = o.CSR != ""
		p.syncUsers = o.CSR == ""
		if o.CSR != "" {
			p.messages = append(p.messages, "CSR 已签发；将证书复制给持有原私钥的客户端："+certPath)
		}
	}
	p.messages = append(p.messages, fmt.Sprintf("%s 证书已生成，到期：%s", role, m.Leaf.NotAfter.Format(time.RFC3339)))
	return nil
}

func statusReport(c config.Server, name string) []string {
	rows := []string{"对象\t授权\t证书状态\t到期时间\tSHA-256 指纹"}
	now := time.Now()
	add := func(label, auth, path string) {
		ins := security.InspectCertificate(path, now)
		if ins.Error != nil {
			rows = append(rows, fmt.Sprintf("%s\t%s\t缺失或无效：%v", label, auth, ins.Error))
			return
		}
		rows = append(rows, fmt.Sprintf("%s\t%s\t%s\t%s\t%s", label, auth, ins.State, ins.Certificate.NotAfter.Format(time.RFC3339), ins.Fingerprint))
	}
	add("CA", "信任锚", c.TLS.CACertificate)
	add("server", "服务端", c.TLS.Certificate)
	for _, u := range c.AuthorizedClients {
		if name != "" && name != u.UserID {
			continue
		}
		auth := "已授权"
		if u.Disabled {
			auth = "已吊销"
		}
		add(u.UserID, auth, u.Certificate)
	}
	return rows
}
