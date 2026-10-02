package server

import (
	"strings"
	"testing"

	"github.com/syan-anan/wb-syan/internal/keys"
)

// 子密钥约束闸门：主 key 全放行；模型 / 域 / 时段 / 有效期 / 配额各自独立拒绝。
func TestKeyAccessDenied(t *testing.T) {
	h := &Handler{cfg: Config{KeyQuota: keys.NewQuota("")}}

	if r := h.keyAccessDenied(keys.Identity{Master: true}, "cn", "any"); r != "" {
		t.Fatalf("主 key 必须放行，got %q", r)
	}
	if r := h.keyAccessDenied(keys.Identity{KeyID: "k", Name: "open"}, "global", "any"); r != "" {
		t.Fatalf("未设约束的子 key 必须放行，got %q", r)
	}

	// 模型白名单
	mid := keys.Identity{KeyID: "km", Models: []string{"glm-5.3", "hy3-*"}}
	if r := h.keyAccessDenied(mid, "cn", "glm-5.3"); r != "" {
		t.Fatalf("白名单内模型应放行，got %q", r)
	}
	if r := h.keyAccessDenied(mid, "cn", "hy3-turbo"); r != "" {
		t.Fatalf("通配命中的模型应放行，got %q", r)
	}
	if r := h.keyAccessDenied(mid, "cn", "gpt-4o"); r == "" || !strings.Contains(r, "gpt-4o") {
		t.Fatalf("白名单外模型应拒绝且说明模型名，got %q", r)
	}

	// 域白名单
	rid := keys.Identity{KeyID: "kr", Realms: []string{"cn"}}
	if r := h.keyAccessDenied(rid, "cn", "m"); r != "" {
		t.Fatalf("cn 域应放行，got %q", r)
	}
	if r := h.keyAccessDenied(rid, "global", "m"); r == "" {
		t.Fatal("global 域应被拒绝")
	}

	// 每日生效时段（空窗口 = 恒拒，与真实时钟无关）
	tid := keys.Identity{KeyID: "kt", TimeStart: "12:00", TimeEnd: "12:00"}
	if r := h.keyAccessDenied(tid, "cn", "m"); r == "" || !strings.Contains(r, "时段") {
		t.Fatalf("时段外应拒绝，got %q", r)
	}

	// 有效期（1970 已过期）
	eid := keys.Identity{KeyID: "ke", ExpiresAt: 1}
	if r := h.keyAccessDenied(eid, "cn", "m"); r == "" || !strings.Contains(r, "过期") {
		t.Fatalf("已过期应拒绝，got %q", r)
	}

	// 配额：第一次放行并计数，第二次拒绝
	qid := keys.Identity{KeyID: "kq", DailyLimit: 1}
	if r := h.keyAccessDenied(qid, "cn", "m"); r != "" {
		t.Fatalf("配额内首次应放行，got %q", r)
	}
	if r := h.keyAccessDenied(qid, "cn", "m"); r == "" || !strings.Contains(r, "配额") {
		t.Fatalf("超配额应拒绝，got %q", r)
	}

	// 未装配配额计数器：不 panic、不误拒（受限配额在无计数器时按不限额处理）
	bare := &Handler{}
	if r := bare.keyAccessDenied(keys.Identity{KeyID: "kq2", DailyLimit: 1}, "cn", "m"); r != "" {
		t.Fatalf("无计数器时不应拒绝，got %q", r)
	}
}
