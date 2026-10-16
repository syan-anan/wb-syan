// growth_snapshot_test.go 成长中心快照的投影白名单与单段失败隔离。
package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/syan-anan/wb-syan/internal/auth"
)

// TestProjectGrowthSectionWhitelists 白名单投影：展示字段保留，其余（邮箱 / 地址 / 内部 id）
// 一律不透出 —— 面板是给人看的，多透一个字段就多一份泄露面。
func TestProjectGrowthSectionWhitelists(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []string
		notWant []string
	}{
		{"energy", `{"balance":120,"total_consumed":30,"total_earned":150,"email":"a@b.c"}`,
			[]string{"balance", "total_consumed", "total_earned"}, []string{"email"}},
		{"quota", `{"balance":2,"affordable":true,"cost_per_open":10,"max_open_count":3,"address":"某地址"}`,
			[]string{"balance", "affordable", "cost_per_open", "max_open_count"}, []string{"address"}},
		{"profile", `{"level":7,"level_name":"Lv7","completed":12,"total":20,"max_level":30,"uid":"secret"}`,
			[]string{"level", "level_name", "completed", "total", "max_level"}, []string{"uid"}},
		{"redeem", `{"starter_count":1,"advanced_count":2,"legendary_count":0,"remaining_days":5,"phone":"138"}`,
			[]string{"starter_count", "advanced_count", "legendary_count", "remaining_days"}, []string{"phone"}},
		{"lottery", `{"chances":3,"module":{"enabled":true,"token":"secret"}}`,
			[]string{"chances", "module"}, []string{"token"}},
	}
	for _, c := range cases {
		var data map[string]any
		if err := json.Unmarshal([]byte(c.raw), &data); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		out := ProjectGrowthSection(c.name, data)
		raw, _ := json.Marshal(out)
		for _, w := range c.want {
			if !strings.Contains(string(raw), w) {
				t.Errorf("%s: 缺字段 %s（%s）", c.name, w, raw)
			}
		}
		for _, nw := range c.notWant {
			if strings.Contains(string(raw), nw) {
				t.Errorf("%s: 不该透出 %s（%s）", c.name, nw, raw)
			}
		}
	}
}

// TestProjectGrowthSectionNested 嵌套段（热力图 / 连登 / 宠物 / 旅行）的投影。
func TestProjectGrowthSectionNested(t *testing.T) {
	var heat map[string]any
	_ = json.Unmarshal([]byte(`{"cells":[{"date":"2026-10-16","score":3,"has_new_buddy":true,"uid":"x"}],
	  "today":{"date":"2026-10-16","score":3,"is_active":true,"status_text":"活跃"},
	  "range":{"start_date":"2026-01-01","end_date":"2026-12-31","days":365},"secret":"y"}`), &heat)
	out := ProjectGrowthSection("heatmap", heat)
	raw, _ := json.Marshal(out)
	for _, w := range []string{"cells", "date", "score", "has_new_buddy", "today", "status_text", "range", "days"} {
		if !strings.Contains(string(raw), w) {
			t.Errorf("heatmap 缺 %s: %s", w, raw)
		}
	}
	for _, nw := range []string{"secret", `"uid"`} {
		if strings.Contains(string(raw), nw) {
			t.Errorf("heatmap 不该透出 %s: %s", nw, raw)
		}
	}

	var streak map[string]any
	_ = json.Unmarshal([]byte(`{"streak":{"days":5,"month_total_days":12,"next_tier":"7d","next_tier_remaining":2},
	  "redemption_status":{"tier_7d_status":"locked","remaining_days":3,
	    "tiers":[{"tier":"7d","days":7,"credit":50,"energy":10,"cards":1,"chances":2,"extra":"z"}]},
	  "makeup_cards":{"balance":1,"max":3},"timezone":"Asia/Shanghai"}`), &streak)
	sraw, _ := json.Marshal(ProjectGrowthSection("streak", streak))
	for _, w := range []string{"days", "next_tier", "tier_7d_status", "remaining_days", "tiers", "chances", "makeup_cards"} {
		if !strings.Contains(string(sraw), w) {
			t.Errorf("streak 缺 %s: %s", w, sraw)
		}
	}
	if strings.Contains(string(sraw), `"extra"`) {
		t.Errorf("streak/tiers 不该透出 extra: %s", sraw)
	}

	var buddy map[string]any
	_ = json.Unmarshal([]byte(`{"buddy":{"instance_id":"i1","name":"小喵","rarity":"rare","secret":"s"}}`), &buddy)
	braw, _ := json.Marshal(ProjectGrowthSection("buddy", buddy))
	if !strings.Contains(string(braw), "小喵") || strings.Contains(string(braw), "secret") {
		t.Errorf("buddy 投影不对: %s", braw)
	}
	// 没有宠物时给 null（前端据此显示"还没有宠物"，而不是空白键）。
	var empty map[string]any
	_ = json.Unmarshal([]byte(`{}`), &empty)
	if eraw, _ := json.Marshal(ProjectGrowthSection("buddy", empty)); !strings.Contains(string(eraw), "null") {
		t.Errorf("无宠物应为 null: %s", eraw)
	}
}

// TestGrowthSnapshotSectionIsolation 单段失败不影响整体：坏的那段带 error，好的段照常有数据。
func TestGrowthSnapshotSectionIsolation(t *testing.T) {
	client := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "heatmap") {
				return &http.Response{StatusCode: 500,
					Header: http.Header{"Content-Type": []string{"application/json"}},
					Body:   io.NopCloser(strings.NewReader(`{"code":500,"msg":"boom"}`))}, nil
			}
			return &http.Response{StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"code":0,"data":{"balance":7,"secret":"x"}}`))}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	snap := client.GrowthSnapshot(&auth.Auth{UID: "u1"})
	if snap == nil {
		t.Fatal("快照为空")
	}
	if len(snap.Sections) != len(GrowthSectionDefs) {
		t.Fatalf("段数=%d want %d", len(snap.Sections), len(GrowthSectionDefs))
	}
	if snap.Sections["heatmap"].Error == "" {
		t.Error("坏的那段应带 error")
	}
	if snap.Sections["energy"].Error != "" {
		t.Errorf("好的段不该带 error: %+v", snap.Sections["energy"])
	}
	if v, ok := snap.Sections["energy"].Data["balance"]; !ok || v == nil {
		t.Errorf("好的段应带数据: %+v", snap.Sections["energy"].Data)
	}
	if len(snap.Errors) != 1 {
		t.Errorf("errors 只该有 1 条: %+v", snap.Errors)
	}
}
