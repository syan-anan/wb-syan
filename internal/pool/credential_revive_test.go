package pool

import (
	"testing"

	"github.com/syan-anan/wb-syan/internal/auth"
)

// 换凭证（重新登录/重新导入同一 uid）必须解除「上游登录态失效」类自动禁用，
// 但人工禁用保持原样（对齐 workbuddy2api 1.0.5）。
func TestAddCredentialRevivesAutoDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "old"})
	p.Disable("u1", "12153 session dead")
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("前置：应已禁用")
	}
	p.Add(&auth.Auth{UID: "u1", AccessToken: "new"})
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("换凭证后应自动解除自动禁用: %+v", st)
	}
	if st, _ := p.Status("u1"); st.DisabledReason != "" {
		t.Fatalf("禁用原因应清空，got %q", st.DisabledReason)
	}
}

func TestAddCredentialKeepsManualDisable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u2", AccessToken: "old"})
	p.Disable("u2", "manual disable (panel)")
	p.Add(&auth.Auth{UID: "u2", AccessToken: "new"})
	if st, _ := p.Status("u2"); !st.Disabled {
		t.Fatal("人工禁用不应被导入悄悄推翻")
	}
}
