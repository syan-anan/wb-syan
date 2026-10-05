package panel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/syan-anan/wb-syan/internal/usage"
)

// TestUsageResponseJSONShape 锁定 /panel/api/usage 的响应契约：内嵌 Snapshot 的
// 字段必须继续出现在 JSON 顶层（改造前前端就按顶层字段读），新增的积分字段必须
// 存在且类型正确。把内嵌改成命名字段会让整个用量页空白，而 Go 编译、前端语法
// 冒烟都抓不到——只有断言键名才拦得住。
func TestUsageResponseJSONShape(t *testing.T) {
	rec := usage.New("")
	rec.Add(time.Now(), "cn", "uid1", "glm-5.2", "codex", usage.Delta{
		PromptTokens:     10,
		HasPromptTokens:  true,
		CompletionTokens: 5,
		HasCompletion:    true,
	}, true)

	resp := usageResponse{
		Snapshot:            rec.Snapshot(72, nil),
		CreditUsedTotal:     1234,
		CreditUsedPoolTotal: 5678,
		CreditUsedByAccount: map[string]int64{"uid1": 9012},
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal usageResponse: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"totals", "by_realm", "by_account", "by_model", "by_client", "series", "buckets"} {
		if _, ok := got[k]; !ok {
			t.Errorf("usage 响应缺少顶层字段 %q（内嵌 Snapshot 被破坏？）", k)
		}
	}
	var total float64
	if err := json.Unmarshal(got["credit_used_total"], &total); err != nil || total != 1234 {
		t.Errorf("credit_used_total = %s (err=%v), want 1234", got["credit_used_total"], err)
	}
	var poolTotal int64
	if err := json.Unmarshal(got["credit_used_pool_total"], &poolTotal); err != nil || poolTotal != 5678 {
		t.Errorf("credit_used_pool_total = %s (err=%v), want 5678", got["credit_used_pool_total"], err)
	}
	// 逐账号累计口径：键名变了前端「按账号」列就整列空白，锁住。
	var byAcctPool map[string]int64
	if err := json.Unmarshal(got["credit_used_by_account"], &byAcctPool); err != nil {
		t.Fatalf("credit_used_by_account unmarshal: %v", err)
	}
	if byAcctPool["uid1"] != 9012 {
		t.Errorf("credit_used_by_account[uid1] = %d, want 9012", byAcctPool["uid1"])
	}
	// 逐账号积分走 by_account[].credits（Agg 内嵌字段），不再是单独的 map：
	// 键名/类型变了前端就读不到，故连 credits_n 一起锁住。
	var byAcct []map[string]json.RawMessage
	if err := json.Unmarshal(got["by_account"], &byAcct); err != nil || len(byAcct) != 1 {
		t.Fatalf("by_account = %s (err=%v), want 1 行", got["by_account"], err)
	}
	for _, k := range []string{"credits", "credits_n"} {
		if _, ok := byAcct[0][k]; !ok {
			t.Errorf("by_account[0] 缺少字段 %q（积分窗口口径断了？）", k)
		}
	}
	var tot map[string]json.RawMessage
	if err := json.Unmarshal(got["totals"], &tot); err != nil {
		t.Fatalf("totals unmarshal: %v", err)
	}
	for _, k := range []string{"credits", "credits_n"} {
		if _, ok := tot[k]; !ok {
			t.Errorf("totals 缺少字段 %q（积分窗口口径断了？）", k)
		}
	}
}
