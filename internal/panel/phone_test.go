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
	"strings"
	"testing"

	"github.com/syan-anan/wb-syan/internal/phonelogin"
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
