package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
)

// 余额钩子：流水记账的正确性全押在「基线只在第一次写入时建立」这一条上。
// 新导入一个 500 分的号若发事件，池里旧值是 0 → 立刻产出一条假的「+500」，
// 而它根本不是任何一次发放。

func TestCreditsHookBaselineFirstWriteNoEvent(t *testing.T) {
	type ev struct {
		uid           string
		before, after int64
	}
	var got []ev
	p := New("")
	p.SetCreditsHook(func(uid string, before, after int64) { got = append(got, ev{uid, before, after}) })
	p.Add(&auth.Auth{UID: "u1"})

	p.SetCredits("u1", 500, 0)
	if len(got) != 0 {
		t.Fatalf("首次写入只应建立基线，不该发事件：%+v", got)
	}
	p.SetCredits("u1", 500, 0)
	if len(got) != 0 {
		t.Fatalf("值没变不该发事件：%+v", got)
	}
	p.SetCredits("u1", 620, 0)
	if len(got) != 1 || got[0].uid != "u1" || got[0].before != 500 || got[0].after != 620 {
		t.Fatalf("真实变更应发一条事件：%+v", got)
	}
}

func TestCreditsHookAllWritePaths(t *testing.T) {
	var n int
	p := New("")
	p.SetCreditsHook(func(string, int64, int64) { n++ })
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100, 0) // 建基线，不计数

	p.SetCreditsDetailed("u1", 180, 0, 0, time.Time{}, 0)
	if n != 1 {
		t.Fatalf("SetCreditsDetailed 应发事件，n=%d", n)
	}
	// 余额>0 且未禁用 → 走 reviveCoolingLocked（解冻路径）。
	p.ReenableIfCredits("u1", 90, 500)
	if n != 2 {
		t.Fatalf("ReenableIfCredits 解冻路径应发事件，n=%d", n)
	}
	// 余额=0 → 走直接写入路径。
	p.ReenableIfCredits("u1", 0, 500)
	if n != 3 {
		t.Fatalf("ReenableIfCredits 写入路径应发事件，n=%d", n)
	}
	// 取消钩子后恢复零开销路径。
	p.SetCreditsHook(nil)
	p.SetCredits("u1", 10, 0)
	if n != 3 {
		t.Fatalf("钩子置 nil 后不该再发事件，n=%d", n)
	}
}

func TestCreditsHookRestoredStateKeepsBaseline(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 400, 900)
	p.Flush()

	// 重载：state.json 里有余额 = 早就认识这个号，基线成立。
	// 若这里丢掉基线，重启后第一次刷新会产出一条假的「+400」。
	var n int
	p2 := New(fp)
	p2.SetCreditsHook(func(string, int64, int64) { n++ })
	p2.Add(&auth.Auth{UID: "u1"})
	if st, ok := p2.Status("u1"); !ok || st.Credits != 400 {
		t.Fatalf("重载后余额丢失：%+v ok=%v", st, ok)
	}
	p2.SetCredits("u1", 400, 900)
	if n != 0 {
		t.Fatalf("重载后同值不该发事件：%d", n)
	}
	p2.SetCredits("u1", 430, 900)
	if n != 1 {
		t.Fatalf("重载后真实变更应发事件：%d", n)
	}
}

func TestAccountInFlightThreeState(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(3)
	p.SetMaxInFlightGlobal(8)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	one := 1
	if eff, ok := p.SetAccountInFlight("u1", &one); !ok || eff != 1 {
		t.Fatalf("账号覆盖应生效：%d %v", eff, ok)
	}
	if eff, ok := p.SetAccountInFlight("u1", nil); !ok || eff != 3 {
		t.Fatalf("清除覆盖后应回落全局 3，得到 %d", eff)
	}
	zero := 0
	if eff, ok := p.SetAccountInFlight("u1", &zero); !ok || eff != 0 {
		t.Fatalf("0 = 不限，应原样返回 0，得到 %d", eff)
	}
	if _, ok := p.SetAccountInFlight("nope", &one); ok {
		t.Fatal("不存在的 uid 应返回 ok=false")
	}
	// 负值钳到 0（不限）：负上限会被当成「恒满载」把号锁死。
	neg := -5
	if eff, ok := p.SetAccountInFlight("u2", &neg); !ok || eff != 0 {
		t.Fatalf("负值应钳到 0，得到 %d", eff)
	}
	if v, ok := p.AccountInFlightLimit("u1"); !ok || v != 0 {
		t.Fatalf("AccountInFlightLimit 与设置不一致：%d", v)
	}
}

func TestAccountInFlightPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	one := 1
	p.SetAccountInFlight("u1", &one)
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.MaxInFlight == nil || *st.MaxInFlight != 1 {
		t.Fatalf("重载后账号级上限丢失：%+v", st.MaxInFlight)
	}
	// 未设置过覆盖的号：重载后必须是 nil（跟随全局），不能被写成 0（不限）。
	p2.Add(&auth.Auth{UID: "u9"})
	if st9, ok := p2.Status("u9"); !ok || st9.MaxInFlight != nil {
		t.Fatalf("未设置的账号应保持 nil：%+v", st9.MaxInFlight)
	}
}
