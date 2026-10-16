package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
)

// ---------------------------------------------------------------------------
// 上游「模型繁忙度」观测（官方桌面端 /v2/chat/busy）
// ---------------------------------------------------------------------------
//
// 端点形状来自同源实现（Python：xiaofan6ya/workbuddy2api 的 admin/model_busy.py
// 与 admin/backend.py），实测复现的关键片段：
//
//	POST {chatBase}/v2/chat/busy            ← 与 /v2/chat/completions 同 base
//	请求体:  {"models": ["模型名", ...]}     ← 单次最多 50 个（上游硬限制）
//	响应:    {"code": 0, "data": [{"model": "x", "busy_level": "idle|normal|busy|congested|unavailable"}, ...]}
//	超时:    5 秒（参考实现 httpx.Client(timeout=5)）
//
// 这是**观测数据**，不是路由依据：真正决定选号的是 pool 的冷却/熔断状态。
// 本文件不参与任何选号决策，也绝不允许拖慢 chat 主链路。

const (
	// modelBusyPath 官方桌面端繁忙度端点路径（与 chat 同 base）。
	modelBusyPath = "/v2/chat/busy"

	// modelBusyBatchMax 单次请求的模型数上限。上游硬限制，不是可调旋钮，超限整批会
	// 被拒——所以超出的模型必须分批，见 FetchModelBusy。
	modelBusyBatchMax = 50

	// modelBusyCacheMaxEntries 进程内缓存条目上限。缓存只是「可选观测」的优化，
	// 不设上限就等于给一个装饰性功能留一个慢速内存泄漏口子。
	modelBusyCacheMaxEntries = 128

	// modelBusyCacheKeyMaxModels 参与缓存键的模型数上限（4 批 = 200）。缓存键由
	// 模型名拼接而成，键长度必须和条目数一起有界——超限时直接放行到上游、不缓存。
	modelBusyCacheKeyMaxModels = modelBusyBatchMax * 4
)

// modelBusyTimeout 单批请求的整体上限。
//
// 为什么是 5 秒（而不是出站 client 默认的 120s）：繁忙度是可观测性装饰，主链路
// （chat）完全不依赖它。上游抖动时宁可这一屏数字晚一点或空着，也绝不能让一个观测
// 请求把面板或后台任务按分钟级挂住。取值与 Python 参考实现一致。
//
// 声明为 var 而非 const：测试需要把它压到毫秒级来验证超时路径——真跑 5 秒的超时
// 用例没有意义，只会拖慢 CI。生产代码不修改它。
var modelBusyTimeout = 5 * time.Second

// ModelBusy 单个模型的繁忙度观测。
//
// 字段依据 Python 参考实现的 normalize_response：上游只保证 "model" 与
// "busy_level" 两个键，其余键若有则原样保留在 Raw 里，不猜语义。
type ModelBusy struct {
	Model string `json:"model"`
	// Busy 数值繁忙度；上游未给数值键时恒 0（见 parseBusyItem 的取值顺序）。
	// 参考实现没有消费任何数值键，所以这个字段是宽松解析的产物，别拿它做阈值判断。
	Busy float64 `json:"busy"`
	// Level busy_level 原值（小写归一）；缺失/空 → "unknown"。
	// 已知档位：idle / normal / busy / congested / unavailable。
	// 本实现**不**把未知档位改写成 "unknown"——上游随时可能加档位，改写会丢真实信息。
	Level string `json:"level"`
	// Raw 上游原始条目，供上层读参考实现没消费的字段；不参与 JSON 输出。
	Raw json.RawMessage `json:"-"`
}

// busyItemShape /v2/chat/busy data 数组里单条的已知键。
// 备用键名（level / busy_rate / load）是防御性宽松解析：上游若换键名，这里仍能取到，
// 取不到也不报错——观测数据不值得为键名漂移整批失败。
type busyItemShape struct {
	Model     string   `json:"model"`
	BusyLevel string   `json:"busy_level"`
	Level     string   `json:"level"`
	Busy      *float64 `json:"busy"`
	BusyRate  *float64 `json:"busy_rate"`
	Load      *float64 `json:"load"`
}

// parseBusyItem 解析 data 数组里的一条；无模型名的条目直接丢弃（无法归属就是噪声）。
func parseBusyItem(raw json.RawMessage) (ModelBusy, bool) {
	var s busyItemShape
	if err := json.Unmarshal(raw, &s); err != nil {
		return ModelBusy{}, false
	}
	model := strings.TrimSpace(s.Model)
	if model == "" {
		return ModelBusy{}, false
	}
	level := strings.TrimSpace(s.BusyLevel)
	if level == "" {
		level = strings.TrimSpace(s.Level)
	}
	if level == "" {
		level = "unknown" // 与参考实现 LABELS 的缺省档一致
	}
	busy := 0.0
	switch {
	case s.Busy != nil:
		busy = *s.Busy
	case s.BusyRate != nil:
		busy = *s.BusyRate
	case s.Load != nil:
		busy = *s.Load
	}
	return ModelBusy{
		Model: model,
		Busy:  busy,
		Level: strings.ToLower(level),
		Raw:   append(json.RawMessage(nil), raw...),
	}, true
}

// normalizeBusyModels 去空格 + 去重 + 升序排序。
//
// 排序不只是好看：缓存键由模型名拼接而成（见 modelBusyCacheKey），同一组模型的不同
// 排列必须命中同一条缓存，否则缓存形同虚设；分批结果也因此稳定可断言。
func normalizeBusyModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// busyHTTPClient 返回非流式出站 client；零值 Client（测试只注入 HTTP 之外的字段）
// 时回落默认 client，避免 nil 解引用。
func (c *Client) busyHTTPClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// FetchModelBusy 调上游 /v2/chat/busy 拉取给定模型的繁忙度。
//
// 强制分批（每批 ≤ modelBusyBatchMax）：上游单请求上限 50 个，超限会被整批拒绝，
// 不是「自动截断到 50 个」——所以必须由调用方（本函数）切开。
//
// 失败语义：任一批失败即返回 (nil, err)，**不返回部分结果**。理由：半份观测比没有
// 更危险——缺的那几个模型会被界面读成「不拥堵」。宁可整块显示为空。
//
// models 为空、a 为 nil 时返回 (nil, nil)：不构成错误，只是没东西可查。
func (c *Client) FetchModelBusy(a *auth.Auth, models []string) ([]ModelBusy, error) {
	uniq := normalizeBusyModels(models)
	if a == nil || len(uniq) == 0 {
		return nil, nil
	}
	out := make([]ModelBusy, 0, len(uniq))
	for start := 0; start < len(uniq); start += modelBusyBatchMax {
		end := start + modelBusyBatchMax
		if end > len(uniq) {
			end = len(uniq)
		}
		items, err := c.fetchModelBusyBatch(a, uniq[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// fetchModelBusyBatch 发一批（≤50 个）并解析信封。
// 头族复用 CommonHeaders（Origin/Referer/UA/X-CodeBuddy-Request/账号设备头），
// 与 fetchEnterpriseModels 同一条非 chat 出站路径——不造第二套指纹。
func (c *Client) fetchModelBusyBatch(a *auth.Auth, batch []string) ([]ModelBusy, error) {
	body, err := json.Marshal(struct {
		Models []string `json:"models"`
	}{Models: batch})
	if err != nil {
		return nil, fmt.Errorf("model busy marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelBusyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.chatBase(a)+modelBusyPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("model busy request: %w", err)
	}
	c.CommonHeaders(req, a)
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	resp, err := c.busyHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("model busy http: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("model busy read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("model busy status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int               `json:"code"`
		Msg  string            `json:"msg"`
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("model busy parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("model busy code=%d msg=%s", env.Code, truncate(env.Msg, 120))
	}
	out := make([]ModelBusy, 0, len(env.Data))
	for _, item := range env.Data {
		if mb, ok := parseBusyItem(item); ok {
			out = append(out, mb)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 进程内缓存
// ---------------------------------------------------------------------------

// modelBusyCache 进程内繁忙度缓存（Mutex + map + 到期时刻）。
//
// 为什么是包级而不是 Client 的字段：Client 定义在 client.go 里，本次改动不允许碰
// 它（见任务约束：只新增文件）。生产上 Client 由 upstream.New() 在进程内构造一次，
// 包级与字段级行为等价；键里已含 realm + uid，多个 Client 共用一桶也不会串号。
type modelBusyCache struct {
	mu      sync.Mutex
	entries map[string]modelBusyCacheEntry
}

type modelBusyCacheEntry struct {
	items    []ModelBusy
	expireAt time.Time
}

var modelBusyMemo = &modelBusyCache{entries: make(map[string]modelBusyCacheEntry)}

// modelBusyCacheKey 缓存键 = realm + uid + 排序后的模型列表。
//
// 按 uid（而非只看 realm）分桶：不同账号看到的繁忙度可能不同，混桶会把 A 号的观测
// 当成 B 号的。模型数超过 modelBusyCacheKeyMaxModels 时返回空串表示「不缓存」。
func modelBusyCacheKey(a *auth.Auth, models []string) string {
	if a == nil || len(models) == 0 || len(models) > modelBusyCacheKeyMaxModels {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(a.Realm())
	sb.WriteByte('|')
	sb.WriteString(a.UID)
	for _, m := range models {
		sb.WriteByte('|')
		sb.WriteString(m)
	}
	return sb.String()
}

// get 取未过期的缓存；过期条目顺手删除，不给「过期但还占位」的机会。
func (c *modelBusyCache) get(key string) ([]ModelBusy, bool) {
	if c == nil || key == "" {
		return nil, false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !now.Before(e.expireAt) {
		delete(c.entries, key)
		return nil, false
	}
	return e.items, true
}

// put 写入缓存：先清过期，仍超上限就淘汰最早到期的一条。
func (c *modelBusyCache) put(key string, items []ModelBusy, ttl time.Duration) {
	if c == nil || key == "" || ttl <= 0 {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if !now.Before(e.expireAt) {
			delete(c.entries, k)
		}
	}
	if len(c.entries) >= modelBusyCacheMaxEntries {
		c.evictLocked()
	}
	c.entries[key] = modelBusyCacheEntry{items: items, expireAt: now.Add(ttl)}
}

// evictLocked 淘汰最早到期的一条。O(n) 扫描（n ≤ 128），不值得为此上堆。
func (c *modelBusyCache) evictLocked() {
	victim := ""
	var victimExp time.Time
	for k, e := range c.entries {
		if victim == "" || e.expireAt.Before(victimExp) {
			victim, victimExp = k, e.expireAt
		}
	}
	if victim != "" {
		delete(c.entries, victim)
	}
}

// FetchModelBusyCached 带进程内缓存的 FetchModelBusy：同一 (realm, uid, 模型集合)
// 在 ttl 内不重复打上游。
//
// ttl<=0 退化为直通（不读不写缓存）。
// 失败不入缓存：一次上游抖动不该被固化成整个 TTL 的黑洞，下次请求照常重试。
// 返回的切片是共享只读视图，调用方不得原地修改。
func (c *Client) FetchModelBusyCached(a *auth.Auth, models []string, ttl time.Duration) ([]ModelBusy, error) {
	uniq := normalizeBusyModels(models)
	if a == nil || len(uniq) == 0 {
		return nil, nil
	}
	if ttl <= 0 {
		return c.FetchModelBusy(a, uniq)
	}
	key := modelBusyCacheKey(a, uniq)
	if key == "" {
		return c.FetchModelBusy(a, uniq) // 模型太多，键过长 → 不缓存
	}
	if items, ok := modelBusyMemo.get(key); ok {
		return items, nil
	}
	items, err := c.FetchModelBusy(a, uniq)
	if err != nil {
		return nil, err
	}
	modelBusyMemo.put(key, items, ttl)
	return items, nil
}

// resetModelBusyCache 清空进程内缓存。仅供同包测试使用，生产无调用点。
func resetModelBusyCache() {
	modelBusyMemo.mu.Lock()
	modelBusyMemo.entries = make(map[string]modelBusyCacheEntry)
	modelBusyMemo.mu.Unlock()
}
