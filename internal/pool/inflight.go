// 账号级在途上限（面板「并发」按钮）：在池级参数之上给单个号开一层覆盖。
//
// 三态语义（对齐上游 workbuddy2api 1.0.9 的 Account.max_in_flight）：
//   - 未设置（nil）：跟随全局，global 域仍走域级分档——存量 state.json 没有这个键，
//     升级后行为与今天完全一致（零回归）
//   - 0：该号不限并发（与全局 max_in_flight=0 同语义：计数照常累加供观测，但不拒绝）
//   - 正整数：该号自己的上限
//
// 为什么需要逐号覆盖：一个池里同时有快号和慢号（老设备、低额度、被 WAF 盯上的域）。
// 池级上限只能取一个折中值——调高了慢号被三路并发挤成超时、连带吃 5xx 熔断；
// 调低了快号喂不饱、吞吐白扔。逐号设定把这件事交还给运维的一次判断：
// 「这个号只准跑 1 路」。
package pool

// maxAccountInFlight 账号级上限的取值上界。面板输入框同值，防的是把天文数字写进
// state.json：真到那个量级，Acquire 的 CAS 自旋本身就是瓶颈，上限数字毫无意义。
const maxAccountInFlight = 2147483647

// SetAccountInFlight 设置账号级在途上限并置脏（由后台 flusher 落盘）。
//
// limit == nil 表示清除覆盖、恢复跟随全局。返回值是**设置后生效**的上限
// （已解析三态），面板据此直接显示「现在这个号是 N 路」，不必再猜。
// uid 不存在 → ok=false，调用方据此回 404。
func (p *Pool) SetAccountInFlight(uid string, limit *int) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, false
	}
	if limit == nil {
		e.maxInFlight = nil
	} else {
		v := *limit
		// 负值与超界值一律钳到合法域：面板/ curl 传错数字不该把号锁死
		// （负上限会被 inFlightLimit 当成正数比较，cur >= 负数 恒真 = 永久满载）。
		if v < 0 {
			v = 0
		}
		if v > maxAccountInFlight {
			v = maxAccountInFlight
		}
		e.maxInFlight = &v
	}
	p.dirty.Store(true)
	return p.inFlightLimit(e), true
}

// AccountInFlightLimit 返回账号的生效上限（账号覆盖 > global 分档 > 全局）。
// 只读，供面板与运维接口显示；uid 不存在 → ok=false。
func (p *Pool) AccountInFlightLimit(uid string) (int, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, false
	}
	return p.inFlightLimit(e), true
}
