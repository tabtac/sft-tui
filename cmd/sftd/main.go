package main

import (
	"fmt"
	"os"

	"sft-tui/internal/buildinfo"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "-v") {
		fmt.Printf("sftd (Secure File Transfer Daemon) %s\n", buildinfo.Version)
		return
	}
	fmt.Printf("sftd %s - 安全文件传输协议服务端与管理工具\n", buildinfo.Version)
	fmt.Println("用法: sftd [ui|serve|init|cert|version]")
}
