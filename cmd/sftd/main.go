package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"sft-tui/internal/buildinfo"
	"sft-tui/internal/certmanage"
	"sft-tui/internal/config"
	"sft-tui/internal/console"
	"sft-tui/internal/control"
	"sft-tui/internal/logfile"
	"sft-tui/internal/monitor"
	"sft-tui/internal/service"
	"sft-tui/internal/setup"
	"sft-tui/internal/transfer"
	"sft-tui/internal/tui"
)

func main() {
	if console.Auto() {
		os.Exit(console.Run(func(s *console.Session) int {
			return runUI(nil, 0, s.Out, s.Out)
		}))
	}
	os.Exit(runCommandWithInput(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func runCommand(args []string, out, errOut io.Writer) int {
	return runCommandWithInput(args, nil, out, errOut)
}

func runCommandWithInput(args []string, in io.Reader, out, errOut io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "ui":
			return runUI(args[1:], 0, out, errOut)
		case "users":
			return runUI(args[1:], 1, out, errOut)
		case "init":
			return runInitWithInput(args[1:], in, out, errOut)
		case "cert":
			return runCertWithInput(args[1:], in, out, errOut)
		case "start":
			return runStart(args[1:], out, errOut)
		case "reload":
			return runControlAction("reload", args[1:], out, errOut)
		case "stop":
			return runControlAction("stop", args[1:], out, errOut)
		case "status":
			return runStatus(args[1:], out, errOut)
		case "logs":
			return runLogs(args[1:], out, errOut)
		case "serve":
			args = args[1:]
		}
	}
	return runServe(args, out, errOut)
}

func runUI(args []string, initialPage int, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("sftd ui", flag.ContinueOnError)
	flags.SetOutput(errOut)
	cfgPath := flags.String("config", "configs/server.local.json", "服务端配置文件")
	logPath := flags.String("log", "", "可选查看指定日志文件尾部（省略时查询运行服务结构化日志）")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	if f, ok := out.(*os.File); !ok || (!isatty.IsTerminal(f.Fd()) && !isatty.IsCygwinTerminal(f.Fd())) {
		fmt.Fprintln(errOut, "sftd ui：需要在交互式终端中运行")
		return 1
	}
	exeDir := "."
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	if err := tui.RunServer(tui.ServerOptions{
		ExecutableDir: exeDir,
		ConfigPath:    *cfgPath,
		LogPath:       *logPath,
		Page:          initialPage,
	}); err != nil {
		fmt.Fprintln(errOut, "sftd ui：", err)
		return 1
	}
	return 0
}

func runServe(args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("sftd", flag.ContinueOnError)
	flags.SetOutput(errOut)
	cfgPath := flags.String("config", "configs/server.local.json", "服务端 JSON 配置路径")
	version := flags.Bool("version", false, "显示版本")
	logLevel := flags.String("log-level", "info", "日志等级：debug、info、warn、error")
	logPath := flags.String("log-file", "", "可选有界服务日志文件路径（省略时输出标准错误）")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), `用法：sftd ui [-config FILE] [-log FILE]
      sftd users [-config FILE]
      sftd [serve] [-config FILE] [-log-level LEVEL] [-log-file FILE]
      sftd init [-config FILE] [-dir DIR] [-name USER]
      sftd cert <init|ca|server|user|revoke|status|sync|export> [选项]
      sftd start|status|logs|reload|stop [-config FILE]`)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *version && flags.NArg() == 0 {
		fmt.Fprintf(out, "sftd %s\n", buildinfo.Version)
		return 0
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(errOut, "sftd：无效的日志等级：", *logLevel)
		return 2
	}

	logs := &monitor.Logs{}
	logger := slog.New(logs.Handler(slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: level})))
	prevDefault := slog.Default()
	defer slog.SetDefault(prevDefault)
	slog.SetDefault(logger)

	cfg, err := config.LoadServer(*cfgPath)
	if err != nil {
		slog.Error("加载服务端配置失败", "error", err)
		return 1
	}
	if *logPath != "" {
		w, err := logfile.Open(*logPath, int64(cfg.Logging.MaxFileBytes))
		if err != nil {
			logger.Error("无法打开有界日志文件", "error", err)
			return 1
		}
		defer w.Close()
		logger = slog.New(logs.Handler(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})))
		slog.SetDefault(logger)
	}

	root, err := control.Locate(*cfgPath, cfg.StorageRoot)
	if err == nil {
		var running bool
		running, err = control.Probe(*cfgPath, root)
		if running {
			err = fmt.Errorf("同一配置已有运行中的服务端实例")
		}
	}
	if err != nil {
		logger.Error("无法启动服务", "error", err)
		return 1
	}

	daemon, err := transfer.NewDaemonWithLogger(cfg, logger)
	if err != nil {
		slog.Error("初始化服务端失败", "error", err)
		return 1
	}
	defer daemon.Close()

	reloadConfig := func() error {
		updated, err := config.LoadServer(*cfgPath)
		if err == nil {
			err = daemon.ApplySaved(updated)
		}
		if err != nil {
			logger.Error("配置热更新失败，保持原策略", "error", err)
		} else {
			logger.Info("证书、用户 ACL 及限速策略已热更新")
		}
		return err
	}

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		slog.Error("监听 TLS 端口失败", "listen", cfg.Listen, "error", err)
		return 1
	}
	defer listener.Close()

	ctrl, err := control.StartManaged(*cfgPath, cfg.StorageRoot, control.Handlers{
		Reload:   reloadConfig,
		Snapshot: daemon.Snapshot,
		Logs:     logs.Query,
		Stop:     daemon.Stop,
	})
	if err != nil {
		slog.Error("启动本地控制端点失败", "error", err)
		return 1
	}
	defer ctrl.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reloadCh := make(chan os.Signal, 1)
	if runtime.GOOS != "windows" {
		signal.Notify(reloadCh, syscall.SIGHUP)
		defer signal.Stop(reloadCh)
	}
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	go func() {
		forced := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-sigCh:
				daemon.Stop(forced)
				forced = true
			}
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloadCh:
				_ = reloadConfig()
			}
		}
	}()

	logger.Info(fmt.Sprintf("sftd %s 正在监听 %s", buildinfo.Version, listener.Addr()), "config", *cfgPath, "storage", cfg.StorageRoot)
	if err := daemon.Serve(ctx, listener); err != nil {
		slog.Error("服务端运行异常退出", "error", err)
		return 1
	}
	logger.Info("服务端已优雅停止")
	return 0
}

func runInitWithInput(args []string, in io.Reader, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("sftd init", flag.ContinueOnError)
	flags.SetOutput(errOut)
	var o certmanage.Options
	flags.StringVar(&o.ServerConfig, "config", "configs/server.local.json", "生成或调整的服务端配置路径")
	flags.StringVar(&o.Dir, "dir", "certs", "证书输出或扫描目录")
	flags.StringVar(&o.ClientConfigDir, "client-config-dir", "", "客户端配置输出目录（默认为服务端配置同目录）")
	flags.StringVar(&o.Name, "name", "alice", "初始授权用户名")
	flags.StringVar(&o.Hosts, "hosts", "localhost,127.0.0.1,::1", "服务端证书 SAN 列表")
	flags.StringVar(&o.Address, "server-address", "", "写入客户端配置的服务端地址")
	flags.IntVar(&o.CADays, "ca-days", 365, "CA 有效天数")
	flags.IntVar(&o.Days, "days", 30, "服务端及用户证书有效天数")
	yes := flags.Bool("y", false, "非交互式直接按参数或默认值初始化")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	visited := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	interactive := in != nil && !*yes && !visited["name"] && !visited["hosts"] && !visited["server-address"] && !visited["ca-days"] && !visited["days"]
	if interactive {
		reader, ok := in.(*bufio.Reader)
		if !ok {
			reader = bufio.NewReader(in)
		}
		scanDir := "."
		if visited["dir"] {
			scanDir = o.Dir
		}
		if _, err := os.Stat(o.ServerConfig); os.IsNotExist(err) {
			var matches []config.TLSFiles
			if _, statErr := os.Stat(scanDir); statErr == nil {
				matches, err = setup.ScanServerTLS(scanDir)
				if err != nil {
					fmt.Fprintln(errOut, "sftd init：", err)
					return 1
				}
			} else if !os.IsNotExist(statErr) {
				fmt.Fprintln(errOut, "sftd init：", statErr)
				return 1
			}
			if len(matches) == 0 {
				if o.ClientConfigDir == "" {
					o.ClientConfigDir = filepath.Dir(o.ServerConfig)
				}
				o, err = certmanage.InitializeDefaultDialog(reader, out, o)
				if err != nil {
					fmt.Fprintln(errOut, "sftd init：", err)
					return 1
				}
				res, err := certmanage.Execute("init", o)
				if err != nil {
					fmt.Fprintln(errOut, "sftd init：", err)
					return 1
				}
				for _, msg := range res.Messages {
					fmt.Fprintln(out, msg)
				}
				fmt.Fprintln(out, "初始化完成；可运行 sftd ui 或 sftd start 启动管理与服务，sftd cert export 导出用户包。")
				return 0
			}
		}
		if err := setup.Server(reader, out, o.ServerConfig, scanDir); err != nil {
			fmt.Fprintln(errOut, "sftd init：", err)
			return 1
		}
		return 0
	}

	res, err := certmanage.Execute("init", o)
	if err != nil {
		fmt.Fprintln(errOut, "sftd init：", err)
		return 1
	}
	for _, msg := range res.Messages {
		fmt.Fprintln(out, msg)
	}
	return 0
}

func runStart(args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("sftd start", flag.ContinueOnError)
	flags.SetOutput(errOut)
	cfgPath := flags.String("config", "configs/server.local.json", "服务端配置文件")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(errOut, "sftd start：", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	st, err := service.Start(ctx, exe, *cfgPath)
	if err != nil {
		fmt.Fprintln(errOut, "sftd start：", err)
		return 1
	}
	if st.AlreadyRunning {
		fmt.Fprintln(out, "sftd 已在运行中")
	} else {
		fmt.Fprintf(out, "sftd 已在后台启动（PID %d），日志：%s\n", st.PID, st.Log)
	}
	return 0
}

func runControlAction(action string, args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("sftd "+action, flag.ContinueOnError)
	flags.SetOutput(errOut)
	cfgPath := flags.String("config", "configs/server.local.json", "服务端配置文件")
	var force *bool
	if action == "stop" {
		force = flags.Bool("force", false, "立即强制断开活动传输")
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	fallback := "var/storage"
	if cfg, err := config.LoadServerDraft(*cfgPath); err == nil {
		fallback = cfg.StorageRoot
	}
	var running bool
	var err error
	if action == "reload" {
		running, err = control.Reload(*cfgPath, fallback)
	} else {
		running, err = control.Stop(*cfgPath, fallback, force != nil && *force)
	}
	if err != nil {
		fmt.Fprintf(errOut, "sftd %s：%v\n", action, err)
		return 1
	}
	if !running {
		fmt.Fprintln(errOut, "未检测到运行中的 sftd 实例")
		return 1
	}
	if action == "reload" {
		fmt.Fprintln(out, "已触发 sftd 配置热重载")
	} else {
		fmt.Fprintln(out, "已向 sftd 发送停止指令")
	}
	return 0
}

func runStatus(args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("sftd status", flag.ContinueOnError)
	flags.SetOutput(errOut)
	cfgPath := flags.String("config", "configs/server.local.json", "服务端配置文件")
	asJSON := flags.Bool("json", false, "以 JSON 格式输出监控快照")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	fallback := "var/storage"
	if cfg, err := config.LoadServerDraft(*cfgPath); err == nil {
		fallback = cfg.StorageRoot
	}
	snap, err := control.Status(*cfgPath, fallback)
	if err != nil {
		fmt.Fprintln(errOut, "sftd status：", err)
		return 1
	}
	if snap == nil {
		fmt.Fprintln(errOut, "未检测到运行中的 sftd 实例")
		return 1
	}
	if *asJSON {
		data, _ := json.MarshalIndent(snap, "", "  ")
		fmt.Fprintln(out, string(data))
		return 0
	}
	fmt.Fprintf(out, "状态: %s  监听: %s  连接: %d  在线用户: %d  活动任务: %d  已完成: %d  失败: %d\n",
		snap.State, snap.Listen, snap.Connections, snap.OnlineUsers, snap.ActiveTasks, snap.Completed, snap.Failed)
	return 0
}

func runLogs(args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("sftd logs", flag.ContinueOnError)
	flags.SetOutput(errOut)
	cfgPath := flags.String("config", "configs/server.local.json", "服务端配置文件")
	after := flags.Uint64("after", 0, "仅显示序号大于此值的日志")
	level := flags.String("level", "", "按等级过滤")
	user := flags.String("user", "", "按用户过滤")
	file := flags.String("file", "", "按文件名关键字过滤")
	text := flags.String("text", "", "按正文关键字过滤")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	fallback := "var/storage"
	if cfg, err := config.LoadServerDraft(*cfgPath); err == nil {
		fallback = cfg.StorageRoot
	}
	page, err := control.QueryLogs(*cfgPath, fallback, monitor.LogQuery{
		After: *after,
		Level: *level,
		User:  *user,
		File:  *file,
		Text:  *text,
	})
	if err != nil {
		fmt.Fprintln(errOut, "sftd logs：", err)
		return 1
	}
	for _, ev := range page.Events {
		fmt.Fprintf(out, "#%d %s [%s] %s %s\n", ev.Sequence, ev.Time.Format(time.RFC3339), ev.Level, ev.Message, ev.Detail)
	}
	return 0
}
