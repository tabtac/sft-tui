package main

import (
	"fmt"
	"os"

	"sft-tui/internal/buildinfo"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "-v") {
		fmt.Printf("sft (Secure File Transfer Client) %s\n", buildinfo.Version)
		return
	}
	fmt.Printf("sft %s - 安全文件传输协议客户端\n", buildinfo.Version)
	fmt.Println("用法: sft [ui|upload|download|list|version]")
}
