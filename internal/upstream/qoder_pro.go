// qoder_pro.go —— Pro 升级包（+1800 credits）资格与领取。
//
// 端点（与签到同一套鉴权，job token / device token 都吃）：
//
//	GET  /sash/api/v1/me/pro-upgrade/eligibility
//	POST /sash/api/v1/me/pro-upgrade/claim
//
// 幂等：已领取时上游返回 409 或 body 含 ALREADY。这不是失败——
// 必须区分「本轮真的领到了」和「本来就领过」，否则批量汇总会每次虚增 1800。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// QoderProResult 单个账号的 Pro 领取结果。
type QoderProResult struct {
	Account string `json:"account"`
	UID     string `json:"uid"`
	Realm   string `json:"realm"`
	State   string `json:"state"`
	Message string `json:"message"`
	Earned  int    `json:"earned_credit"`
}

// ProEligibility 查询是否可领取。端点 404/403/410 视为「不可领取」而不是错误。
func (c *QoderClient) ProEligibility(ctx context.Context) (bool, error) {
	raw, status, err := c.qoderDo(ctx, http.MethodGet, c.OpenAPI+"/sash/api/v1/me/pro-upgrade/eligibility", nil)
	if err != nil {
		if status == http.StatusNotFound || status == http.StatusForbidden || status == http.StatusGone {
			return false, nil
		}
		return false, err
	}
	var body struct {
		Eligible bool `json:"eligible"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return false, err
	}
	return body.Eligible, nil
}

// ProClaim 领取 Pro 升级包。
func (c *QoderClient) ProClaim(ctx context.Context) (QoderProResult, error) {
	raw, status, err := c.qoderDo(ctx, http.MethodPost, c.OpenAPI+"/sash/api/v1/me/pro-upgrade/claim", []byte("{}"))
	res := QoderProResult{Account: c.Nickname, UID: c.UID, Realm: c.Realm}
	if err != nil {
		if status == http.StatusConflict || strings.Contains(strings.ToUpper(string(raw)), "ALREADY") {
			res.State = "already_claimed"
			res.Message = "已领取过，不重复发放"
			return res, nil
		}
		res.State = "error"
		res.Message = err.Error()
		return res, err
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if v, ok := body["success"].(bool); ok && !v {
		res.State = "error"
		if m, ok := body["message"].(string); ok && m != "" {
			res.Message = m
		} else {
			res.Message = "上游返回 success=false"
		}
		return res, fmt.Errorf("%s", res.Message)
	}
	res.State = "claimed_now"
	res.Message = "领取成功 +1800 积分"
	res.Earned = 1800
	if v, ok := body["amount"].(float64); ok && v > 0 {
		res.Earned = int(v)
		res.Message = fmt.Sprintf("领取成功 +%d 积分", res.Earned)
	}
	return res, nil
}

// ProClaimAll 逐个账号领取；单个失败不影响其余。
func (p *QoderPool) ProClaimAll(ctx context.Context) []QoderProResult {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	clients := append([]*QoderClient(nil), p.clients...)
	p.mu.Unlock()

	out := make([]QoderProResult, 0, len(clients))
	for _, c := range clients {
		eligible, err := c.ProEligibility(ctx)
		if err != nil {
			out = append(out, QoderProResult{
				Account: c.Nickname, UID: c.UID, Realm: c.Realm,
				State: "error", Message: "资格查询失败: " + err.Error(),
			})
			continue
		}
		if !eligible {
			out = append(out, QoderProResult{
				Account: c.Nickname, UID: c.UID, Realm: c.Realm,
				State: "not_available", Message: "不可领取（已领或活动未开放）",
			})
			continue
		}
		res, _ := c.ProClaim(ctx)
		out = append(out, res)
	}
	return out
}
