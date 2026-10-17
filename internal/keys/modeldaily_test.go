package keys

import (
	"strings"
	"testing"
	"time"
)

func TestModelDailyLimitMatch(t *testing.T) {
	id := Identity{KeyID: "k1", ModelDaily: map[string]int{
		"glm-5.2":    100,
		"glm-*":      10,
		"qoder:auto": 5,
		"*":          1,
	}}
	cases := []struct {
		model string
		want  int
	}{
		{"glm-5.2", 100},       // 精确优先
		{"cn:glm-5.2", 100},    // 前缀剥离后精确命中
		{"glm-5.3", 10},        // 通配
		{"qoder:auto", 5},      // 原名精确
		{"everything-else", 1}, // 全局兜底
	}
	for _, c := range cases {
		if got := id.ModelDailyLimit(c.model); got != c.want {
			t.Fatalf("ModelDailyLimit(%q)=%d want %d", c.model, got, c.want)
		}
	}
	if got := (Identity{Master: true}).ModelDailyLimit("glm-5.2"); got != 0 {
		t.Fatalf("master should not be limited, got %d", got)
	}
}

func TestReserveModelCountsAndLimits(t *testing.T) {
	q := NewQuota("")
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.Local)
	// 日配额 5，模型配额 2：第 3 次模型超限，但日计数不能被多记。
	for i := 0; i < 2; i++ {
		if ok, why := q.ReserveModel("k1", 5, 0, "cn:GLM-5.2", 2, now); !ok {
			t.Fatalf("reserve %d: %s", i, why)
		}
	}
	ok, why := q.ReserveModel("k1", 5, 0, "glm-5.2", 2, now)
	if ok || !strings.Contains(why, "模型") {
		t.Fatalf("want model-limit rejection, got ok=%v why=%q", ok, why)
	}
	day, _ := q.Usage("k1", now)
	if day != 2 {
		t.Fatalf("day count=%d want 2 (rejected request must not count)", day)
	}
	if got := q.ModelUsage("k1", "GLM-5.2", now); got != 2 {
		t.Fatalf("model usage=%d want 2", got)
	}
	// 次日窗口滚动：模型计数清零。
	next := now.Add(24 * time.Hour)
	if got := q.ModelUsage("k1", "glm-5.2", next); got != 0 {
		t.Fatalf("model usage after day roll=%d want 0", got)
	}
}

func TestConcurrencyAcquireRelease(t *testing.T) {
	q := NewQuota("")
	if ok, why := q.Acquire("k1", 0); !ok {
		t.Fatalf("limit 0 should always pass: %s", why)
	}
	if ok, _ := q.Acquire("k1", 2); !ok {
		t.Fatal("first acquire failed")
	}
	if ok, _ := q.Acquire("k1", 2); !ok {
		t.Fatal("second acquire failed")
	}
	if ok, why := q.Acquire("k1", 2); ok || !strings.Contains(why, "并发") {
		t.Fatalf("third acquire should be rejected, got ok=%v why=%q", ok, why)
	}
	if got := q.InFlight("k1"); got != 2 {
		t.Fatalf("in-flight=%d want 2", got)
	}
	q.Release("k1")
	q.Release("k1")
	q.Release("k1") // 幂等：多余释放不能把计数打成负数
	if got := q.InFlight("k1"); got != 0 {
		t.Fatalf("in-flight=%d want 0", got)
	}
}

// 白名单写法容错：带前缀与不带前缀都要能用，且 "qoder:*" 不能误放行 cn 模型。
func TestAllowsModelPrefixTolerance(t *testing.T) {
	id := Identity{KeyID: "k", Models: []string{"glm-*", "qoder:auto", "qoder:*"}}
	cases := []struct {
		model string
		want  bool
	}{
		{"glm-5.2", true},
		{"cn:glm-5.2", true},  // 带前缀的全名按裸名命中
		{"qoder:auto", true},  // 精确
		{"auto", false},       // 裸名不会命中带前缀的精确项
		{"qoder:gpt-5", true}, // 前缀通配
		{"gpt-5", false},      // 不能被 qoder:* 误放行
	}
	for _, c := range cases {
		if got := id.AllowsModel(c.model); got != c.want {
			t.Fatalf("AllowsModel(%q)=%v want %v", c.model, got, c.want)
		}
	}
}
