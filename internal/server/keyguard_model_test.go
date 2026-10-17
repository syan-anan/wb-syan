package server

import (
	"strings"
	"testing"

	"github.com/syan-anan/wb-syan/internal/keys"
)

// 单模型日限：同密钥同模型达到上限后被拒，换模型照常放行。
func TestKeyAccessDeniedModelDaily(t *testing.T) {
	h := &Handler{cfg: Config{KeyQuota: keys.NewQuota("")}}
	id := keys.Identity{KeyID: "kmd", ModelDaily: map[string]int{"glm-5.2": 1}}
	if r := h.keyAccessDenied(id, "cn", "glm-5.2"); r != "" {
		t.Fatalf("首次应放行，got %q", r)
	}
	if r := h.keyAccessDenied(id, "cn", "glm-5.2"); r == "" || !strings.Contains(r, "模型") {
		t.Fatalf("超过模型日限应拒绝，got %q", r)
	}
	if r := h.keyAccessDenied(id, "cn", "glm-5.3"); r != "" {
		t.Fatalf("其它模型不应受影响，got %q", r)
	}
}

// 并发名额：limit=1 时第二个并发请求被拒，Release 后恢复。
func TestKeySlotConcurrency(t *testing.T) {
	h := &Handler{cfg: Config{KeyQuota: keys.NewQuota("")}}
	id := keys.Identity{KeyID: "kc", MaxConcurrent: 1}
	release, why := h.acquireKeySlot(id)
	if why != "" || release == nil {
		t.Fatalf("首个名额应拿到，why=%q", why)
	}
	if _, why := h.acquireKeySlot(id); why == "" || !strings.Contains(why, "并发") {
		t.Fatalf("第二个名额应被拒，got %q", why)
	}
	release()
	if _, why := h.acquireKeySlot(id); why != "" {
		t.Fatalf("释放后应可再次获取，got %q", why)
	}
	// 主身份 / 未配置上限：恒放行且返回 nil（调用方 defer 前会判空）。
	if rel, why := h.acquireKeySlot(keys.Identity{Master: true}); rel != nil || why != "" {
		t.Fatalf("主身份不应受限，got rel!=nil=%v why=%q", rel != nil, why)
	}
}
