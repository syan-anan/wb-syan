// 本文件：后台维护闸门 BackendGate —— 让后台任务（签到 / 余额刷新 / 积分明细 /
// 模型目录同步 / 保活）与对话路径互斥地消费同一批账号。
//
// 要解决的问题：后台正在刷新某账号余额时，对话请求可能同时选中它 —— 上游并发
// 压力翻倍、WAF 风控更容易触发。参考实现是 workbuddy2api(Python)
// admin/maintenance.py 的 account_slot，语义照搬如下：
//
//  1. 后台只预留「空闲账号身份」；预留期间对话不再获得该账号的在途租约。
//  2. 对话已持有在途租约时，后台直接跳过，不等待。
//  3. 节流等待发生在**预留之前**：等待期间到达的对话优先取得账号，后台随后跳过。
//  4. 多个后台操作共享一个执行锁 + 账号间隔（默认 0.8s，0 可关闭，单次间隔最多 60s）。
//  5. 预留后重读账号状态；异常 / 返回 / 跳过都要释放预留与执行锁。
//
// 解耦约束：本文件 **不 import internal/pool 与 internal/session**。闸门不需要认识
// 池的具体类型，账号状态由调用方通过 snapshot func() AccountState 注入 —— 这样既
// 避免把调度包与池/会话包的依赖方向缠成环，也让闸门能被任何上层安全复用。
//
// 只依赖标准库：context / sync / time。
package scheduler

import (
	"context"
	"sync"
	"time"
)

// ── 跳过原因（blocked 的取值集合）────────────────────────────────────────
//
// 与参考实现 account_slot 的 reason 一一对应：
//
//	inactive        → ReasonDisabled
//	account_cooling → ReasonCooling
//	inflight        → ReasonInFlight
//	background_busy → ReasonPeer
//
// 另补两个 Go 侧独有的：ReasonCanceled（ctx 取消）、ReasonInvalidUID（uid 为空）。
const (
	ReasonDisabled   = "disabled"    // 账号已禁用
	ReasonCooling    = "cooling"     // 账号级冷却 / 熔断 / 连败降权任一有效
	ReasonInFlight   = "in_flight"   // 对话已持有该账号的在途租约
	ReasonPeer       = "peer"        // 该 uid 已被另一个后台任务预留（后台竞争）
	ReasonCanceled   = "canceled"    // context 在等待期间被取消
	ReasonInvalidUID = "invalid_uid" // uid 为空
)

// gateMaxAccountGap 单次节流间隔上限，对齐参考实现 min(60, max(0, gap))：
// 配错（例如写成 10m）也不会把后台卡太久，更不会让对话排队饿死。
const gateMaxAccountGap = 60 * time.Second

// AccountState 调用方注入的账号只读快照。本包不认识 pool，三个字段由调用方翻译：
//
//	Disabled ← pool.Status(uid).Disabled
//	Cooling  ← pool.Status(uid).Cooling（until / breakerUntil / 连败降权任一有效）
//	InFlight ← pool.Status(uid).InFlight > 0（对话持有的在途名额）
//
// snapshot 必须廉价且无副作用：闸门在一次 Acquire 内最多读三次（预留前、节流后、
// 预留后复检）。
type AccountState struct {
	Disabled bool
	Cooling  bool
	InFlight bool
}

// GateStats 闸门观测快照，供面板 / 日志使用。
type GateStats struct {
	Reserved   int              // 当前预留数（跨 uid 求和）
	Skipped    map[string]int64 // 按原因累计的跳过次数（进程内累计，不持久化）
	AccountGap time.Duration    // 节流间隔配置（已按上限钳过）
	MaxSkip    int              // 尝试轮数配置
}

// BackendGate 后台维护的账号预留闸门：与对话路径互斥。
//
// 并发模型：
//   - execSem 是「执行锁」——容量 1 的信号量，串行化「节流等待 + 预留」临界区，
//     让账号间隔真正生效：否则并发进入的多个后台任务会同时判定「间隔已过」，
//     一起抢占上游，节流形同虚设。
//   - reserved 是「预留集合」——同一 uid 同时只能被一个后台任务预留；不同 uid
//     可以同时处于已预留状态（预留覆盖整个维护操作，是长生命周期的，不占执行锁）。
//   - 用 channel 而不是 sync.Mutex 做执行锁，是为了让等待本身可被 context 取消。
type BackendGate struct {
	accountGap time.Duration // 构造后不可变
	maxSkip    int           // 构造后不可变

	execSem chan struct{}

	mu         sync.Mutex
	reserved   map[string]struct{}
	lastFinish time.Time // 上次预留释放时刻：节流基准

	statsMu sync.Mutex
	skipped map[string]int64
}

// NewBackendGate 构造闸门。
//
// accountGap：相邻两次预留之间的最小间隔（建议 0.8s；0 关闭节流；上限 60s）。
// maxSkip：单次 Acquire 的尝试轮数上限。1 表示与参考实现完全一致 —— 评估一次、
// 节流一次、重评估一次、预留一次，任何冲突立刻跳过、不等待、不重试。>1 时才允许
// 对临时性竞争（peer / in_flight）多等一轮；<=0 视为 1。
func NewBackendGate(accountGap time.Duration, maxSkip int) *BackendGate {
	if accountGap < 0 {
		accountGap = 0
	}
	if accountGap > gateMaxAccountGap {
		accountGap = gateMaxAccountGap
	}
	if maxSkip <= 0 {
		maxSkip = 1
	}
	return &BackendGate{
		accountGap: accountGap,
		maxSkip:    maxSkip,
		execSem:    make(chan struct{}, 1),
		reserved:   map[string]struct{}{},
		skipped:    map[string]int64{},
	}
}

// Acquire 等价于 AcquireCtx(context.Background(), ...)：不参与取消的简化入口。
//
// 返回 (release, blocked, ok)：
//
//	ok=true  → 预留成功，调用方**必须** defer release()（release 幂等，可安全重复调）。
//	ok=false → blocked 说明跳过原因（见 Reason* 常量），release 恒为 nil。
//
// 失败路径绝不泄漏预留：任何异常都在函数内部就地释放。
func (g *BackendGate) Acquire(uid string, snapshot func() AccountState) (func(), string, bool) {
	return g.AcquireCtx(context.Background(), uid, snapshot)
}

// AcquireCtx 是 Acquire 的可取消版本：节流等待期间 ctx 被取消时立刻返回
// ReasonCanceled，不再占用执行锁、不留下任何预留。
func (g *BackendGate) AcquireCtx(ctx context.Context, uid string, snapshot func() AccountState) (func(), string, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if uid == "" {
		return nil, ReasonInvalidUID, false
	}
	if snapshot == nil {
		// 漏注入时退化为「纯互斥」而不是 panic：后台任务不该因为接线漏了一处就整体停摆。
		snapshot = func() AccountState { return AccountState{} }
	}

	reason := ""
	for attempt := 1; attempt <= g.maxSkip; attempt++ {
		if ctx.Err() != nil {
			reason = ReasonCanceled
			break
		}
		rel, r := g.tryOnce(ctx, uid, snapshot)
		if rel != nil {
			return rel, "", true
		}
		reason = r
		// 终态原因（禁用 / 冷却）不重试：重试只会对上游重复做无意义判定。
		if r == ReasonCanceled || !retryableReason(r) {
			break
		}
	}
	g.noteSkip(reason)
	return nil, reason, false
}

// tryOnce 走一遍完整流程：评估 → 节流 → 重评估 → 预留 → 预留后复检。
// 成功返回 (release, "")；失败返回 (nil, reason)。
func (g *BackendGate) tryOnce(ctx context.Context, uid string, snapshot func() AccountState) (func(), string) {
	// ① 预留前第一次评估：直接命中禁用/冷却/在途就跳过，连执行锁都不占。
	if r := classify(snapshot()); r != "" {
		return nil, r
	}

	if !g.enter(ctx) {
		return nil, ReasonCanceled
	}
	defer g.leave()

	// ② 节流等待发生在预留之前（对齐 account_slot）：等待期间到达的对话优先取得
	// 账号，随后第 ③ 步的重评估把本次后台自然跳过 —— 后台永远不抢在对话前面。
	if !g.waitGap(ctx) {
		return nil, ReasonCanceled
	}

	// ③ 节流后的重评估：状态可能在等待期间变化（账号被禁用、转冷，或对话拿了租约）。
	if r := classify(snapshot()); r != "" {
		return nil, r
	}

	// ④ 预留身份：同一 uid 同时只能被一个后台任务预留。
	if !g.reserve(uid) {
		return nil, ReasonPeer
	}

	// ⑤ 预留后复检：等待/竞争期间状态仍可能变差，命中就立刻释放，绝不留悬挂预留。
	if r := classify(snapshot()); r != "" {
		g.release(uid)
		return nil, r
	}
	return g.makeRelease(uid), ""
}

// enter 获取执行锁；ctx 取消时返回 false（不持有锁）。
func (g *BackendGate) enter(ctx context.Context) bool {
	select {
	case g.execSem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// leave 释放执行锁。必须与 enter 成对（调用点用 defer 兜底）。
func (g *BackendGate) leave() { <-g.execSem }

// waitGap 在执行锁内等待「距上次预留释放至少 accountGap」。首次预留（无基准）或
// gap<=0 时立即返回；等待可被 ctx 取消。
func (g *BackendGate) waitGap(ctx context.Context) bool {
	if g.accountGap <= 0 {
		return ctx.Err() == nil
	}
	g.mu.Lock()
	last := g.lastFinish
	g.mu.Unlock()
	if last.IsZero() {
		return ctx.Err() == nil // 进程内第一次预留：不制造无谓等待
	}
	remain := g.accountGap - time.Since(last)
	if remain <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(remain)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// reserve 把 uid 加入预留集合；已被其他后台任务预留时返回 false。
func (g *BackendGate) reserve(uid string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, dup := g.reserved[uid]; dup {
		return false
	}
	g.reserved[uid] = struct{}{}
	return true
}

// release 幂等移除预留并刷新节流基准。重复调用是安全的 no-op：第二次找不到该 uid，
// 因此不会再次刷新 lastFinish（否则重复 release 会无故推后其他后台任务的节流窗口）。
func (g *BackendGate) release(uid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.reserved[uid]; ok {
		delete(g.reserved, uid)
		g.lastFinish = time.Now()
	}
}

// makeRelease 生成对外返回的 release 闭包：sync.Once 兜底 + release 自身幂等，
// 双保险保证重复调用安全。
func (g *BackendGate) makeRelease(uid string) func() {
	var once sync.Once
	return func() {
		once.Do(func() { g.release(uid) })
	}
}

// noteSkip 累加一次按原因分类的跳过计数。
func (g *BackendGate) noteSkip(reason string) {
	if reason == "" {
		return
	}
	g.statsMu.Lock()
	g.skipped[reason]++
	g.statsMu.Unlock()
}

// Stats 返回观测快照。Skipped 是副本，调用方改动不影响闸门内部状态。
func (g *BackendGate) Stats() GateStats {
	g.statsMu.Lock()
	sk := make(map[string]int64, len(g.skipped))
	for k, v := range g.skipped {
		sk[k] = v
	}
	g.statsMu.Unlock()

	g.mu.Lock()
	n := len(g.reserved)
	g.mu.Unlock()

	return GateStats{
		Reserved:   n,
		Skipped:    sk,
		AccountGap: g.accountGap,
		MaxSkip:    g.maxSkip,
	}
}

// classify 把账号快照翻译成跳过原因；可预留时返回 ""。
//
// 判定顺序固定为 禁用 → 冷却 → 在途：先报终态原因，避免把「已禁用的号」报成
// 「在途」，让面板上的 skip 归因与运维直觉一致。
func classify(st AccountState) string {
	switch {
	case st.Disabled:
		return ReasonDisabled
	case st.Cooling:
		return ReasonCooling
	case st.InFlight:
		return ReasonInFlight
	default:
		return ""
	}
}

// retryableReason 报告该原因是否属于「临时性竞争」，值得在 maxSkip>1 时再来一轮。
// 默认 maxSkip=1 时该分支永不触发 —— 与参考实现「竞争即跳过、不等待」一致。
func retryableReason(reason string) bool {
	return reason == ReasonPeer || reason == ReasonInFlight
}
