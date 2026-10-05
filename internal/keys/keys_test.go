package keys

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(token string) *http.Request {
	r := httptest.NewRequest("GET", "/v1/models", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestResolveMasterAndSub(t *testing.T) {
	st := New("master-key", []Entry{
		{ID: "k1", Name: "A", Key: "sk-aaaa", Accounts: []string{"u1", "u2"}},
		{ID: "k2", Name: "B", Key: "sk-bbbb"},
	})
	if !st.Enabled() {
		t.Fatal("store must be enabled with keys configured")
	}
	if id, ok := st.Resolve(req("master-key")); !ok || !id.Master {
		t.Fatalf("master: ok=%v id=%+v", ok, id)
	}
	id, ok := st.Resolve(req("sk-aaaa"))
	if !ok || id.Master || id.KeyID != "k1" || id.Name != "A" {
		t.Fatalf("sub key resolve: ok=%v id=%+v", ok, id)
	}
	if !id.Restricted() || !id.Allows("u1") || !id.Allows("u2") || id.Allows("u3") {
		t.Fatalf("allow semantics wrong: %+v", id)
	}
	if m := id.AllowSet(); len(m) != 2 || !m["u1"] || !m["u2"] {
		t.Fatalf("AllowSet=%v", m)
	}
	// 无白名单的子密钥 = 不限定（与主密钥同口径）
	id2, ok := st.Resolve(req("sk-bbbb"))
	if !ok || id2.Restricted() || id2.AllowSet() != nil || !id2.Allows("anyone") {
		t.Fatalf("unrestricted sub key wrong: ok=%v id=%+v", ok, id2)
	}
}

func TestResolveRejectsBadTokens(t *testing.T) {
	st := New("master-key", []Entry{{ID: "k1", Name: "A", Key: "sk-aaaa"}})
	for _, tok := range []string{"", "wrong", "sk-aaa", "sk-aaaa ", "master-ke", "MASTER-KEY"} {
		if _, ok := st.Resolve(req(tok)); ok {
			t.Errorf("token %q must be rejected", tok)
		}
	}
	// 无 Authorization 头
	if _, ok := st.Resolve(httptest.NewRequest("GET", "/v1/models", nil)); ok {
		t.Error("missing header must be rejected")
	}
}

func TestDisabledKeyRejected(t *testing.T) {
	st := New("", []Entry{{ID: "k1", Key: "sk-aaaa", Disabled: true}})
	if _, ok := st.Resolve(req("sk-aaaa")); ok {
		t.Error("disabled key must be rejected")
	}
	// 主 key 为空且唯一子 key 被禁用：仍启用鉴权（存在子 key 配置）
	if !st.Enabled() {
		t.Error("store with a disabled sub key is still enabled")
	}
}

func TestNoAuthMode(t *testing.T) {
	st := New("", nil)
	if st.Enabled() {
		t.Fatal("empty store must be disabled")
	}
	id, ok := st.Resolve(req(""))
	if !ok || !id.Master {
		t.Fatalf("no-auth mode must resolve to master: ok=%v id=%+v", ok, id)
	}
}

func TestNormalizeEntries(t *testing.T) {
	in := []Entry{
		{Key: "  sk-a  ", Name: " A ", Accounts: []string{"u1", " u1 ", "", "u2"}},
		{Key: "sk-a", Name: "dup"},           // 重复 key → 丢弃
		{Key: "   ", Name: "blank"},          // 空 key → 丢弃
		{Key: "sk-b", ID: "keep", Name: "B"}, // 已有 ID 保留
	}
	out, changed := NormalizeEntries(in)
	if !changed {
		t.Fatal("changed must be true")
	}
	if len(out) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(out), out)
	}
	if out[0].Key != "sk-a" || out[0].Name != "A" || len(out[0].Accounts) != 2 {
		t.Fatalf("entry0 not normalized: %+v", out[0])
	}
	if out[0].ID == "" {
		t.Fatal("missing ID must be filled")
	}
	if out[1].ID != "keep" {
		t.Fatalf("existing ID must be preserved, got %q", out[1].ID)
	}
}

func TestContextIdentity(t *testing.T) {
	if id := FromContext(httptest.NewRequest("GET", "/", nil).Context()); !id.Master {
		t.Fatal("default context identity must be master")
	}
	r := httptest.NewRequest("GET", "/", nil)
	id := Identity{KeyID: "k1", Accounts: []string{"u1"}}
	r = r.WithContext(WithIdentity(r.Context(), id))
	got := FromContext(r.Context())
	if got.KeyID != "k1" || !got.Allows("u1") || got.Allows("u2") {
		t.Fatalf("context identity roundtrip wrong: %+v", got)
	}
}
