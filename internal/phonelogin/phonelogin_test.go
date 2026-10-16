// phonelogin_test.go 手机号登录全链路的行为锁定（用假上游把 11 步走完）。
package phonelogin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUpstream 假上游：把 11 步链路全实现一遍，并记录关键请求便于断言。
type fakeUpstream struct {
	mu           sync.Mutex
	sendBodies   []map[string]any
	tokenCalls   int
	sessionCook  []string // 每次 /v2/plugin/auth/token 请求带的 Cookie
	needCaptcha  bool
	tokenExpires int
}

func (f *fakeUpstream) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/v2/plugin/auth/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSONRaw(w, map[string]any{"code": 0, "data": map[string]any{
			"state": "plugin-state-1", "authUrl": "https://example.invalid/auth"}})
	})
	mux.HandleFunc("/console/auth/login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/keycloak", http.StatusFound)
	})
	mux.HandleFunc("/keycloak", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html><a data-idp=\"oneid\" href=\"/broker/oneid/login?kc=1\">OneID</a></html>"))
	})
	mux.HandleFunc("/broker/oneid/login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v1/authorize?state=oneid-state-1", http.StatusFound)
	})
	mux.HandleFunc("/v1/authorize", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>ok</html>"))
	})

	mux.HandleFunc("/v1/auth/sms/code/send", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.sendBodies = append(f.sendBodies, body)
		need := f.needCaptcha
		f.mu.Unlock()
		if need {
			writeJSONRaw(w, map[string]any{"captcha": map[string]any{"type": "slider"}, "status": "need_captcha"})
			return
		}
		writeJSONRaw(w, map[string]any{"state_token": "sms-token-1", "expires_in": 60})
	})
	mux.HandleFunc("/v1/auth/sms/code/verify", func(w http.ResponseWriter, r *http.Request) {
		writeJSONRaw(w, map[string]any{"state_token": "sms-token-1"})
	})
	mux.HandleFunc("/v1/auth/accounts", func(w http.ResponseWriter, r *http.Request) {
		writeJSONRaw(w, map[string]any{"code": "oneid-code-1", "accounts": []any{map[string]any{"id": "acc-1"}}})
	})
	mux.HandleFunc("/auth/realms/copilot/broker/oneid/endpoint", func(w http.ResponseWriter, r *http.Request) {
		// 会话建立的判据是这几个 cookie。
		http.SetCookie(w, &http.Cookie{Name: "KEYCLOAK_SESSION", Value: "sess-1", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "KEYCLOAK_IDENTITY", Value: "id-1", Path: "/"})
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v2/plugin/auth/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokenCalls++
		f.sessionCook = append(f.sessionCook, r.Header.Get("Cookie"))
		exp := f.tokenExpires
		f.mu.Unlock()
		if exp == 0 {
			exp = 3600
		}
		writeJSONRaw(w, map[string]any{"code": 0, "data": map[string]any{
			"accessToken": "at-1", "refreshToken": "rt-1",
			"expiresIn": exp, "refreshExpiresIn": 7200,
			"domain": "www.workbuddy.cn", "tokenType": "Bearer"}})
	})
	mux.HandleFunc("/v2/plugin/accounts", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			writeJSONRaw(w, map[string]any{"code": 12151, "msg": "no auth"})
			return
		}
		writeJSONRaw(w, map[string]any{"code": 0, "data": map[string]any{"accounts": []any{map[string]any{
			"uid": "uid-1", "nickname": "小号", "phoneNumber": "15516263612", "enterpriseId": "ent-1"}}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeJSONRaw(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	raw, _ := json.Marshal(v)
	_, _ = w.Write(raw)
}

func testManager(t *testing.T, f *fakeUpstream, srv *httptest.Server) *Manager {
	t.Helper()
	return New(Config{Host: srv.URL, OAuth2: srv.URL, HTTP: srv.Client()})
}

// TestNormalizeMobile 三个地区各自的规则（长度 + 首位），以及带国家码/分隔符的写法。
func TestNormalizeMobile(t *testing.T) {
	ok := []struct{ mobile, cc, want string }{
		{"13800000000", "86", "13800000000"},
		{"+86 138-0000-0000", "86", "13800000000"},
		{"8613800000000", "86", "13800000000"},
		{"51234567", "852", "51234567"},
		{"+852 5123 4567", "852", "51234567"},
		{"66123456", "853", "66123456"},
	}
	for _, c := range ok {
		got, err := NormalizeMobile(c.mobile, c.cc)
		if err != nil || got != c.want {
			t.Errorf("NormalizeMobile(%q,%q)=%q,%v want %q", c.mobile, c.cc, got, err, c.want)
		}
	}
	bad := []struct{ mobile, cc string }{
		{"13800000000", "852"}, // 长度不符
		{"12800000000", "86"},  // 首位不对（1[3-9]）
		{"5123456", "852"},     // 少一位
		{"56123456", "853"},    // 澳门首位必须是 6
		{"13800000000", "1"},   // 不支持的地区
		{"", "86"},
	}
	for _, c := range bad {
		if got, err := NormalizeMobile(c.mobile, c.cc); err == nil {
			t.Errorf("NormalizeMobile(%q,%q)=%q 应该报错", c.mobile, c.cc, got)
		}
	}
}

// TestFullPhoneLoginChain 走完整条链路：Start → SendCode → Verify → 凭证字段正确。
func TestFullPhoneLoginChain(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server(t)
	m := testManager(t, f, srv)
	ctx := context.Background()

	sess, err := m.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sess.pluginState != "plugin-state-1" {
		t.Fatalf("设备 state 未取到: %+v", sess)
	}
	if sess.oneidState != "oneid-state-1" {
		t.Fatalf("OneID state 未取到: %+v", sess)
	}

	exp, retry, err := m.SendCode(ctx, sess.SID(), "13800000000", "86")
	if err != nil {
		t.Fatalf("SendCode: %v", err)
	}
	if exp != 60 || retry != 60 {
		t.Errorf("expires=%d retry=%d", exp, retry)
	}
	if len(f.sendBodies) != 1 {
		t.Fatalf("发码请求数=%d", len(f.sendBodies))
	}
	body := f.sendBodies[0]
	if body["client_code"] != "codebuddy" {
		t.Errorf("client_code=%v", body["client_code"])
	}
	if body["mobile"] != "+86 13800000000" {
		t.Errorf("mobile=%v（必须是 +国家码 空格 号码）", body["mobile"])
	}

	res, err := m.Verify(ctx, sess.SID(), "123456")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.UID != "uid-1" || res.Nickname != "小号" || res.Phone != "15516263612" {
		t.Errorf("账号信息不对: %+v", res)
	}
	if res.Realm != "cn" {
		t.Errorf("手机号登录只有 CN: %s", res.Realm)
	}

	// 凭证必须是桌面端同构结构：顶层 account/auth，expiresAt 是**毫秒**。
	var doc struct {
		Account map[string]any `json:"account"`
		Auth    map[string]any `json:"auth"`
	}
	if err := json.Unmarshal(res.Credential, &doc); err != nil {
		t.Fatalf("凭证不是 JSON: %v", err)
	}
	if doc.Auth["accessToken"] != "at-1" || doc.Auth["refreshToken"] != "rt-1" {
		t.Errorf("auth 字段不对: %+v", doc.Auth)
	}
	if doc.Account["uid"] != "uid-1" || doc.Account["enterpriseId"] != "ent-1" {
		t.Errorf("account 字段不对: %+v", doc.Account)
	}
	expiresAt, _ := doc.Auth["expiresAt"].(float64)
	if expiresAt < 1e12 {
		t.Errorf("expiresAt 应是毫秒时间戳，实际 %v", doc.Auth["expiresAt"])
	}
	if doc.Auth["expiresIn"].(float64) != 3600 {
		t.Errorf("expiresIn 应是秒，实际 %v", doc.Auth["expiresIn"])
	}
}

// TestNeedCaptchaIsTypedError 按号风控要给出可执行提示（换号 / 稍后），而不是笼统失败。
func TestNeedCaptchaIsTypedError(t *testing.T) {
	f := &fakeUpstream{needCaptcha: true}
	srv := f.server(t)
	m := testManager(t, f, srv)
	sess, err := m.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = m.SendCode(context.Background(), sess.SID(), "13800000000", "86")
	if err == nil {
		t.Fatal("应报错")
	}
	var pe *Error
	if !asError(err, &pe) {
		t.Fatalf("应是 *Error，实际 %T", err)
	}
	if pe.Reason != "need_captcha" || pe.Step != "sms_send" {
		t.Errorf("err=%+v", pe)
	}
	if !strings.Contains(pe.Msg, "其他手机号") {
		t.Errorf("提示应指出换号可解: %s", pe.Msg)
	}
}

// TestVerifyIsIdempotent 同一会话重复校验复用结果，不再打上游。
// （state_token 是一次性的：并发/重复校验会变成"成功一次 + 报一次无效 token"。）
func TestVerifyIsIdempotent(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server(t)
	m := testManager(t, f, srv)
	ctx := context.Background()
	sess, _ := m.Start(ctx)
	if _, _, err := m.SendCode(ctx, sess.SID(), "13800000000", "86"); err != nil {
		t.Fatal(err)
	}
	r1, err := m.Verify(ctx, sess.SID(), "123456")
	if err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := f.tokenCalls
	r2, err := m.Verify(ctx, sess.SID(), "123456")
	if err != nil {
		t.Fatalf("第二次校验应复用结果: %v", err)
	}
	if string(r1.Credential) != string(r2.Credential) {
		t.Error("两次结果不一致")
	}
	if f.tokenCalls != callsAfterFirst {
		t.Errorf("第二次不该再打上游: %d -> %d", callsAfterFirst, f.tokenCalls)
	}
}

// TestSessionCookieIsolation 每个会话必须有独立的 cookie jar。
// 共用的话不同用户的 OneID 登录态会串在一起，并发登录表现为"串号"。
func TestSessionCookieIsolation(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server(t)
	m := testManager(t, f, srv)
	ctx := context.Background()
	s1, err := m.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// 走完整条链：Keycloak 会话 cookie 是第 9 步（broker endpoint）才落的。
	if _, _, err := m.SendCode(ctx, s1.SID(), "13800000000", "86"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx, s1.SID(), "123456"); err != nil {
		t.Fatal(err)
	}
	s2, err := m.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s1.sid == s2.sid {
		t.Fatal("会话 id 撞了")
	}
	if s1.client.Jar == s2.client.Jar {
		t.Fatal("两个会话共用了 cookie jar")
	}
	if !s1.hasKeycloakSession(srv.URL) {
		t.Error("会话 1 完成登录后应在自己 jar 里有 Keycloak 会话 cookie")
	}
	if s2.hasKeycloakSession(srv.URL) {
		t.Error("会话 2 不该看到会话 1 的 cookie（隔离失效）")
	}
}

// TestSessionExpiryAndUnknown 过期与未知 sid 都要给人话错误。
func TestSessionExpiryAndUnknown(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server(t)
	now := time.Now()
	m := New(Config{Host: srv.URL, OAuth2: srv.URL, HTTP: srv.Client(),
		TTL: time.Minute, Now: func() time.Time { return now }})
	sess, err := m.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.SendCode(context.Background(), "no-such-sid", "13800000000", "86"); err == nil {
		t.Error("未知 sid 应报错")
	}
	now = now.Add(2 * time.Minute) // 越过 TTL
	if _, err := m.Session(sess.SID()); err == nil {
		t.Error("过期会话应报错")
	}
}

// asError 是 errors.As 的小包装（避免本文件再引一个 import）。
func asError(err error, target **Error) bool {
	for err != nil {
		if e, ok := err.(*Error); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
