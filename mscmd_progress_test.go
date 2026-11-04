package main

import (
	"strings"
	"testing"
	"time"

	"travellings-bot-go/master"
)

// bot master 的进度条复用 bot run 的同一组件：五行 = 爬取派发/爬取响应/渲染派发/渲染响应/所有任务。
// 派发与响应的差值即该腿在途（master 哈希表规模）。
func TestMasterProgressFrame(t *testing.T) {
	snap := func() master.Progress {
		return master.Progress{
			HTTPDisp: 100, HTTPTotal: 123, HTTPDone: 90,
			BrowserDisp: 40, BrowserTotal: 50, BrowserDone: 30,
			Reports: 120,
		}
	}
	p := newMasterProgress(snap, false)
	frame := p.render(time.Now())
	if len(frame) != 6 { // 标题 + 五行
		t.Fatalf("want 6 lines (title+5 bars), got %d: %q", len(frame), frame)
	}
	if !strings.Contains(frame[0], "开往巡查") {
		t.Fatalf("title line missing: %q", frame[0])
	}
	want := []struct{ label, counts string }{
		{"爬取派发", "100/123"}, {"爬取响应", "90/123"},
		{"渲染派发", "40/50"}, {"渲染响应", "30/50"},
		{"所有任务", "120/173"}, // 分母 = 两腿累计任务之和
	}
	for i, w := range want {
		line := frame[i+1]
		if !strings.Contains(line, w.label) {
			t.Fatalf("line %d 缺标签 %s: %q", i+1, w.label, line)
		}
		if !strings.Contains(line, w.counts) {
			t.Fatalf("line %d 计数应 %s: %q", i+1, w.counts, line)
		}
	}
}
