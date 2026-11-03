package ui

import (
	"strings"
	"testing"
)

// 边界逻辑各锁一条：CJK 宽度表、ClipDisplay 窄预算截可见字符、rune 截断不切坏多字节。
func TestPrimitives(t *testing.T) {
	if got := DisplayWidth("判定"); got != 4 {
		t.Fatalf("DisplayWidth(判定)=%d want 4", got)
	}
	if got := DisplayWidth("raw"); got != 3 {
		t.Fatalf("DisplayWidth(raw)=%d want 3", got)
	}
	// ClipDisplay 的真实契约（逐字节搬自 progress.go）：预算只约束可见字符，
	// ANSI 转义序列整段原样带走、不计宽也不被拦腰截断。
	// width=2 只够第一个 CJK「红」，「色」应被截掉，转义仍在。
	clipped := ClipDisplay("\x1b[31m红色\x1b[0m", 2)
	if !strings.Contains(clipped, "红") {
		t.Fatalf("ClipDisplay 应保留可见的「红」: %q", clipped)
	}
	if strings.Contains(clipped, "色") {
		t.Fatalf("ClipDisplay 超出预算的「色」应被截断: %q", clipped)
	}
	if !strings.Contains(clipped, "\x1b[31m") {
		t.Fatalf("ClipDisplay 应原样保留 ANSI 转义: %q", clipped)
	}
	// truncate 按 rune 切，绝不把多字节字符拦腰截断。
	if tr := Truncate("中文测试串", 3); tr != "中文…" {
		t.Fatalf("Truncate=%q want 中文…", tr)
	}
	if Pad("判定", 6) != "判定  " {
		t.Fatalf("Pad 未按显示宽度补白: %q", Pad("判定", 6))
	}
}
