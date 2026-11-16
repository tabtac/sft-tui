//go:build windows

package console

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsExplorerStandardHandles(t *testing.T) {
	const reportKey = "SFT_EXPLORER_CONSOLE_REPORT"
	if reportPath := os.Getenv(reportKey); reportPath != "" {
		report, err := os.Create(reportPath)
		if err != nil {
			t.Fatal(err)
		}
		defer report.Close()
		logf := func(format string, args ...any) {
			fmt.Fprintf(report, format+"\n", args...)
			_ = report.Sync()
		}
		args := os.Args
		os.Args = args[:1]
		defer func() { os.Args = args }()
		logf("before Auto")
		if !Auto() {
			logf("Auto=false")
			t.Fatal("Explorer console was not detected")
		}
		inputCP, _, _ := kernel.NewProc("GetConsoleCP").Call()
		outputCP, _, _ := kernel.NewProc("GetConsoleOutputCP").Call()
		logf("before Run")
		code := Run(func(s *Session) int {
			logf("entered interactive callback")
			currentInputCP, _, _ := kernel.NewProc("GetConsoleCP").Call()
			currentOutputCP, _, _ := kernel.NewProc("GetConsoleOutputCP").Call()
			if currentInputCP != inputCP || currentOutputCP != outputCP {
				t.Error("interactive startup changed console code pages")
			}
			if _, err := fmt.Fprintln(s.Out, "控制台启动回归 / Console startup regression"); err != nil {
				t.Error(err)
			}
			s.In = bufio.NewReader(strings.NewReader("\n"))
			return 0
		})
		if code != 0 {
			t.Fatal("interactive startup failed")
		}
		return
	}
	reportPath := filepath.Join(t.TempDir(), "console.log")
	t.Setenv(reportKey, reportPath)
	command, err := windows.UTF16PtrFromString(syscall.EscapeArg(os.Args[0]) + " -test.run=^TestWindowsExplorerStandardHandles$")
	if err != nil {
		t.Fatal(err)
	}
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var process windows.ProcessInformation
	if err := windows.CreateProcess(nil, command, nil, nil, false, windows.CREATE_NEW_CONSOLE, nil, nil, &startup, &process); err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(process.Process)
	defer windows.CloseHandle(process.Thread)
	status, err := windows.WaitForSingleObject(process.Process, 20000)
	if err != nil || status != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(process.Process, 1)
		_, _ = windows.WaitForSingleObject(process.Process, 5000)
		report, _ := os.ReadFile(reportPath)
		t.Fatalf("Explorer console startup blocked: wait=%d err=%v\n%s", status, err, report)
	}
	var exit uint32
	if err := windows.GetExitCodeProcess(process.Process, &exit); err != nil || exit != 0 {
		report, _ := os.ReadFile(reportPath)
		t.Fatalf("Explorer console startup failed: exit=%d err=%v\n%s", exit, err, report)
	}
}
