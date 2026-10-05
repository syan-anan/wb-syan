package usage

import (
	"testing"
	"time"
)

// TestModelStatsWindows 锁 ModelStats 的两个窗口口径：
//   - 今日 = 本地自然日（不是「此刻往前 24 小时」）
//   - 近 N 天 = 含今天在内的 N 个自然日
//   - 状态条只吃整点桶，且按时间升序补齐（缺观测的格子 Req=0，不是被丢掉）
//
// 这三个边界任一写错，模型页显示的可用率就会静默偏小/偏大，而 Go 编译和前端
// 语法检查都抓不到。
func TestModelStatsWindows(t *testing.T) {
	rec := New("")
	now := time.Date(2026, 10, 3, 14, 30, 0, 0, time.Local)

	add := func(scope, model string, req, err, lat int64, tps float64) {
		rec.Add(parseScopeTime(t, scope), "cn", "uid1", model, "codex", Delta{
			TotalTokens: req * 100, HasTotal: true,
			LatencyMs: lat, HasLatency: true,
			TokensPerSecond: tps, HasTPS: true,
		}, true)
		// Add 每次只记 1 次请求；要造 N 次就再补 N-1 次失败样本太啰嗦，
		// 直接用内部桶补足计数（测试同包，可用私有字段）。
		key := scope + "|cn|uid1|" + model + "|codex"
		b := rec.buckets[key]
		if b == nil {
			t.Fatalf("bucket %q 不存在", key)
		}
		b.Req = req
		b.Err = err
		b.LatMs = lat * req
		b.LatN = req
		b.TPS = tps * float64(req)
		b.TPSN = req
	}

	// 今日（10-03）两个整点 + 昨天一个整点 + 40 天前一个整点
	add("h:2026-10-03T13", "m1", 10, 1, 500, 100)
	add("h:2026-10-03T14", "m1", 30, 0, 1000, 200)
	add("h:2026-10-02T23", "m1", 7, 7, 900, 50)
	add("h:2026-08-24T10", "m1", 5, 0, 100, 10)

	stats := rec.ModelStats(now, 30, 24)
	if len(stats) != 1 {
		t.Fatalf("模型数 = %d, want 1", len(stats))
	}
	s := stats[0]
	if s.Model != "m1" {
		t.Errorf("Model = %q, want m1", s.Model)
	}
	if s.TodayReq != 40 || s.TodayErr != 1 {
		t.Errorf("今日 = %d/%d, want 40/1（昨日 23 点与 40 天前都不算今日）", s.TodayReq, s.TodayErr)
	}
	if s.WindowReq != 47 || s.WindowErr != 8 {
		t.Errorf("近 30 天 = %d/%d, want 47/8（含今天，不含 40 天前）", s.WindowReq, s.WindowErr)
	}
	if s.WindowDays != 30 {
		t.Errorf("WindowDays = %d, want 30", s.WindowDays)
	}
	// 状态条：24 格，最后一格是当前整点 14，第一格是 13-23 小时前（昨天 15 点）。
	if len(s.Slots) != 24 {
		t.Fatalf("状态条格数 = %d, want 24", len(s.Slots))
	}
	last := s.Slots[23]
	if last.T != "2026-10-03T14" || last.Req != 30 || last.Err != 0 {
		t.Errorf("末格 = %+v, want 2026-10-03T14 req=30 err=0", last)
	}
	if s.Slots[22].T != "2026-10-03T13" || s.Slots[22].Req != 10 {
		t.Errorf("倒数第二格 = %+v, want 2026-10-03T13 req=10", s.Slots[22])
	}
	// 中间的空格必须是 Req=0 的占位（时间连续），不是被删掉。
	for i, sl := range s.Slots {
		if sl.Req == 0 && sl.T == "" {
			t.Errorf("第 %d 格是空占位但没写时间键", i)
		}
	}
	if s.Slots[0].T != "2026-10-02T15" {
		t.Errorf("首格 = %q, want 2026-10-02T15", s.Slots[0].T)
	}
	// 均延迟/吞吐按样本数加权（不是对每桶均值再平均）。
	if s.AvgLatencyMs < 1 {
		t.Errorf("AvgLatencyMs = %v, want > 0", s.AvgLatencyMs)
	}
	if s.AvgTPS < 1 {
		t.Errorf("AvgTPS = %v, want > 0", s.AvgTPS)
	}
}

// TestModelStatsNoData 从没被调用过的模型不出现在结果里——前端据此显示「—」，
// 而不是被伪造成 100% 可用。
func TestModelStatsNoData(t *testing.T) {
	rec := New("")
	if got := rec.ModelStats(time.Now(), 30, 24); len(got) != 0 {
		t.Fatalf("空记录器返回了 %d 条，want 0", len(got))
	}
	var nilRec *Recorder
	if got := nilRec.ModelStats(time.Now(), 30, 24); got != nil {
		t.Fatalf("nil Recorder 返回了 %d 条，want nil", len(got))
	}
}

// TestModelStatsSort 近 30 天调用降序：模型页按可用率条的顺序读，排序抖了会看着闪。
func TestModelStatsSort(t *testing.T) {
	rec := New("")
	now := time.Date(2026, 10, 3, 14, 0, 0, 0, time.Local)
	for _, c := range []struct {
		model string
		n     int64
	}{{"small", 3}, {"big", 30}, {"mid", 12}} {
		rec.Add(now, "cn", "uid1", c.model, "codex", Delta{TotalTokens: 1, HasTotal: true}, true)
		key := "h:2026-10-03T14|cn|uid1|" + c.model + "|codex"
		rec.buckets[key].Req = c.n
	}
	got := rec.ModelStats(now, 30, 24)
	want := []string{"big", "mid", "small"}
	if len(got) != len(want) {
		t.Fatalf("条数 = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Model != want[i] {
			t.Errorf("第 %d 名 = %q, want %q", i, got[i].Model, want[i])
		}
	}
}

// parseScopeTime 把 "h:2006-01-02T15" / "d:2006-01-02" 解析成本地时间。
func parseScopeTime(t *testing.T, scope string) time.Time {
	t.Helper()
	var ts time.Time
	var err error
	if len(scope) > 2 && scope[0] == 'h' {
		ts, err = time.ParseInLocation(hourLayout, scope[2:], time.Local)
	} else {
		ts, err = time.ParseInLocation(dayLayout, scope[2:], time.Local)
	}
	if err != nil {
		t.Fatalf("解析 %q: %v", scope, err)
	}
	return ts
}
