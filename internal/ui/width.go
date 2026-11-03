package ui

import (
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// DisplayWidth 估算终端显示宽度：CJK/全角算 2 列，其余算 1。
// 表格含中文表头，用 len() 排版会错位。
func DisplayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115f, // 谚文字母
			r >= 0x2e80 && r <= 0xa4cf && r != 0x303f, // CJK 部首~彝文
			r >= 0xac00 && r <= 0xd7a3,                // 谚文音节
			r >= 0xf900 && r <= 0xfaff,                // CJK 兼容
			r >= 0xfe30 && r <= 0xfe6f,                // CJK 兼容标点
			r >= 0xff00 && r <= 0xff60,                // 全角
			r >= 0xffe0 && r <= 0xffe6:                // 全角符号
			w += 2
		default:
			w++
		}
	}
	return w
}

// Pad 右补空格到指定显示宽度。
func Pad(s string, width int) string {
	if d := DisplayWidth(s); d < width {
		return s + strings.Repeat(" ", width-d)
	}
	return s
}

// Truncate 按 rune 截断，避免把多字节字符切坏。
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// WrapText 按显示宽度折行，不切断单词也不吞字符。
// evidence 里常见长 URL 与错误链，定宽截断会丢失诊断信息。
func WrapText(s string, width int) []string {
	if width < 20 {
		width = 20
	}
	var out []string
	for _, paragraph := range strings.Split(s, "\n") {
		line := ""
		for _, word := range strings.Fields(paragraph) {
			if line == "" {
				line = word
				continue
			}
			if DisplayWidth(line)+1+DisplayWidth(word) <= width {
				line += " " + word
				continue
			}
			out = append(out, line)
			line = word
		}
		if line != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		out = []string{s}
	}
	return out
}

// TerminalWidth 返回可用列数；取不到时给保守默认，避免一行撑爆换行。
// 以 stderr 为准（进度块/报表都往 stderr 排）。
func TerminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 0 {
		return w - 1
	}
	return 79
}

// ClipDisplay 把已带 ANSI 的字符串按显示宽度截断。
// 逐字符扫描，遇到 ESC 序列整段原样带走（不计宽度），其余按显示宽度累计。
func ClipDisplay(s string, width int) string {
	var b strings.Builder
	used := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
				j++
			}
			if j < len(s) {
				j++
			}
			b.WriteString(s[i:j])
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := DisplayWidth(string(r))
		if used+rw > width {
			break
		}
		used += rw
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}
