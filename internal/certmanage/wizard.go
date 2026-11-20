package certmanage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"sft-tui/internal/config"
	"sft-tui/internal/prompt"
	"sft-tui/internal/security"
)

func validWizardUser(s string) error {
	if !config.ValidUserPath(s) || s == "ca" || s == "server" {
		return fmt.Errorf("用户名须为 1–64 个字母、数字、下划线或连字符，且不能为保留名称")
	}
	return nil
}

func certificateName(s string) error {
	if s == "" || len(s) > 128 || strings.ContainsAny(s, "\r\n\x00") {
		return fmt.Errorf("证书名称须为 1–128 字节且不能含控制换行符")
	}
	return nil
}

// InitializeDialog 交互式收集初始化参数；输入中断时不会落盘任何部分配置或私钥。
func InitializeDialog(in io.Reader, out io.Writer, o Options) (Options, error) {
	return initializeDialog(in, out, o, true)
}

// InitializeDefaultDialog 保留默认部署目录固定路径，仅收集证书名称、SAN 与初期用户。
func InitializeDefaultDialog(in io.Reader, out io.Writer, o Options) (Options, error) {
	return initializeDialog(in, out, o, false)
}

func initializeDialog(in io.Reader, out io.Writer, o Options, choosePaths bool) (Options, error) {
	d := prompt.New(in, out)
	var err error
	if choosePaths {
		for _, field := range []struct {
			label string
			value *string
		}{
			{"证书保存目录", &o.Dir},
			{"服务端配置路径", &o.ServerConfig},
			{"用户配置输出目录", &o.ClientConfigDir},
		} {
			*field.value, err = d.Text(field.label, *field.value, prompt.Nonempty)
			if err != nil {
				return o, err
			}
		}
	}
	if o.CAName == "" {
		o.CAName = "SFT Local CA"
	}
	if o.ServerCommonName == "" {
		o.ServerCommonName = "SFT server"
	}
	if o.Hosts == "" {
		o.Hosts = "localhost,127.0.0.1,::1"
	}
	for _, field := range []struct {
		label string
		value *string
		check func(string) error
	}{
		{"CA 证书名称 (CN)", &o.CAName, certificateName},
		{"服务端证书名称 (CN)", &o.ServerCommonName, certificateName},
		{"服务端 SAN (DNS/IP，逗号分隔)", &o.Hosts, func(s string) error { _, _, err := security.ParseServerNames(s); return err }},
	} {
		*field.value, err = d.Text(field.label, *field.value, field.check)
		if err != nil {
			return o, err
		}
	}
	if o.CADays == 0 {
		o.CADays = 365
	}
	n, err := d.Uint("CA 有效天数", uint64(o.CADays), 1, 3650)
	if err != nil {
		return o, err
	}
	o.CADays = int(n)
	if o.Days == 0 {
		o.Days = 30
	}
	n, err = d.Uint("服务端与初始用户证书有效天数", uint64(o.Days), 1, 3650)
	if err != nil {
		return o, err
	}
	o.Days = int(n)
	if o.Name == "" {
		o.Name = "alice"
	}
	o.Name, err = d.Text("初始用户名（输入 - 跳过）", o.Name, func(s string) error {
		if s == "-" {
			return nil
		}
		return validWizardUser(s)
	})
	if o.Name == "-" {
		o.Name = ""
	}
	return o, err
}

// UsersDialog 提供命令行交互式用户管理菜单（新建、改策略、导出、续期、状态）。
func UsersDialog(in io.Reader, out io.Writer, o Options) (string, Options, error) {
	d := prompt.New(in, out)
	c, err := config.LoadServerDraft(o.ServerConfig)
	if err != nil {
		return "", o, err
	}
	o.Dir = filepath.Dir(c.TLS.CACertificate)
	choices := []string{"编辑用户权限、带宽与启用状态", "新建用户", "导出用户配置", "刷新用户证书", "查看用户状态"}
	if len(c.AuthorizedClients) == 0 {
		choices = []string{"新建用户"}
	}
	choice, err := d.Choose("选择管理操作", choices)
	if err != nil {
		return "", o, err
	}
	if len(c.AuthorizedClients) == 0 || choice == 1 {
		return createUserDialog(d, c, o)
	}
	index := -1
	for i, u := range c.AuthorizedClients {
		if u.UserID == o.Name {
			index = i
		}
	}
	if o.Name != "" && index < 0 {
		return "", o, fmt.Errorf("用户不存在：%s", o.Name)
	}
	if index < 0 {
		users := make([]string, len(c.AuthorizedClients))
		for i, u := range c.AuthorizedClients {
			users[i] = u.UserID
			if u.Disabled {
				users[i] += "（已禁用）"
			}
		}
		index, err = d.Choose("选择用户", users)
		if err != nil {
			return "", o, err
		}
	}
	u := c.AuthorizedClients[index]
	uCopy := u
	o.Name = u.UserID
	switch choice {
	case 0:
		o.ExpectedUser = &uCopy
		o.Policy, err = policyDialog(d, c, u)
		return "policy", o, err
	case 2:
		o.Out, err = d.Text("导出到新目录", o.Name+"-bundle", prompt.Nonempty)
		return "export", o, err
	case 3:
		o.ExpectedUser = &uCopy
		days := uint64(o.Days)
		if days == 0 {
			days = 30
		}
		n, err := d.Uint("新证书有效天数", days, 1, 3650)
		o.Days, o.Renew = int(n), true
		return "user", o, err
	default:
		return "status", o, nil
	}
}

// CreateUserDialog 交互式收集新建用户所需参数（当 -name 省略时调用）。
func CreateUserDialog(in io.Reader, out io.Writer, o Options) (string, Options, error) {
	c, err := config.LoadServerDraft(o.ServerConfig)
	if err != nil {
		return "", o, err
	}
	o.Dir = filepath.Dir(c.TLS.CACertificate)
	return createUserDialog(prompt.New(in, out), c, o)
}

func createUserDialog(d *prompt.Dialog, c config.Server, o Options) (string, Options, error) {
	var err error
	name := o.Name
	if name == "" {
		name = "alice"
		for n := 2; ; n++ {
			exists := false
			for _, u := range c.AuthorizedClients {
				exists = exists || strings.EqualFold(u.UserID, name)
			}
			if !exists {
				break
			}
			name = fmt.Sprintf("user%d", n)
		}
	}
	o.Name, err = d.Text("新用户名", name, func(s string) error {
		if err := validWizardUser(s); err != nil {
			return err
		}
		for _, u := range c.AuthorizedClients {
			if u.UserID == s || (os.PathSeparator == '\\' && strings.EqualFold(u.UserID, s)) {
				return fmt.Errorf("用户已存在")
			}
		}
		return nil
	})
	if err != nil {
		return "", o, err
	}
	days := uint64(o.Days)
	if days == 0 {
		days = 30
	}
	n, err := d.Uint("用户证书有效天数", days, 1, 3650)
	if err != nil {
		return "", o, err
	}
	o.Days = int(n)
	defaults := config.DefaultClient()
	if leaf, e := security.ReadCertificate(c.TLS.Certificate); e == nil {
		if len(leaf.DNSNames) > 0 && !strings.Contains(leaf.DNSNames[0], "*") {
			defaults.ServerName = leaf.DNSNames[0]
		} else if len(leaf.IPAddresses) > 0 {
			defaults.ServerName = leaf.IPAddresses[0].String()
		}
	}
	if o.Address == "" {
		o.Address, err = config.ClientAddressFromListen(c.Listen)
		if err != nil {
			return "", o, err
		}
	}
	o.Address, err = d.Text("客户端连接地址（主机:端口）", o.Address, func(s string) error {
		_, _, err := security.ValidateConnectAddress(s)
		return err
	})
	if err != nil {
		return "", o, err
	}
	if o.ServerName == "" {
		o.ServerName = defaults.ServerName
	}
	o.ServerName, err = d.Text("客户端校验的服务端 SAN 名称", o.ServerName, func(s string) error {
		leaf, err := security.ReadCertificate(c.TLS.Certificate)
		if err != nil {
			return err
		}
		return leaf.VerifyHostname(s)
	})
	if err != nil {
		return "", o, err
	}
	o.Policy, err = policyDialog(d, c, config.AuthorizedClient{Permissions: []string{"upload", "download"}})
	return "user", o, err
}

func policyDialog(d *prompt.Dialog, c config.Server, u config.AuthorizedClient) (*UserPolicy, error) {
	p := &UserPolicy{Permissions: []string{}}
	enabled, err := d.Bool("启用用户访问", !u.Disabled)
	if err != nil {
		return nil, err
	}
	p.Disabled = !enabled
	for _, permission := range []string{"upload", "download"} {
		allowed := u.Permissions == nil
		for _, old := range u.Permissions {
			allowed = allowed || old == permission
		}
		allowed, err = d.Bool("允许 "+permission, allowed)
		if err != nil {
			return nil, err
		}
		if allowed {
			p.Permissions = append(p.Permissions, permission)
		}
	}
	inherit, err := d.Bool("继承全局带宽（n 可单独设置上下行）", u.Bandwidth == nil)
	if err != nil || inherit {
		return p, err
	}
	b := c.Bandwidth
	if u.Bandwidth != nil {
		b = *u.Bandwidth
	}
	for _, field := range []struct {
		label     string
		value     *uint64
		low, high uint64
	}{
		{"上传带宽（字节/秒）", &b.UploadBytesPerSecond, 1, 1 << 53},
		{"下载带宽（字节/秒）", &b.DownloadBytesPerSecond, 1, 1 << 53},
		{"突发字节数", &b.BurstBytes, uint64(c.Limits.ChunkSize), 1 << 30},
	} {
		*field.value, err = d.Uint(field.label, *field.value, field.low, field.high)
		if err != nil {
			return nil, err
		}
	}
	p.Bandwidth = &b
	return p, nil
}
