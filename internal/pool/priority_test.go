package pool

import (
	"testing"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
)

// 手动优先级：上移 / 下移 / 恢复自动的值语义与钳位。
func TestAdjustPriorityValueAndClamp(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	if got, ok := p.AdjustPriority("u1", "up"); !ok || got != 1 {
		t.Fatalf("up: priority=%d ok=%v want 1 true", got, ok)
	}
	if got, _ := p.AdjustPriority("u1", "up"); got != 2 {
		t.Fatalf("up again: %d want 2", got)
	}
	if got, _ := p.AdjustPriority("u1", "down"); got != 1 {
		t.Fatalf("down: %d want 1", got)
	}
	if got, _ := p.AdjustPriority("u1", "auto"); got != 0 {
		t.Fatalf("auto: %d want 0", got)
	}
	// 未知 dir 按 auto 处理（前端传错不炸）：先抬起来再喂一个错值。
	p.AdjustPriority("u1", "up")
	if got, _ := p.AdjustPriority("u1", "bogus"); got != 0 {
		t.Fatalf("bogus dir: %d want 0", got)
	}
	// 钳位：连点不越过值域边界。
	for i := 0; i < priorityMax+5; i++ {
		p.AdjustPriority("u2", "up")
	}
	if v := p.byUID["u2"].priority; v != priorityMax {
		t.Fatalf("up clamp: %d want %d", v, priorityMax)
	}
	for i := 0; i < (priorityMax-priorityMin)+5; i++ {
		p.AdjustPriority("u2", "down")
	}
	if v := p.byUID["u2"].priority; v != priorityMin {
		t.Fatalf("down clamp: %d want %d", v, priorityMin)
	}
	// 不存在的账号：ok=false 且不 panic。
	if _, ok := p.AdjustPriority("nope", "up"); ok {
		t.Fatal("missing uid should report ok=false")
	}
}

// 上移的账号进入手动档：高于加权随机，稳定被选中。
func TestPickPrefersManualPriority(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid})
	}
	// u3 积分最低，正常加权下几乎不会被选中；上移一次后应独占。
	p.SetCredits("u1", 50000, 0)
	p.SetCredits("u2", 40000, 0)
	p.SetCredits("u3", 1, 0)
	p.AdjustPriority("u3", "up")
	for i := 0; i < 200; i++ {
		if got := p.Pick(); got == nil || got.UID != "u3" {
			t.Fatalf("pick=%v want u3（手动档必须压过加权随机）", got)
		}
	}
	// 恢复自动后回到加权随机：u3 不再独占（200 次里必然出现别人）。
	p.AdjustPriority("u3", "auto")
	sawOther := false
	for i := 0; i < 200 && !sawOther; i++ {
		if got := p.Pick(); got != nil && got.UID != "u3" {
			sawOther = true
		}
	}
	if !sawOther {
		t.Fatal("恢复自动后仍被独占：auto 未生效")
	}
}

// 下移的账号沉到未干预账号之后（0 档大于负数档）。
func TestPickDownrankedLosesToUntouched(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	// u1 积分远高，正常一定偏 u1；把 u1 下移，u2 应独占。
	p.SetCredits("u1", 50000, 0)
	p.SetCredits("u2", 1, 0)
	p.AdjustPriority("u1", "down")
	for i := 0; i < 200; i++ {
		if got := p.Pick(); got == nil || got.UID != "u2" {
			t.Fatalf("pick=%v want u2（被下移的号必须让位）", got)
		}
	}
}

// 全员被下移时仍要能选出账号（手动档不得把候选集清空成 0 个）。
func TestPickAllDownrankedStillServes(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.AdjustPriority("u1", "down")
	p.AdjustPriority("u2", "down")
	p.AdjustPriority("u2", "down") // -2：u1（-1）应更靠前
	for i := 0; i < 100; i++ {
		if got := p.Pick(); got == nil || got.UID != "u1" {
			t.Fatalf("pick=%v want u1（同为负数时取较大者）", got)
		}
	}
}

// 手动档只排序、不创造资格：优先号被禁用/冷却时，正常回落到其他账号。
func TestManualPriorityKeepsHardFilters(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.AdjustPriority("u1", "up")
	p.Disable("u1", "test")
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("禁用号的优先档不应生效：pick=%v", got)
	}
	p.Revive("u1")
	p.Cooldown("u1", CoolSoft, time.Hour, "test")
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("冷却号的优先档不应生效：pick=%v", got)
	}
	p.Revive("u1")
	// tried（请求级轮换）同样压过手动档：优先号已被本请求试过 → 必须换人。
	if got := p.PickExcluding(map[string]bool{"u1": true}); got == nil || got.UID != "u2" {
		t.Fatalf("tried 应压过手动档：pick=%v", got)
	}
}

// 列表排序的第一档 = 手动优先级（与选号同口径）。
func TestListSortedRespectsPriority(t *testing.T) {
	p := New("")
	p.byUID["old"] = &entry{a: &auth.Auth{UID: "old"}, addedAt: 100}
	p.byUID["mid"] = &entry{a: &auth.Auth{UID: "mid"}, addedAt: 200}
	p.byUID["new"] = &entry{a: &auth.Auth{UID: "new"}, addedAt: 300}
	if got := p.List(); got[0].UID != "old" {
		t.Fatalf("未干预时仍按添加时间：got %s", got[0].UID)
	}
	p.AdjustPriority("new", "up")
	got := p.List()
	if got[0].UID != "new" || got[1].UID != "old" || got[2].UID != "mid" {
		t.Fatalf("上移后应在最前：%v", []string{got[0].UID, got[1].UID, got[2].UID})
	}
	if got[0].Priority != 1 {
		t.Fatalf("Status.Priority 未透出：%d", got[0].Priority)
	}
	p.AdjustPriority("old", "down")
	got = p.List()
	if got[len(got)-1].UID != "old" {
		t.Fatalf("下移后应沉底：%s", got[len(got)-1].UID)
	}
	// 域分组维度下优先级仍是第一档。
	p.AdjustPriority("mid", "up")
	p.AdjustPriority("mid", "up") // 2 > new(1)
	if got := p.ListSorted(SortRealm); got[0].UID != "mid" {
		t.Fatalf("SortRealm 下优先级应仍居首：%s", got[0].UID)
	}
}

// 手动优先级落盘 / 恢复往返：重启后人工排的序不丢。
func TestPriorityPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.AdjustPriority("u1", "up")
	p.AdjustPriority("u2", "down")
	p.mu.Lock()
	p.saveLocked()
	p.mu.Unlock()

	p2 := New(fp)
	if v := p2.byUID["u1"].priority; v != 1 {
		t.Fatalf("u1 恢复后 priority=%d want 1", v)
	}
	if v := p2.byUID["u2"].priority; v != -1 {
		t.Fatalf("u2 恢复后 priority=%d want -1", v)
	}
	// 0 档不落盘（omitempty）：未干预的账号在 state.json 里不占字段。
	p3 := New(fp)
	if _, ok := p3.Status("u3"); ok {
		t.Fatal("u3 不该存在")
	}
}
