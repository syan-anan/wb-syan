// gate_wiring_test.go —— BackendGate 接线的行为锁定。
//
// 要锁住两件事：
//  1. 对话路径持有在途租约时，后台维护确实跳过该账号（闸门存在的唯一理由）；
//  2. 签到不被闸门的 Cooling 判定挡掉 —— 签到的目的就是给冷却号解冻，
//     挡掉等于把解冻路径整条废掉（thaw=true 的分支就是为它留的）。
package scheduler

import (
	"testing"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/pool"
	"github.com/syan-anan/wb-syan/internal/upstream"
)

// gateTestScheduler 起一套 fake upstream + 单账号池并注入闸门。
// gap=0：测试只验证「跳不跳」，不需要节流等待（有 gap 只会让用例白等）。
func gateTestScheduler(t *testing.T, f *fakeUpstream, p *pool.Pool, gate *BackendGate) *Scheduler {
	t.Helper()
	srv := f.server()
	t.Cleanup(srv.Close)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{
		Pool:           p,
		Upstream:       up,
		Gate:           gate,
		CheckinHours:   []int{9, 21},
		KeepaliveHours: []int{22},
	})
}

func gateTestPool() *pool.Pool {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	return p
}

// TestGateNilKeepsLegacyBehavior 未注入闸门 = 直通，行为与引入闸门前逐字一致。
func TestGateNilKeepsLegacyBehavior(t *testing.T) {
	f := &fakeUpstream{}
	s := gateTestScheduler(t, f, gateTestPool(), nil)
	s.RunKeepaliveNow()
	if n := f.refreshCalls.Load(); n != 1 {
		t.Errorf("无闸门时应照常刷新: calls=%d want 1", n)
	}
}

// TestGateSkipsInFlightAccount 对话持有在途名额时，后台维护必须跳过该账号。
func TestGateSkipsInFlightAccount(t *testing.T) {
	f := &fakeUpstream{}
	p := gateTestPool()
	gate := NewBackendGate(0, 1)
	s := gateTestScheduler(t, f, p, gate)

	if !p.Acquire("u1") {
		t.Fatal("pool.Acquire 失败（默认不限在途，应恒成功）")
	}
	s.RunKeepaliveNow()
	if n := f.refreshCalls.Load(); n != 0 {
		t.Errorf("在途账号不该被后台刷新: calls=%d", n)
	}
	if got := gate.Stats().Skipped[ReasonInFlight]; got != 1 {
		t.Errorf("skipped[%s]=%d want 1", ReasonInFlight, got)
	}
	if got := gate.Stats().Reserved; got != 0 {
		t.Errorf("跳过不该留下悬挂预留: reserved=%d", got)
	}

	p.Release("u1")
	s.RunKeepaliveNow()
	if n := f.refreshCalls.Load(); n != 1 {
		t.Errorf("释放后应恢复刷新: calls=%d want 1", n)
	}
}

// TestGateBalanceRefreshSkipsInFlight 余额刷新（最频繁的一类后台操作）同样受闸门约束。
func TestGateBalanceRefreshSkipsInFlight(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 321}
	p := gateTestPool()
	gate := NewBackendGate(0, 1)
	s := gateTestScheduler(t, f, p, gate)

	if !p.Acquire("u1") {
		t.Fatal("pool.Acquire 失败")
	}
	s.RunBalanceRefreshNow()
	if got := gate.Stats().Skipped[ReasonInFlight]; got != 1 {
		t.Errorf("在途账号应被余额刷新跳过: skipped=%d", got)
	}

	p.Release("u1")
	s.RunBalanceRefreshNow()
	if st, _ := p.Status("u1"); st.Credits != 321 {
		t.Errorf("释放后应写入新鲜余额: credits=%d want 321", st.Credits)
	}
}

// TestGateCheckinStillThawsCooling 签到是解冻操作，冷却号必须照常签到
// （thaw=true 不把 Cooling 当跳过理由）。
func TestGateCheckinStillThawsCooling(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	p := gateTestPool()
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")
	gate := NewBackendGate(0, 1)
	s := gateTestScheduler(t, f, p, gate)

	s.RunCheckinNow()
	if n := f.checkinCalls.Load(); n != 1 {
		t.Errorf("冷却号应照常签到: calls=%d want 1", n)
	}
	if got := gate.Stats().Skipped[ReasonCooling]; got != 0 {
		t.Errorf("签到不该按 Cooling 跳过: skipped[%s]=%d", ReasonCooling, got)
	}
	if st, _ := p.Status("u1"); st.Cooling {
		t.Errorf("签到后应解冻: %+v", st)
	}
}

// TestGateSnapshotIgnoresCoolingOnlyForThaw 直接锁 gateSnapshot 的两分支语义：
// thaw=true 不报 Cooling，thaw=false 照常报（其余后台操作不该动冷却号）。
func TestGateSnapshotIgnoresCoolingOnlyForThaw(t *testing.T) {
	p := gateTestPool()
	p.Cooldown("u1", pool.CoolHard, time.Hour, "余额不足")
	s := New(Config{Pool: p, Upstream: upstream.New()})

	if st := s.gateSnapshot("u1", true)(); st.Cooling {
		t.Errorf("thaw=true 不该报 Cooling: %+v", st)
	}
	if st := s.gateSnapshot("u1", false)(); !st.Cooling {
		t.Errorf("thaw=false 应报 Cooling: %+v", st)
	}
	// 未知 uid：视为不可动，避免后台对着不存在的账号发请求。
	if st := s.gateSnapshot("nope", false)(); !st.Disabled {
		t.Errorf("未知 uid 应视为 disabled: %+v", st)
	}
}
