package panel

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/upstream"
)

// ---------------------------------------------------------------------------
// 模型繁忙度 + 延迟抖动（两个只读端点）
// ---------------------------------------------------------------------------
//
// 两个端点都是可选增强：拿不到数据一律 200 + 空数组，绝不因为观测能力出问题
// 而让面板报错或变慢。
//
// 路由由 panel.go 侧接线，本文件不注册任何路由（本次改动不允许碰 panel.go）：
//
//	GET /panel/api/model_busy?models=cn:glm-5.2,global:gpt-5.6-sol[&limit=N]
//	GET /panel/api/model_latency_jitter?days=7
//
// 接线方式（两行）：
//
//	p.mux.HandleFunc("GET /panel/api/model_busy", p.withAuth(p.modelBusy))
//	p.mux.HandleFunc("GET /panel/api/model_latency_jitter", p.withAuth(p.modelLatencyJitter))
//
// 缓存说明：两个缓存都是包级单例——本次改动不允许给 Panel 加字段（只新增文件），
// 生产上 Panel 也是进程内单例，行为等价；键里已含 realm + uid，多个 Panel 共用
// 一桶也不会串号。两者都带上限与过期清理。

const (
	// busyCacheMaxEntries 面板侧繁忙度缓存条目上限。
	busyCacheMaxEntries = 64

	// busyCacheTTL 面板侧繁忙度缓存时长。为什么 50 秒：与 Python 参考实现的
	// CACHE_SECONDS 一致。上游繁忙度是分钟级变化量，50s 既不会把「刚变拥堵」藏太久，
	// 也足够让面板连点 / 多标签页只打一次上游。
	busyCacheTTL = 50 * time.Second

	// busyUpstreamTTL 传给 upstream.FetchModelBusyCached 的 TTL。两层缓存同寿命，
	// 避免出现「面板缓存已过期但上游缓存还热」这种看不出来源的状态。
	busyUpstreamTTL = 50 * time.Second

	// busyDefaultLimit 未显式传 limit 时最多查多少个模型（键长度与批次数都随它增长）。
	busyDefaultLimit = 50

	// busyMaxLimit 单次查询模型数硬上限（4 批 = 200）：再多就不是「看板上几个模型」，
	// 而是把上游当批量接口用，不是本端点的用途。
	busyMaxLimit = 200

	// jitterDefaultDays / jitterMaxDays 抖动窗口默认值与上限（上限与 usage 侧一致）。
	jitterDefaultDays = 7
	jitterMaxDays     = 90

	// jitterMinSamples 抖动的最低样本数：低于它不出分位数（见 computeJitter）。
	jitterMinSamples = 2
)

// ---------------------------------------------------------------------------
// 面板侧繁忙度缓存
// ---------------------------------------------------------------------------

// modelBusyCache 面板侧繁忙度缓存：key → 结果 + 到期时刻。
// 只缓存成功结果；失败返回空数组且不落缓存，下次请求自然重试。
type modelBusyCache struct {
	mu      sync.Mutex
	entries map[string]modelBusyCacheEntry
}

type modelBusyCacheEntry struct {
	items    []upstream.ModelBusy
	expireAt time.Time
}

var panelBusyCache = &modelBusyCache{entries: make(map[string]modelBusyCacheEntry)}

func (c *modelBusyCache) get(key string) ([]upstream.ModelBusy, bool) {
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

func (c *modelBusyCache) put(key string, items []upstream.ModelBusy, ttl time.Duration) {
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
	if len(c.entries) >= busyCacheMaxEntries {
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
	c.entries[key] = modelBusyCacheEntry{items: items, expireAt: now.Add(ttl)}
}

// panelBusyCacheKey 键 = realm + uid + 模型列表（已排序，故与传入顺序无关）。
// 返回空串表示不缓存（无账号 / 无模型），调用方原样放行到上游。
func panelBusyCacheKey(a *auth.Auth, models []string) string {
	if a == nil || len(models) == 0 {
		return ""
	}
	return a.Realm() + "|" + a.UID + "|" + strings.Join(models, ",")
}

// resetPanelBusyCache 清空面板侧缓存。仅供同包测试使用，生产无调用点。
func resetPanelBusyCache() {
	panelBusyCache.mu.Lock()
	panelBusyCache.entries = make(map[string]modelBusyCacheEntry)
	panelBusyCache.mu.Unlock()
}

// ---------------------------------------------------------------------------
// 端点 1：繁忙度
// ---------------------------------------------------------------------------

// modelBusy GET /panel/api/model_busy?models=a,b,c[&limit=N]
//
// 契约：{"ok":true,"busy":[{"model":...,"busy":...,"level":...},...],"cached":bool}
// 拿不到数据（无可用账号 / 未给模型 / 上游出错）→ {"ok":true,"busy":[],"cached":false}，
// 而且一定是 200：观测能力缺失不是面板的故障，不该在界面上变成一片红。
//
// 模型列表由调用方给（前端已经有 /panel/api/models 的目录），不在本端点内部去探测
// 上游目录——那会把一个轻量观测端点变成两次重探测。
func (p *Panel) modelBusy(w http.ResponseWriter, r *http.Request) {
	empty := map[string]any{"ok": true, "busy": []upstream.ModelBusy{}, "cached": false}

	models := parseBusyModels(r.URL.Query().Get("models"))
	limit := parseLimit(r.URL.Query().Get("limit"), busyDefaultLimit, busyMaxLimit)
	if len(models) > limit {
		models = models[:limit]
	}
	if len(models) == 0 || p.cfg.Upstream == nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	acct := p.busyProbeAccount(models)
	if acct == nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}

	key := panelBusyCacheKey(acct, models)
	if items, ok := panelBusyCache.get(key); ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "busy": items, "cached": true})
		return
	}
	items, err := p.cfg.Upstream.FetchModelBusyCached(acct, models, busyUpstreamTTL)
	if err != nil || len(items) == 0 {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	panelBusyCache.put(key, items, busyCacheTTL)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "busy": items, "cached": false})
}

// parseBusyModels 解析逗号分隔的模型列表：去 realm 前缀（前端显示值形如
// "cn:glm-5.2"，而调用上游只能用裸名）、去空格、去重、升序。
// 排序保证同一组模型的不同排列命中同一条缓存键。
func parseBusyModels(raw string) []string {
	out := make([]string, 0, 16)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if i := strings.IndexByte(part, ':'); i >= 0 {
			part = strings.TrimSpace(part[i+1:])
		}
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	sort.Strings(out)
	dedup := out[:0]
	for i, m := range out {
		if i > 0 && m == out[i-1] {
			continue
		}
		dedup = append(dedup, m)
	}
	return dedup
}

// busyProbeAccount 选一个探测账号：按请求里模型的前缀偏好 realm（"global:" → 国际版），
// 该域没有可用账号再退到另一域。只借凭证，不做 Acquire/Release——观测请求不该占用
// 选号池的在途配额。
func (p *Panel) busyProbeAccount(models []string) *auth.Auth {
	if p.cfg.Pool == nil {
		return nil
	}
	prefer := "cn"
	for _, m := range models {
		if strings.HasPrefix(m, "global:") {
			prefer = "global"
			break
		}
	}
	for _, realm := range []string{prefer, "cn", "global"} {
		if uids := p.cfg.Pool.AvailableUIDsForRealm(realm); len(uids) > 0 {
			if a := p.cfg.Pool.AuthByUID(uids[0]); a != nil {
				return a
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 端点 2：延迟抖动
// ---------------------------------------------------------------------------

// modelJitter 单模型的延迟抖动视图。
type modelJitter struct {
	Model   string  `json:"model"`
	Samples int     `json:"samples"`
	MeanMs  float64 `json:"mean_ms"`
	P50Ms   float64 `json:"p50_ms"`
	P95Ms   float64 `json:"p95_ms"`
	StdMs   float64 `json:"std_ms"`
	CV      float64 `json:"cv"`
}

// modelLatencyJitter GET /panel/api/model_latency_jitter?days=7
//
// 契约：{"ok":true,"days":N,"models":[{"model":...,"samples":...,"mean_ms":...,
// "p50_ms":...,"p95_ms":...,"std_ms":...,"cv":...}, ...]}
//
// 只读 internal/usage 已有的分桶，不触发任何上游调用。这个端点可以在面板里被反复
// 刷新，成本是零。
//
// 样本定义见 usage.ModelLatencySamples：一个样本 = 一个分桶（时间片 × 账号 × 模型 ×
// 客户端）的延迟均值，因为分桶里没有逐请求原始延迟。
func (p *Panel) modelLatencyJitter(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "days": 0, "models": []modelJitter{}})
		return
	}
	days := parseLimit(r.URL.Query().Get("days"), jitterDefaultDays, jitterMaxDays)
	samples := p.cfg.Usage.ModelLatencySamples(days)
	out := make([]modelJitter, 0, len(samples))
	for model, vals := range samples {
		out = append(out, computeJitter(model, vals))
	}
	// 抖动最大的排前面（cv 降序）；同 cv 按模型名升序，输出稳定，前端 diff 不抖。
	sort.Slice(out, func(i, j int) bool {
		if out[i].CV != out[j].CV {
			return out[i].CV > out[j].CV
		}
		return out[i].Model < out[j].Model
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "days": days, "models": out})
}

// computeJitter 由一列分桶均值算抖动。
//
// 为什么用变异系数（CV = 标准差 / 均值）而不是直接比标准差：标准差是绝对量，
// 快模型天然比慢模型的绝对抖动小，按标准差排名永远是「最慢的模型抖动最大」，
// 那只是把延迟榜重排一遍，没有新信息。CV 是相对量，才回答「哪个模型的时间不可预测」。
//
// 样本不足（少于 jitterMinSamples）时的行为：不出分位数（P50 = P95 = 唯一的均值），
// Std 与 CV 记 0，并用 Samples 如实告知样本只有 1 个。既不返回 null 也不报错：
// 单样本不是错误，只是还谈不上「抖动」，调用方看 Samples 就知道该不该信。
func computeJitter(model string, samples []float64) modelJitter {
	j := modelJitter{Model: model, Samples: len(samples)}
	if len(samples) == 0 {
		return j
	}
	var sum float64
	for _, v := range samples {
		sum += v
	}
	mean := sum / float64(len(samples))
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	j.MeanMs = mean
	j.P50Ms = percentile(sorted, 0.50)
	j.P95Ms = percentile(sorted, 0.95)
	if len(samples) >= jitterMinSamples {
		j.StdMs = sampleStdDev(sorted, mean)
		if mean > 0 {
			j.CV = j.StdMs / mean
		}
	}
	return j
}

// percentile 线性插值分位数（与 numpy 默认 method="linear" 同口径）：
// 位置 = p × (n-1)，落在两个样本之间就按比例插值。
// 为什么不用「最近秩」：分桶均值样本量通常很小（几到几十），最近秩会让分位数在样本
// 增删一格时整档跳变，插值平滑得多。
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 || p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[n-1]
	}
	pos := p * float64(n-1)
	lo := int(math.Floor(pos))
	hi := lo + 1
	if hi >= n {
		return sorted[n-1]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// sampleStdDev 样本标准差（贝塞尔校正，除以 n-1）。
// 用样本而非总体标准差：这里的样本是「从这段时间里抽到的若干分桶」，不是全部可能的
// 观测。n<2 时无定义，返回 0（调用方已按 jitterMinSamples 拦住）。
func sampleStdDev(values []float64, mean float64) float64 {
	n := len(values)
	if n < 2 {
		return 0
	}
	var acc float64
	for _, v := range values {
		d := v - mean
		acc += d * d
	}
	return math.Sqrt(acc / float64(n-1))
}

// parseLimit 解析正整数查询参数：非法/缺省 → def；超过 max → max。
// 上限收敛而非报错：观测端点的参数写错不该变成一次 400。
func parseLimit(raw string, def, max int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
