// compensation.go 官网 web 域的「限时补偿 / 权益领取」接口。
//
// 背景（2026-10）：上游把补偿端点放在**官网 web 域**上，并要求请求带
// Origin/Referer 指向官网「套餐与用量」页；只按 billing 域（codebuddy.cn）
// 裸 POST 会被当作非浏览器来源拒掉。
//
//	POST {webBase}/billing/meter/compensation-status   查询状态（可随便轮询）
//	POST {webBase}/billing/meter/claim-compensation    领取（**非幂等**）
//
// 幂等边界：claim 不是幂等的 —— 上游已发放但响应丢失时重发会重复领取。
// 调用方必须「先落盘『已提交』标记 → 再发请求」，响应丢失时先回查 status。
package upstream

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
)

// 官网补偿端点（相对 webBase）。
const (
	compensationStatusPath = "/billing/meter/compensation-status"
	compensationClaimPath  = "/billing/meter/claim-compensation"
	compensationReferer    = "/profile/plans-usage?action=addon"
)

// CompensationStatus 上游补偿活动状态。字段用指针语义：上游在不同活动期
// 下发的字段集合不同，「缺字段」与「显式 false / 0」必须能区分。
type CompensationStatus struct {
	Active    *bool  `json:"active"`
	Claimed   *bool  `json:"claimed"`
	CreditNum *int64 `json:"credit_num"`
	// ValidityPeriod 上游给的是**数字**（实测），类型不保证，用 RawMessage 收。
	ValidityPeriod json.RawMessage `json:"validity_period"`
	EndTime        string          `json:"end_time"`
}

// compensationWebJSON 发一次官网 web 域补偿请求，返回解信封后的 data 段。
func (c *Client) compensationWebJSON(a *auth.Auth, path string) (json.RawMessage, error) {
	base := c.webBase(a)
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("Content-Type", "application/json")
	// 上游按来源校验：Origin/Referer 必须落在官网自身域内。
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+compensationReferer)
	return c.doJSON(req)
}

// CompensationStatusOf 查询某账号当前可领取权益的状态。
//
// 与领取不同，查询是只读的，可安全重试/轮询。返回零值 + nil error 表示
// 上游 200 但没给 data（视作「本轮无权益」）。
func (c *Client) CompensationStatusOf(a *auth.Auth) (CompensationStatus, error) {
	var st CompensationStatus
	raw, err := c.compensationWebJSON(a, compensationStatusPath)
	if err != nil {
		return st, err
	}
	if len(raw) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return CompensationStatus{}, err
	}
	return st, nil
}

// ClaimCompensationWeb 走**官网 web 域**领取补偿（带 Origin/Referer）。
//
// 与 blackcat.go 的 ClaimCompensation 的区别：那个走 billing 域（老活动通道），
// 这个走官网 web 域（2026-10 新活动的通道）。**非幂等**，调用方必须先落盘
// 「已提交」标记；出错时先回查 CompensationStatusOf 的 claimed 再决定重试。
func (c *Client) ClaimCompensationWeb(a *auth.Auth) (int64, error) {
	raw, err := c.compensationWebJSON(a, compensationClaimPath)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit    int64 `json:"credit"`
		CreditNum int64 `json:"credit_num"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &resp)
	}
	if resp.Credit > 0 {
		return resp.Credit, nil
	}
	return resp.CreditNum, nil
}

// CompensationEnded 判断上游给的 end_time 是否已过（本地时钟）。
// 解析不出来时返回 false —— 宁可让用户去试，也不要因解析失败武断说「已截止」。
func CompensationEnded(endTime string) bool {
	if endTime == "" {
		return false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, endTime, time.Local); err == nil {
			return time.Now().After(t)
		}
	}
	return false
}
