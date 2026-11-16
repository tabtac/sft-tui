package main

import (
	"flag"
	"fmt"
	"io"

	"sft-tui/internal/certmanage"
	"sft-tui/internal/control"
)

func runCertWithInput(args []string, in io.Reader, out, errOut io.Writer) int {
	usage := func() {
		fmt.Fprintln(errOut, `用法：sftd cert 操作 [选项]
  init                          生成 CA、服务端证书与初始用户授权配置
  users / manage [-name USER]   交互式新建、编辑权限/带宽、导出、续期或查看用户
  ca                            创建或续期（-renew）CA
  server                        签发或刷新服务端证书及私钥
  user [-name USER]             签发或续期（-renew）用户证书（支持 -csr FILE）
  revoke -name USER             吊销用户访问权限并断开已有连接
  status [-name USER]           查看 CA/服务端/用户证书状态与 SHA-256 指纹
  sync [-name USER]             同步客户端配置文件并通知服务端热加载
  export -name USER -out DIR    导出客户端分发包（-public-only 仅导出公开证书）`)
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		usage()
		return 0
	}
	action := args[0]
	switch action {
	case "init", "users", "manage", "ca", "server", "user", "revoke", "status", "sync", "export":
	default:
		usage()
		return 2
	}

	flags := flag.NewFlagSet("sftd cert "+action, flag.ContinueOnError)
	flags.SetOutput(errOut)
	var o certmanage.Options
	flags.StringVar(&o.Dir, "dir", "certs", "CA 与证书存储目录")
	flags.StringVar(&o.ServerConfig, "server-config", "configs/server.local.json", "服务端 JSON 配置路径")
	flags.StringVar(&o.ClientConfigDir, "client-config-dir", "configs", "客户端配置文件输出目录")
	flags.StringVar(&o.Name, "name", "", "用户名")
	flags.StringVar(&o.Hosts, "hosts", "", "服务端 SAN，逗号分隔的 DNS 或 IP")
	flags.StringVar(&o.Address, "server-address", "", "写入客户端配置的连接地址（主机:端口）")
	flags.StringVar(&o.ServerName, "server-name", "", "客户端校验的服务端 SAN 名称")
	flags.StringVar(&o.Out, "out", "", "export 导出目录")
	flags.StringVar(&o.CSR, "csr", "", "user 签发外部客户端生成的 PEM CSR 文件")
	flags.BoolVar(&o.CertificateOnly, "public-only", false, "export 仅导出公开证书，不读取用户私钥")
	flags.StringVar(&o.CAName, "ca-name", "SFT Local CA", "init 时的 CA 通用名")
	flags.StringVar(&o.ServerCommonName, "cert-name", "SFT server", "init 时的服务端通用名")
	flags.IntVar(&o.CADays, "ca-days", 365, "init 时的 CA 有效天数")
	defaultDays := 30
	if action == "ca" {
		defaultDays = 365
	}
	flags.IntVar(&o.Days, "days", defaultDays, "证书有效天数（1–3650）")
	flags.BoolVar(&o.Renew, "renew", false, "续期 CA 或刷新已有服务端/用户证书")
	flags.BoolVar(&o.Enable, "enable", false, "user -renew 时重新启用已被吊销的用户")
	reload := flags.Bool("reload", true, "配置更新后自动通知本机运行中的 sftd 热加载")
	flags.Usage = func() {
		usage()
		flags.PrintDefaults()
	}

	if err := flags.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	if o.CertificateOnly && action != "export" ||
		o.CSR != "" && action != "user" ||
		o.Enable && action != "user" ||
		o.Hosts != "" && action != "server" && action != "init" ||
		o.Renew && action != "ca" && action != "user" && action != "server" {
		fmt.Fprintln(errOut, "sftd cert：参数与操作不匹配")
		return 2
	}

	visited := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	var err error
	switch {
	case action == "init" && in != nil && !visited["name"] && !visited["hosts"] && !visited["server-address"]:
		o, err = certmanage.InitializeDialog(in, out, o)
	case action == "users" || action == "manage":
		if in == nil {
			err = fmt.Errorf("%s 需要交互式终端输入", action)
		} else {
			action, o, err = certmanage.UsersDialog(in, out, o)
		}
	case action == "user" && o.Name == "" && in != nil:
		action, o, err = certmanage.CreateUserDialog(in, out, o)
	}
	if err != nil {
		fmt.Fprintln(errOut, "sftd cert：", err)
		return 1
	}

	res, err := certmanage.Execute(action, o)
	if err != nil {
		fmt.Fprintln(errOut, "sftd cert：", err)
		return 1
	}
	for _, msg := range res.Messages {
		fmt.Fprintln(out, msg)
	}
	if res.Changed && *reload {
		applied, err := control.Reload(o.ServerConfig, res.Server.StorageRoot)
		if err != nil {
			fmt.Fprintln(errOut, "配置已保存，但服务端热重载失败（可执行 sftd cert sync 重试）：", err)
			return 1
		}
		if applied {
			fmt.Fprintln(out, "运行中的服务端已热加载新证书与授权策略")
		} else {
			fmt.Fprintln(out, "配置已保存；服务端启动后生效")
		}
	}
	return 0
}
