package check

import (
	"net/url"
	"strings"
)

// 域名表：travellings.cn 系 + travellings.link。host 精确等值比较（剥离 www 后），
// 不做子串匹配——原实现的 "https://travellings.link" 子串会命中
// travellings.link.evil.com 这类后缀欺骗，这里用 Hostname() 消掉。
var allowedHosts = map[string]bool{
	"travellings.cn":   true,
	"travellings.link": true,
}

// 入口路径白名单（**不含 .html 后缀**）。
//
// 线上两种写法都可用：实测 https://www.travellings.cn/go.html 返回 307 跳转，
// 而 /go 直接返回 200。成员站挂哪个都算合法入口，所以匹配时统一剥掉 .html 再比。
var entryPaths = map[string]bool{
	"/go":           true,
	"/plain":        true,
	"/coder-1024":   true,
	"/go-by-clouds": true,
	"/go-robots":    true,
	"/train":        true,
	"/train-star":   true,
	"/typewriter":   true,
	"/go-start":     true,
	"/arsvine":      true,
}

// GitHub 仓库形态（吸收自 Python 分支 bot.py:44）。
const ghHost = "github.com"
const ghPath = "/travellings-link/travellings"

// specialCases：非链接的合法入口签名，只能原文子串匹配（见包注释）。
// 白名单保持与线上一致，新增须走你们组口径讨论，不由匹配器自行扩容。
var specialCases = []string{
	"anzhiyu.totraveling()",
	"totraveling()",
	"https%3A%2F%2Fwww.travellings.cn%2Fplain.html",
}

// MatchTravellings 返回第一个命中的证据；hit=false 表示页面不含有效开往入口。
func MatchTravellings(p Page) (string, bool) {
	for _, l := range p.Links {
		if ev, ok := matchURL(l); ok {
			return ev, true
		}
		// 中间跳转页：?url=<encoded travellings URL>（吸收自 Python 分支）
		if ev, ok := matchRedirect(l); ok {
			return ev, true
		}
	}
	for _, s := range specialCases {
		if strings.Contains(p.HTML, s) {
			return "signature " + s, true
		}
	}
	return "", false
}

func matchURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", false
	}
	host := normalizeHost(u.Hostname())
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", false
	}
	// GitHub 仓库：全路径精确（容忍大小写、尾斜杠、.git）
	if host == ghHost {
		p := strings.TrimSuffix(strings.ToLower(u.Path), "/")
		p = strings.TrimSuffix(p, ".git")
		if p == ghPath {
			return raw, true
		}
		return "", false
	}
	if !allowedHosts[host] {
		return "", false
	}
	// 归一化后比对：剥掉 .html 后缀，让 /go 与 /go.html 等价。
	// 尾斜杠也一并去掉，容忍 /go/ 这种写法。
	if entryPaths[normalizePath(u.Path)] {
		return raw, true
	}
	// travellings.link 原语义不要求路径
	if host == "travellings.link" {
		return raw, true
	}
	return "", false
}

var jumpParams = []string{"url", "link", "target", "to", "goto", "redirect"}

// matchRedirect 解一层跳转参数（对齐 Python 分支；不递归，防跳转链放大）。
func matchRedirect(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	for _, name := range jumpParams {
		v := u.Query().Get(name)
		if v == "" {
			continue
		}
		if dec, err := url.QueryUnescape(v); err == nil {
			v = dec
		}
		// 参数值可能是相对形态，先对宿主基址绝对化再判
		if !strings.Contains(v, "://") {
			base, err2 := url.Parse(raw)
			if err2 != nil {
				continue
			}
			if ref, err3 := url.Parse(v); err3 == nil {
				v = base.ResolveReference(ref).String()
			}
		}
		if ev, ok := matchURL(v); ok {
			return ev + " (via " + name + "=)", true
		}
	}
	return "", false
}

func normalizeHost(h string) string {
	h = strings.ToLower(h)
	return strings.TrimPrefix(h, "www.")
}

// normalizePath 归一化入口路径：去尾斜杠、剥 .html 后缀，
// 让 /go、/go.html、/go/ 三种写法都落到同一个键。
func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimSuffix(p, ".html")
	if p == "" {
		return "/"
	}
	return p
}

// Evaluate 是 B 的判定核：纯函数，含防环路由决策。
func Evaluate(p Page, st Stage) Result {
	if p.Status == 0 {
		// 传输层失败分两类处理，判据是「再试一次会不会有不同结果」：
		//
		//  A. 超时 —— 抓取阶段已经**用满了整个超时预算**（默认 15s）都没拿到响应。
		//     再进渲染，浏览器面对的是同一张网、同一个服务器，只会把等待重演一遍，
		//     最终仍以超时收场（实测 15s + 30s = 45s 花在一个站上）。
		//     所以直接判 TIMEOUT 终态——它本来就是「超时」这个结论，不是工具链故障。
		//
		//  B. 确定性失败（TLS 证书、DNS 不存在、连接被拒）—— 协议层决定，换客户端也不会变。
		//
		//  C. 其余（EOF、连接被重置等）—— 可能是一过性抖动，值得让浏览器再试一次。
		//
		// 注意：A 类不再进 C，所以不会再被标成 "infra js-leg"——
		// 那顶帽子只该盖在「我们自己的 CDP/浏览器出问题」上，不该盖站点超时。
		if isTimeoutErr(p.FetchErr) {
			return Result{Verdict: Timeout, Evidence: p.FetchErr}
		}
		if st == Raw && !terminalErr(p.FetchErr) {
			return Result{Verdict: Error, Evidence: p.FetchErr, Escalate: true}
		}
		return Result{Verdict: Error, Evidence: p.FetchErr}
	}
	if p.Status < 200 || p.Status >= 300 {
		// 非 2xx 一律终判，不升级到渲染。
		//
		// 理由：状态码是服务端给出的事实，浏览器拿到的是**同一个响应**——
		// 同一台服务器、同一个路由、同一份证书，换个客户端不会变。
		// 拿 526/500 去渲染，只是再等一遍超时，结论必然相同，纯属白烧 C 腿预算。
		// 实测：blog.oalo.cn 证书过期，抓取阶段 64ms 就拿到错误，
		//       渲染却要再花好几秒，最终仍是同一个结论。
		//
		// 与"未命中"区分：200 但没找到入口才是**值得**渲染的（SPA 内容在 JS 里）。
		return Result{Verdict: Verdict(itoa(p.Status)), Evidence: "HTTP " + itoa(p.Status)}
	}
	if ev, ok := MatchTravellings(p); ok {
		return Result{Verdict: Run, Evidence: ev}
	}
	if st == Raw {
		// 初筛未命中 ≠ LOST：SPA 站内容在 JS 里，必须升级（khbit/官网案例教训）。
		return Result{Verdict: Lost, Evidence: "no entry in raw html", Escalate: true}
	}
	return Result{Verdict: Lost, Evidence: "no entry after render"}
}

// isTimeoutErr 判断传输层失败是否为超时。
//
// 必须覆盖多种写法：不同客户端/协议层次措辞不一，只匹配 "timeout" 会漏判——
// 实测 chromedp 报的是 "operation timed out"（带空格），
// 曾因此把真正的超时误判成 ERROR。
func isTimeoutErr(e string) bool {
	l := strings.ToLower(e)
	for _, s := range []string{
		"timeout",     // io timeout / context deadline exceeded 的常见形态
		"timed out",   // chromedp: operation timed out
		"deadline",    // context deadline exceeded
		"exceeded",    // navigation exceeded 30000ms deadline
		"i/o timeout", // net 包标准措辞
	} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// TerminalRenderErr 判断渲染失败是否**不值得重试**。
//
// 重试的价值前提：失败可能是偶发。而这几类是确定性结论——
// 域名解析不了、证书过期、协议不支持，重试多少次都是同样结果，
// 只会白占浏览器池。真正值得重试的是超时、CDP 抖动这类。
func TerminalRenderErr(msg string) bool {
	l := strings.ToLower(msg)
	for _, s := range []string{
		"dns error", "no such host", "nxdomain",
		"x509", "certificate",
		"unsupported protocol",
	} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// IsSiteNavTimeout 判断渲染阶段的错误是「站点超时」还是「我们自己的工具链故障」。
//
// 区分很重要：
//   - 导航超时：浏览器正常启动了，是这个站点在 30 秒内没能加载完 → 站点问题
//   - 工具链故障：CDP 连不上、浏览器崩溃、target 丢失 → 我们的问题
//
// 两者都值得单列（都不该直接写库），但报告里的措辞必须不同——
// 把站点超时说成"工具链故障"会让运维去查 CDP，查半天发现 CDP 好好的。
func IsSiteNavTimeout(errMsg string) bool {
	l := strings.ToLower(errMsg)

	// 先排除真正的工具链故障：CDP 连不上、浏览器崩了、target 丢了。
	// 命中这些说明我们的浏览器根本没跑起来，与站点无关。
	for _, s := range []string{
		"attach cdp", "executor doesn't exist",
		"target closed", "browser closed", "websocket:",
	} {
		if strings.Contains(l, s) {
			return false
		}
	}

	// 站点侧失败：浏览器正常启动了，但**访问这个站**失败。包括
	//   - 导航超时（页面太重/服务端太慢）
	//   - DNS 解析失败（域名不存在或解析不了）
	//   - TLS 证书问题（浏览器也验不过）
	// 这些都不是"工具链坏了"，而是这个站点有问题。
	for _, s := range []string{
		"deadline", "timed out", "timeout",
		"dns error", "dns", "no such host",
		"x509", "certificate", "tls",
		"connection refused", "connect",
	} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// terminalErr 判断传输层失败是否**必然可复现、浏览器也救不了**。
// 命中即直接终判（ERROR），不升级到渲染——那是白烧 C 腿预算。
//
// 判据：这类错误由协议层决定，与页面内容无关，换任何客户端结论都一样。
//   - DNS 不存在 / 域名解析失败
//   - 连接被拒（目标明确拒绝，不是抖动）
//   - TLS 握手失败、证书过期/无效/自签名        ← blog.oalo.cn 实测 64ms 即报
//   - 不支持的协议
//
// 反例（**应当**升级）：超时由调用方先行拦下（见 Evaluate），
// EOF 与"连接被重置"可能是服务端瞬间抖动，重试有机会成功。
func terminalErr(e string) bool {
	l := strings.ToLower(e)
	for _, s := range []string{
		"no such host", "nxdomain",
		"connection refused", "refused",
		"x509", "certificate", "tls:",
		"unsupported protocol scheme",
	} {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
