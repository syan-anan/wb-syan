package usage

import (
	"testing"
	"time"
)

// 计费量（BilledCredits）与实测成本（Credits）必须分开累计：
// 前者是向调用方收的钱，后者是给上游交的钱，混在一起就算不出毛利。
func TestBilledCreditsSeparateFromCost(t *testing.T) {
	r := New("")
	now := time.Now()
	// 实测花 10 积分，倍率 2.0 → 收 20
	r.Add(now, "cn", "u", "m", "codex", Delta{
		Credits: 10, HasCredits: true, BilledCredits: 20, HasBilled: true,
	}, true)
	// 实测花 5，倍率 0.8 → 收 4
	r.Add(now, "cn", "u", "m", "codex", Delta{
		Credits: 5, HasCredits: true, BilledCredits: 4, HasBilled: true,
	}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Credits != 15 {
		t.Fatalf("实测成本 = %v, want 15", s.Totals.Credits)
	}
	if s.Totals.BilledCredits != 24 {
		t.Fatalf("计费量 = %v, want 24", s.Totals.BilledCredits)
	}
	if s.Totals.CreditsN != 2 {
		t.Fatalf("成本样本数 = %d, want 2", s.Totals.CreditsN)
	}
}

// 没有成本观测时（上游没给 credit）不该计费——不能拿 token 数反推一个价格。
// 这条是账单可信度的底线：猜出来的价对不上账比没有账单更麻烦。
func TestNoBillingWithoutCostObservation(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u", "m", "codex", Delta{
		PromptTokens: 1000, HasPromptTokens: true,
	}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.BilledCredits != 0 {
		t.Fatalf("无观测却计了费 = %v, want 0", s.Totals.BilledCredits)
	}
	if s.Totals.CreditsN != 0 {
		t.Fatalf("无观测却有成本样本 = %d, want 0", s.Totals.CreditsN)
	}
}

// 倍率为 1（未配置）时计费量恒等于实测成本——既有口径不能被新字段改变。
func TestBilledEqualsCostAtRateOne(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u", "m", "codex", Delta{
		Credits: 7.5, HasCredits: true, BilledCredits: 7.5, HasBilled: true,
	}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Credits != s.Totals.BilledCredits {
		t.Fatalf("倍率 1 时计费量(%v) 应等于实测(%v)", s.Totals.BilledCredits, s.Totals.Credits)
	}
}

// 折叠与落盘必须带上计费量：漏了的话，90 天前的数据会只记成本不记收入，
// 长期对账会系统性少算收入。
func TestBilledSurvivesRollupAndFlush(t *testing.T) {
	path := t.TempDir() + "/usage.json"
	r := New(path)
	old := time.Now().AddDate(0, 0, -100)
	r.Add(old, "cn", "u", "m", "codex", Delta{
		Credits: 4, HasCredits: true, BilledCredits: 8, HasBilled: true,
	}, true)
	r.Add(time.Now(), "cn", "u", "m", "codex", Delta{
		Credits: 6, HasCredits: true, BilledCredits: 12, HasBilled: true,
	}, true)
	r.Rollup(time.Now())
	r.Save()

	s := New(path).Snapshot(0, nil)
	if s.Totals.Credits != 10 {
		t.Fatalf("折叠后成本 = %v, want 10", s.Totals.Credits)
	}
	if s.Totals.BilledCredits != 20 {
		t.Fatalf("折叠后计费量 = %v, want 20（CrB 没被搬进日桶）", s.Totals.BilledCredits)
	}
}

// 按账号分组聚合时计费量同样要跟着走（用量页按 key/账号看毛利靠这条）。
func TestBilledAggregatesByAccount(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "m", "codex", Delta{
		Credits: 3, HasCredits: true, BilledCredits: 9, HasBilled: true,
	}, true)
	r.Add(now, "cn", "u2", "m", "codex", Delta{
		Credits: 2, HasCredits: true, BilledCredits: 2, HasBilled: true,
	}, true)

	s := r.Snapshot(24, nil)
	got := map[string]float64{}
	for _, k := range s.ByAccount {
		got[k.Key] = k.BilledCredits
	}
	if got["u1"] != 9 || got["u2"] != 2 {
		t.Fatalf("按账号计费量 = %+v, want u1=9 u2=2", got)
	}
}
