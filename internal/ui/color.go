// Package ui 汇集终端渲染原语：ANSI 颜色、显示宽度、排版助手。
//
// 从 package main 抽出（根目录瘦身，MS 重构）。零业务依赖——只依赖标准库/os
// 终端探测，是叶子包，供 report/feishu/progress 复用而不产生环。
package ui

import (
	"os"
	"sync"
)

// —— ANSI 颜色（零依赖）。非 TTY 或 NO_COLOR 时自动降级为纯文本，
// 保证管道/重定向/CI 日志里不出现控制字符。 ——
type Color int

const (
	_ Color = iota // 占位，保证后续色值从 1 开始
	Dim
	Bold
	Red
	Green
	Yellow
	Cyan
)

var (
	useColorOnce       = sync.OnceValue(func() bool { return DetectColor(os.Stdout) })
	useColorStderrOnce = sync.OnceValue(func() bool { return DetectColor(os.Stderr) })
)

func UseColor() bool       { return useColorOnce() }
func UseColorStderr() bool { return useColorStderrOnce() }

// DetectColor 判定该流是否支持颜色，遵循业界通行的环境变量约定：
// NO_COLOR / TERM=dumb 强制关闭；CLICOLOR_FORCE / CLICOLOR 可覆盖终端探测。
// Windows 上同时尝试开启 VT 解析（旧 conhost 默认不认 ANSI）。
func DetectColor(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if v, ok := os.LookupEnv("CLICOLOR_FORCE"); ok && v != "0" {
		return true
	}
	if os.Getenv("CLICOLOR") == "0" {
		return false
	}
	return enableVT(f)
}

func Colorize(c Color, s string) string {
	return ColorizeTo(os.Stdout, c, s)
}

// ColorizeTo 按目标流的终端能力决定是否上色。
func ColorizeTo(f *os.File, c Color, s string) string {
	on := UseColor()
	if f == os.Stderr {
		on = UseColorStderr()
	}
	if !on {
		return s
	}
	var code string
	switch c {
	case Dim:
		code = "\x1b[2m"
	case Bold:
		code = "\x1b[1m"
	case Red:
		code = "\x1b[31m"
	case Green:
		code = "\x1b[32m"
	case Yellow:
		code = "\x1b[33m"
	case Cyan:
		code = "\x1b[36m"
	default:
		return s
	}
	return code + s + "\x1b[0m"
}
