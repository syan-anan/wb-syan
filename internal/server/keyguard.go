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
