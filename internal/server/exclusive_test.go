// exclusive_test.go Key 独占账号绑定的端到端语义 + 出口保护接线。
package server

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/keys"
	"github.com/syan-anan/wb-syan/internal/livecfg"
	"github.com/syan-anan/wb-syan/internal/pool"
	"github.com/syan-anan/wb-syan/internal/upstream"
	"github.com/syan-anan/wb-syan/internal/wafguard"
)

// TestExclusiveBindingEndToEnd 独占绑定的三面语义（用上游收到的账号 token 判定用了哪个号）：
//
//	主密钥（公共池）        → 看不到别人的专属号
//	其他子密钥（白名单含它）→ 也看不到（不给"借白名单蹭号"留口子）
//	绑定它的那条密钥        → 照常使用
func TestExclusiveBindingEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var used []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		used = append(used, authz)
		mu.Unlock()
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	live := livecfg.New(livecfg.Snapshot{
		APIKey: "master",
		Keys: keys.New("master", []keys.Entry{
			{ID: "k1", Key: "sk-owner", Name: "owner", Accounts: []string{"u1"}, Exclusive: true},
			{ID: "k2", Key: "sk-other", Name: "other", Accounts: []string{"u1", "u2"}},
		}),
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})

	const body = `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	for _, tc := range []struct{ name, token, want string }{
		{"主密钥", "master", "Bearer at2"},
		{"其他子密钥", "sk-other", "Bearer at2"},
		{"绑定密钥", "sk-owner", "Bearer at1"},
	} {
		mu.Lock()
		used = nil
		mu.Unlock()
		for i := 0; i < 3; i++ {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+tc.token)
			h.ServeHTTP(rec, req)
			if rec.Code != 200 {
				t.Fatalf("%s: code=%d body=%s", tc.name, rec.Code, rec.Body)
			}
		}
		mu.Lock()
		got := append([]string(nil), used...)
		mu.Unlock()
		if len(got) != 3 {
			t.Fatalf("%s: 上游调用数=%d want 3", tc.name, len(got))
		}
		for _, g := range got {
			if g != tc.want {
				t.Errorf("%s: 用了 %s，期望 %s", tc.name, g, tc.want)
			}
		}
	}
}

// TestExclusiveDenyHelper deny 集合的构造口径（自己独占的号不进自己的 deny）。
func TestExclusiveDenyHelper(t *testing.T) {
	all := map[string]bool{"u1": true, "u2": true}
	if got := exclusiveDeny(all, keys.Identity{Master: true}); len(got) != 2 {
		t.Errorf("主密钥应全挡: %v", got)
	}
	other := keys.Identity{KeyID: "k2", Accounts: []string{"u1", "u2"}}
	if got := exclusiveDeny(all, other); len(got) != 2 {
		t.Errorf("非绑定方应全挡: %v", got)
	}
	owner := keys.Identity{KeyID: "k1", Accounts: []string{"u1"}, Exclusive: true}
	// 集合里还有别人独占的 u2：绑定 u1 的 key 仍然看不到 u2。
	if got := exclusiveDeny(all, owner); len(got) != 1 || !got["u2"] {
		t.Errorf("绑定方应只挡住别人的 u2: %v", got)
	}
	// 只存在自己独占的那一个号时，deny 必须为 nil（选号侧零成本直通）。
	if got := exclusiveDeny(map[string]bool{"u1": true}, owner); got != nil {
		t.Errorf("绑定方自己独占的号不该进自己的 deny（应返回 nil）: %v", got)
	}
	if got := exclusiveDeny(nil, keys.Identity{Master: true}); got != nil {
		t.Errorf("无独占账号应返回 nil: %v", got)
	}
}

// TestWAFIPGateActivatesProcessGuard IP 级拦截激活时同步置位进程级出口保护
// （后台维护据此顺延）——同一份证据驱动两个消费者。
func TestWAFIPGateActivatesProcessGuard(t *testing.T) {
	g := wafguard.New()
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), WAF: g})
	if h.wafIP.noteWaf("a") {
		t.Fatal("单号命中不该触发 IP 级拦截")
	}
	if g.Active() {
		t.Fatal("单号命中不该置位出口保护")
	}
	if !h.wafIP.noteWaf("b") {
		t.Fatal("两个不同号应触发 IP 级拦截")
	}
	if !g.Active() {
		t.Fatal("IP 级拦截应同步置位出口保护")
	}
	if g.Remaining() <= 0 {
		t.Fatalf("出口保护应有剩余时间，实际 %v", g.Remaining())
	}
}
