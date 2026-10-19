package panel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/pool"
)

// 积分流水：两套账必须各自独立可查，且同一事件不能记两遍。

func TestJournalDedupeByKey(t *testing.T) {
	j := NewJournal("")
	if !j.Append(CreditEntry{Key: "k1", UID: "u1", Kind: "confirmed", Source: "每日权益", Delta: 50}) {
		t.Fatal("首次写入应成功")
	}
	if j.Append(CreditEntry{Key: "k1", UID: "u1", Kind: "confirmed", Source: "每日权益", Delta: 50}) {
		t.Fatal("同 Key 重复写入必须被拒（重试不该记两遍）")
	}
	res := j.Query(journalQuery{})
	if res.Total != 1 || res.ConfirmedTotal != 50 {
		t.Fatalf("期望 1 条 / 小计 50：%+v", res)
	}
}

func TestJournalObservedHookShape(t *testing.T) {
	j := NewJournal("")
	j.OnBalanceChange("u1", 100, 150)
	j.OnBalanceChange("u1", 150, 150) // 无变化 → 不记
	j.OnBalanceChange("u1", 150, 120) // 扣减 → 记负数
	res := j.Query(journalQuery{Kind: "observed"})
	if res.Total != 2 {
		t.Fatalf("应只有两条观察：%+v", res.Rows)
	}
	if res.ObservedTotal != 20 {
		t.Fatalf("观察小计应为 +50-30=20，得到 %d", res.ObservedTotal)
	}
	if res.ConfirmedTotal != 0 {
		t.Fatal("观察记录不该混进确认小计")
	}
	if res.Rows[0].Delta != -30 {
		t.Fatalf("应倒序返回：%+v", res.Rows)
	}
	if res.Rows[0].BalanceAfter == nil || *res.Rows[0].BalanceAfter != 120 {
		t.Fatal("观察记录必须带变更后余额")
	}
}

func TestJournalQueryFiltersAndPaging(t *testing.T) {
	j := NewJournal("")
	base := time.Now().Add(-72 * time.Hour)
	for i := 0; i < 7; i++ {
		uid := "u1"
		if i%2 == 0 {
			uid = "u2"
		}
		j.Append(CreditEntry{
			Key: fmt.Sprintf("k%d", i), UID: uid, Kind: "confirmed",
			Source: "任务奖励", Delta: int64(10 * (i + 1)), At: base.Add(time.Duration(i) * time.Hour),
		})
	}
	j.Append(CreditEntry{Key: "obs1", UID: "u1", Kind: "observed", Source: "余额观察（来源未确认）", Delta: 3, At: base.Add(8 * time.Hour)})

	all := j.Query(journalQuery{Page: 1, Size: 3})
	if all.Total != 8 || len(all.Rows) != 3 {
		t.Fatalf("分页：total=%d rows=%d", all.Total, len(all.Rows))
	}
	// i%2==0 落 u2：i=0,2,4,6 共 4 条，积分 10/30/50/70。
	only := j.Query(journalQuery{UID: "u2", Kind: "confirmed"})
	if only.Total != 4 {
		t.Fatalf("按账号+类型筛选：%+v", only.Rows)
	}
	// 小计覆盖筛选后的全部匹配，而不是当前页——否则翻页时数字会跳。
	if only.ConfirmedTotal != 10+30+50+70 {
		t.Fatalf("确认小计不该随分页变化，得到 %d", only.ConfirmedTotal)
	}
	// 下界含边界：At == From 保留；base+2h 起有 i=2..6 与 obs1 共 6 条。
	win := j.Query(journalQuery{From: base.Add(2 * time.Hour)})
	if win.Total != 6 {
		t.Fatalf("时间下界筛选：total=%d rows=%+v", win.Total, win.Rows)
	}
	// 上界同样含边界，但只比上界本身：base+4h 以内有 i=0..4 共 5 条。
	upper := j.Query(journalQuery{To: base.Add(4 * time.Hour)})
	if upper.Total != 5 {
		t.Fatalf("时间上界筛选：total=%d rows=%+v", upper.Total, upper.Rows)
	}
	// 区间交集：base+2h 到 base+4h 只有 i=2,3,4 共 3 条。
	both := j.Query(journalQuery{From: base.Add(2 * time.Hour), To: base.Add(4 * time.Hour)})
	if both.Total != 3 {
		t.Fatalf("时间区间筛选：total=%d rows=%+v", both.Total, both.Rows)
	}
	src := j.Query(journalQuery{Source: "余额观察"})
	if src.Total != 1 || src.Rows[0].Kind != "observed" {
		t.Fatalf("来源子串筛选：%+v", src.Rows)
	}
	full := j.Query(journalQuery{})
	if len(full.Accounts) != 2 || len(full.Sources) != 2 {
		t.Fatalf("筛选候选集应覆盖全部条目：%+v %+v", full.Accounts, full.Sources)
	}
}

func TestJournalPersistRoundTrip(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "journal.json")
	j := NewJournal(fp)
	j.RecordConfirmed("u1", "每日权益", 50, "comp|u1|x")
	j.Save()
	j2 := NewJournal(fp)
	if j2.Query(journalQuery{}).Total != 1 {
		t.Fatal("重载后流水丢失")
	}
	if j2.Append(CreditEntry{Key: "comp|u1|x", UID: "u1", Kind: "confirmed", Delta: 50}) {
		t.Fatal("重载后幂等键必须仍然生效")
	}
}

func TestJournalHandler(t *testing.T) {
	j := NewJournal("")
	j.RecordConfirmed("u1", "每日权益", 50, "k1")
	p := New(Config{Version: "test", APIKey: "k", Journal: j})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, authReq(httptest.NewRequest("GET", "/panel/api/journal?kind=confirmed", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	if !strings.Contains(raw, `"confirmed_total":50`) {
		t.Fatalf("响应缺少确认小计：%s", raw)
	}

	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, authReq(httptest.NewRequest("GET", "/panel/api/journal?kind=nope", nil)))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("非法 kind 必须 400（静默忽略会把用户筛到另一个区间），得到 %d", rec2.Code)
	}
}

func TestJournalNilReturns501(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, authReq(httptest.NewRequest("GET", "/panel/api/journal", nil)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("未装配流水应 501，得到 %d", rec.Code)
	}
}

func TestUsageRangeExplicitDates(t *testing.T) {
	q := mustParseQuery("")
	if _, _, ranged, err := usageRange(q); err != nil || ranged {
		t.Fatalf("空参数应回落滚动窗口：%v %v", ranged, err)
	}
	q = mustParseQuery("start=2026-10-01&end=2026-10-01")
	from, to, ranged, err := usageRange(q)
	if err != nil || !ranged {
		t.Fatalf("显式区间应被识别：%v %v", ranged, err)
	}
	// 结束日期含当天：上界落到 23:59:59.999999999，否则当天的桶会被判出区间。
	if !to.After(from.Add(23 * time.Hour)) {
		t.Fatalf("结束边界应覆盖整天：from=%v to=%v", from, to)
	}
	if to.After(from.Add(24 * time.Hour)) {
		t.Fatalf("结束边界不该超过当天：from=%v to=%v", from, to)
	}
	if _, _, _, err := usageRange(mustParseQuery("start=2026-10-05&end=2026-10-01")); err == nil {
		t.Fatal("开始晚于结束必须报错")
	}
	if _, _, _, err := usageRange(mustParseQuery("start=10%2F01%2F2026")); err == nil {
		t.Fatal("非法日期格式必须报错，不能静默忽略")
	}
	// 只给 end：不设下界（上游同口径），且不能报错。
	f2, t2, r2, err := usageRange(mustParseQuery("end=2026-10-09"))
	if err != nil || !r2 || !f2.IsZero() || t2.IsZero() {
		t.Fatalf("只给 end 应只设上界：%v %v %v %v", f2, t2, r2, err)
	}
}

func TestAccountInFlightHandler(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1"})
	p := New(Config{Version: "test", APIKey: "k", Pool: pl})
	post := func(body string) int {
		req := authReq(httptest.NewRequest("POST", "/panel/api/accounts/u1/inflight", strings.NewReader(body)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := post(`{"limit":2}`); c != http.StatusOK {
		t.Fatalf("设置上限应 200，得到 %d", c)
	}
	if v, ok := pl.AccountInFlightLimit("u1"); !ok || v != 2 {
		t.Fatalf("池里没生效：%d %v", v, ok)
	}
	if c := post(`{}`); c != http.StatusBadRequest {
		t.Fatalf("缺 limit 字段必须 400（不能把误操作当成清除），得到 %d", c)
	}
	for _, bad := range []string{`{"limit":true}`, `{"limit":"3"}`, `{"limit":1.5}`, `{"limit":-1}`, `{"limit":1e3}`} {
		if c := post(bad); c != http.StatusBadRequest {
			t.Fatalf("%s 必须 400，得到 %d", bad, c)
		}
	}
	if c := post(`{"limit":null}`); c != http.StatusOK {
		t.Fatalf("null（跟随全局）必须 200，得到 %d", c)
	}
	if v, _ := pl.AccountInFlightLimit("u1"); v != 0 {
		t.Fatalf("清除后应回落到未设置的全局值 0，得到 %d", v)
	}

}

func authReq(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer k")
	return r
}

func mustParseQuery(q string) url.Values {
	v, err := url.ParseQuery(q)
	if err != nil {
		panic(err)
	}
	return v
}
