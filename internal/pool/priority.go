// 手动优先级：面板账号池的「上移 / 下移 / 恢复自动排序」。
//
// 与自动选号策略的关系：手动档**高于**全部自动策略（成本分层 costTier / 最早到期
// preferExpiring / 加权随机 / 闲置补偿）——人工指定是运维的明确意图，不该被启发式
// 覆盖（对齐上游 workbuddy2api 1.0.5 口径：手动优先级 > 到期优先 > 免费均衡）。
//
// 边界：「手动排序只决定先选谁，不创造资格」。disabled、冷却、熔断、连败降权、
// 模型级冷却、在途上限、tried（请求级轮换）、独占绑定 deny、子密钥白名单 allow
// 这些**资格**过滤全部照常生效，手动档只在上方过滤完成后缩减候选集（见 pick.go）。
package pool

// priorityMax / priorityMin 手动优先级值域。量级足够表达「排到最前 / 沉到最后」，
// 又不至于让面板连点把数字推到无意义的大数；钳到边界后继续点不再变化。
const (
	priorityMax = 99
	priorityMin = -99
)

// AdjustPriority 调整账号的手动优先级并置脏（下一次 flusher 落盘）。
//   - "up"：priority + 1（钳 priorityMax）—— 更靠前，优先于未干预的账号
//   - "down"：priority - 1（钳 priorityMin）—— 沉到最后
//   - "auto" / 其它取值：清回 0，恢复自动排序（前端传错不炸，零回归）
//
// 返回调整后的 priority 与是否命中账号（uid 不存在 → ok=false，调用方据此 404）。
func (p *Pool) AdjustPriority(uid, dir string) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, false
	}
	switch dir {
	case "up":
		e.priority++
		if e.priority > priorityMax {
			e.priority = priorityMax
		}
	case "down":
		e.priority--
		if e.priority < priorityMin {
			e.priority = priorityMin
		}
	default:
		e.priority = 0
	}
	p.dirty.Store(true)
	return e.priority, true
}

// manualPriorityLocked 返回候选集的手动档判定：maxP 是生效档位、on 表示是否启用手动档。
//
// 档位口径（关键）：档位 = **全体候选里最大的 priority**（0 自然参与比较）。
//   - 有人被「上移」（> 0）→ 只从优先级最高者里选；
//   - 只有人被「下移」（< 0）→ 未干预的 0 档更大，被下移者落选（这才是「下移」本意）；
//   - 全员都被下移（全 < 0）→ 取其中最大者（下移得最少的号），仍能选出账号；
//   - 全员未干预（全 0）→ on=false，完全走既有自动策略（零回归）。
//
// 为什么用「取最大档硬过滤」而不是加权排序：pick 的自动路径末端是加权随机，
// 只排序的话优先号仍可能抽不中，达不到「手动优先」的语义（同 costTier 用硬过滤
// 而非软排序的理由）。
func manualPriorityLocked(cands []*entry) (maxP int, on bool) {
	first := true
	for _, e := range cands {
		if e.priority != 0 {
			on = true
		}
		if first || e.priority > maxP {
			maxP = e.priority
			first = false
		}
	}
	return maxP, on
}
