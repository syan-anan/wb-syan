package server

import (
	"net/http"
	"time"

	"github.com/syan-anan/wb-syan/internal/keys"
)

// keyguard.go 子密钥分流约束的运行时判定（模型 / 域 / 时段 / 有效期 / 请求配额）。
//
// 账号白名单**不在这里**判定：选号侧（Pool.PickExcludingForRealmAllow 的 allow 参数
// 与粘性会话的 id.Allows）已经兜底，两处都判会形成两份口径。这里只判那些无法在
// 选号侧表达的维度。
//
// 拒绝结果分两类口径（对齐上游 1.0.8）：
//
//	key_restricted      403 —— 约束类：模型白名单 / 域 / 生效时段 / 有效期。
//	key_quota_exceeded  402 —— 额度类：日配额 / 时配额 / 单模型日配额已用完。
//
// 为什么要拆开：客户端拿到 403 的合理反应是「这把 key 不该这么用」（改调用参数或
// 改配置，重试无意义）；拿到 402 的合理反应是「额度用满了」（等窗口重置或提额度，
// 换个时间就好）。两个都回 403 时，自动化客户端会把「明天再来」误判成「永远别用」，
// 直接把这把 key 拉黑——这是真实的可用性事故，不是洁癖。
//
// 配额在放行时同一次调用内完成"检查 + 自增"（keys.Quota.Reserve），一次 HTTP 请求
// 只计一次（轮转重试不重复计数）。

// keyDeny 子密钥拒绝结果。Reason 为空 = 放行。
type keyDeny struct {
	Reason string
	Code   string
}

// 子密钥拒绝的两种错误码。
const (
	keyCodeRestricted = "key_restricted"
	keyCodeQuota      = "key_quota_exceeded"
)

// Denied 报告是否需要拒绝。
func (d keyDeny) Denied() bool { return d.Reason != "" }

// Status 把错误码翻成 HTTP 状态码：额度类 402，其余 403。
// 状态码由 code 单点推导，调用方不再各自写死——两处写就有走样的机会。
func (d keyDeny) Status() int {
	if d.Code == keyCodeQuota {
		return http.StatusPaymentRequired
	}
	return http.StatusForbidden
}

// keyAllow 放行的零值。
func keyAllow() keyDeny { return keyDeny{} }

// keyAccessDenied 判定子密钥能否调用 (realm, model)。放行返回零值。
func (h *Handler) keyAccessDenied(id keys.Identity, realm, model string) keyDeny {
	if id.Master {
		return keyAllow()
	}
	if !id.AllowsModel(model) {
		return keyDeny{Reason: "该密钥不允许调用模型 " + model, Code: keyCodeRestricted}
	}
	if !id.AllowsRealm(realm) {
		if realm == "global" {
			return keyDeny{Reason: "该密钥不允许访问国际版（global）模型", Code: keyCodeRestricted}
		}
		return keyDeny{Reason: "该密钥不允许访问国内（cn）模型", Code: keyCodeRestricted}
	}
	return h.keyBudgetDenied(id, model, time.Now())
}

// keyAccessDeniedAnyRealm 同 keyAccessDenied，但跳过域判定：用于域语义不适用的
// 上游（qoder / OpenAI 兼容 provider）——它们没有 cn/global 之分，拿 cn 口径
// 去卡一个 global-only 密钥只会误伤。模型白名单、时段、有效期与配额照常生效。
func (h *Handler) keyAccessDeniedAnyRealm(id keys.Identity, model string) keyDeny {
	if id.Master {
		return keyAllow()
	}
	if !id.AllowsModel(model) {
		return keyDeny{Reason: "该密钥不允许调用模型 " + model, Code: keyCodeRestricted}
	}
	return h.keyBudgetDenied(id, model, time.Now())
}

// keyBudgetDenied 时段 / 有效期 / 请求配额（含单模型日限）判定。
// 时段与有效期归 403 key_restricted，配额耗尽归 402 key_quota_exceeded。
func (h *Handler) keyBudgetDenied(id keys.Identity, model string, now time.Time) keyDeny {
	if ok, why := id.AllowsAt(now); !ok {
		return keyDeny{Reason: why, Code: keyCodeRestricted}
	}
	daily, hourly := id.QuotaLimits()
	modelDaily := id.ModelDailyLimit(model)
	if daily > 0 || hourly > 0 || modelDaily > 0 {
		if h.cfg.KeyQuota == nil {
			// 未装配配额计数器（裸用/测试）：不静默放行受限配额，按不限额处理并留痕。
			return keyAllow()
		}
		if ok, why := h.cfg.KeyQuota.ReserveModel(id.KeyID, daily, hourly, model, modelDaily, now); !ok {
			return keyDeny{Reason: "当前密钥额度已用完：" + why, Code: keyCodeQuota}
		}
	}
	return keyAllow()
}

// acquireKeySlot 占用该密钥的一个并发名额。返回的 release 必须在请求结束时
// 调用一次（defer）；未配置并发上限时返回 nil，调用方直接跳过。
//
// 单独于 keyAccessDenied：拒绝原因（403/402）与并发（429）是不同的状态码，
// 而且并发名额必须在整个请求生命周期内持有——塞进返回 string 的判定函数
// 就无法保证释放。
func (h *Handler) acquireKeySlot(id keys.Identity) (func(), string) {
	limit := id.ConcurrencyLimit()
	if limit <= 0 || id.KeyID == "" || h.cfg.KeyQuota == nil {
		return nil, ""
	}
	if ok, why := h.cfg.KeyQuota.Acquire(id.KeyID, limit); !ok {
		return nil, why
	}
	keyID := id.KeyID
	return func() { h.cfg.KeyQuota.Release(keyID) }, ""
}

// exclusiveDeny 计算本次调用方「看不到」的独占账号集合。
//
// 独占绑定（keys.Entry.Exclusive）：账号只对绑定它的那条子密钥开放，公共池
// （主密钥、其他子密钥、自动换号、测速、全冷却兜底）一律跳过。这里是「挡掉别人的
// 专属号」的那一步：自己独占的号不进自己的 deny，所以绑定 key 照常能用；主密钥全挡
// （它就是"公共池"本身）。
//
// 返回 nil = 当前没有任何独占账号（选号侧跳过过滤，零成本）。数据源是 live 子密钥
// 快照，改配置后下一个请求即生效，与 allow 同源同寿命。
func exclusiveDeny(all map[string]bool, id keys.Identity) map[string]bool {
	if len(all) == 0 {
		return nil
	}
	if id.Master {
		return all
	}
	deny := make(map[string]bool, len(all))
	for uid := range all {
		if !id.OwnsExclusive(uid) {
			deny[uid] = true
		}
	}
	if len(deny) == 0 {
		return nil
	}
	return deny
}

// exclusiveUIDs 返回当前被独占绑定的账号集合（live 快照；未启用子密钥时 nil）。
func (h *Handler) exclusiveUIDs() map[string]bool {
	snap := h.loadLive()
	if snap.Keys == nil {
		return nil
	}
	return snap.Keys.ExclusiveUIDs()
}
