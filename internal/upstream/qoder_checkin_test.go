package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// 只领取未领取的活动；已 CLAIMED 的一律跳过。
func TestQoderCheckinSkipsClaimed(t *testing.T) {
	var claimed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sash/api/v1/me/campaigns":
			_, _ = w.Write([]byte(`{"claimable":true,"campaigns":[
				{"campaignId":"a","campaignKey":"k-a","claimStatus":"CLAIMED"},
				{"campaignId":"b","campaignKey":"k-b","claimStatus":"PENDING"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/sash/api/v1/me/campaigns/b/claim":
			claimed = append(claimed, "b")
			_, _ = w.Write([]byte(`{"grantId":"g1","status":"CLAIMED","campaignId":"b","campaignKey":"k-b"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := NewQoderClient("pt", "")
	c.OpenAPI = srv.URL
	c.tok = &QoderToken{Token: "jt", ExpiresAt: "2999-01-01T00:00:00Z"}
	pool := &QoderPool{clients: []*QoderClient{c}, names: []string{"main"}, sticky: map[string]int{}, cooling: map[int]time.Time{}}

	results := pool.CheckinAll(context.Background())
	if len(claimed) != 1 || claimed[0] != "b" {
		t.Fatalf("claimed = %v, want [b]（已领取的活动不应再请求）", claimed)
	}
	if len(results) != 1 || results[0].GrantID != "g1" {
		t.Fatalf("results = %+v", results)
	}
}

// 没有可领活动时给出 NOTHING_TO_CLAIM，而不是静默成功。
func TestQoderCheckinNothingToClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"claimable":false,"campaigns":[]}`))
	}))
	defer srv.Close()

	c := NewQoderClient("pt", "")
	c.OpenAPI = srv.URL
	c.tok = &QoderToken{Token: "jt", ExpiresAt: "2999-01-01T00:00:00Z"}
	pool := &QoderPool{clients: []*QoderClient{c}, names: []string{"main"}, sticky: map[string]int{}, cooling: map[int]time.Time{}}

	results := pool.CheckinAll(context.Background())
	if len(results) != 1 || results[0].Status != "NOTHING_TO_CLAIM" {
		t.Fatalf("results = %+v", results)
	}
}

// 上游报错要落到结果里，不能被吞掉。
func TestQoderCheckinErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	defer srv.Close()

	c := NewQoderClient("pt", "")
	c.OpenAPI = srv.URL
	c.tok = &QoderToken{Token: "jt", ExpiresAt: "2999-01-01T00:00:00Z"}
	pool := &QoderPool{clients: []*QoderClient{c}, names: []string{"main"}, sticky: map[string]int{}, cooling: map[int]time.Time{}}

	results := pool.CheckinAll(context.Background())
	if len(results) != 1 || results[0].Status != "ERROR" {
		t.Fatalf("results = %+v", results)
	}
}

// 连续天数：今天没签不算断签（从昨天往前数）。
func TestQoderCheckinStreak(t *testing.T) {
	h := &QoderCheckinHistory{Days: map[string]int{
		"2026-10-14": 1,
		"2026-10-15": 2,
		"2026-10-16": 1,
	}}
	today := time.Date(2026, 10, 17, 9, 0, 0, 0, time.Local) // 今天还没签
	cur, total := h.Streak(today)
	if cur != 3 {
		t.Fatalf("current = %d, want 3（今天未签不应断签）", cur)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}

	h.Mark("2026-10-17", 1)
	cur, total = h.Streak(today)
	if cur != 4 || total != 4 {
		t.Fatalf("after mark: cur=%d total=%d, want 4/4", cur, total)
	}
}

// 历史落盘 → 回读一致。
func TestQoderCheckinHistoryRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkin.json")
	h := &QoderCheckinHistory{Days: map[string]int{"2026-10-17": 2}}
	if err := h.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	back := LoadQoderCheckinHistory(path)
	if back.Days["2026-10-17"] != 2 {
		t.Fatalf("roundtrip = %+v", back.Days)
	}
	// 文件不存在时返回空历史而不是 nil（调用方不该判空）。
	empty := LoadQoderCheckinHistory(filepath.Join(t.TempDir(), "missing.json"))
	if empty == nil || len(empty.Days) != 0 {
		t.Fatalf("empty = %+v", empty)
	}
}

// 活动列表解析：字段缺失不应 panic。
func TestQoderCampaignsParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"uid":"u1","campaigns":[{"campaignId":"x"}]}`))
	}))
	defer srv.Close()

	c := NewQoderClient("pt", "")
	c.OpenAPI = srv.URL
	c.tok = &QoderToken{Token: "jt", ExpiresAt: "2999-01-01T00:00:00Z"}
	list, err := c.Campaigns(context.Background())
	if err != nil {
		t.Fatalf("campaigns: %v", err)
	}
	b, _ := json.Marshal(list)
	if len(list.Campaigns) != 1 || list.Campaigns[0].CampaignID != "x" {
		t.Fatalf("list = %s", b)
	}
}
