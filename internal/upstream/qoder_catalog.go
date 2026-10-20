// qoder_catalog.go —— Qoder 双区模型清单。
//
// 数据来源是官方客户端导出的目录快照（qoder_catalog_cn.json / _intl.json）。
// 动态接口 /algo/api/v2/model/list 需要 COSY 签名 GET，且字段随版本漂移；
// 快照足够稳定，也不会在没登录时把面板拖成空白。
//
// 两区清单不同：CN 有 q37fmodel / gm51model，Intl 有 smodel / cmodel / ultimate。
package upstream

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

//go:embed qoder_catalog_cn.json qoder_catalog_intl.json
var qoderCatalogFS embed.FS

// QoderCatalogEntry 面板展示用的模型条目。
// 字段名与官方目录一一对应，只是把嵌套结构拍平了。
type QoderCatalogEntry struct {
	Key string `json:"key"`
	// Slug 是「模型名转小写去空格」的调用名：客户端看到 qodercn:deepseek-flash，
	// 网关再用它反查回 key（dfmodel）。
	Slug      string  `json:"slug"`
	Name      string  `json:"name"`
	Realm     string  `json:"realm"`
	Enabled   bool    `json:"enabled"`
	Vision    bool    `json:"vision"`
	Reasoning bool    `json:"reasoning"`
	Default   bool    `json:"default"`
	IsNew     bool    `json:"is_new"`
	IsFree    bool    `json:"is_free"`
	PriceRate float64 `json:"price_rate"`
	OrigPrice float64 `json:"original_price_rate,omitempty"`
	Context   int64   `json:"context"`

	// Contexts 可选窗口标签（如 200K / 400K / 1M），按 token 数升序。
	Contexts       []string `json:"contexts,omitempty"`
	DefaultContext string   `json:"default_context,omitempty"`

	// ThinkEfforts 思考档位（low/medium/high/xhigh/max），按固定顺序。
	ThinkEfforts []string `json:"think_efforts,omitempty"`
	DefaultThink string   `json:"default_think,omitempty"`
	CanDisable   bool     `json:"can_disable_thinking"`

	// 峰谷价
	PromoActive bool    `json:"promo_active"`
	PromoBadge  string  `json:"promo_badge,omitempty"`
	PromoDesc   string  `json:"promo_desc,omitempty"`
	PromoOff    float64 `json:"promo_off,omitempty"`
}

type qoderCatalogRaw struct {
	Key            string  `json:"key"`
	DisplayName    string  `json:"display_name"`
	Enable         bool    `json:"enable"`
	IsVL           bool    `json:"is_vl"`
	IsReasoning    bool    `json:"is_reasoning"`
	IsDefault      bool    `json:"is_default"`
	IsNew          bool    `json:"is_new"`
	IsFree         bool    `json:"is_free"`
	PriceFactor    float64 `json:"price_factor"`
	OriginalPrice  float64 `json:"original_price_factor"`
	MaxInputTokens int64   `json:"max_input_tokens"`
	ContextConfig  map[string]struct {
		TokenCount int64 `json:"token_count"`
		IsDefault  bool  `json:"is_default"`
	} `json:"context_config"`
	ThinkingConfig struct {
		Disabled json.RawMessage `json:"disabled"`
		Enabled  struct {
			Efforts map[string]struct {
				IsDefault bool `json:"is_default"`
			} `json:"efforts"`
			IsDefault bool `json:"is_default"`
		} `json:"enabled"`
	} `json:"thinking_config"`
	Promotion struct {
		Active         bool    `json:"active"`
		DiscountFactor float64 `json:"discount_factor"`
		Badge          struct {
			Zh string `json:"zh"`
			En string `json:"en"`
		} `json:"badge"`
		Description struct {
			Zh string `json:"zh"`
			En string `json:"en"`
		} `json:"description"`
	} `json:"promotion"`
}

// qoderThinkOrder 思考档位的展示顺序（官方从低到高）。
var qoderThinkOrder = []string{"low", "medium", "high", "xhigh", "max"}

var (
	qoderCatalogOnce sync.Once
	qoderCatalogCN   []QoderCatalogEntry
	qoderCatalogIntl []QoderCatalogEntry
	qoderCatalogErr  error
)

func loadQoderCatalog() {
	qoderCatalogOnce.Do(func() {
		qoderCatalogCN, qoderCatalogErr = readQoderCatalog("qoder_catalog_cn.json", "cn")
		if qoderCatalogErr != nil {
			return
		}
		qoderCatalogIntl, qoderCatalogErr = readQoderCatalog("qoder_catalog_intl.json", "intl")
	})
}

func readQoderCatalog(name, realm string) ([]QoderCatalogEntry, error) {
	raw, err := qoderCatalogFS.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var list []qoderCatalogRaw
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("qoder: 模型目录 %s 解析失败: %w", name, err)
	}
	return buildQoderCatalogEntries(list, realm), nil
}

// buildQoderCatalogEntries 把官方原始条目拍平成面板用的结构。
// 动态接口与内嵌快照共用这一份，保证两条数据源口径一致。
func buildQoderCatalogEntries(list []qoderCatalogRaw, realm string) []QoderCatalogEntry {
	out := make([]QoderCatalogEntry, 0, len(list))
	for _, m := range list {
		if m.Key == "" {
			continue
		}
		slug := qoderSlug(m.DisplayName)
		if slug == "" {
			slug = m.Key
		}
		e := QoderCatalogEntry{
			Key:       m.Key,
			Slug:      slug,
			Name:      m.DisplayName,
			Realm:     realm,
			Enabled:   m.Enable,
			Vision:    m.IsVL,
			Reasoning: m.IsReasoning,
			Default:   m.IsDefault,
			IsNew:     m.IsNew,
			IsFree:    m.IsFree,
			PriceRate: m.PriceFactor,
			OrigPrice: m.OriginalPrice,
			Context:   m.MaxInputTokens,
		}
		if len(m.ContextConfig) > 0 {
			type win struct {
				label string
				n     int64
			}
			var wins []win
			for label, w := range m.ContextConfig {
				wins = append(wins, win{label, w.TokenCount})
				if w.IsDefault {
					e.DefaultContext = label
				}
			}
			sort.Slice(wins, func(i, j int) bool { return wins[i].n < wins[j].n })
			for _, w := range wins {
				e.Contexts = append(e.Contexts, w.label)
			}
		}
		if len(m.ThinkingConfig.Enabled.Efforts) > 0 {
			for _, name := range qoderThinkOrder {
				if _, ok := m.ThinkingConfig.Enabled.Efforts[name]; ok {
					e.ThinkEfforts = append(e.ThinkEfforts, name)
					if m.ThinkingConfig.Enabled.Efforts[name].IsDefault {
						e.DefaultThink = name
					}
				}
			}
			var extra []string
			for name := range m.ThinkingConfig.Enabled.Efforts {
				found := false
				for _, k := range qoderThinkOrder {
					if k == name {
						found = true
						break
					}
				}
				if !found {
					extra = append(extra, name)
				}
			}
			sort.Strings(extra)
			e.ThinkEfforts = append(e.ThinkEfforts, extra...)
		}
		e.CanDisable = len(m.ThinkingConfig.Disabled) > 0 &&
			string(m.ThinkingConfig.Disabled) != "null" && len(e.ThinkEfforts) > 0
		e.PromoActive = m.Promotion.Active
		e.PromoBadge = m.Promotion.Badge.Zh
		if e.PromoBadge == "" {
			e.PromoBadge = m.Promotion.Badge.En
		}
		e.PromoDesc = m.Promotion.Description.Zh
		if e.PromoDesc == "" {
			e.PromoDesc = m.Promotion.Description.En
		}
		e.PromoOff = m.Promotion.DiscountFactor
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// QoderCatalog 返回某区域的模型清单；realm 为空时返回两区合并（按 key 去重）。
func QoderCatalog(realm string) []QoderCatalogEntry {
	loadQoderCatalog()
	switch realm {
	case "cn":
		return append([]QoderCatalogEntry(nil), qoderCatalogCN...)
	case "intl":
		return append([]QoderCatalogEntry(nil), qoderCatalogIntl...)
	}
	seen := map[string]bool{}
	out := make([]QoderCatalogEntry, 0, len(qoderCatalogCN)+len(qoderCatalogIntl))
	for _, e := range append(append([]QoderCatalogEntry{}, qoderCatalogIntl...), qoderCatalogCN...) {
		if seen[e.Key] {
			continue
		}
		seen[e.Key] = true
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// qoderSlug 把官方模型名压成可调用的小写 slug：
// "DeepSeek-Flash" → "deepseek-flash"、"Qwen3.8-Max" → "qwen3.8-max"。
func qoderSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '-' || r == '_' || r == '/' || r == '(' || r == ')':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			// 中文/其它符号直接丢弃
		}
	}
	return strings.Trim(b.String(), "-")
}

// QoderResolveKey 用 slug 反查上游 key；也接受直接传 key（兼容旧写法）。
func QoderResolveKey(realm, name string) string {
	if name == "" {
		return name
	}
	for _, e := range QoderCatalog(realm) {
		if strings.EqualFold(e.Slug, name) || strings.EqualFold(e.Key, name) {
			return e.Key
		}
	}
	// 动态目录里可能有快照没有的模型：原样传下去，让上游自己判断。
	return name
}
