// pick_exclusive_test.go 选号 deny 集合（Key 独占账号）的行为锁定。
package pool

import (
	"testing"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
)

func denyTestPool() *Pool {
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1"})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1000, 0)
	return p
}

// TestPickRespectsDenySet deny 内的账号不参与候选：只剩一个可选号时结果必须确定。
func TestPickRespectsDenySet(t *testing.T) {
	p := denyTestPool()
	deny := map[string]bool{"u1": true}
	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealmAllowDeny(nil, "", "", nil, deny)
		if a == nil || a.UID != "u2" {
			t.Fatalf("第 %d 次选到 %v，期望 u2（u1 在 deny 里）", i, a)
		}
	}
	// 不传 deny 时两个号都可能是候选（确认 deny 而不是别的因素在起作用）。
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		if a := p.PickExcludingForRealmAllowDeny(nil, "", "", nil, nil); a != nil {
			seen[a.UID] = true
		}
	}
	if !seen["u1"] || !seen["u2"] {
		t.Fatalf("无 deny 时两个号都应可被选中，实际 %v", seen)
	}
}

// TestFallbackRespectsDenySet 全冷却兜底同样遵守 deny：否则"专享号"会在公共池
// 全冷却时被借出去——这正是独占绑定最不能漏的一条路径。
func TestFallbackRespectsDenySet(t *testing.T) {
	p := denyTestPool()
	// u1 先恢复（软冷却更短）：若 deny 失效，兜底会挑它。
	p.Cooldown("u1", CoolSoft, time.Minute, "soft")
	p.Cooldown("u2", CoolSoft, time.Hour, "soft")
	if a := p.PickExcludingForRealmAllowDeny(nil, "", "", nil, map[string]bool{"u1": true}); a == nil || a.UID != "u2" {
		t.Fatalf("兜底选到 %v，期望 u2（u1 在 deny 里）", a)
	}

	// 反向对照：同一组冷却条件下不 deny 时应选最早到期的 u1 —— 证明上面的 u2
	// 确实来自 deny 生效，而不是兜底本来就会选 u2。
	q := denyTestPool()
	q.Cooldown("u1", CoolSoft, time.Minute, "soft")
	q.Cooldown("u2", CoolSoft, time.Hour, "soft")
	if b := q.PickExcludingForRealmAllowDeny(nil, "", "", nil, nil); b == nil || b.UID != "u1" {
		t.Fatalf("无 deny 时兜底应选最早到期的 u1，实际 %v", b)
	}
}
