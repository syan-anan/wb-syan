package keys

import (
	"path/filepath"
	"testing"
	"time"
)

func TestAllowsModelAndRealm(t *testing.T) {
	id := Identity{KeyID: "k1", Models: []string{"glm-5.3", "hy3-*"}, Realms: []string{"cn"}}
	cases := []struct {
		model string
		want  bool
	}{
		{"glm-5.3", true},
		{"GLM-5.3", true},  // 大小写不敏感
		{"glm-5.4", false}, // 精确项不前缀匹配
		{"hy3-x", true},    // 结尾 * 前缀通配
		{"hy3-", true},
		{"hy4-x", false},
		{"", true}, // 无模型上下文（/v1/models）不做模型判定
	}
	for _, c := range cases {
		if got := id.AllowsModel(c.model); got != c.want {
			t.Errorf("AllowsModel(%q)=%v want %v", c.model, got, c.want)
		}
	}
	if !id.AllowsRealm("cn") || id.AllowsRealm("global") {
		t.Error("realm whitelist must accept cn only")
	}
	if !id.AllowsRealm("") {
		t.Error("empty realm normalizes to cn and must be allowed")
	}

	// 未限定 = 全放行
	open := Identity{KeyID: "k2"}
	if !open.AllowsModel("anything") || !open.AllowsRealm("global") {
		t.Error("unrestricted identity must allow everything")
	}
	// 主 key 不受约束（即便结构体里带了限制字段）
	master := Identity{Master: true, Models: []string{"none"}, Realms: []string{"cn"}, ExpiresAt: 1}
	if !master.AllowsModel("x") || !master.AllowsRealm("global") {
		t.Error("master identity must bypass model/realm limits")
	}
	if ok, _ := master.AllowsAt(time.Unix(2, 0)); !ok {
		t.Error("master identity must bypass time limits")
	}
}

func TestAllowsAt(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 2, h, m, 0, 0, time.Local) }
	cases := []struct {
		name string
		id   Identity
		now  time.Time
		want bool
	}{
		{"日内窗口-内", Identity{KeyID: "k", TimeStart: "09:00", TimeEnd: "18:00"}, at(9, 0), true},
		{"日内窗口-中", Identity{KeyID: "k", TimeStart: "09:00", TimeEnd: "18:00"}, at(12, 30), true},
		{"日内窗口-早于", Identity{KeyID: "k", TimeStart: "09:00", TimeEnd: "18:00"}, at(8, 59), false},
		{"日内窗口-到点即止", Identity{KeyID: "k", TimeStart: "09:00", TimeEnd: "18:00"}, at(18, 0), false},
		{"跨零点-夜内", Identity{KeyID: "k", TimeStart: "22:00", TimeEnd: "06:00"}, at(23, 30), true},
		{"跨零点-凌晨", Identity{KeyID: "k", TimeStart: "22:00", TimeEnd: "06:00"}, at(2, 0), true},
		{"跨零点-白天", Identity{KeyID: "k", TimeStart: "22:00", TimeEnd: "06:00"}, at(12, 0), false},
		{"跨零点-终点即止", Identity{KeyID: "k", TimeStart: "22:00", TimeEnd: "06:00"}, at(6, 0), false},
		{"只有起点-之后", Identity{KeyID: "k", TimeStart: "09:00"}, at(23, 59), true},
		{"只有起点-之前", Identity{KeyID: "k", TimeStart: "09:00"}, at(8, 59), false},
		{"只有终点-之前", Identity{KeyID: "k", TimeEnd: "09:00"}, at(8, 59), true},
		{"只有终点-到点", Identity{KeyID: "k", TimeEnd: "09:00"}, at(9, 0), false},
		{"空窗口恒拒", Identity{KeyID: "k", TimeStart: "12:00", TimeEnd: "12:00"}, at(12, 0), false},
		{"有效期-未到", Identity{KeyID: "k", ExpiresAt: at(12, 0).Unix()}, at(11, 59), true},
		{"有效期-已到", Identity{KeyID: "k", ExpiresAt: at(12, 0).Unix()}, at(12, 0), false},
		{"无限制", Identity{KeyID: "k"}, at(3, 0), true},
	}
	for _, c := range cases {
		got, why := c.id.AllowsAt(c.now)
		if got != c.want {
			t.Errorf("%s: AllowsAt=%v want %v (reason=%q)", c.name, got, c.want, why)
		}
		if !got && why == "" {
			t.Errorf("%s: 拒绝时必须给出可读原因", c.name)
		}
	}
}

func TestParseClock(t *testing.T) {
	if v, ok := ParseClock("09:30"); !ok || v != 9*60+30 {
		t.Errorf("ParseClock(09:30)=%d,%v", v, ok)
	}
	for _, bad := range []string{"", "  ", "9", "24:00", "09:60", "-1:00", "aa:bb"} {
		if _, ok := ParseClock(bad); ok {
			t.Errorf("ParseClock(%q) must fail", bad)
		}
	}
}

func TestQuotaReserveAndRoll(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.Local)
	q := NewQuota("")
	for i := 0; i < 3; i++ {
		if ok, why := q.Reserve("k1", 3, 0, now); !ok {
			t.Fatalf("第 %d 次应放行，got reason=%q", i+1, why)
		}
	}
	if ok, why := q.Reserve("k1", 3, 0, now); ok || why == "" {
		t.Fatalf("第 4 次应被每日配额拒绝（reason=%q）", why)
	}
	if d, h := q.Usage("k1", now); d != 3 || h != 3 {
		t.Fatalf("Usage=%d,%d want 3,3", d, h)
	}
	// 跨日窗口滚动：计数清零
	if ok, _ := q.Reserve("k1", 3, 0, now.Add(24*time.Hour)); !ok {
		t.Fatal("次日应重新放行")
	}
	// 时窗口独立：同一天下一小时重新计数
	if ok, _ := q.Reserve("k1", 0, 2, now.Add(time.Hour)); !ok {
		t.Fatal("每小时配额应在下一小时重新计数")
	}
	// 0 = 不限：恒放行且不计数
	q2 := NewQuota("")
	for i := 0; i < 5; i++ {
		if ok, _ := q2.Reserve("k2", 0, 0, now); !ok {
			t.Fatal("未配置配额时必须恒放行")
		}
	}
	if d, h := q2.Usage("k2", now); d != 0 || h != 0 {
		t.Fatalf("未配置配额不应计数，got %d,%d", d, h)
	}
	// 不同 key 互不影响
	if ok, _ := q2.Reserve("k3", 1, 0, now); !ok {
		t.Fatal("新 key 首次应放行")
	}
	if ok, _ := q2.Reserve("k3", 1, 0, now); ok {
		t.Fatal("同 key 第二次应被拒")
	}
	// Reset 清零
	q2.Reset("k3")
	if ok, _ := q2.Reserve("k3", 1, 0, now); !ok {
		t.Fatal("Reset 后应重新放行")
	}
}

func TestQuotaPersistRoundTrip(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "keys_quota.json")
	now := time.Date(2026, 10, 2, 10, 0, 0, 0, time.Local)
	q := NewQuota(fp)
	if ok, _ := q.Reserve("k1", 5, 0, now); !ok {
		t.Fatal("first reserve must pass")
	}
	q.Flush()
	reloaded := NewQuota(fp)
	if d, _ := reloaded.Usage("k1", now); d != 1 {
		t.Fatalf("重启后计数应保留，got day=%d", d)
	}
	// 落盘损坏时不 panic，退化为空计数
	if err := writeFile(fp, []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if d, _ := NewQuota(fp).Usage("k1", now); d != 0 {
		t.Fatalf("损坏文件应退化为空计数，got %d", d)
	}
}
