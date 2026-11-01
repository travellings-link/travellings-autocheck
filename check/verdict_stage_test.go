package check

import (
	"strings"
	"testing"
)

// Evaluate 在 JS 阶段必须仍能靠 specialCases 命中，
// 因为渲染器只在 Links 全部落空时才补取 HTML。
func TestEvaluateJSSpecialCaseStillWorks(t *testing.T) {
	p := Page{
		FinalURL: "https://example.com/",
		Status:   200,
		HTML:     "<script>anzhiyu.totraveling()</script>",
		Links:    nil,
	}
	res := Evaluate(p, JS)
	if res.Verdict != Run {
		t.Fatalf("specialCase should yield RUN at JS stage, got %v (%s)", res.Verdict, res.Evidence)
	}
	if !strings.Contains(res.Evidence, "signature") {
		t.Errorf("evidence should mention signature, got %q", res.Evidence)
	}
}

// Links 命中时不需要 HTML：这是渲染器省掉最贵一次往返的前提。
func TestEvaluateJSLinksWinWithoutHTML(t *testing.T) {
	p := Page{
		FinalURL: "https://example.com/",
		Status:   200,
		HTML:     "",
		Links:    []string{"https://www.travellings.cn/go.html"},
	}
	res := Evaluate(p, JS)
	if res.Verdict != Run {
		t.Fatalf("links should yield RUN without HTML, got %v", res.Verdict)
	}
}
