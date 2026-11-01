//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVT 为 Windows 控制台开启 ANSI 转义解析。
// 旧版 conhost 默认不解析 ANSI，直接打印 ESC 会显示成乱码；
// SetConsoleMode 失败（如重定向到文件/管道）即视为非终端。
func enableVT(f *os.File) bool {
	handle := windows.Handle(f.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	if err := windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return false
	}
	return true
}
