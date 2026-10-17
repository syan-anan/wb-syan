package server

import (
	"testing"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/pool"
	"github.com/syan-anan/wb-syan/internal/upstream"
)

// 6008（当日请求上限）冷却到下一个本地零点，且不推进 soft_streak——
// 指数退避对日配额没有意义（几分钟后重试一样被拒，只是白烧上游调用）。
func TestApplyErrorPolicy6008CoolsToMidnight(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 10 * time.Second})

	body := `{"code":6008,"msg":"daily request limit exceeded"}`
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "glm-5.2", nil)

	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("应为 soft_rate 冷却: %+v", st)
	}
	if st.SoftStreak != 0 {
		t.Errorf("6008 不应推进指数退避 soft_streak, got %d", st.SoftStreak)
	}
	want := int64(time.Until(nextLocalMidnight(time.Now())).Seconds())
	if diff := st.CoolRemaining - want; diff > 5 || diff < -5 {
		t.Errorf("cool_remaining=%d want ~%d（到本地零点）", st.CoolRemaining, want)
	}
}

func TestNextLocalMidnightShape(t *testing.T) {
	now := time.Date(2026, 10, 17, 23, 59, 30, 0, time.Local)
	got := nextLocalMidnight(now)
	want := time.Date(2026, 10, 18, 0, 0, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("nextLocalMidnight=%v want %v", got, want)
	}
}
