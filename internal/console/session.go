// Package console 支持 Windows 双击独立控制台启动及退出保留窗口行为。
package console

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Session 描述双击启动时所持有的独立控制台输入/输出会话。
type Session struct {
	In  *bufio.Reader
	Out io.Writer
}

// Run 切换到可执行文件所在目录并执行交互入口；若当前独占独立控制台窗口，退出前等待回车确认。
func Run(run func(*Session) int) int {
	restore, own := prepare()
	defer restore()
	s := &Session{In: bufio.NewReader(os.Stdin), Out: os.Stdout}
	finish := func() {
		if own {
			fmt.Fprint(s.Out, "\n按回车键关闭窗口……")
			_, _ = s.In.ReadString('\n')
		}
	}
	defer finish()
	executable, err := os.Executable()
	if err == nil {
		err = os.Chdir(filepath.Dir(executable))
	}
	if err != nil {
		fmt.Fprintln(s.Out, "无法进入程序所在目录：", err)
		return 1
	}
	fmt.Fprintln(s.Out, "工作目录：", filepath.Dir(executable))
	return run(s)
}
