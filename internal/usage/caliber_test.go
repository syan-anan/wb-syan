package usage

import (
	"testing"
	"time"
)

// 模型级用量口径的四态：有观测 / 实测免费 / 未知 / 未记录。
//
// 这四种状态在前端是四句不同的话。混成一个 0 会让运维把「没记账」读成
// 「免费」，或者把「上游没回 usage」读成「0 token」——两种误读都会直接
// 影响选号与对账判断。本测试把四条边界钉死在聚合层。
func TestModelStatsUsageCaliber(t *testing.T) {
	rec := New("")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.Local)

	// free：实测免费（CrN>0 且 Cr=0），token 明细齐全。
	rec.Add(now, "cn", "u1", "free", "c", Delta{TotalTokens: 100, HasTotal: true, Credits: 0, HasCredits: true}, true)
	// unknown：上游没回 credit（未知），也没回 usage（未记录）。
	rec.Add(now, "cn", "u1", "unknown", "c", Delta{}, true)
	// mixed：两次请求，一次带 token 明细、一次不带；成本都有观测。
	rec.Add(now, "cn", "u1", "mixed", "c", Delta{TotalTokens: 50, HasTotal: true, Credits: 1.5, HasCredits: true}, true)
	rec.Add(now, "cn", "u1", "mixed", "c", Delta{Credits: 0.5, HasCredits: true}, true)

	by := map[string]ModelStat{}
	for _, s := range rec.ModelStats(now, 1, 1) {
		by[s.Model] = s
	}

	if s := by["free"]; s.CreditSamples != 1 || s.Credits != 0 {
		t.Fatalf("free: samples=%d credits=%v want 1/0（实测免费是结论，不是未知）", s.CreditSamples, s.Credits)
	}
	if s := by["free"]; s.TokensRecorded != 1 || s.TokensMissing != 0 {
		t.Fatalf("free: recorded=%d missing=%d want 1/0", s.TokensRecorded, s.TokensMissing)
	}

	if s := by["unknown"]; s.CreditSamples != 0 {
		t.Fatalf("unknown: samples=%d want 0（无观测 → 前端显示「未知」）", s.CreditSamples)
	}
	if s := by["unknown"]; s.TokensRecorded != 0 || s.TokensMissing != 1 {
		t.Fatalf("unknown: recorded=%d missing=%d want 0/1（未记录）", s.TokensRecorded, s.TokensMissing)
	}

	mixed := by["mixed"]
	if mixed.CreditSamples != 2 || mixed.Credits != 2.0 {
		t.Fatalf("mixed: samples=%d credits=%v want 2/2.0", mixed.CreditSamples, mixed.Credits)
	}
	if mixed.TokensRecorded != 1 || mixed.TokensMissing != 1 {
		t.Fatalf("mixed: recorded=%d missing=%d want 1/1", mixed.TokensRecorded, mixed.TokensMissing)
	}
}

// 老桶兼容：加 TokN 之前落盘的数据没有明细计数，但 PT/CT/TT 有累计值——
// 应按「有明细」处理，不能把历史数据整体误报成「未记录」。
func TestModelStatsLegacyBucketCaliber(t *testing.T) {
	rec := New("")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.Local)
	rec.Add(now, "cn", "u1", "legacy", "c", Delta{}, true)
	key := "h:2026-10-17T12|cn|u1|legacy|c|"
	b := rec.buckets[key]
	if b == nil {
		t.Fatalf("bucket %q 不存在", key)
	}
	// 模拟旧版本写入的桶：有累计 token，没有 TokN。
	b.TokN = 0
	b.PT = 30
	b.CT = 70
	b.TT = 100
	b.Req = 3

	for _, s := range rec.ModelStats(now, 1, 1) {
		if s.Model != "legacy" {
			continue
		}
		if s.TokensMissing != 0 {
			t.Fatalf("老桶不该报「未记录」：missing=%d", s.TokensMissing)
		}
		if s.TokensRecorded != 3 {
			t.Fatalf("老桶 recorded=%d want 3", s.TokensRecorded)
		}
	}
}
