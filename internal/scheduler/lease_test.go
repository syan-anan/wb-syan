// lease_test.go：BackendGate 的行为自证。
// 覆盖：同 uid 互斥 / 终态跳过原因 / 节流间隔 / release 幂等 / ctx 取消 / 计数分类 / 预留不泄漏。
package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func idleSnapshot() AccountState { return AccountState{} }

// 16 路并发争抢同一 uid：只能有一个拿到预留，其余全部以 peer 跳过。
func TestGateSameUIDExclusive(t *testing.T) {
	g := NewBackendGate(0, 1)
	snap := idleSnapshot

	const n = 16
	var winners int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	rels := make(chan func(), n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rel, blocked, ok := g.Acquire("uid-1", snap)
			if ok {
				atomic.AddInt64(&winners, 1)
				rels <- rel
				return
			}
			if blocked != ReasonPeer {
				t.Errorf("blocked=%q want %q", blocked, ReasonPeer)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(rels)

	if got := atomic.LoadInt64(&winners); got != 1 {
		t.Fatalf("winners=%d want 1（同 uid 必须互斥）", got)
	}
	for rel := range rels {
		rel()
	}
	if got := g.Stats().Reserved; got != 0 {
		t.Fatalf("Reserved=%d want 0（全部释放后不得残留）", got)
	}
}

// 禁用 / 冷却 / 在途 的账号被跳过，且 blocked 原因正确。
func TestGateSkipsBadStates(t *testing.T) {
	cases := []struct {
		name string
		st   AccountState
		want string
	}{
		{"disabled", AccountState{Disabled: true}, ReasonDisabled},
		{"cooling", AccountState{Cooling: true}, ReasonCooling},
		{"in_flight", AccountState{InFlight: true}, ReasonInFlight},
		// 优先级：终态原因先报，不能把禁用号报成「在途」。
		{"disabled_wins", AccountState{Disabled: true, Cooling: true, InFlight: true}, ReasonDisabled},
	}
	for _, tc := range cases {
		g := NewBackendGate(0, 1)
		st := tc.st
		rel, blocked, ok := g.Acquire("uid-x", func() AccountState { return st })
		if ok || rel != nil {
			t.Fatalf("%s: ok=%v release!=nil:%v，不应拿到预留", tc.name, ok, rel != nil)
		}
		if blocked != tc.want {
			t.Fatalf("%s: blocked=%q want %q", tc.name, blocked, tc.want)
		}
		if got := g.Stats().Skipped[tc.want]; got != 1 {
			t.Fatalf("%s: skipped[%s]=%d want 1", tc.name, tc.want, got)
		}
		if got := g.Stats().Reserved; got != 0 {
			t.Fatalf("%s: Reserved=%d want 0", tc.name, got)
		}
	}
}

// 节流间隔生效：相邻两次预留之间至少间隔 accountGap。
func TestGateThrottleGapEnforced(t *testing.T) {
	const gap = 60 * time.Millisecond
	g := NewBackendGate(gap, 1)
	snap := idleSnapshot

	rel1, _, ok := g.Acquire("a", snap)
	if !ok {
		t.Fatal("第一次预留失败")
	}
	rel1() // 打上节流基准

	start := time.Now()
	rel2, blocked, ok := g.Acquire("b", snap)
	elapsed := time.Since(start)
	if !ok {
		t.Fatalf("第二次预留失败 blocked=%q", blocked)
	}
	rel2()

	if elapsed < gap {
		t.Fatalf("elapsed=%v want >= %v（节流未生效）", elapsed, gap)
	}
	if got := g.Stats().AccountGap; got != gap {
		t.Fatalf("Stats().AccountGap=%v want %v", got, gap)
	}
}

// 间隔设为 0 时不等：不得出现人为延迟。
func TestGateThrottleZeroGapNoWait(t *testing.T) {
	g := NewBackendGate(0, 1)
	snap := idleSnapshot

	rel1, _, ok := g.Acquire("a", snap)
	if !ok {
		t.Fatal("第一次预留失败")
	}
	rel1()

	start := time.Now()
	rel2, blocked, ok := g.Acquire("b", snap)
	elapsed := time.Since(start)
	if !ok {
		t.Fatalf("第二次预留失败 blocked=%q", blocked)
	}
	rel2()

	if elapsed > 50*time.Millisecond {
		t.Fatalf("elapsed=%v want ~0（gap=0 不应等待）", elapsed)
	}
}

// release() 重复调用安全；释放后可再次拿到同一 uid（无泄漏、无重复扣减）。
func TestGateReleaseIdempotent(t *testing.T) {
	g := NewBackendGate(0, 1)
	snap := idleSnapshot

	rel, blocked, ok := g.Acquire("u", snap)
	if !ok {
		t.Fatalf("首次预留失败 blocked=%q", blocked)
	}
	if got := g.Stats().Reserved; got != 1 {
		t.Fatalf("Reserved=%d want 1", got)
	}

	rel()
	rel() // 幂等：不得 panic，也不得把 Reserved 扣成负数
	rel()
	if got := g.Stats().Reserved; got != 0 {
		t.Fatalf("Reserved=%d want 0（重复 release 后）", got)
	}

	rel2, blocked, ok2 := g.Acquire("u", snap)
	if !ok2 {
		t.Fatalf("释放后重新预留失败 blocked=%q", blocked)
	}
	rel2()
	if got := g.Stats().Reserved; got != 0 {
		t.Fatalf("Reserved=%d want 0", got)
	}
}

// ctx 在节流等待期间被取消：等待中的 Acquire 立刻返回 canceled。
func TestGateCtxCancelDuringWait(t *testing.T) {
	g := NewBackendGate(400*time.Millisecond, 1)
	snap := idleSnapshot

	rel, _, ok := g.Acquire("a", snap)
	if !ok {
		t.Fatal("第一次预留失败")
	}
	rel() // 打上节流基准，让下一次必须等满 400ms

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	rel2, blocked, ok2 := g.AcquireCtx(ctx, "b", snap)
	elapsed := time.Since(start)

	if ok2 || rel2 != nil {
		t.Fatalf("ok=%v 取消后不应拿到预留", ok2)
	}
	if blocked != ReasonCanceled {
		t.Fatalf("blocked=%q want %q", blocked, ReasonCanceled)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("elapsed=%v want 立刻返回（远小于 400ms 的节流窗口）", elapsed)
	}
	if got := g.Stats().Reserved; got != 0 {
		t.Fatalf("Reserved=%d want 0（取消不得留下预留）", got)
	}
	// 取消后执行锁必须已释放：下一次预留仍能正常进行。
	rel3, blocked, ok3 := g.AcquireCtx(context.Background(), "c", snap)
	if !ok3 {
		t.Fatalf("取消后闸门应仍可用 blocked=%q", blocked)
	}
	rel3()
}

// Stats() 的跳过计数按原因分类累加；返回的是副本。
func TestGateStatsSkipCounts(t *testing.T) {
	g := NewBackendGate(0, 1)

	seq := []struct {
		st   AccountState
		want string
	}{
		{AccountState{Disabled: true}, ReasonDisabled},
		{AccountState{Disabled: true}, ReasonDisabled},
		{AccountState{Cooling: true}, ReasonCooling},
		{AccountState{InFlight: true}, ReasonInFlight},
		{AccountState{InFlight: true}, ReasonInFlight},
		{AccountState{InFlight: true}, ReasonInFlight},
	}
	for i, s := range seq {
		st := s.st
		_, blocked, ok := g.Acquire("uid-skip", func() AccountState { return st })
		if ok {
			t.Fatalf("case %d 不应拿到预留", i)
		}
		if blocked != s.want {
			t.Fatalf("case %d blocked=%q want %q", i, blocked, s.want)
		}
	}

	stats := g.Stats()
	want := map[string]int64{ReasonDisabled: 2, ReasonCooling: 1, ReasonInFlight: 3}
	for k, v := range want {
		if stats.Skipped[k] != v {
			t.Fatalf("skipped[%s]=%d want %d", k, stats.Skipped[k], v)
		}
	}
	if stats.Reserved != 0 {
		t.Fatalf("Reserved=%d want 0", stats.Reserved)
	}
	if stats.MaxSkip != 1 {
		t.Fatalf("MaxSkip=%d want 1", stats.MaxSkip)
	}

	// peer 计数：先占住 uid，再一次同 uid 请求。
	rel, _, ok := g.Acquire("uid-peer", idleSnapshot)
	if !ok {
		t.Fatal("peer 场景前置预留失败")
	}
	_, blocked, ok := g.Acquire("uid-peer", idleSnapshot)
	if ok || blocked != ReasonPeer {
		t.Fatalf("blocked=%q want %q", blocked, ReasonPeer)
	}
	rel()
	if got := g.Stats().Skipped[ReasonPeer]; got != 1 {
		t.Fatalf("skipped[peer]=%d want 1", got)
	}

	// Stats 返回副本：外部改写不得污染闸门内部状态。
	mut := g.Stats()
	mut.Skipped[ReasonPeer] = 999
	if got := g.Stats().Skipped[ReasonPeer]; got != 1 {
		t.Fatalf("Stats 泄漏了内部 map：skipped[peer]=%d", got)
	}
}

// 预留之间不互相泄漏：一个 release 后另一个能拿到。
func TestGateNoLeakBetweenReservations(t *testing.T) {
	g := NewBackendGate(0, 1)
	snap := idleSnapshot

	relA, blocked, ok := g.Acquire("A", snap)
	if !ok {
		t.Fatalf("A 预留失败 blocked=%q", blocked)
	}
	if got := g.Stats().Reserved; got != 1 {
		t.Fatalf("Reserved=%d want 1", got)
	}
	relA()
	if got := g.Stats().Reserved; got != 0 {
		t.Fatalf("释放 A 后 Reserved=%d want 0", got)
	}

	relB, blocked, ok := g.Acquire("B", snap)
	if !ok {
		t.Fatalf("B 预留失败 blocked=%q（A 的预留泄漏了？）", blocked)
	}
	if got := g.Stats().Reserved; got != 1 {
		t.Fatalf("Reserved=%d want 1", got)
	}
	relB()
	if got := g.Stats().Reserved; got != 0 {
		t.Fatalf("Reserved=%d want 0", got)
	}
}

// 闸门按 uid 粒度互斥（不是全局互斥）：不同 uid 可同时处于预留态。
func TestGateDifferentUIDsConcurrent(t *testing.T) {
	g := NewBackendGate(0, 1)
	snap := idleSnapshot

	relA, blocked, ok := g.Acquire("A", snap)
	if !ok {
		t.Fatalf("A 预留失败 blocked=%q", blocked)
	}
	relB, blocked, ok := g.Acquire("B", snap)
	if !ok {
		t.Fatalf("B 预留失败 blocked=%q", blocked)
	}
	if got := g.Stats().Reserved; got != 2 {
		t.Fatalf("Reserved=%d want 2（不同 uid 应可并行预留）", got)
	}
	relA()
	relB()
	if got := g.Stats().Reserved; got != 0 {
		t.Fatalf("Reserved=%d want 0", got)
	}
}

// uid 为空：直接以 invalid_uid 拒绝，不占任何资源。
func TestGateEmptyUIDRejected(t *testing.T) {
	g := NewBackendGate(0, 1)
	rel, blocked, ok := g.Acquire("", idleSnapshot)
	if ok || rel != nil {
		t.Fatal("空 uid 不应拿到预留")
	}
	if blocked != ReasonInvalidUID {
		t.Fatalf("blocked=%q want %q", blocked, ReasonInvalidUID)
	}
	if got := g.Stats().Reserved; got != 0 {
		t.Fatalf("Reserved=%d want 0", got)
	}
}
