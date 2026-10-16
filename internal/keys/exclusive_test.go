// exclusive_test.go 独占绑定（Key 独占账号）的语义锁定。
package keys

import "testing"

// TestExclusiveUIDsSkipsDisabled 停用的 key 不再霸占账号——停用是运维的"先松开"
// 动作，账号应当立刻回到公共池。
func TestExclusiveUIDsSkipsDisabled(t *testing.T) {
	s := New("master", []Entry{
		{ID: "k1", Key: "sk-1", Accounts: []string{"u1"}, Exclusive: true},
		{ID: "k2", Key: "sk-2", Accounts: []string{"u2"}, Exclusive: true, Disabled: true},
		{ID: "k3", Key: "sk-3", Accounts: []string{"u3"}}, // 非独占：不进集合
	})
	got := s.ExclusiveUIDs()
	if len(got) != 1 || !got["u1"] || got["u2"] || got["u3"] {
		t.Fatalf("ExclusiveUIDs=%v want {u1}", got)
	}
	if empty := New("master", nil).ExclusiveUIDs(); empty != nil {
		t.Errorf("无独占绑定应返回 nil（调用方据此跳过过滤），实际 %v", empty)
	}
	if nilStore := (*Store)(nil).ExclusiveUIDs(); nilStore != nil {
		t.Errorf("nil Store 应返回 nil，实际 %v", nilStore)
	}
}

// TestOwnsExclusive 只有「勾了独占 + 账号在名单里 + 非主密钥」才算拥有。
func TestOwnsExclusive(t *testing.T) {
	owner, ok := New("m", []Entry{
		{ID: "k1", Key: "sk-1", Accounts: []string{"u1"}, Exclusive: true},
	}).ResolveToken("sk-1")
	if !ok {
		t.Fatal("解析失败")
	}
	if !owner.OwnsExclusive("u1") {
		t.Error("独占 key 应拥有 u1")
	}
	if owner.OwnsExclusive("u2") {
		t.Error("名单外的号不该算拥有")
	}

	plain, _ := New("m", []Entry{{ID: "k2", Key: "sk-2", Accounts: []string{"u1"}}}).ResolveToken("sk-2")
	if plain.OwnsExclusive("u1") {
		t.Error("没勾独占就只是白名单，不算拥有")
	}
	if (Identity{Master: true}).OwnsExclusive("u1") {
		t.Error("主密钥不是任何账号的独占方")
	}
}

// TestNormalizeEntriesDowngradesBadExclusive 手改 config.json 的兜底：独占必须
// 恰好一个账号，多/少都降级为普通白名单并报告改动。
func TestNormalizeEntriesDowngradesBadExclusive(t *testing.T) {
	out, changed := NormalizeEntries([]Entry{
		{ID: "a", Key: "sk-a", Accounts: []string{"u1", "u2"}, Exclusive: true},
		{ID: "b", Key: "sk-b", Exclusive: true},
		{ID: "c", Key: "sk-c", Accounts: []string{"u3"}, Exclusive: true},
	})
	if !changed {
		t.Fatal("降级应报告改动（调用方据此回写 config.json）")
	}
	if out[0].Exclusive || out[1].Exclusive {
		t.Errorf("多账号/零账号的独占应降级：%+v %+v", out[0], out[1])
	}
	if !out[2].Exclusive {
		t.Errorf("单账号独占应保留：%+v", out[2])
	}
}
