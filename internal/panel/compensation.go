package panel

// 限时补偿 / 每日权益领取（对齐 xiaofan6ya/workbuddy2api 的 compensation 模块）。
//
// 上游把补偿端点放在**官网 web 域**并要求 Origin/Referer，只按 billing 域裸 POST
// 会被当成非浏览器来源。查询走 upstream.CompensationStatusOf，领取走
// upstream.ClaimCompensationWeb。
//
// 安全边界（这是本模块最重要的部分）：
//  1. **先查后领**：永远先读一次上游状态，claimed=true 直接跳过，绝不盲发领取。
//  2. **领取非幂等**：领取请求发出后不立即重试；先回查上游 claimed，确认真实结果。
//  3. **失败不猜**：查询失败 -> error；查询成功但字段不全 -> unknown，都不发起领取。
//  4. 领取成功后才刷新余额，且刷新失败**不影响**已领取的事实（下次再刷）。
//
// 以上四条保证「任何一次抖动都不会导致同一账号被重复发放」。

import (
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/upstream"
)

// compensationConcurrency 并发查询上限：补偿查询是冷路径，别把上游打热。
const compensationConcurrency = 4

// compensationResult 单账号结果（直接给前端渲染）。
type compensationResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	// Status 取值：available / claimed / ineligible / expired / unknown / error / disabled
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Credit  int64  `json:"credit"`
	EndTime string `json:"end_time,omitempty"`
	Claimed bool   `json:"claimed"`
}

// compensationStatus GET /panel/api/compensation —— 查全部启用账号的可领取状态。
func (p *Panel) compensationStatus(w http.ResponseWriter, r *http.Request) {
	list := p.cfg.Pool.List()
	sem := make(chan struct{}, compensationConcurrency)
	out := make([]compensationResult, 0, len(list))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, st := range list {
		if st.Disabled {
			out = append(out, compensationResult{UID: st.UID, Nickname: st.Nickname, Status: "disabled", Reason: "账号已禁用"})
			continue
		}
		wg.Add(1)
		go func(uid, nick string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := p.queryCompensation(uid, nick)
			mu.Lock()
			out = append(out, res)
			mu.Unlock()
		}(st.UID, st.Nickname)
	}
	wg.Wait()

	available, claimed := 0, 0
	for _, x := range out {
		switch x.Status {
		case "available":
			available++
		case "claimed":
			claimed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"total":     len(out),
		"available": available,
		"claimed":   claimed,
		"results":   out,
	})
}

// queryCompensation 读一次上游状态并归一成 compensationResult。只读，可安全重试。
func (p *Panel) queryCompensation(uid, nick string) compensationResult {
	res := compensationResult{UID: uid, Nickname: nick, Status: "unknown", Reason: "上游未返回明确状态"}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		res.Status, res.Reason = "error", "账号未载入池"
		return res
	}
	st, err := p.cfg.Upstream.CompensationStatusOf(a)
	if err != nil {
		res.Status, res.Reason = "error", "查询失败："+err.Error()
		return res
	}
	res.EndTime = st.EndTime
	if st.Claimed != nil && *st.Claimed {
		res.Status, res.Claimed, res.Reason = "claimed", true, "本期权益已领取"
		return res
	}
	if st.Active != nil && !*st.Active {
		res.Status, res.Reason = "ineligible", "当前暂无可领取权益"
		return res
	}
	if upstream.CompensationEnded(st.EndTime) {
		res.Status, res.Reason = "expired", "已超过领取截止时间"
		return res
	}
	if st.Active != nil && *st.Active && st.Claimed != nil && !*st.Claimed {
		if st.CreditNum != nil {
			res.Credit = *st.CreditNum
		}
		res.Status = "available"
		if res.Credit > 0 {
			res.Reason = "可领取 " + itoa(res.Credit) + " 积分"
		} else {
			res.Reason = "可领取权益"
		}
		return res
	}
	return res
}

// compensationClaim POST /panel/api/accounts/{uid}/compensation/claim —— 单账号领取。
func (p *Panel) compensationClaim(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	status, credit, reason, ok := p.claimCompensation(uid, a)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "status": status, "credit": credit, "reason": reason})
}

// claimCompensation 单账号领取的核心逻辑（HTTP 与每日排程共用）。
// 返回值 ok=false 表示「没确认领取成功」（查询失败 / 领取响应异常且回查未确认）；
// status=claimed 且 ok=true 才是真的到账。
func (p *Panel) claimCompensation(uid string, a *auth.Auth) (status string, credit int64, reason string, ok bool) {

	// ① 先查：只读，失败就如实报错，不盲目领取。
	before, err := p.cfg.Upstream.CompensationStatusOf(a)
	if err != nil {
		return "error", 0, "查询失败：" + err.Error(), false
	}
	if before.Claimed != nil && *before.Claimed {
		return "claimed", 0, "本期权益已领取", true
	}
	if before.Active == nil || !*before.Active {
		return "ineligible", 0, "当前暂无可领取权益", true
	}
	if upstream.CompensationEnded(before.EndTime) {
		return "expired", 0, "已超过领取截止时间", true
	}

	// ② 领取（非幂等）。
	credit, claimErr := p.cfg.Upstream.ClaimCompensationWeb(a)
	if claimErr != nil {
		// ③ 响应丢失不代表没发放：先回查上游真实状态再下结论。
		time.Sleep(800 * time.Millisecond)
		if after, qerr := p.cfg.Upstream.CompensationStatusOf(a); qerr == nil && after.Claimed != nil && *after.Claimed {
			p.refreshAfterCompensation(uid, a)
			log.Printf("panel: compensation uid=%s 领取响应异常但上游已确认到账", uid)
			return "claimed", credit, "已通过官方状态确认领取成功（原响应异常：" + claimErr.Error() + "）", true
		}
		return "unknown", 0, "领取未确认，已核对官方状态；稍后重试不会重复提交同一笔：" + claimErr.Error(), false
	}

	p.refreshAfterCompensation(uid, a)
	log.Printf("panel: compensation uid=%s 领取成功 credit=%d", uid, credit)
	msg := "已领取权益"
	if credit > 0 {
		msg = "已领取 " + itoa(credit) + " 积分"
	}
	return "claimed", credit, msg, true
}

// ClaimAllBenefits 遍历全部启用账号，先查后领，只对「确实可领取」的发请求。
// 返回 (claimed, skipped, failed)。供面板「一键领取」与每日排程共用。
//
// 并发固定 2：领取是写操作，串行太慢（十几个号要等几分钟），并发太高又会
// 让上游把这批请求当成异常流量。失败不重试——重试交给下一次排程。
func (p *Panel) ClaimAllBenefits() (claimed, skipped, failed int, results []compensationResult) {
	list := p.cfg.Pool.List()
	sem := make(chan struct{}, 2)
	var mu sync.Mutex
	var wg sync.WaitGroup
	results = []compensationResult{}
	for _, st := range list {
		if st.Disabled {
			skipped++
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			skipped++
			continue
		}
		wg.Add(1)
		go func(uid, nick string, acct *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			status, credit, reason, ok := p.claimCompensation(uid, acct)
			mu.Lock()
			defer mu.Unlock()
			results = append(results, compensationResult{
				UID: uid, Nickname: nick, Status: status, Credit: credit,
				Reason: reason, Claimed: status == "claimed",
			})
			switch {
			case !ok:
				failed++
			case status == "claimed" && credit > 0:
				claimed++
			default:
				skipped++
			}
		}(st.UID, st.Nickname, a)
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].UID < results[j].UID })
	return claimed, skipped, failed, results
}

// ClaimAllHandler POST /panel/api/compensation/claim_all —— 面板「一键领取全部」。
func (p *Panel) compensationClaimAll(w http.ResponseWriter, r *http.Request) {
	claimed, skipped, failed, results := p.ClaimAllBenefits()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "claimed": claimed, "skipped": skipped, "failed": failed, "results": results,
	})
}

// refreshAfterCompensation 领取成功后刷新余额与到期快照。
//
// 刻意吞掉错误：领取本身已经成功，刷新失败只影响展示，**绝不能**让调用方
// 以为「领取失败」而重试（那才是重复发放的来源）。
func (p *Panel) refreshAfterCompensation(uid string, a *auth.Auth) {
	remain, total, expiring, earliestAt, earliestRemaining, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
	if err != nil {
		log.Printf("panel: compensation uid=%s 余额刷新失败（领取已成功，忽略）：%v", uid, err)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	p.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
}

// itoa 小工具：避免为了一个整数再引 strconv 进本文件。
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
