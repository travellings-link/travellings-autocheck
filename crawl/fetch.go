package crawl

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"travellings-bot-go/check"

	"github.com/carlmjohnson/requests"
)

// 抓取身份：与线上 bot 逐字一致（契约，改动须过你们组口径）。
const (
	BotUA   = "Mozilla/5.0 (compatible; Travellings Check Bot; +https://www.travellings.cn/docs/qa)"
	Referer = "https://www.travellings.cn/go.html"
)

const maxBody int64 = 2 << 20 // 2MB；线上最大样本 821KB（CF 挑战页），2.5 倍余量

// normalizeURL 补全缺失的 scheme。
//
// 线上数据存在历史遗留的不规范记录：API 里至少有 1 条只写了裸域名
// （id=855 blog.joker2yue.com，无 https:// 前缀）。
// 这类站点本身是好的，不该因为数据格式问题被判失败——
// 统一补 https:// 后即可正常巡查。渲染阶段也复用本函数，避免相对 URL 报错。
func normalizeURL(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	// 其他 scheme（ftp 等）保持原样，交给下游判为非法
	if i := strings.Index(s, "://"); i >= 0 {
		return s
	}
	// 裸域名/路径：补 https。开往成员站基本都是 https。
	return "https://" + strings.TrimPrefix(s, "//")
}

// Raw 执行抓取阶段：GET、跟随 3xx（requests 默认跟随，上限同 http.Client 默认 10 次）。
// 自定义 ResponseHandler 捕获状态码 + body——非 2xx 也要（403 记 403，CheckStatus 默认
// 会拒绝，故不用它）。
// 传输层失败折叠进 Page.FetchErr（证据而非 Go error，B 依它路由）。
func Raw(ctx context.Context, urlstr string, timeout time.Duration) check.Page {
	urlstr = normalizeURL(urlstr)
	p := check.Page{FinalURL: urlstr}
	if !strings.HasPrefix(urlstr, "http://") && !strings.HasPrefix(urlstr, "https://") {
		p.FetchErr = "invalid url scheme"
		return p
	}
	cl := &http.Client{Timeout: timeout}

	var buf []byte
	status := 0
	handle := func(r *http.Response) error {
		status = r.StatusCode
		b, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			return err
		}
		buf = b
		return nil
	}

	err := requests.
		URL(urlstr).
		Client(cl).
		Header("User-Agent", BotUA).
		Header("Referer", Referer).
		Header("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8").
		Header("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8").
		Header("Sec-Fetch-Dest", "document").
		Header("Sec-Fetch-Mode", "navigate").
		Header("Sec-Fetch-User", "?1").
		Header("Upgrade-Insecure-Requests", "1").
		Handle(handle).
		Fetch(ctx)
	if err != nil {
		p.FetchErr = classify(err, ctx)
		return p
	}
	if status == 0 {
		p.FetchErr = "no response status"
		return p
	}
	p.Status = status
	p.HTML = string(buf)
	// FinalURL：resolved 后的 URL。requests 不给 final，用原始 url（重定向后
	// 页面里的相对链接以最终域为 base，此处为近似，渲染阶段以浏览器 location 为准）。
	p.FinalURL = urlstr
	p.Links = check.ExtractLinks(p.FinalURL, p.HTML)
	return p
}

func classify(err error, ctx context.Context) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout: " + err.Error()
	}
	if ctx.Err() != nil {
		return "timeout: " + ctx.Err().Error()
	}
	return err.Error()
}
