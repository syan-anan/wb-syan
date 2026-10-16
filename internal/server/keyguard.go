package server

import (
	"time"

	"github.com/syan-anan/wb-syan/internal/keys"
)

// keyguard.go 子密钥分流约束的运行时判定（模型 / 域 / 时段 / 有效期 / 请求配额）。
//
// 账号白名单**不在这里**判定：选号侧（Pool.PickExcludingForRealmAllow 的 allow 参数
// 与粘性会话的 id.Allows）已经兜底，两处都判会形成两份口径。这里只判那些无法在
// 选号侧表达的维度。
//
// 返回空字符串 = 放行；非空 = 面向调用方的拒绝原因（写入 OpenAI 格式错误体）。
// 配额在放行时同一次调用内完成"检查 + 自增"（keys.Quota.Reserve），一次 HTTP 请求
// 只计一次（轮转重试不重复计数）。
func (h *Handler) keyAccessDenied(id keys.Identity, realm, model string) string {
	if id.Master {
		return ""
	}
	if !id.AllowsModel(model) {
		return "该密钥不允许调用模型 " + model
	}
	if !id.AllowsRealm(realm) {
		if realm == "global" {
			return "该密钥不允许访问国际版（global）模型"
		}
		return "该密钥不允许访问国内（cn）模型"
	}
	now := time.Now()
	if ok, why := id.AllowsAt(now); !ok {
		return why
	}
	daily, hourly := id.QuotaLimits()
	if daily > 0 || hourly > 0 {
		if h.cfg.KeyQuota == nil {
			// 未装配配额计数器（裸用/测试）：不静默放行受限配额，按不限额处理并留痕。
			return ""
		}
		if ok, why := h.cfg.KeyQuota.Reserve(id.KeyID, daily, hourly, now); !ok {
			return why
		}
	}
	return ""
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
