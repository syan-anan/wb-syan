// growth_snapshot.go 成长中心快照：一次把「全年活跃 / 连登 / 能量 / 盲盒 / 宠物 / 旅行 /
// 抽奖 / 兑换 / 成长等级」拉齐，供面板一屏看清。
//
// 三条硬规则（对齐参考实现的 admin/growth_center.py）：
//
//  1. **只读**。这一层只发 GET，不触发对话、不领奖、不兑换 —— 拉一份快照不该改变账号状态。
//
//  2. **单段失败不影响整体**。上游任一接口抖动/下线都很常见，某段报错就只把这段的
//     error 记下来，其余段照常展示（快照的价值在于"一眼看清"，不是"全绿"）。
//
//  3. **白名单投影**。上游返回里可能夹带邮箱、物流地址、凭证之类的字段；这里按段做
//     字段白名单投影，只把展示需要的字段透出（见 ProjectGrowthSection）。
package upstream

import (
	"encoding/json"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
	"net/http"
)

// GrowthSectionDef 一段的定义：名字 / 展示名 / 上游路径。
type GrowthSectionDef struct {
	Name  string
	Label string
	Path  string
}

// GrowthSectionDefs 成长中心的分段（顺序即展示顺序）。
//
// checkin（签到状态）不在这里：它走的是另一套端点，且账号池自己已经在记"今日是否已签到"，
// 重复展示两份口径会让运维不知道该信哪个。
var GrowthSectionDefs = []GrowthSectionDef{
	{Name: "heatmap", Label: "全年活跃", Path: "/activity/growth/heatmap"},
	{Name: "streak", Label: "连续登录", Path: "/activity/growth/streak"},
	{Name: "energy", Label: "能量", Path: "/activity/growth/energy"},
	{Name: "quota", Label: "盲盒额度", Path: "/activity/growth/buddy/quota"},
	{Name: "buddy", Label: "当前宠物", Path: "/activity/growth/buddy/info"},
	{Name: "buddies", Label: "宠物图鉴", Path: "/activity/growth/buddy/list"},
	{Name: "travel", Label: "旅行礼物", Path: "/activity/growth/buddy/travel/status"},
	{Name: "lottery", Label: "抽奖次数", Path: "/activity/growth/lottery/summary"},
	{Name: "redeem", Label: "兑换统计", Path: "/activity/growth/redeem/summary"},
	{Name: "profile", Label: "成长等级", Path: "/v2/activity/growth/profile"},
}

// GrowthSection 一段的结果：有数据或有一条可读的失败说明。
type GrowthSection struct {
	Name      string         `json:"name"`
	Label     string         `json:"label"`
	Data      map[string]any `json:"data,omitempty"`
	Error     string         `json:"error,omitempty"`
	UpdatedAt string         `json:"updated_at,omitempty"`
}

// GrowthSnapshot 一个账号的成长中心快照。
type GrowthSnapshot struct {
	UID       string                   `json:"uid"`
	Sections  map[string]GrowthSection `json:"sections"`
	Errors    map[string]string        `json:"errors,omitempty"`
	UpdatedAt string                   `json:"updated_at"`
}

// GrowthSnapshot 拉取一个账号的成长中心快照（只读）。
//
// 逐段独立：任一段失败只影响它自己，其余段照常返回。这样上游某个活动下线时，
// 面板仍然能显示其它段，而不是整页空白。
func (c *Client) GrowthSnapshot(a *auth.Auth) *GrowthSnapshot {
	if a == nil {
		return nil
	}
	snap := &GrowthSnapshot{
		UID:      a.UID,
		Sections: make(map[string]GrowthSection, len(GrowthSectionDefs)),
		Errors:   map[string]string{},
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, def := range GrowthSectionDefs {
		raw, err := c.growthJSON(a, http.MethodGet, def.Path, nil)
		sec := GrowthSection{Name: def.Name, Label: def.Label}
		if err != nil {
			sec.Error = def.Label + "暂时不可用（" + err.Error() + "）"
			snap.Errors[def.Name] = sec.Error
			snap.Sections[def.Name] = sec
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			sec.Error = def.Label + "返回格式无法解析"
			snap.Errors[def.Name] = sec.Error
			snap.Sections[def.Name] = sec
			continue
		}
		sec.Data = ProjectGrowthSection(def.Name, data)
		sec.UpdatedAt = now
		snap.Sections[def.Name] = sec
	}
	snap.UpdatedAt = now
	return snap
}

// projectFields 按键名取子集（缺失的键不写进结果 —— 展示层据此区分"没有"与"零"）。
func projectFields(v any, names ...string) map[string]any {
	src, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	out := make(map[string]any, len(names))
	for _, n := range names {
		if val, exists := src[n]; exists {
			out[n] = val
		}
	}
	return out
}

func projectList(v any, names ...string) []map[string]any {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		out = append(out, projectFields(item, names...))
	}
	return out
}

// ProjectGrowthSection 白名单投影：只透出展示需要的字段。
//
// 为什么必须投影而不是原样透传：上游返回里夹带邮箱、物流地址、内部 id 之类的字段，
// 面板是给人看的，多透一个字段就多一份泄露面，而且前端也用不上。
func ProjectGrowthSection(name string, data map[string]any) map[string]any {
	switch name {
	case "heatmap":
		return map[string]any{
			"cells": projectList(data["cells"], "date", "score", "has_new_buddy"),
			"today": projectFields(data["today"], "date", "score", "is_active", "status_text"),
			"range": projectFields(data["range"], "start_date", "end_date", "days"),
		}
	case "streak":
		rs := data["redemption_status"]
		out := map[string]any{
			"streak": projectFields(data["streak"],
				"days", "month_total_days", "next_tier", "next_tier_remaining", "makeup_dates"),
			"redemption_status": projectFields(rs,
				"tier_7d_status", "tier_14d_status", "tier_28d_status",
				"tier_7d_count", "tier_14d_count", "tier_28d_count", "remaining_days"),
			"makeup_cards": projectFields(data["makeup_cards"], "balance", "max"),
			"timezone":     data["timezone"],
		}
		if rsm, ok := rs.(map[string]any); ok {
			out["redemption_status"].(map[string]any)["tiers"] =
				projectList(rsm["tiers"], "tier", "days", "credit", "energy", "cards", "chances")
		}
		return out
	case "buddy":
		b := data["buddy"]
		if b == nil {
			return map[string]any{"buddy": nil}
		}
		return map[string]any{"buddy": projectFields(b,
			"instance_id", "template_id", "name", "personality", "rarity",
			"thumbnail_url", "base_static_url")}
	case "buddies":
		return map[string]any{
			"buddies": projectList(data["buddies"],
				"instance_id", "name", "personality", "rarity",
				"thumbnail_url", "base_static_url", "acquired_at", "source", "current_buddy"),
			"count": data["count"],
		}
	case "travel":
		out := projectFields(data,
			"state", "depart_at", "arrive_at", "server_now", "daily_limit_reached", "reward_credit")
		out["location"] = projectFields(data["location"], "id", "code", "name", "duration_hours")
		return out
	case "lottery":
		return map[string]any{
			"chances": data["chances"],
			"module":  projectFields(data["module"], "enabled"),
		}
	case "energy":
		return projectFields(data, "balance", "total_consumed", "total_earned")
	case "quota":
		return projectFields(data, "balance", "affordable", "cost_per_open", "max_open_count")
	case "profile":
		return projectFields(data, "level", "level_name", "completed", "total", "max_level")
	case "redeem":
		return projectFields(data,
			"starter_count", "advanced_count", "legendary_count", "remaining_days", "month_total_days")
	}
	return map[string]any{}
}
