// Package usage 记录并聚合逐请求 token 用量，供面板「用量」视图展示。
//
// 与 internal/pool 的 TokenUsage 的区别：
//   - pool 的 TokenUsage 是**每账号一个累计计数器**，只保留总量与「最近一次」，
//     没有时间维度，也无法按模型/时间下钻；
//   - 本包按 (时间片, realm, uid, model) 分桶累计，因此可以出「今天各模型各用了多少」
//     「这一小时 prompt 涨得多快」这类问题，且能长期保留。
//
// 保留策略（分片粒度自动降级，总量因此有界）：
//   - 近 hourlyKeep 小时内：小时桶（细粒度，看尖峰）
//   - 更早：折叠为日桶，**永久保留**（看长期趋势）
//
// 落盘：data/usage.json，原子替换 + 防抖刷新（默认 30s），重启不丢。
// 桶数上界 ≈ 账号数 × 模型数 × (hourlyKeep + 已过天数)，实测单桶约 90 字节。
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// legacyClientLabel 老版本落盘数据（没有 client 维度）在面板上的归类标签。
// 加维度之前的桶无法回溯归因——不猜，明确标成历史数据。
const legacyClientLabel = "(历史数据)"

// hourlyKeep 小时桶的保留时长；超出后折叠为日桶。
const hourlyKeep = 90 * 24 * time.Hour

// flushInterval 防抖落盘间隔。
const flushInterval = 30 * time.Second

// maxBuckets 桶数硬上限。超过时立即触发一次折叠，避免异常流量把内存/文件撑爆。
const maxBuckets = 400_000

// hourLayout / dayLayout 分片键的时间格式（本地时区，与用户直觉一致）。
const (
	hourLayout = "2006-01-02T15"
	dayLayout  = "2006-01-02"
)

// bucket 一个 (时间片, realm, uid, model) 的累计量。
// JSON 字段名刻意取短，因为桶数量会随时间增长。
type bucket struct {
	Scope string `json:"s"` // "h:2006-01-02T15" 或 "d:2006-01-02"
	Realm string `json:"r"`
	UID   string `json:"u"`
	Model string `json:"m"`
	// Client 调用方客户端标签（识别不到时形如 "未识别: <UA首段>"）。
	// 维度加在 model 之后：同一个模型可能被多个客户端调用，分开才看得清。
	Client string  `json:"cl"`
	Req    int64   `json:"q"`  // 请求数（含失败）
	Err    int64   `json:"e"`  // 失败数
	PT     int64   `json:"p"`  // prompt tokens
	CT     int64   `json:"c"`  // completion tokens
	TT     int64   `json:"t"`  // total tokens（上游给什么用什么的合计）
	LatMs  int64   `json:"l"`  // 延迟累计（ms）
	LatN   int64   `json:"ln"` // 延迟样本数
	TPS    float64 `json:"v"`  // 吐字速率累计
	TPSN   int64   `json:"vn"` // 速率样本数
	// Cr 逐请求实测消耗的积分合计（上游 usage.credit 累加，可为小数）。
	// CrN 是有成本观测的请求数：用来区分「窗口内没有观测」（CrN=0 → 前端显示
	// 「—」）与「观测到 0 消耗」（免费模型：CrN>0 且 Cr=0）。
	Cr  float64 `json:"cr,omitempty"`
	CrN int64   `json:"crn,omitempty"`
}

// file 落盘结构。
type file struct {
	Version int      `json:"version"`
	Saved   string   `json:"saved"`
	Buckets []bucket `json:"buckets"`
}

// Recorder 并发安全的用量记录器。
type Recorder struct {
	mu      sync.Mutex
	path    string
	buckets map[string]*bucket // key: scope|realm|uid|model
	dirty   bool
	started time.Time

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 创建记录器。path 为空时禁用落盘（纯内存，测试用）。
func New(path string) *Recorder {
	r := &Recorder{
		path:    path,
		buckets: make(map[string]*bucket),
		started: time.Now(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[usage] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return r
}

// Start 启动后台防抖落盘与折叠。Stop 前一直运行。
func (r *Recorder) Start() {
	go func() {
		defer close(r.done)
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-t.C:
				r.mu.Lock()
				n := len(r.buckets)
				r.mu.Unlock()
				if n > maxBuckets {
					r.Rollup(time.Now())
				}
				r.flush(false)
			}
		}
	}()
}

// Stop 停止后台循环并做最后一次落盘。
func (r *Recorder) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// Delta 一次请求尝试的用量增量（与 pool.TokenUsageDelta 同形，避免包间依赖）。
type Delta struct {
	PromptTokens     int64
	HasPromptTokens  bool
	CompletionTokens int64
	HasCompletion    bool
	TotalTokens      int64
	HasTotal         bool
	LatencyMs        int64
	HasLatency       bool
	TokensPerSecond  float64
	HasTPS           bool
	// Credits 本次尝试实测消耗的积分（上游 usage 里的 credit，可为小数）。
	// HasCredits=false 表示这次没有成本观测（失败 / 上游没返回 credit），
	// 与「观测到 0 消耗」（免费模型）区分开——两者都写 0 会让「没记账」
	// 被读成「没消耗」。
	Credits    float64
	HasCredits bool
}

// Add 记录一次请求尝试。
//
// ok=false 表示该次尝试失败（传输错误 / 上游 >=400 / 解析失败）。失败尝试通常
// 没有 usage，但**仍要计入请求数与失败数**——重试放大正是靠这一列才看得出来。
func (r *Recorder) Add(now time.Time, realm, uid, model, client string, d Delta, ok bool) {
	if r == nil {
		return
	}
	if realm == "" {
		realm = "cn"
	}
	if model == "" {
		model = "(unknown)"
	}
	// 空 client 不该出现（识别函数总会给一个非空标签），但兜底一次，
	// 避免将来有人绕过 agentClientLabel 直接调用时产生一个空字符串维度。
	if client == "" {
		client = "(unknown)"
	}
	scope := "h:" + now.Format(hourLayout)
	key := scope + "|" + realm + "|" + uid + "|" + model + "|" + client

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[key]
	if b == nil {
		b = &bucket{Scope: scope, Realm: realm, UID: uid, Model: model, Client: client}
		r.buckets[key] = b
	}
	b.Req++
	if !ok {
		b.Err++
	}
	if d.HasPromptTokens {
		b.PT += d.PromptTokens
	}
	if d.HasCompletion {
		b.CT += d.CompletionTokens
	}
	if d.HasTotal {
		b.TT += d.TotalTokens
	} else if d.HasPromptTokens || d.HasCompletion {
		// 上游没给 total：用 pt+ct 兜底，保证总量口径连续。
		b.TT += d.PromptTokens + d.CompletionTokens
	}
	if d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	if d.HasTPS {
		b.TPS += d.TokensPerSecond
		b.TPSN++
	}
	if d.HasCredits {
		b.Cr += d.Credits
		b.CrN++
	}
	r.dirty = true
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶（按本地日历日）。
// 幂等：同一小时反复折叠不会重复计数（先累加再删源桶）。
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	cutoff := now.Add(-hourlyKeep)

	r.mu.Lock()
	defer r.mu.Unlock()

	type move struct{ from, to string }
	var moves []move
	for k, b := range r.buckets {
		if !strings.HasPrefix(b.Scope, "h:") {
			continue
		}
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
		if err != nil || !ts.Before(cutoff) {
			continue
		}
		day := "d:" + ts.Format(dayLayout)
		moves = append(moves, move{from: k, to: day + "|" + b.Realm + "|" + b.UID + "|" + b.Model + "|" + b.Client})
	}
	for _, m := range moves {
		src := r.buckets[m.from]
		if src == nil {
			continue
		}
		dst := r.buckets[m.to]
		if dst == nil {
			cp := *src
			cp.Scope = strings.SplitN(m.to, "|", 2)[0]
			dst = &cp
			r.buckets[m.to] = dst
		} else {
			dst.Req += src.Req
			dst.Err += src.Err
			dst.PT += src.PT
			dst.CT += src.CT
			dst.TT += src.TT
			dst.LatMs += src.LatMs
			dst.LatN += src.LatN
			dst.TPS += src.TPS
			dst.TPSN += src.TPSN
			dst.Cr += src.Cr
			dst.CrN += src.CrN
		}
		delete(r.buckets, m.from)
	}
	if len(moves) > 0 {
		r.dirty = true
		log.Printf("[usage] 折叠 %d 个小时桶为日桶（保留 %v 细粒度）", len(moves), hourlyKeep)
	}
}

// ---------------------------------------------------------------- 持久化 ----

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		// 旧版本落盘的桶没有 Client 字段（反序列化后为空串），key 里同样带空段，
		// 与 Add 的拼法保持一致：老数据因此自然归入「(历史数据)」那一档。
		if b.Client == "" {
			b.Client = legacyClientLabel
		}
		r.buckets[b.Scope+"|"+b.Realm+"|"+b.UID+"|"+b.Model+"|"+b.Client] = &b
	}
	log.Printf("[usage] 已恢复 %d 个用量桶（%s）", len(r.buckets), r.path)
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	snap := file{Version: 1, Saved: time.Now().Format(time.RFC3339), Buckets: make([]bucket, 0, len(r.buckets))}
	for _, b := range r.buckets {
		snap.Buckets = append(snap.Buckets, *b)
	}
	r.dirty = false
	r.mu.Unlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[usage] 序列化失败: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("[usage] 建目录失败: %v", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[usage] 写临时文件失败: %v", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("[usage] 原子替换失败: %v", err)
	}
}

// Save 立即落盘（面板「刷新」或关闭前调用）。
func (r *Recorder) Save() { r.flush(true) }

// ---------------------------------------------------------------- 聚合 ----

// Agg 一组累计量。
type Agg struct {
	Requests      int64   `json:"requests"`
	Errors        int64   `json:"errors"`
	PromptTokens  int64   `json:"prompt_tokens"`
	CompletionTok int64   `json:"completion_tokens"`
	TotalTokens   int64   `json:"total_tokens"`
	AvgLatencyMs  float64 `json:"avg_latency_ms"`
	AvgTPS        float64 `json:"avg_tokens_per_second"`
	// Credits 窗口内逐请求实测消耗的积分合计（可为小数，展示时四舍五入）。
	// CreditsN 是有成本观测的请求数：0 表示本口径没有样本（不是「消耗 0」）。
	Credits  float64 `json:"credits"`
	CreditsN int64   `json:"credits_n"`
}

// aggAcc 是聚合过程中的累加器：Agg 只放已算好的结果，均值需要样本数才能
// 正确加权（不能对每桶的均值再取平均），所以样本数留在这里。
type aggAcc struct {
	Agg
	latSum     int64
	latSamples int64
	tpsSum     float64
	tpsSamples int64
}

func (g *aggAcc) add(b *bucket) {
	g.Requests += b.Req
	g.Errors += b.Err
	g.PromptTokens += b.PT
	g.CompletionTok += b.CT
	g.TotalTokens += b.TT
	g.latSum += b.LatMs
	g.latSamples += b.LatN
	g.tpsSum += b.TPS
	g.tpsSamples += b.TPSN
	g.Credits += b.Cr
	g.CreditsN += b.CrN
}

func (g *aggAcc) finish() Agg {
	a := g.Agg
	if g.latSamples > 0 {
		a.AvgLatencyMs = float64(g.latSum) / float64(g.latSamples)
	}
	if g.tpsSamples > 0 {
		a.AvgTPS = g.tpsSum / float64(g.tpsSamples)
	}
	return a
}

// KeyedAgg 按某个维度聚合的一行。
type KeyedAgg struct {
	Key   string `json:"key"`
	Realm string `json:"realm,omitempty"`
	Extra string `json:"extra,omitempty"` // 账号行放昵称
	Agg
}

// Point 时序上的一个点。
type Point struct {
	T     string `json:"t"`
	Scope string `json:"scope"` // "hour" | "day"
	Agg
}

// Snapshot 面板一次拉取的全部用量视图数据。
type Snapshot struct {
	Totals    Agg        `json:"totals"`
	ByRealm   []KeyedAgg `json:"by_realm"`
	ByAccount []KeyedAgg `json:"by_account"`
	ByModel   []KeyedAgg `json:"by_model"`
	// ByClient 按调用方客户端聚合（Codex / Claude Code / WorkBuddy / 未识别…）。
	ByClient  []KeyedAgg `json:"by_client"`
	Series    []Point    `json:"series"`
	Buckets   int        `json:"buckets"`
	FileBytes int64      `json:"file_bytes"`
	Since     string     `json:"since,omitempty"`
	Generated string     `json:"generated"`
}

// Snapshot 聚合**所选窗口内**的桶，产出面板一次拉取的全部用量视图数据。
//
// hours>0：窗口 = [当前整点-(hours-1)小时, now]，卡片汇总/按域/按账号/按模型/
// 时序**全部**按同一窗口口径统计——切窗口时所有数字随之变化（曾长期是"卡片为
// 全部历史累计、hours 只改时序分片"的口径，界面上被读成"筛选没生效"，已废弃）。
// 小时桶按整点入窗；日桶（Rollup 折叠出的长期数据）按日起点入窗，故小时窗口
// 天然不含更早的日桶。
// hours<=0：全部历史（含已折叠日桶），供「全部历史」选项看长期趋势。
//
// nicks 是 uid→昵称映射，仅用于展示。
func (r *Recorder) Snapshot(hours int, nicks map[string]string) Snapshot {
	if r == nil {
		return Snapshot{Generated: time.Now().Format(time.RFC3339)}
	}
	windowed := hours > 0
	if windowed && hours > 24*60 {
		hours = 24 * 60
	}

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	var total aggAcc
	realmAgg := map[string]*aggAcc{}
	acctAgg := map[string]*aggAcc{}
	acctRealm := map[string]string{}
	modelAgg := map[string]*aggAcc{}
	clientAgg := map[string]*aggAcc{}
	hourSeries := map[string]*aggAcc{}
	daySeries := map[string]*aggAcc{}

	var hourFrom time.Time
	if windowed {
		nowHour := time.Now().Truncate(time.Hour)
		hourFrom = nowHour.Add(-time.Duration(hours-1) * time.Hour)
	}

	// 数据起点（全库最早分片）：不受窗口影响，表示"记录自何时开始"。scope 字典序
	// 即时间序（同前缀内同格式排序；"d:" 恒早于 "h:"——日桶只来自 90 天前的小时折叠）。
	since := ""
	matched := 0
	for i := range bs {
		b := &bs[i]
		if b.Scope < since || since == "" {
			since = b.Scope
		}
		if windowed {
			var ts time.Time
			var err error
			if strings.HasPrefix(b.Scope, "h:") {
				ts, err = time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
			} else {
				ts, err = time.ParseInLocation(dayLayout, strings.TrimPrefix(b.Scope, "d:"), time.Local)
			}
			// 解析失败的脏桶不进窗口聚合（也不该出现在任何口径里）。
			if err != nil || ts.Before(hourFrom) {
				continue
			}
		}
		matched++
		total.add(b)

		if realmAgg[b.Realm] == nil {
			realmAgg[b.Realm] = &aggAcc{}
		}
		realmAgg[b.Realm].add(b)

		if acctAgg[b.UID] == nil {
			acctAgg[b.UID] = &aggAcc{}
		}
		acctAgg[b.UID].add(b)
		// 一个账号只属于一个 realm，这里记下来供前端展示「域」列；
		// keyed() 的 Realm 字段默认是空的（它按 key 分组，不知道 realm）。
		if acctRealm[b.UID] == "" {
			acctRealm[b.UID] = b.Realm
		}

		if modelAgg[b.Model] == nil {
			modelAgg[b.Model] = &aggAcc{}
		}
		modelAgg[b.Model].add(b)

		if clientAgg[b.Client] == nil {
			clientAgg[b.Client] = &aggAcc{}
		}
		clientAgg[b.Client].add(b)

		if strings.HasPrefix(b.Scope, "h:") {
			scope := strings.TrimPrefix(b.Scope, "h:")
			if hourSeries[scope] == nil {
				hourSeries[scope] = &aggAcc{}
			}
			hourSeries[scope].add(b)
		} else {
			scope := strings.TrimPrefix(b.Scope, "d:")
			if daySeries[scope] == nil {
				daySeries[scope] = &aggAcc{}
			}
			daySeries[scope].add(b)
		}
	}

	snap := Snapshot{
		Totals:  total.finish(),
		ByRealm: keyed(realmAgg, func(k string) (string, string) { return k, "" }),
		ByAccount: keyed(acctAgg, func(k string) (string, string) {
			return k, nicks[k]
		}),
		ByModel:   keyed(modelAgg, func(k string) (string, string) { return k, "" }),
		ByClient:  keyed(clientAgg, func(k string) (string, string) { return k, "" }),
		Buckets:   matched,
		Generated: time.Now().Format(time.RFC3339),
	}
	for i := range snap.ByAccount {
		snap.ByAccount[i].Realm = acctRealm[snap.ByAccount[i].Key]
	}

	// 日点（升序）+ 小时点（升序）拼成一条连续时序。
	dayKeys := make([]string, 0, len(daySeries))
	for k := range daySeries {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	for _, k := range dayKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "day", Agg: daySeries[k].finish()})
	}
	hourKeys := make([]string, 0, len(hourSeries))
	for k := range hourSeries {
		hourKeys = append(hourKeys, k)
	}
	sort.Strings(hourKeys)
	for _, k := range hourKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "hour", Agg: hourSeries[k].finish()})
	}

	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			snap.FileBytes = fi.Size()
		}
	}
	// since 去掉 scope 前缀（"h:2026-09-16T13" → "2026-09-16T13"）给前端展示；
	// 无任何桶时保持空（无数据不伪造起点）。
	snap.Since = strings.TrimPrefix(strings.TrimPrefix(since, "h:"), "d:")
	return snap
}

func keyed(m map[string]*aggAcc, label func(string) (string, string)) []KeyedAgg {
	out := make([]KeyedAgg, 0, len(m))
	for k, v := range m {
		key, extra := label(k)
		out = append(out, KeyedAgg{Key: key, Extra: extra, Agg: v.finish()})
	}
	// 按总量降序；同量按 key 升序，保证输出稳定（前端 diff 不抖）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Describe 返回一行人类可读的占用摘要（启动日志用）。
func (r *Recorder) Describe() string {
	if r == nil {
		return "disabled"
	}
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	var sz int64
	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			sz = fi.Size()
		}
	}
	return fmt.Sprintf("%d buckets, file %d bytes", n, sz)
}
