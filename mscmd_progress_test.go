package main

import (
	"strings"
	"testing"
	"time"
)

// bot master 的进度条复用 bot run 的同一组件：三行 = 爬取队列/渲染队列/所有任务。
func TestMasterProgressFrame(t *testing.T) {
	snap := func() (hd, ht, bd, bt, rep int) { return 100, 123, 40, 50, 300 }
	p := newMasterProgress(snap, false)
	frame := p.render(time.Now())
	if len(frame) != 4 { // 标题 + 三条
		t.Fatalf("want 4 lines (title+3 bars), got %d: %q", len(frame), frame)
	}
	if !strings.Contains(frame[0], "开往巡查") {
		t.Fatalf("title line missing: %q", frame[0])
	}
	for i, want := range []string{"爬取队列", "渲染队列", "所有任务"} {
		if !strings.Contains(frame[i+1], want) {
			t.Fatalf("line %d 缺标签 %s: %q", i+1, want, frame[i+1])
		}
	}
	// 数字口径：爬取 100/123；渲染 40/50；所有任务 300/173（=123+50）。
	if !strings.Contains(frame[1], "100/123") {
		t.Fatalf("爬取队列计数应 100/123: %q", frame[1])
	}
	if !strings.Contains(frame[2], "40/50") {
		t.Fatalf("渲染队列计数应 40/50: %q", frame[2])
	}
	if !strings.Contains(frame[3], "300/173") {
		t.Fatalf("所有任务计数应 300/173: %q", frame[3])
	}
}
