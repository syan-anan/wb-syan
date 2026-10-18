package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/keys"
	"github.com/syan-anan/wb-syan/internal/livecfg"
)

// 子密钥约束闸门：主 key 全放行；模型 / 域 / 时段 / 有效期 / 配额各自独立拒绝。
//
// 拒绝的**口径**也要断言：约束类走 403 key_restricted，额度类走 402
// key_quota_exceeded。这两个状态码混在一起时，客户端无法区分「这把 key 不该这么用」
// 和「这把 key 今天用完了」，会自动把后者拉黑。
func TestKeyAccessDenied(t *testing.T) {
	h := &Handler{cfg: Config{KeyQuota: keys.NewQuota("")}}

	if r := h.keyAccessDenied(keys.Identity{Master: true}, "cn", "any"); r.Denied() {
		t.Fatalf("主 key 必须放行，got %q", r.Reason)
	}
	if r := h.keyAccessDenied(keys.Identity{KeyID: "k", Name: "open"}, "global", "any"); r.Denied() {
		t.Fatalf("未设约束的子 key 必须放行，got %q", r.Reason)
	}

	// 模型白名单
	mid := keys.Identity{KeyID: "km", Models: []string{"glm-5.3", "hy3-*"}}
	if r := h.keyAccessDenied(mid, "cn", "glm-5.3"); r.Denied() {
		t.Fatalf("白名单内模型应放行，got %q", r.Reason)
	}
	if r := h.keyAccessDenied(mid, "cn", "hy3-turbo"); r.Denied() {
		t.Fatalf("通配命中的模型应放行，got %q", r.Reason)
	}
	if r := h.keyAccessDenied(mid, "cn", "gpt-4o"); !r.Denied() || !strings.Contains(r.Reason, "gpt-4o") {
		t.Fatalf("白名单外模型应拒绝且说明模型名，got %q", r.Reason)
	}

	// 域白名单
	rid := keys.Identity{KeyID: "kr", Realms: []string{"cn"}}
	if r := h.keyAccessDenied(rid, "cn", "m"); r.Denied() {
		t.Fatalf("cn 域应放行，got %q", r.Reason)
	}
	if r := h.keyAccessDenied(rid, "global", "m"); !r.Denied() {
		t.Fatal("global 域应被拒绝")
	}

	// 每日生效时段（空窗口 = 恒拒，与真实时钟无关）
	tid := keys.Identity{KeyID: "kt", TimeStart: "12:00", TimeEnd: "12:00"}
	if r := h.keyAccessDenied(tid, "cn", "m"); !r.Denied() || !strings.Contains(r.Reason, "时段") {
		t.Fatalf("时段外应拒绝，got %q", r.Reason)
	}

	// 有效期（1970 已过期）
	eid := keys.Identity{KeyID: "ke", ExpiresAt: 1}
	if r := h.keyAccessDenied(eid, "cn", "m"); !r.Denied() || !strings.Contains(r.Reason, "过期") {
		t.Fatalf("已过期应拒绝，got %q", r.Reason)
	}

	// 配额：第一次放行并计数，第二次拒绝
	qid := keys.Identity{KeyID: "kq", DailyLimit: 1}
	if r := h.keyAccessDenied(qid, "cn", "m"); r.Denied() {
		t.Fatalf("配额内首次应放行，got %q", r.Reason)
	}
	if r := h.keyAccessDenied(qid, "cn", "m"); !r.Denied() || !strings.Contains(r.Reason, "配额") {
		t.Fatalf("超配额应拒绝，got %q", r.Reason)
	}

	// 未装配配额计数器：不 panic、不误拒（受限配额在无计数器时按不限额处理）
	bare := &Handler{}
	if r := bare.keyAccessDenied(keys.Identity{KeyID: "kq2", DailyLimit: 1}, "cn", "m"); r.Denied() {
		t.Fatalf("无计数器时不应拒绝，got %q", r.Reason)
	}
}

// 拒绝口径分流：约束类 403 key_restricted，额度类 402 key_quota_exceeded。
// 每个维度都要锁死，任何一处退回 403 都会让客户端误判成「key 永久不可用」。
func TestKeyDeniedStatusSplit(t *testing.T) {
	h := &Handler{cfg: Config{KeyQuota: keys.NewQuota("")}}

	// 约束类：模型白名单 / 域 / 时段 / 有效期
	type cse struct {
		name string
		id   keys.Identity
		gate func(keys.Identity) keyDeny
	}
	for _, c := range []cse{
		{"model", keys.Identity{KeyID: "c1", Models: []string{"glm-5.3"}},
			func(i keys.Identity) keyDeny { return h.keyAccessDenied(i, "cn", "gpt-4o") }},
		{"realm", keys.Identity{KeyID: "c2", Realms: []string{"cn"}},
			func(i keys.Identity) keyDeny { return h.keyAccessDenied(i, "global", "m") }},
		{"window", keys.Identity{KeyID: "c3", TimeStart: "12:00", TimeEnd: "12:00"},
			func(i keys.Identity) keyDeny { return h.keyAccessDenied(i, "cn", "m") }},
		{"expired", keys.Identity{KeyID: "c4", ExpiresAt: 1},
			func(i keys.Identity) keyDeny { return h.keyAccessDenied(i, "cn", "m") }},
	} {
		d := c.gate(c.id)
		if !d.Denied() {
			t.Fatalf("%s 应被拒绝", c.name)
		}
		if d.Code != keyCodeRestricted || d.Status() != http.StatusForbidden {
			t.Errorf("%s: 约束类应是 403 key_restricted，got %d %q", c.name, d.Status(), d.Code)
		}
	}

	// 额度类：日限 / 时限 / 单模型日限，三种都要 402 key_quota_exceeded
	quotaCases := []struct {
		name string
		id   keys.Identity
	}{
		{"daily", keys.Identity{KeyID: "qd", DailyLimit: 1}},
		{"hourly", keys.Identity{KeyID: "qh", HourlyLimit: 1}},
		{"modelDaily", keys.Identity{KeyID: "qm", ModelDaily: map[string]int{"m": 1}}},
	}
	for _, c := range quotaCases {
		if d := h.keyAccessDenied(c.id, "cn", "m"); d.Denied() {
			t.Fatalf("%s 首次应放行，got %q", c.name, d.Reason)
		}
		d := h.keyAccessDenied(c.id, "cn", "m")
		if !d.Denied() {
			t.Fatalf("%s 超配额应拒绝", c.name)
		}
		if d.Code != keyCodeQuota || d.Status() != http.StatusPaymentRequired {
			t.Errorf("%s: 额度类应是 402 key_quota_exceeded，got %d %q", c.name, d.Status(), d.Code)
		}
		if !strings.Contains(d.Reason, "额度已用完") {
			t.Errorf("%s: 拒绝原因要说明额度用完，got %q", c.name, d.Reason)
		}
	}

	// AnyRealm 口径与主口径一致（compat / qoder 走的就是这条）
	ah := &Handler{cfg: Config{KeyQuota: keys.NewQuota("")}}
	iq := keys.Identity{KeyID: "qa", DailyLimit: 1}
	if d := ah.keyAccessDeniedAnyRealm(iq, "glm-5.2"); d.Denied() {
		t.Fatalf("首次应放行，got %q", d.Reason)
	}
	if d := ah.keyAccessDeniedAnyRealm(iq, "glm-5.2"); d.Status() != http.StatusPaymentRequired || d.Code != keyCodeQuota {
		t.Errorf("AnyRealm 额度类应 402，got %d %q", d.Status(), d.Code)
	}
	if d := ah.keyAccessDeniedAnyRealm(keys.Identity{KeyID: "qa2", Models: []string{"x"}}, "glm-5.2"); d.Status() != http.StatusForbidden || d.Code != keyCodeRestricted {
		t.Errorf("AnyRealm 约束类应 403，got %d %q", d.Status(), d.Code)
	}
}

// TestChatQuotaExceededIs402 端到端口径：子密钥日配额用完后，HTTP 层必须回
// 402 + key_quota_exceeded，而不是 403。同时约束类（模型白名单）仍回 403。
// 这两个码在真实客户端里走的是两条分支：402 = 等窗口重置，403 = 这把 key 配错了。
func TestChatQuotaExceededIs402(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	live := livecfg.New(livecfg.Snapshot{
		APIKey: "master",
		Keys: keys.New("master", []keys.Entry{
			{ID: "kq", Key: "sk-quota", Name: "quota", DailyLimit: 1},
			{ID: "km", Key: "sk-model", Name: "model", Models: []string{"glm-5.3"}},
		}),
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live, KeyQuota: keys.NewQuota("")})

	const body = `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	// 额度类：第一次放行，第二次 402
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-quota")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("配额内首次应 200，got %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-quota")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("超配额应 402，got %d %s", rec.Code, rec.Body)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Error.Code != "key_quota_exceeded" {
		t.Errorf("error.code 应为 key_quota_exceeded，got %q", e.Error.Code)
	}
	if !strings.Contains(e.Error.Message, "额度已用完") {
		t.Errorf("拒绝原因要说明额度用完，got %q", e.Error.Message)
	}

	// 约束类：模型白名单不匹配仍是 403 key_restricted
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-model")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("模型白名单外应 403，got %d %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Error.Code != "key_restricted" {
		t.Errorf("error.code 应为 key_restricted，got %q", e.Error.Code)
	}

	// 主密钥不受配额影响
	for i := 0; i < 3; i++ {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer master")
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("主密钥不该被配额挡住，got %d %s", rec.Code, rec.Body)
		}
	}
}

// 放行时零值的 Code/Status 不能被误当成拒绝：Reason 是唯一判据。
func TestKeyAllowZeroValue(t *testing.T) {
	d := keyAllow()
	if d.Denied() {
		t.Fatal("放行不该 Denied")
	}
}
