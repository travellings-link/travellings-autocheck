package pipeline

import (
	"context"
	"fmt"

	"travellings-bot-go/crawl"

	"github.com/carlmjohnson/requests"
)

// FetchSitesFromAPI 从 https://api.travellings.cn/all 拉全量成员清单，
// 映射 id/url → Site。适合 --from-api 脱离数据库做 dry-run。
func FetchSitesFromAPI(ctx context.Context) ([]Site, error) {
	var body struct {
		Success bool `json:"success"`
		Data    []struct {
			ID  int    `json:"id"`
			URL string `json:"url"`
		} `json:"data"`
	}
	err := requests.
		URL("https://api.travellings.cn/all").
		Header("User-Agent", crawl.BotUA).
		Header("Referer", crawl.Referer).
		ToJSON(&body).
		Fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch api: %w", err)
	}
	if !body.Success {
		return nil, fmt.Errorf("api success=false")
	}
	sites := make([]Site, 0, len(body.Data))
	for _, d := range body.Data {
		sites = append(sites, Site{ID: d.ID, URL: d.URL})
	}
	return sites, nil
}
