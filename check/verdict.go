package check

// Verdict 沿用线上状态词汇（API 直接消费该字符串，新值需上游对齐后才能加）。
type Verdict string

const (
	Run     Verdict = "RUN"
	Lost    Verdict = "LOST"
	Error   Verdict = "ERROR"
	Timeout Verdict = "TIMEOUT"
)

// Stage 是 B 状态机的防环键：raw 结论可升级，js 结论必终态。
type Stage string

const (
	Raw Stage = "raw"
	JS  Stage = "js"
)

// Page 是一站点的证据包。Links 为绝对化后的候选链接；raw 阶段由 ExtractLinks 生成，
// 渲染阶段可直接取页内 a.href（IDL 属性，浏览器已替我们绝对化）。
type Page struct {
	FinalURL string
	Status   int
	HTML     string
	Links    []string
	FetchErr string // 非空表示传输层失败，Status=0
}

type Result struct {
	Verdict  Verdict
	Evidence string // 命中的链接或签名，写入 failedReason 时带上
	// Escalate: true 仅当 Stage=Raw。Judge 依此路由到 Renderer；渲染阶段永不升级（防环）。
	Escalate bool
}
