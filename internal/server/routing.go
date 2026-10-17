// routing.go —— 模型别名、模型级降级链、上游时段闸门。
//
// 三件事放在一起是因为它们共享同一个"请求进来先决定打到哪"的阶段：
//
//	别名   fast           → cn:glm-5.2
//	降级   cn:deepseek    → [cn:glm-5.2, qoder:auto]
//	时段   qoder 只在 20:00-08:00 开放
//
// 解析顺序固定为「别名 → 时段 → 降级链」。别名先展开，否则用户写 fast 时
// 降级表按 "fast" 去查永远查不到；时段在降级链之前判，避免把请求降级到一个
// 当前关停的上游上。
package server

import (
	"fmt"
	"strings"
	"time"

	"github.com/syan-anan/wb-syan/internal/keys"
	"github.com/syan-anan/wb-syan/internal/upstream"
)

// RouteTable 只读路由表，由配置构建后不再修改（并发安全）。
type RouteTable struct {
	// aliases 别名 → 目标模型（目标可再带 cn:/global:/qoder: 前缀）。
	aliases map[string]string
	// fallback 模型 → 降级链（按顺序尝试）。键可以是带前缀的全名，也可以是裸名。
	fallback map[string][]string
	// windows 上游名（workbuddy / qoder）→ 允许的时段。
	windows map[string][]HourRange
}

// HourRange 一个每日生效时段（本地时区，支持跨零点如 22:00-06:00）。
type HourRange struct {
	Start int // 当日第几分钟
	End   int
}

// NewRouteTable 构建路由表。空表必须安全：调用方未配置时所有方法退化为恒等。
func NewRouteTable(aliases map[string]string, fallback map[string][]string,
	windows map[string][]HourRange) *RouteTable {
	return &RouteTable{aliases: aliases, fallback: fallback, windows: windows}
}

// emptyRouteTable 未配置时的零值表（所有方法返回恒等结果）。
var emptyRouteTable = NewRouteTable(nil, nil, nil)

// Expand 展开别名（可多级别名，最多 8 层防环）。
//
// 返回展开后的模型名。未命中别名时原样返回。环引用返回原串并附带 error，
// 调用方把 error 当配置错误报 500——静默取一个环里的值会让线上行为无法解释。
func (rt *RouteTable) Expand(model string) (string, error) {
	if rt == nil || len(rt.aliases) == 0 {
		return model, nil
	}
	cur := model
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		next, ok := rt.aliases[cur]
		if !ok {
			return cur, nil
		}
		if seen[cur] {
			return model, fmt.Errorf("alias 存在环引用: %s", cur)
		}
		seen[cur] = true
		cur = next
	}
	return model, fmt.Errorf("alias 嵌套超过 8 层: %s", model)
}

// Chain 返回该模型的降级链（首元素是模型自身）。
//
// 查表顺序：全名（"cn:deepseek"）→ 裸名（"deepseek"）。两者都支持是因为用户
// 配降级时通常只关心模型、不关心域；但同名模型跨域时又需要能精确指定。
// 链里会去重并剔除自身，避免 "a → a" 这种自指配置把重试次数白吃掉。
func (rt *RouteTable) Chain(model string) []string {
	chain := []string{model}
	if rt == nil || len(rt.fallback) == 0 {
		return chain
	}
	seen := map[string]bool{model: true}
	add := func(list []string) {
		for _, m := range list {
			m = strings.TrimSpace(m)
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			chain = append(chain, m)
		}
	}
	add(rt.fallback[model])
	if bare := bareModelName(model); bare != model {
		add(rt.fallback[bare])
	}
	return chain
}

// bareModelName 只做"去掉已知上游前缀"这一件事，不依赖 resolveModel 的 realm 语义
// （qoder: 不是 realm，但降级表同样希望按裸名命中）。
func bareModelName(model string) string {
	for _, p := range []string{"cn:", "global:", qoderModelPrefix} {
		if strings.HasPrefix(model, p) {
			return strings.TrimPrefix(model, p)
		}
	}
	return model
}

// AllowedNow 报告某上游此刻是否在允许时段内。未配置时段 = 恒允许。
func (rt *RouteTable) AllowedNow(upstreamName string, now time.Time) bool {
	if rt == nil {
		return true
	}
	ranges := rt.windows[upstreamName]
	if len(ranges) == 0 {
		return true
	}
	minute := now.Hour()*60 + now.Minute()
	for _, r := range ranges {
		if r.Start <= r.End {
			if minute >= r.Start && minute < r.End {
				return true
			}
			continue
		}
		// 跨零点：22:00-06:00 拆成 [22:00,24:00) ∪ [00:00,06:00)
		if minute >= r.Start || minute < r.End {
			return true
		}
	}
	return false
}

// upstreamOf 从模型名推断上游名。用于时段闸门与降级过滤。
func upstreamOf(model string) string {
	if strings.HasPrefix(model, qoderModelPrefix) {
		return "qoder"
	}
	return "workbuddy"
}

// FilterChain 按"当前可服务"过滤降级链：剔除时段关闭的上游。
//
// 首元素（用户显式请求的模型）不在这里剔除——它关停时应该明确报错，而不是
// 静默换成别的模型；只有降级候选才需要过滤。返回的链可能只剩首元素。
func (rt *RouteTable) FilterChain(chain []string, now time.Time) []string {
	if rt == nil || len(chain) <= 1 {
		return chain
	}
	out := []string{chain[0]}
	for _, m := range chain[1:] {
		if rt.AllowedNow(upstreamOf(m), now) {
			out = append(out, m)
		}
	}
	return out
}

// AliasList 返回别名表快照（面板展示用，按名字排序由调用方处理）。
func (rt *RouteTable) AliasList() map[string]string {
	if rt == nil || len(rt.aliases) == 0 {
		return nil
	}
	out := make(map[string]string, len(rt.aliases))
	for k, v := range rt.aliases {
		out[k] = v
	}
	return out
}

// FallbackList 返回降级表快照（面板展示用）。
func (rt *RouteTable) FallbackList() map[string][]string {
	if rt == nil || len(rt.fallback) == 0 {
		return nil
	}
	out := make(map[string][]string, len(rt.fallback))
	for k, v := range rt.fallback {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// WindowList 返回时段配置的字符串形式（面板展示用）。
func (rt *RouteTable) WindowList() map[string][]string {
	if rt == nil || len(rt.windows) == 0 {
		return nil
	}
	out := make(map[string][]string, len(rt.windows))
	for up, ranges := range rt.windows {
		list := make([]string, 0, len(ranges))
		for _, r := range ranges {
			list = append(list, fmt.Sprintf("%02d:%02d-%02d:%02d",
				r.Start/60, r.Start%60, r.End/60, r.End%60))
		}
		out[up] = list
	}
	return out
}

// routingSummary 面板只读快照：别名 / 降级 / 时段。
func (h *Handler) routingSummary() map[string]any {
	if h.cfg.Routes == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled":  true,
		"aliases":  h.cfg.Routes.AliasList(),
		"fallback": h.cfg.Routes.FallbackList(),
		"hours":    h.cfg.Routes.WindowList(),
	}
}

// upstreamSummary 多上游概览（面板顶部的"连了几家"）。
//
// WorkBuddy 的账号数不在这里重复统计——pool 的 /status 已有一整套
// accounts/total/healthy，这里只标出"存在哪些上游、各自什么形态"。
func (h *Handler) upstreamSummary() []map[string]any {
	out := []map[string]any{{
		"name":    "workbuddy",
		"kind":    "account-pool",
		"label":   "WorkBuddy（账号池）",
		"enabled": true,
	}}
	if h.cfg.Qoder != nil {
		out = append(out, map[string]any{
			"name":          "qoder",
			"kind":          "pat-pool",
			"label":         "Qoder（PAT 池）",
			"enabled":       true,
			"accounts":      h.cfg.Qoder.Len(),
			"account_names": h.cfg.Qoder.Names(),
			"models":        upstream.QoderModels(),
		})
	}
	// 通用 OpenAI 兼容上游（provider 名来自配置）。
	out = append(out, h.compatSummary()...)
	return out
}

// aliasAllowedForIdentity 判断别名是否对该调用方可见：别名目标必须通过
// 子密钥的模型/域白名单，否则受限 key 能借别名绕过白名单。
func aliasAllowedForIdentity(target string, id keys.Identity) bool {
	if id.Master {
		return true
	}
	realm, bare := resolveModel(target)
	return id.AllowsRealm(realm) && id.AllowsModel(bare)
}

// ParseHourRanges 解析 ["22:00-06:00", "09:00-12:00"] 形式的时段配置。
// 空列表返回 nil（= 全天允许）。格式错误返回 error（配置期就该炸，不要留到线上）。
func ParseHourRanges(specs []string) ([]HourRange, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]HourRange, 0, len(specs))
	for _, s := range specs {
		parts := strings.SplitN(strings.TrimSpace(s), "-", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("时段格式应为 HH:MM-HH:MM: %q", s)
		}
		a, ok := keys.ParseClock(strings.TrimSpace(parts[0]))
		if !ok {
			return nil, fmt.Errorf("时段起点非法: %q", s)
		}
		b, ok := keys.ParseClock(strings.TrimSpace(parts[1]))
		if !ok {
			return nil, fmt.Errorf("时段终点非法: %q", s)
		}
		out = append(out, HourRange{Start: a, End: b})
	}
	return out, nil
}
