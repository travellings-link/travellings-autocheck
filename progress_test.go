package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"travellings-bot-go/pipeline"
)

func newTestProgress(buf *bytes.Buffer, total int) *progressView {
	p := newProgress(total, &pipeline.StageStats{}, false)
	p.w = buf
	p.enabled = true
	return p
}

// 非 TTY 必须完全不画：管道/CI 里出现 ESC 会污染日志。
func TestProgressDisabledWhenNotTTY(t *testing.T) {
	if useColorStderr() {
		t.Skip("running on a TTY")
	}
	var buf bytes.Buffer
	p := newProgress(10, &pipeline.StageStats{}, false)
	p.w = &buf
	p.Tick(true)
	p.Finish()
	if buf.Len() != 0 {
		t.Fatalf("non-TTY progress must write nothing, got %q", buf.String())
	}
}

func TestProgressDisabledWhenVerbose(t *testing.T) {
	if newProgress(10, &pipeline.StageStats{}, true).enabled {
		t.Fatal("progress must be disabled in verbose mode")
	}
}

// 关键回归：画 N 行后光标停在最后一行，上移应为 N-1 而非 N。
func TestProgressMovesUpByNMinusOne(t *testing.T) {
	var buf bytes.Buffer
	p := newTestProgress(&buf, 10)

	p.Tick(true)
	n := p.drawn
	if n < 1 {
		t.Fatalf("至少应画 1 行，实际 %d", n)
	}
	buf.Reset()
	p.Tick(true)

	out := buf.String()
	want := fmt.Sprintf("\x1b[%dA", n-1)
	if !strings.HasPrefix(out, want) {
		t.Fatalf("must move up N-1=%d, got %q", n-1, out[:min(12, len(out))])
	}
}

// 帧必须含：标题 + 抓取 + 判定 + 状态行。
// 渲染行是条件性的（只有存在升级任务才出现），不在此断言。
func TestProgressFrameHasCoreStages(t *testing.T) {
	var buf bytes.Buffer
	p := newTestProgress(&buf, 100)
	p.Tick(true)
	out := buf.String()
	for _, want := range []string{"开往巡查", "抓取", "判定"} {
		if !strings.Contains(out, want) {
			t.Errorf("frame missing %q", want)
		}
	}
}

// 有升级任务时，渲染行必须出现，且含渲染标签。
func TestProgressRenderLineOnlyWhenEscalating(t *testing.T) {
	var buf bytes.Buffer
	stats := &pipeline.StageStats{}
	stats.Done(100, 100, 5, 5) // 有 5 个待渲染
	p := newProgress(100, stats, false)
	p.w = &buf
	p.enabled = true

	p.Tick(true)
	if !strings.Contains(buf.String(), "渲染") {
		t.Fatal("有升级任务时应显示渲染行")
	}
}

// 状态行必须显示判定分类与标签页数。
func TestProgressStatusLine(t *testing.T) {
	var buf bytes.Buffer
	stats := &pipeline.StageStats{}
	for i := 0; i < 3; i++ {
		stats.Record(pipeline.Verdict{Status: "RUN"})
	}
	stats.Record(pipeline.Verdict{Status: "LOST"})
	stats.Record(pipeline.Verdict{Status: "ERROR"})

	p := newProgress(10, stats, false)
	p.w = &buf
	p.enabled = true
	p.Tick(true)

	out := buf.String()
	// tabs 走真实 CDP 会话（crawl.TabCount），单测环境没有浏览器，
	// 恒为 -1 时该段不显示，故这里只断言判定分类。
	for _, want := range []string{"RUN 3", "LOST 1", "ERROR 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("status line missing %q:\n%s", want, out)
		}
	}
}

// Finish 后必须擦净并归零
// Finish 必须保留最终帧（完成画面是可视凭证），而不是擦掉。
// 只补一个换行，把光标放到进度区下方，让后续输出自然接续。
func TestProgressFinishKeepsFinalFrame(t *testing.T) {
	var buf bytes.Buffer
	stats := &pipeline.StageStats{}
	p := newProgress(5, stats, false)
	p.w = &buf
	p.enabled = true

	// 模拟真实完成：5 站全部抓取并判定
	stats.Done(5, 5, 0, 0)
	p.Tick(true)
	buf.Reset()
	p.Finish()

	out := buf.String()
	// 必须重绘一次最终帧：出现进度条内容（含百分比）
	if !strings.Contains(out, "100%") {
		t.Errorf("Finish should redraw final frame with 100%%, got %q", out)
	}
	// 必须换行，否则后续报表会盖在同一行
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("Finish should end with newline, got %q", out)
	}
	// 必须把 drawn 归零，避免 defer 二次调用时重复上移
	if p.drawn != 0 {
		t.Errorf("drawn should reset, got %d", p.drawn)
	}
}

// 超宽行必须截断：自动换行会让上移行数失效。
func TestClipDisplayPreventsWrap(t *testing.T) {
	long := "巡查中 " + strings.Repeat("█", 100)
	if w := visibleWidth(clipDisplay(long, 40)); w > 40 {
		t.Fatalf("clipped width %d exceeds 40", w)
	}
	colored := "\x1b[32m" + strings.Repeat("█", 100) + "\x1b[0m"
	clipped := clipDisplay(colored, 20)
	if w := visibleWidth(clipped); w > 20 {
		t.Fatalf("ANSI width miscount: %d", w)
	}
	if !strings.HasPrefix(clipped, "\x1b[32m") {
		t.Error("leading ANSI must be preserved")
	}
}

// visibleWidth 计算含 ANSI 字符串的可见宽度（测试用）。
func visibleWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
				j++
			}
			if j < len(s) {
				j++
			}
			i = j
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += displayWidth(string(r))
		i += size
	}
	return w
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 收尾竞态：Finish 之后 ticker 若再画一帧，终端上就会出现两块进度条。
// 复现方式：Start 起 goroutine，立刻 Finish，然后反复 Tick(true)——
// 已 stopped 的 Tick 必须完全无输出。
func TestProgressNoDrawAfterFinish(t *testing.T) {
	var buf bytes.Buffer
	p := newTestProgress(&buf, 10)
	stop := make(chan struct{})
	p.Start(stop)
	close(stop)
	p.Finish()
	n := buf.Len()
	if n == 0 {
		t.Fatal("Finish 应画出最终帧")
	}

	// 收尾后任何重绘都必须被拒绝
	for i := 0; i < 5; i++ {
		p.Tick(true)
	}
	if buf.Len() != n {
		t.Errorf("Finish 后不应再画：大小从 %d 变为 %d", n, buf.Len())
	}

	// 二次 Finish 也不能重复输出（defer + 正常路径都会调）
	p.Finish()
	if buf.Len() != n {
		t.Errorf("二次 Finish 不应重复绘制：%d -> %d", n, buf.Len())
	}
}

// 进度块靠「上移上一帧行数」相对定位，任何一行插入都会把基准永久推低，
// 之后整轮都是两块进度条。Suspend/Resume 必须让日志落在块上方、块只有一块。
func TestProgressSuspendResumeKeepsSingleBlock(t *testing.T) {
	var buf bytes.Buffer
	p := newTestProgress(&buf, 5)
	frame := []string{"开往巡查 0s", "总进度", "抓取", "判定", "RUN 0"}

	p.paint(frame)
	if p.drawn != len(frame) {
		t.Fatalf("drawn = %d, want %d", p.drawn, len(frame))
	}

	p.Suspend()
	if p.drawn != 0 {
		t.Fatalf("Suspend 后 drawn 应为 0，实际 %d", p.drawn)
	}
	// 模拟外部输出：此时块已让开，日志写在块首位置。
	fmt.Fprintln(&buf, "WRN 一条日志")
	p.Resume()

	out := buf.String()
	if n := strings.Count(out, "开往巡查"); n != 2 {
		t.Fatalf("标题应出现 2 次（首帧 + Resume 重画），实际 %d 次:\n%s", n, out)
	}
	if strings.Index(out, "WRN 一条日志") > strings.LastIndex(out, "开往巡查") {
		t.Fatalf("日志应落在进度块上方，实际排在最后一块之后:\n%s", out)
	}
	if p.drawn != len(frame) {
		t.Fatalf("Resume 后 drawn = %d, want %d", p.drawn, len(frame))
	}
}

// 日志 writer 必须真的触发 Suspend/Resume，否则插进块里就错位。
func TestProgWriterSuspendsProgress(t *testing.T) {
	var buf bytes.Buffer
	p := newTestProgress(&buf, 5)
	p.paint([]string{"开往巡查 0s"})

	activeProg.Store(p)
	defer activeProg.Store(nil)

	w := progWriter{w: &buf}
	if _, err := w.Write([]byte("INF hello\n")); err != nil {
		t.Fatal(err)
	}
	if p.suspended {
		t.Fatal("写完日志后应已 Resume，实际仍处于 suspended")
	}
	if p.drawn == 0 {
		t.Fatal("Resume 后应重新画出进度块")
	}
}

// 没有活动进度条时，progWriter 必须是纯透传。
func TestProgWriterPassthroughWithoutProgress(t *testing.T) {
	activeProg.Store(nil)
	var buf bytes.Buffer
	w := progWriter{w: &buf}
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hello\n" {
		t.Fatalf("透传内容被改动: %q", buf.String())
	}
}
