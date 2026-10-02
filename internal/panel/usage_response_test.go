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
		Snapshot:        rec.Snapshot(72, nil),
		CreditUsedTotal: 1234,
		CreditUsed:      map[string]int64{"uid1": 1234},
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
	var total int64
	if err := json.Unmarshal(got["credit_used_total"], &total); err != nil || total != 1234 {
		t.Errorf("credit_used_total = %s (err=%v), want 1234", got["credit_used_total"], err)
	}
	var per map[string]int64
	if err := json.Unmarshal(got["credit_used"], &per); err != nil || per["uid1"] != 1234 {
		t.Errorf("credit_used = %s (err=%v), want {\"uid1\":1234}", got["credit_used"], err)
	}
}
