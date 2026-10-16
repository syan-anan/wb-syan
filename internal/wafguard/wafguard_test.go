package wafguard

import (
	"testing"
	"time"
)

// TestGuardLifecycle 零值可用 + 激活/查询/解除全链路。
func TestGuardLifecycle(t *testing.T) {
	var g Guard
	if g.Active() || g.Remaining() != 0 {
		t.Fatal("零值不该处于保护期")
	}
	g.Activate(80 * time.Millisecond)
	if !g.Active() {
		t.Fatal("Activate 后应 Active")
	}
	if r := g.Remaining(); r <= 0 || r > 80*time.Millisecond {
		t.Fatalf("remaining=%v want (0,80ms]", r)
	}
	g.Clear()
	if g.Active() || g.Remaining() != 0 {
		t.Fatalf("Clear 后应完全解除: active=%v remaining=%v", g.Active(), g.Remaining())
	}
}

// TestActivateNeverShortens 新证据只延长、不缩短保护期——出口风控的窗口只会被后续
// 证据确认，缩短等于提前放后台出去继续撞。
func TestActivateNeverShortens(t *testing.T) {
	var g Guard
	g.Activate(time.Second)
	g.Activate(10 * time.Millisecond)
	if r := g.Remaining(); r < 900*time.Millisecond {
		t.Fatalf("保护期被更短的证据缩短了: %v", r)
	}
	g.Activate(2 * time.Second)
	if r := g.Remaining(); r < 1900*time.Millisecond {
		t.Fatalf("更长的证据应生效: %v", r)
	}
	g.Activate(0) // d<=0 = 立即解除
	if g.Active() {
		t.Fatal("Activate(0) 应解除保护")
	}
}

// TestNilGuardSafe nil Guard 恒不激活且不 panic（未装配时的零成本退路）。
func TestNilGuardSafe(t *testing.T) {
	var g *Guard
	g.Activate(time.Second)
	g.Clear()
	if g.Active() || g.Remaining() != 0 {
		t.Fatal("nil Guard 应恒不激活")
	}
	if New().Active() {
		t.Fatal("New() 初始不应激活")
	}
}
