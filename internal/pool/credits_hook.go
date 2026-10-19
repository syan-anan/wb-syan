// 余额变更钩子：把「这个号的积分从 A 变成 B」这件事告诉上层（面板的积分流水）。
//
// 为什么用钩子而不是让面板自己对比：余额写入点散在池的三个 setter（SetCredits /
// SetCreditsDetailed / ReenableIfCredits）与两条驱动路径（面板手动刷新、调度器
// 定时签到保活）。面板逐个包裹 = 漏一个就少记一类；在写入点统一发事件 = 一处不漏。
// 池本身仍然不知道「流水」是什么（依赖方向不变：panel → pool，没有反向 import）。
//
// 基线规则（关键）：账号**第一次**被写入余额时只建立基线、不发事件。
// 新导入一个有 500 积分的号，池里旧值是 0，直接发事件就会产出一条「+500」的假账，
// 而它根本不是任何一次发放。这与上游 1.0.9 的「初次导入/同步只建立余额基线」同口径。
package pool

// CreditsChangeFunc 余额变更回调：uid、变更前、变更后（均为积分余额）。
type CreditsChangeFunc func(uid string, before, after int64)

// SetCreditsHook 注入余额变更钩子。传 nil = 取消（恢复零开销路径）。
//
// 钩子在 p.mu **持有期间**被调用，因此实现必须是纯内存操作（写文件、打网络、
// 回调池自身的读接口都不允许）。面板的流水实现走内存 + 后台落盘，正是为此。
func (p *Pool) SetCreditsHook(fn CreditsChangeFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creditsHook = fn
}

// setCreditsLocked 写入余额并在**值真的变了**时发事件。调用方必须已持 p.mu。
//
// 基线未建立 → 只置基线、不发事件（见文件头）。从 state.json 恢复的账号在
// applyAccountsLocked 里直接置基线，重启后第一次真实刷新就会正常发事件。
func (p *Pool) setCreditsLocked(e *entry, credits int64) {
	before := e.credits
	e.credits = credits
	if !e.creditsBaseline {
		e.creditsBaseline = true
		return
	}
	if before == credits || p.creditsHook == nil {
		return
	}
	p.creditsHook(e.a.UID, before, credits)
}
