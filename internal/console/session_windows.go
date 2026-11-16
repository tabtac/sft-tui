//go:build windows

package console

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var kernel = windows.NewLazySystemDLL("kernel32.dll")

// Auto 判断当前是否由资源管理器双击无参启动并独占新建的控制台窗口。
func Auto() bool {
	if len(os.Args) != 1 {
		return false
	}
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil &&
		windows.GetConsoleMode(windows.Handle(os.Stdout.Fd()), &mode) == nil && ownConsole()
}

// 已存在的 CMD / PowerShell 会与当前进程共享控制台，仅当进程独占新控制台时启用双击工作流。
func ownConsole() bool {
	var processes [2]uint32
	n, _, _ := kernel.NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&processes[0])), uintptr(len(processes)))
	return n == 1
}

func prepare() (func(), bool) {
	// Go 标准库对真实控制台句柄已直接调用 ReadConsoleW/WriteConsoleW，切勿强制 CP65001 避免中文提示失效。
	return func() {}, ownConsole()
}
