// qoder_checkin.go —— Qoder 活动领取（含每日签到）与本地连续统计。
//
// 实测契约（2026-10-17，global 账号）：
//
//	GET  /sash/api/v1/me/campaigns                → 活动列表（含 claimStatus）
//	POST /sash/api/v1/me/campaigns/{id}/claim     → 领取；幂等，重复领取返回 replayed=true
//
// 两个端点都直接吃 job token，不需要 WASM 签名，也不需要设备指纹——
// 与对话端点同一套鉴权。fingerprint 只影响"活动是否下发"，不影响领取本身。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// QoderCampaign 活动列表里的一项（只解我们需要的字段）。
type QoderCampaign struct {
	CampaignID  string `json:"campaignId"`
	CampaignKey string `json:"campaignKey"`
	ActionType  string `json:"actionType"`
	ClaimStatus string `json:"claimStatus"`
	StartAt     int64  `json:"startAt"`
	EndAt       int64  `json:"endAt"`
}

// QoderCampaignList GET campaigns 的响应。
type QoderCampaignList struct {
	UID        string           `json:"uid"`
	Claimable  bool             `json:"claimable"`
	Campaigns  []QoderCampaign  `json:"campaigns"`
}

// QoderClaimResult 单次领取结果。
type QoderClaimResult struct {
	CampaignID  string `json:"campaign_id"`
	CampaignKey string `json:"campaign_key"`
	Status      string `json:"status"`
	GrantID     string `json:"grant_id,omitempty"`
	Replayed    bool   `json:"replayed,omitempty"`
	Skipped     bool   `json:"skipped,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// qoderDo 带 job token 的通用请求（GET/POST），返回原始 body。
func (c *QoderClient) qoderDo(ctx context.Context, method, url string, body []byte) ([]byte, int, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, 0, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range qoderHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, resp.StatusCode, fmt.Errorf("qoder: %s %s status %d: %s",
			method, url, resp.StatusCode, truncateText(string(raw), 160))
	}
	return raw, resp.StatusCode, nil
}

// Campaigns 拉活动列表。
func (c *QoderClient) Campaigns(ctx context.Context) (*QoderCampaignList, error) {
	raw, _, err := c.qoderDo(ctx, http.MethodGet, c.OpenAPI+"/sash/api/v1/me/campaigns", nil)
	if err != nil {
		return nil, err
	}
	var list QoderCampaignList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("qoder: parse campaigns: %w", err)
	}
	return &list, nil
}

// Claim 领取一个活动。
func (c *QoderClient) Claim(ctx context.Context, campaignID string) (*QoderClaimResult, error) {
	raw, _, err := c.qoderDo(ctx, http.MethodPost,
		c.OpenAPI+"/sash/api/v1/me/campaigns/"+campaignID+"/claim", []byte("{}"))
	if err != nil {
		return nil, err
	}
	var out struct {
		GrantID     string `json:"grantId"`
		Status      string `json:"status"`
		CampaignID  string `json:"campaignId"`
		CampaignKey string `json:"campaignKey"`
		Replayed    bool   `json:"replayed"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("qoder: parse claim: %w", err)
	}
	return &QoderClaimResult{
		CampaignID:  out.CampaignID,
		CampaignKey: out.CampaignKey,
		Status:      out.Status,
		GrantID:     out.GrantID,
		Replayed:    out.Replayed,
	}, nil
}

// CheckinAll 拉一次活动列表，把所有"可领"的活动领掉。
//
// 判定只看 claimStatus：非 CLAIMED 的都试一次。上游对重复领取是幂等的
// （replayed=true），所以"多试一次"的代价只是多一个请求，不会重复发奖。
func (p *QoderPool) CheckinAll(ctx context.Context) []QoderClaimResult {
	if p == nil {
		return nil
	}
	out := make([]QoderClaimResult, 0, len(p.clients))
	for i, c := range p.clients {
		name := p.names[i]
		list, err := c.Campaigns(ctx)
		if err != nil {
			out = append(out, QoderClaimResult{
				CampaignKey: name, Status: "ERROR", Reason: err.Error(),
			})
			continue
		}
		claimed := 0
		for _, camp := range list.Campaigns {
			if camp.ClaimStatus == "CLAIMED" {
				continue
			}
			res, cerr := c.Claim(ctx, camp.CampaignID)
			if cerr != nil {
				out = append(out, QoderClaimResult{
					CampaignID: camp.CampaignID, CampaignKey: camp.CampaignKey,
					Status: "ERROR", Reason: cerr.Error(),
				})
				continue
			}
			out = append(out, *res)
			claimed++
		}
		if claimed == 0 {
			out = append(out, QoderClaimResult{
				CampaignKey: name, Status: "NOTHING_TO_CLAIM",
				Reason: "当前没有可领取的活动（可能今日已领或账号无签到活动）",
			})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 连续签到统计（本地记账）
// ---------------------------------------------------------------------------

// QoderCheckinHistory 按日期记录的领取历史。
// 上游不提供连续天数，只能本地记；记的是"当天有成功领取"这一事实。
type QoderCheckinHistory struct {
	// Days 日期 → 当天领取到的 grantId 数量（>0 即算当天已签）。
	Days map[string]int `json:"days"`
}

// LoadQoderCheckinHistory 读取历史；文件不存在返回空历史（不是错误）。
func LoadQoderCheckinHistory(path string) *QoderCheckinHistory {
	h := &QoderCheckinHistory{Days: map[string]int{}}
	if path == "" {
		return h
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return h
	}
	if json.Unmarshal(raw, h) != nil || h.Days == nil {
		return &QoderCheckinHistory{Days: map[string]int{}}
	}
	return h
}

// Save 原子落盘。
func (h *QoderCheckinHistory) Save(path string) error {
	if path == "" || h == nil {
		return nil
	}
	raw, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Mark 记录某天成功领取的次数。
func (h *QoderCheckinHistory) Mark(day string, grants int) {
	if h.Days == nil {
		h.Days = map[string]int{}
	}
	h.Days[day] += grants
}

// Streak 返回截至 today 的连续签到天数与累计天数。
//
// 连续 = 从 today 往前逐日都存在记录；today 当天没记录时从昨天往前数
// （当天还没签不算断签，否则每天早上打开面板都会看到连续天数归零）。
func (h *QoderCheckinHistory) Streak(today time.Time) (current, total int) {
	if h == nil || len(h.Days) == 0 {
		return 0, 0
	}
	for _, n := range h.Days {
		if n > 0 {
			total++
		}
	}
	cur := 0
	day := today
	if h.Days[day.Format("2006-01-02")] == 0 {
		day = day.AddDate(0, 0, -1)
	}
	for {
		key := day.Format("2006-01-02")
		if h.Days[key] <= 0 {
			break
		}
		cur++
		day = day.AddDate(0, 0, -1)
	}
	return cur, total
}

// RecentDays 返回最近 n 天的签到标记（面板画点用），按日期升序。
func (h *QoderCheckinHistory) RecentDays(today time.Time, n int) []map[string]any {
	if n <= 0 {
		n = 30
	}
	out := make([]map[string]any, 0, n)
	for i := n - 1; i >= 0; i-- {
		d := today.AddDate(0, 0, -i)
		key := d.Format("2006-01-02")
		out = append(out, map[string]any{
			"date":  key,
			"count": h.Days[key],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i]["date"].(string) < out[j]["date"].(string)
	})
	return out
}
