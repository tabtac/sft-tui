package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"sft-tui/internal/buildinfo"
	"sft-tui/internal/config"
	"sft-tui/internal/identity"
	"sft-tui/internal/progress"
	"sft-tui/internal/protocol"
	"sft-tui/internal/queue"
	"sft-tui/internal/security"
	"sft-tui/internal/transfer"
	"sft-tui/internal/tui"
)

type cliJob struct {
	upload bool
	target string
}

func main() {
	os.Exit(runCommand(os.Args[1:], os.Stdout, os.Stderr))
}

func runCommand(args []string, out, errOut io.Writer) int {
	// 无参数且当前标准输入/输出为交互式终端时直接进入 TUI 界面
	if len(args) == 0 {
		if f, ok := out.(*os.File); ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())) {
			return runUI(nil, errOut)
		}
		printUsage(errOut)
		return 0
	}
	switch args[0] {
	case "ui":
		return runUI(args[1:], errOut)
	case "init":
		return runInit(args[1:], out, errOut)
	}

	flags := flag.NewFlagSet("sft", flag.ContinueOnError)
	flags.SetOutput(errOut)
	cfgPath := flags.String("config", "", "客户端 JSON 配置；省略时使用已记住身份或自动扫描")
	dirPath := flags.String("dir", "", "导入并激活该目录下的客户端配置")
	outDir := flags.String("out", "downloads", "下载输出目录（严禁覆盖已有文件）")
	parallel := flags.Int("parallel", 3, "并发传输任务数（1–64）")
	progressMode := flags.String("progress", "auto", "终端进度条模式：auto、always、never")
	version := flags.Bool("version", false, "显示版本信息")
	flags.Usage = func() {
		printUsage(flags.Output())
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *version && flags.NArg() == 0 {
		fmt.Fprintf(out, "sft %s\n", buildinfo.Version)
		return 0
	}

	pos := flags.Args()
	if len(pos) < 1 || *parallel < 1 || *parallel > 64 ||
		(*progressMode != "auto" && *progressMode != "always" && *progressMode != "never") {
		flags.Usage()
		return 2
	}

	op := pos[0]
	switch op {
	case "status", "clean-history", "list":
		if len(pos) != 1 {
			flags.Usage()
			return 2
		}
	case "pause", "abort":
		if len(pos) != 2 {
			flags.Usage()
			return 2
		}
	case "upload", "download", "transfer":
		if len(pos) < 2 {
			flags.Usage()
			return 2
		}
	default:
		flags.Usage()
		return 2
	}

	cfg, err := resolveIdentity(*cfgPath, *dirPath, false, out)
	if err != nil {
		fmt.Fprintln(errOut, "sft：", err)
		return 1
	}

	if op == "list" {
		return runList(cfg, out, errOut)
	}

	// 其余所有 CLI 操作均通过统一 SQLite 队列引擎（queue.Manager）驱动
	idKey, err := security.ClientIdentityKey(cfg)
	if err != nil {
		fmt.Fprintln(errOut, "sft：", err)
		return 1
	}
	streams := cfg.Limits.MaxStreams
	if uint16(*parallel) < streams {
		streams = uint16(*parallel)
	}
	mgr, err := queue.Open(queue.Config{
		StateDir:     cfg.StateDir,
		Identity:     idKey,
		MaxFileBytes: cfg.MaxFileBytes,
		MaxStreams:   streams,
	})
	if err != nil {
		fmt.Fprintln(errOut, "sft：", err)
		return 1
	}
	defer mgr.Close()

	switch op {
	case "status":
		return runStatus(mgr, out)
	case "clean-history":
		if err := mgr.ClearHistory(); err != nil {
			fmt.Fprintln(errOut, "sft：", err)
			return 1
		}
		fmt.Fprintln(out, "已清理历史归档记录")
		return 0
	case "pause", "abort":
		return runQueueControl(mgr, cfg, op, pos[1], out, errOut)
	default:
		jobs, err := parseJobs(op, pos[1:])
		if err != nil {
			fmt.Fprintln(errOut, "sft：", err)
			return 1
		}
		if op == "download" || op == "transfer" {
			for _, j := range jobs {
				if !j.upload {
					if mkErr := os.MkdirAll(*outDir, 0750); mkErr != nil {
						fmt.Fprintln(errOut, "sft：", mkErr)
						return 1
					}
					break
				}
			}
		}
		return runTransfers(mgr, cfg, jobs, *outDir, *progressMode, out, errOut)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `用法：sft [ui] [-config FILE | -dir DIR] [-local DIR]
      sft init [-dir DIR | -config FILE]
      sft [选项] upload FILE...
      sft [选项] download NAME...
      sft [选项] transfer put:FILE get:NAME ...
      sft [选项] list
      sft [选项] status | clean-history
      sft [选项] pause|abort TASK_ID`)
}

func runUI(args []string, errOut io.Writer) int {
	flags := flag.NewFlagSet("sft ui", flag.ContinueOnError)
	flags.SetOutput(errOut)
	cfgPath := flags.String("config", "", "客户端 JSON 配置文件路径")
	dirPath := flags.String("dir", "", "客户端导出包目录")
	localDir := flags.String("local", ".", "初始打开的本地目录")
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
	exeDir := "."
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	if err := tui.Run(tui.Options{
		ExecutableDir: exeDir,
		Directory:     *localDir,
		ConfigPath:    *cfgPath,
		BundleDir:     *dirPath,
	}); err != nil {
		fmt.Fprintln(errOut, "sft ui：", err)
		return 1
	}
	return 0
}

func runInit(args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("sft init", flag.ContinueOnError)
	flags.SetOutput(errOut)
	dir := flags.String("dir", "", "用户导出包目录（省略时自动扫描当前目录及程序目录）")
	cfgPath := flags.String("config", "", "客户端配置文件路径")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || (*dir != "" && *cfgPath != "") {
		flags.Usage()
		return 2
	}
	if _, err := resolveIdentity(*cfgPath, *dir, true, out); err != nil {
		fmt.Fprintln(errOut, "sft init：", err)
		return 1
	}
	return 0
}

func resolveIdentity(cfgPath, dirPath string, initMode bool, out io.Writer) (config.Client, error) {
	exeDir := "."
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	wd, err := os.Getwd()
	if err != nil {
		return config.Client{}, err
	}
	return identity.Resolve(exeDir, wd, dirPath, cfgPath, initMode, out)
}

func runList(cfg config.Client, out, errOut io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client, err := transfer.Dial(ctx, cfg)
	if err != nil {
		fmt.Fprintln(errOut, "sft：", err)
		return 1
	}
	defer client.Close()

	fmt.Fprintln(out, "大小(B)\t文件名")
	count := 0
	if err := client.ListFiles(ctx, func(f protocol.FileEntry) error {
		count++
		_, err := fmt.Fprintf(out, "%d\t%s\n", f.Size, f.FileName)
		return err
	}); err != nil {
		fmt.Fprintln(errOut, "sft：", err)
		return 1
	}
	if count == 0 {
		fmt.Fprintln(out, "（远端无文件）")
	}
	return 0
}

func runStatus(mgr *queue.Manager, out io.Writer) int {
	tasks := mgr.Snapshot(false)
	var unfinished []queue.Task
	for _, t := range tasks {
		if !t.Terminal() {
			unfinished = append(unfinished, t)
		}
	}
	if len(unfinished) == 0 {
		fmt.Fprintln(out, "没有待恢复的传输任务")
		return 0
	}
	for _, t := range unfinished {
		dir := "upload"
		if t.Direction == protocol.DirectionDownload {
			dir = "download"
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%d/%d\ttransfer=%s\n", t.ID[:min(12, len(t.ID))], t.State, dir, t.Remote, t.Done, t.Size, t.TransferID)
	}
	return 0
}

func runQueueControl(mgr *queue.Manager, cfg config.Client, op, prefix string, out, errOut io.Writer) int {
	var matched []queue.Task
	for _, t := range mgr.Snapshot(false) {
		if !t.Terminal() && strings.HasPrefix(t.ID, prefix) {
			matched = append(matched, t)
		}
	}
	if len(matched) == 0 {
		fmt.Fprintf(errOut, "sft：未找到匹配的未完成任务：%s\n", prefix)
		return 1
	}
	if len(matched) > 1 {
		fmt.Fprintf(errOut, "sft：任务前缀不唯一（匹配 %d 项）：%s\n", len(matched), prefix)
		return 1
	}
	t := matched[0]
	if op == "pause" {
		if err := mgr.Pause(t.ID); err != nil {
			fmt.Fprintln(errOut, "sft：", err)
			return 1
		}
		fmt.Fprintf(out, "已暂停 %s (%s)\n", t.ID, t.Remote)
		return 0
	}

	runner := transfer.NewQueueRunner(cfg, mgr)
	defer runner.Close()
	mgr.SetRunner(runner.Run)
	if err := mgr.Cancel(t.ID); err != nil {
		fmt.Fprintln(errOut, "sft：", err)
		return 1
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		cur := mgr.Task(t.ID)
		if cur == nil || cur.State == queue.Cancelled {
			fmt.Fprintf(out, "已取消并清理 %s (%s)\n", t.ID, t.Remote)
			return 0
		}
		if cur.State == queue.Pending && cur.Error != "" {
			fmt.Fprintf(errOut, "sft：取消失败：%s\n", cur.Error)
			return 1
		}
		time.Sleep(25 * time.Millisecond)
	}
	fmt.Fprintln(errOut, "sft：取消等待超时")
	return 1
}

func runTransfers(mgr *queue.Manager, cfg config.Client, jobs []cliJob, outDir, progressMode string, out, errOut io.Writer) int {
	display, err := progress.New(progressMode, errOut, out)
	if err != nil {
		fmt.Fprintln(errOut, "sft：", err)
		return 1
	}
	defer display.Close()

	runner := transfer.NewQueueRunner(cfg, mgr)
	defer runner.Close()
	mgr.SetRunner(runner.Run)

	// 映射：检查是否已有同目标的未完成 SQLite 检查点可断点恢复
	existingByKey := make(map[string]queue.Task)
	for _, old := range mgr.Snapshot(false) {
		if !old.Terminal() {
			existingByKey[queue.TargetKey(old.Direction, old.Remote, old.Path)] = old
		}
	}

	type trackedJob struct {
		idx   int
		id    string
		dir   string
		label string
	}
	var tracked []trackedJob
	var toEnqueue []queue.Spec
	var enqueueJobIdx []int

	for idx, j := range jobs {
		var dir protocol.Direction
		var dirLabel, remote, localPath string
		if j.upload {
			dir = protocol.DirectionUpload
			dirLabel = "upload"
			abs, err := filepath.Abs(j.target)
			if err != nil {
				fmt.Fprintln(errOut, "sft：", err)
				return 1
			}
			localPath = abs
			remote = filepath.Base(abs)
		} else {
			dir = protocol.DirectionDownload
			dirLabel = "download"
			remote = j.target
			abs, err := filepath.Abs(filepath.Join(outDir, j.target))
			if err != nil {
				fmt.Fprintln(errOut, "sft：", err)
				return 1
			}
			localPath = abs
		}
		key := queue.TargetKey(dir, remote, localPath)
		if old, ok := existingByKey[key]; ok {
			if err := mgr.Resume(old.ID); err != nil {
				fmt.Fprintf(errOut, "sft：恢复任务 %s 失败：%v\n", remote, err)
				return 1
			}
			display.Start(idx, dirLabel+" "+remote)
			tracked = append(tracked, trackedJob{idx: idx, id: old.ID, dir: dirLabel, label: remote})
		} else {
			toEnqueue = append(toEnqueue, queue.Spec{Direction: dir, Remote: remote, Path: localPath})
			enqueueJobIdx = append(enqueueJobIdx, idx)
		}
	}

	if len(toEnqueue) > 0 {
		enqueued, err := mgr.Enqueue(toEnqueue)
		if err != nil {
			fmt.Fprintln(errOut, "sft：", err)
			return 1
		}
		for i, t := range enqueued {
			dirLabel := "download"
			if t.Direction == protocol.DirectionUpload {
				dirLabel = "upload"
			}
			idx := enqueueJobIdx[i]
			display.Start(idx, dirLabel+" "+t.Remote)
			tracked = append(tracked, trackedJob{idx: idx, id: t.ID, dir: dirLabel, label: t.Remote})
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	finished := make(map[string]bool, len(tracked))
	failed := false
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()

	for len(finished) < len(tracked) {
		select {
		case <-ctx.Done():
			pauseCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			_ = mgr.PauseAll(pauseCtx)
			cancel()
			return 1
		case <-ticker.C:
			for _, tr := range tracked {
				if finished[tr.id] {
					continue
				}
				cur := mgr.Task(tr.id)
				if cur == nil {
					continue
				}
				display.Update(tr.idx, cur.Done, cur.Size, cur.State)
				switch cur.State {
				case queue.Completed:
					finished[tr.id] = true
					display.Finish(tr.idx, fmt.Sprintf("%s 完成：%s (%s, SHA-256 %s)", tr.dir, cur.Remote, progress.FormatBytes(cur.Size), cur.SHA256), false)
				case queue.Pending, queue.Cancelled, queue.Skipped:
					finished[tr.id] = true
					failed = true
					display.Finish(tr.idx, fmt.Sprintf("%s 失败：%s (%s)", tr.dir, cur.Remote, cur.Error), true)
				}
			}
		}
	}
	if failed {
		return 1
	}
	return 0
}

func parseJobs(action string, args []string) ([]cliJob, error) {
	var jobs []cliJob
	for _, arg := range args {
		var j cliJob
		switch action {
		case "upload":
			j = cliJob{upload: true, target: arg}
		case "download":
			j = cliJob{upload: false, target: arg}
		case "transfer":
			switch {
			case strings.HasPrefix(arg, "put:"):
				j = cliJob{upload: true, target: strings.TrimPrefix(arg, "put:")}
			case strings.HasPrefix(arg, "get:"):
				j = cliJob{upload: false, target: strings.TrimPrefix(arg, "get:")}
			default:
				return nil, errors.New("transfer 参数必须以 put: 或 get: 开头")
			}
		}
		if j.target == "" {
			return nil, errors.New("文件参数不能为空")
		}
		if err := protocol.ValidateName(filepath.Base(j.target)); !j.upload && err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}
