package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// allowPool 构造 3 账号全 healthy 的池（关闭防撞号窗口，便于确定性断言）。
func allowPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Domain: ""})
	p.Add(&auth.Auth{UID: "u2", Domain: ""})
	p.Add(&auth.Auth{UID: "u3", Domain: ""})
	return p
}

// 白名单只含 u2：连续选号必须恒为 u2，绝不落到 u1/u3。
func TestPickAllowFiltersToWhitelist(t *testing.T) {
	p := allowPool(t)
	allow := map[string]bool{"u2": true}
	for i := 0; i < 30; i++ {
		a := p.PickExcludingForRealmAllow(nil, "", "", allow)
		if a == nil {
			t.Fatal("allow pick returned nil")
		}
		if a.UID != "u2" {
			t.Fatalf("allow={u2} picked %s (must stay inside whitelist)", a.UID)
		}
	}
	// nil 白名单 = 不过滤（老语义）：三个号都可能出现。
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		if a := p.PickExcludingForRealmAllow(nil, "", "", nil); a != nil {
			seen[a.UID] = true
		}
	}
	if len(seen) < 2 {
		t.Fatalf("nil allow must not filter; seen=%v", seen)
	}
}

// 白名单内账号全部不可用（空池交集 / 不在池内）→ 返回 nil（不跨白名单兜底）。
func TestPickAllowEmptyIntersectionReturnsNil(t *testing.T) {
	p := allowPool(t)
	if a := p.PickExcludingForRealmAllow(nil, "", "", map[string]bool{"nope": true}); a != nil {
		t.Fatalf("allow with no pool match must return nil, got %s", a.UID)
	}
}

// 全冷却兜底同样受白名单约束：白名单里的号被软冷却后仍只从白名单兜底。
func TestPickAllowAppliesToFallback(t *testing.T) {
	p := allowPool(t)
	allow := map[string]bool{"u2": true}
	p.Cooldown("u2", CoolSoft, 10*time.Minute, "test soft")
	a := p.PickExcludingForRealmAllow(nil, "", "", allow)
	if a == nil {
		t.Fatal("fallback within whitelist must still return the whitelisted account")
	}
	if a.UID != "u2" {
		t.Fatalf("fallback escaped whitelist: %s", a.UID)
	}
	// 白名单里没有任何可用/冷却账号 → nil（不跨白名单兜底到 u1/u3）。
	if a := p.PickExcludingForRealmAllow(nil, "", "", map[string]bool{"nope": true}); a != nil {
		t.Fatalf("fallback must not escape whitelist, got %s", a.UID)
	}
}
