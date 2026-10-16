// waf_defer_test.go 出口保护期内的后台顺延（保护期顺延、不记成功）行为锁定。
package scheduler

import (
	"testing"
	"time"

	"github.com/syan-anan/wb-syan/internal/wafguard"
)

// TestWAFProtectionDefersBackground 保护期内后台任务整轮顺延：零上游调用、零账号
// 记账（既不算成功也不算失败），保护解除后自动恢复。
func TestWAFProtectionDefersBackground(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 100}
	g := wafguard.New()
	s := gateTestScheduler(t, f, gateTestPool(), nil)
	s.cfg.WAF = g

	g.Activate(time.Minute)
	s.RunCheckinNow()
	s.RunKeepaliveNow()
	s.RunBalanceRefreshNow()
	if n := f.checkinCalls.Load(); n != 0 {
		t.Errorf("保护期内不该签到: calls=%d", n)
	}
	if n := f.refreshCalls.Load(); n != 0 {
		t.Errorf("保护期内不该保活: calls=%d", n)
	}

	g.Clear()
	s.RunKeepaliveNow()
	if n := f.refreshCalls.Load(); n != 1 {
		t.Errorf("保护解除后应恢复正常执行: calls=%d want 1", n)
	}
}

// TestWAFDeferredNilGuardPassthrough 未装配保护时零成本直通（零配置行为不变）。
func TestWAFDeferredNilGuardPassthrough(t *testing.T) {
	f := &fakeUpstream{}
	s := gateTestScheduler(t, f, gateTestPool(), nil)
	if s.wafDeferred("测试") {
		t.Fatal("未装配 WAF 时不该顺延")
	}
	s.RunKeepaliveNow()
	if n := f.refreshCalls.Load(); n != 1 {
		t.Errorf("calls=%d want 1", n)
	}
}
