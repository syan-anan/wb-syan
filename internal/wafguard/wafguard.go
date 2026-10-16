// Package wafguard 进程级「出口保护」状态。
//
// 背景：WAF 403 拦的是网关出口 IP，不是账号（证据链见 server/wafip.go 的文件头）。
// 对话路径在 IP 级拦截时已经 fail-fast 停止轮转，但**后台维护任务**并不知道这件事：
// 到点照样逐号打上游——出口正被风控时，每一次后台调用都在加重它，而且这些失败还会
// 被记进账号健康度，让好号被冤枉降温。
//
// 本包只做一件事：把「出口保护中」变成进程内可读状态，让后台任务在保护期内**顺延**
// （整轮跳过、不发起任何上游调用、不产生任何账号记账），保护自然解除后自动恢复。
//
// 与 server/wafipGate 的分工（两者由同一份 WAF 证据驱动，不重复判定）：
//   - wafIPGate：对话路径的局部状态机，回答「这一次请求还要不要继续换号」；
//   - Guard：跨路径共享的出口状态，回答「后台任务现在该不该动上游」。
//
// 进程内状态、重启清零：窗口是 60s 级，重建成本极低；反过来持久化会在重启后留下一段
// 过期的保护期，让后台白白顺延一轮——宁可重启后重新判定一次。
package wafguard

import (
	"sync"
	"time"
)

// DefaultWindow 保护窗口默认时长，与 server 侧 wafIPWindow 同量级（60s）。
// 两处各自持有常量而不是互相 import：server 依赖本包，本包不能反向依赖 server。
// 一致性由接线处保证（server 激活时传入自己的 wafIPWindow）。
const DefaultWindow = 60 * time.Second

// Guard 出口保护状态（零值可用，无需构造）。
//
// 并发模型：单个 mutex 保护 until。读写都是 O(1)，调用点在每个后台循环开头与每次
// WAF 判定命中处，不构成热点。
type Guard struct {
	mu    sync.Mutex
	until time.Time
}

// New 返回可用的 Guard（零值本就可用，保留构造函数以明确意图）。
func New() *Guard { return &Guard{} }

// Activate 进入（或延长）保护期到 now+d。d<=0 视为立即解除。
//
// 语义是「取较晚者」而不是「覆盖」：新证据不该缩短已有保护期——出口风控的窗口只会
// 被后续证据确认，缩短等于提前放后台出去继续撞。
func (g *Guard) Activate(d time.Duration) {
	if g == nil {
		return
	}
	if d <= 0 {
		g.Clear()
		return
	}
	until := time.Now().Add(d)
	g.mu.Lock()
	if until.After(g.until) {
		g.until = until
	}
	g.mu.Unlock()
}

// Active 报告当前是否处于保护期。
func (g *Guard) Active() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// Remaining 返回保护期剩余时长；未激活返回 0（不会返回负数）。
func (g *Guard) Remaining() time.Duration {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	d := time.Until(g.until)
	if d < 0 {
		return 0
	}
	return d
}

// Clear 立即解除保护（运维/测试用；生产路径靠窗口自然到期）。
func (g *Guard) Clear() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.until = time.Time{}
	g.mu.Unlock()
}
