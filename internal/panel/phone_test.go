// phone_test.go 面板手机号登录接口的校验与错误口径。
//
// happy path（真发短信 → 校验 → 入池）无法在测试里跑：那条链要真手机号。
// 凭证生成与 11 步链路在 internal/phonelogin 的测试里用假上游全覆盖了，
// 这里只锁面板这一层特有的东西：路由存在性、参数校验、错误翻译、501 退化。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/syan-anan/wb-syan/internal/phonelogin"
	"github.com/syan-anan/wb-syan/internal/pool"
	"github.com/syan-anan/wb-syan/internal/upstream"
)

// phoneFakeUpstream 只实现 start 需要的 5 步（send/verify 不在这里跑）。
func phoneFakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		raw, _ := json.Marshal(v)
		_, _ = w.Write(raw)
	}
	mux.HandleFunc("/v2/plugin/auth/state", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"code": 0, "data": map[string]any{"state": "ps-1", "authUrl": "u"}})
	})
	mux.HandleFunc("/console/auth/login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/keycloak", http.StatusFound)
	})
	mux.HandleFunc("/keycloak", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<a data-idp=\"oneid\" href=\"/broker/oneid/login?k=1\">x</a>"))
	})
	mux.HandleFunc("/broker/oneid/login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v1/authorize?state=os-1", http.StatusFound)
	})
	mux.HandleFunc("/v1/authorize", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func phoneHarness(t *testing.T) *Panel {
	t.Helper()
	srv := phoneFakeUpstream(t)
	mgr := phonelogin.New(phonelogin.Config{Host: srv.URL, OAuth2: srv.URL, HTTP: srv.Client()})
	return New(Config{Version: "test", APIKey: "test-key", PhoneLogin: mgr})
}

// TestPhoneStartReturnsSIDAndCountries start 应返回 sid 与三个地区。
func TestPhoneStartReturnsSIDAndCountries(t *testing.T) {
	p := phoneHarness(t)
	rec := panelReq(p, "POST", "/panel/api/login/phone/start", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		OK        bool             `json:"ok"`
		SID       string           `json:"sid"`
		Countries []map[string]any `json:"countries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.OK || out.SID == "" {
		t.Fatalf("响应不对: %v %s", err, rec.Body)
	}
	if len(out.Countries) != 3 {
		t.Fatalf("应支持三个地区: %v", out.Countries)
	}
	got := map[string]bool{}
	for _, c := range out.Countries {
		got[c["code"].(string)] = true
	}
	for _, want := range []string{"86", "852", "853"} {
		if !got[want] {
			t.Errorf("缺地区 %s: %v", want, out.Countries)
		}
	}
}

// TestPhoneSendValidation 号码格式错要带 step=mobile 的可执行提示。
func TestPhoneSendValidation(t *testing.T) {
	p := phoneHarness(t)
	rec := panelReq(p, "POST", "/panel/api/login/phone/start", "")
	var st struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || st.SID == "" {
		t.Fatalf("start 失败: %v %s", err, rec.Body)
	}
	// 空 sid：本地就能判掉，不该打上游。
	eb, _ := json.Marshal(map[string]any{"mobile": "13800000000", "country_code": "86"})
	rec = panelReq(p, "POST", "/panel/api/login/phone/send", string(eb))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 sid 应 400，实际 %d %s", rec.Code, rec.Body)
	}
	// 真 sid + 香港地区 + 大陆号码 得到 step=mobile
	payload := map[string]any{"sid": st.SID, "mobile": "13800000000", "country_code": "852"}
	raw, _ := json.Marshal(payload)
	rec = panelReq(p, "POST", "/panel/api/login/phone/send", string(raw))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("地区与号码不匹配应 400，实际 %d %s", rec.Code, rec.Body)
	}
	var perr struct {
		Step    string `json:"step"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &perr)
	if perr.Step != "mobile" || !strings.Contains(perr.Message, "中国香港") {
		t.Errorf("错误应指向地区与号码: %+v", perr)
	}
}

// TestPhoneVerifyNeedsCodeSent 没发码就校验 -> step=code（而不是白跑一趟上游）。
func TestPhoneVerifyNeedsCodeSent(t *testing.T) {
	p := phoneHarness(t)
	rec := panelReq(p, "POST", "/panel/api/login/phone/start", "")
	var st struct {
		SID string `json:"sid"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	raw, _ := json.Marshal(map[string]any{"sid": st.SID, "code": "123456"})
	rec = panelReq(p, "POST", "/panel/api/login/phone/verify", string(raw))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应 400，实际 %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "先获取验证码") {
		t.Errorf("应提示先获取验证码: %s", rec.Body)
	}
}

// TestPhoneStatusUnknownSID 未知 sid -> 404（会话不存在是"没找到"，不是"参数错"）。
func TestPhoneStatusUnknownSID(t *testing.T) {
	p := phoneHarness(t)
	rec := panelReq(p, "GET", "/panel/api/login/phone/status?sid=nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应 404，实际 %d %s", rec.Code, rec.Body)
	}
}

// TestPhoneUnavailableWithoutManager 未装配管理器时四个接口都返回 501（不是 500）。
func TestPhoneUnavailableWithoutManager(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/panel/api/login/phone/start", ""},
		{"POST", "/panel/api/login/phone/send", `{}`},
		{"POST", "/panel/api/login/phone/verify", `{}`},
		{"GET", "/panel/api/login/phone/status?sid=x", ""},
	} {
		rec := panelReq(p, tc.method, tc.path, tc.body)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s: code=%d want 501", tc.method, tc.path, rec.Code)
		}
	}
}

// phoneFullUpstream 把手机号登录的 11 步全实现一遍，账号 uid 可注入。
// 面板这一层要测的是「登录成功之后做什么」，所以链路必须真的走得通。
func phoneFullUpstream(t *testing.T, uid, nickname string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		raw, _ := json.Marshal(v)
		_, _ = w.Write(raw)
	}
	mux.HandleFunc("/v2/plugin/auth/state", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"code": 0, "data": map[string]any{"state": "plugin-state-1", "authUrl": "u"}})
	})
	mux.HandleFunc("/console/auth/login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/keycloak", http.StatusFound)
	})
	mux.HandleFunc("/keycloak", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<a data-idp="oneid" href="/broker/oneid/login?k=1">x</a>`))
	})
	mux.HandleFunc("/broker/oneid/login", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v1/authorize?state=os-1", http.StatusFound)
	})
	mux.HandleFunc("/v1/authorize", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/v1/auth/sms/code/send", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"state_token": "sms-token-1", "expires_in": 60})
	})
	mux.HandleFunc("/v1/auth/sms/code/verify", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"state_token": "sms-token-1"})
	})
	mux.HandleFunc("/v1/auth/accounts", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"code": "oneid-code-1", "accounts": []any{map[string]any{"id": "acc-1"}}})
	})
	mux.HandleFunc("/auth/realms/copilot/broker/oneid/endpoint", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "KEYCLOAK_SESSION", Value: "sess-1", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "KEYCLOAK_IDENTITY", Value: "id-1", Path: "/"})
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v2/plugin/auth/token", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"code": 0, "data": map[string]any{
			"accessToken": "at-1", "refreshToken": "rt-1",
			"expiresIn": 3600, "refreshExpiresIn": 7200,
			"domain": "127.0.0.1", "tokenType": "Bearer"}})
	})
	mux.HandleFunc("/v2/plugin/accounts", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"code": 0, "data": map[string]any{"accounts": []any{map[string]any{
			"uid": uid, "nickname": nickname, "phoneNumber": "13800000000", "enterpriseId": "ent-1"}}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// phoneVerifyHarness 组装一个能跑通整条手机号登录链的面板（凭证落到临时目录、独立空池）。
func phoneVerifyHarness(t *testing.T, uid string) (*Panel, string, *pool.Pool) {
	t.Helper()
	srv := phoneFullUpstream(t, uid, "测试号")
	dir := t.TempDir()
	pl := pool.New("")
	p := New(Config{
		Version: "test", APIKey: "test-key", AuthDir: dir, Pool: pl,
		PhoneLogin: phonelogin.New(phonelogin.Config{Host: srv.URL, OAuth2: srv.URL, HTTP: srv.Client()}),
		Upstream:   &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL},
	})
	return p, dir, pl
}

// phoneLoginTo 走完 start + send，返回 sid（verify 交给调用方，便于注入 expect_uid）。
func phoneLoginTo(t *testing.T, p *Panel) string {
	t.Helper()
	rec := panelReq(p, "POST", "/panel/api/login/phone/start", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var st struct {
		SID string `json:"sid"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.SID == "" {
		t.Fatal("start 未返回 sid")
	}
	raw, _ := json.Marshal(map[string]any{"sid": st.SID, "mobile": "13800000000", "country_code": "86"})
	rec = panelReq(p, "POST", "/panel/api/login/phone/send", string(raw))
	if rec.Code != http.StatusOK {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	return st.SID
}

// TestPhoneVerifyExpectUIDMismatch 原位重登时登进来的是**另一个号**：必须 409 拒绝，
// 既不落盘（不覆盖目标号的凭证、也不凭空多一个文件），也不入池。
// 这是本接口唯一的不可逆风险点，所以断言落在文件系统与池两个地方，而不是只看状态码。
func TestPhoneVerifyExpectUIDMismatch(t *testing.T) {
	p, dir, pl := phoneVerifyHarness(t, "uid-real")
	sid := phoneLoginTo(t, p)
	raw, _ := json.Marshal(map[string]any{"sid": sid, "code": "123456", "expect_uid": "uid-target"})
	rec := panelReq(p, "POST", "/panel/api/login/phone/verify", string(raw))
	if rec.Code != http.StatusConflict {
		t.Fatalf("身份不符应 409，got %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["reason"] != "login_uid_mismatch" {
		t.Errorf("reason=%v", out["reason"])
	}
	// 面板的 api() 只认 error，用户看到的必须是人话而不是 "HTTP 409"。
	if msg, _ := out["error"].(string); msg == "" || !strings.Contains(msg, "另一个账号") {
		t.Errorf("error 应是可读中文说明，got %v", out["error"])
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("拒绝时不得写任何凭证文件，实际 %d 个", len(entries))
	}
	if _, ok := pl.Status("uid-real"); ok {
		t.Error("拒绝时不得把错号入池")
	}
	if _, ok := pl.Status("uid-target"); ok {
		t.Error("拒绝时不得动目标号")
	}
}

// TestPhoneVerifyExpectUIDMatch uid 相符 → 原位重登成功，凭证落盘并入池，响应标 in_place。
func TestPhoneVerifyExpectUIDMatch(t *testing.T) {
	p, dir, pl := phoneVerifyHarness(t, "uid-same")
	sid := phoneLoginTo(t, p)
	raw, _ := json.Marshal(map[string]any{"sid": sid, "code": "123456", "expect_uid": "uid-same"})
	rec := panelReq(p, "POST", "/panel/api/login/phone/verify", string(raw))
	if rec.Code != http.StatusOK {
		t.Fatalf("uid 相符应 200，got %d %s", rec.Code, rec.Body)
	}
	var out struct {
		OK      bool   `json:"ok"`
		UID     string `json:"uid"`
		InPlace bool   `json:"in_place"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.OK || out.UID != "uid-same" || !out.InPlace {
		t.Fatalf("响应不对: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-uid-same.json")); err != nil {
		t.Errorf("凭证应落盘: %v", err)
	}
	if _, ok := pl.Status("uid-same"); !ok {
		t.Error("重登成功应热加载入池")
	}
}

// TestPhoneVerifyWithoutExpectUIDKeepsOldBehavior 不传 expect_uid = 新增登录（现网行为零回归）。
func TestPhoneVerifyWithoutExpectUIDKeepsOldBehavior(t *testing.T) {
	p, dir, _ := phoneVerifyHarness(t, "uid-new")
	sid := phoneLoginTo(t, p)
	raw, _ := json.Marshal(map[string]any{"sid": sid, "code": "123456"})
	rec := panelReq(p, "POST", "/panel/api/login/phone/verify", string(raw))
	if rec.Code != http.StatusOK {
		t.Fatalf("无 expect_uid 应照常成功，got %d %s", rec.Code, rec.Body)
	}
	var out struct {
		InPlace bool `json:"in_place"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.InPlace {
		t.Error("新增登录不该被标成原位重登")
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-uid-new.json")); err != nil {
		t.Errorf("新增登录应落盘: %v", err)
	}
}
