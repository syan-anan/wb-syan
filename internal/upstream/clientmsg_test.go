package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// 业务码提取：覆盖 WorkBuddy 主信封 / OpenAI 形态 / 数字字符串 / extError
// 的非数字码 / 纯文本 body 五种真实形态。
func TestExtractCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
		ok   bool
	}{
		{"workbuddy-envelope", `{"code":11133,"msg":"Invalid request parameters","requestId":"x"}`, 11133, true},
		{"openai-error-object", `{"error":{"code":6004,"message":"model limit"}}`, 6004, true},
		{"numeric-string", `{"code":"6008"}`, 6008, true},
		{"ext-error-non-numeric", `{"code":0,"extError":{"code":"model_param_invalid"}}`, 0, false},
		{"plain-text", `upstream said: "code": 11102 something`, 11102, true},
		{"empty", ``, 0, false},
		{"no-code", `{"msg":"boom"}`, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ExtractCode(c.body)
			if ok != c.ok || (ok && got != c.want) {
				t.Fatalf("ExtractCode(%q) = (%d,%v) want (%d,%v)", c.body, got, ok, c.want, c.ok)
			}
		})
	}
}

// 码表必须能命中实测过的关键码，且文案是中文（客户端解释层的存在意义就是
// 把英文/裸码翻译成人话）。
func TestClientMessageKnownCodes(t *testing.T) {
	for _, code := range []int{6004, 6008, 11102, 11133, 11115, 429, 402} {
		m, ok := ClientMessage(code)
		if !ok {
			t.Fatalf("code %d 未收录", code)
		}
		if strings.TrimSpace(m.Message) == "" {
			t.Fatalf("code %d 文案为空", code)
		}
		if !hasHan(m.Message) {
			t.Fatalf("code %d 文案不是中文：%q", code, m.Message)
		}
	}
	if _, ok := ClientMessage(987654); ok {
		t.Fatal("未收录的码不该命中")
	}
}

// hasHan 判断字符串里是否含 CJK 汉字（不算标点：中文文案里也有全角符号）。
func hasHan(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// ExplainBody：解出码但码表没收录 → 返回码 + hit=false（调用方只补 upstream_code，
// 不编造中文）。
func TestExplainBodyUncoveredCode(t *testing.T) {
	code, m, hit := ExplainBody(`{"code":987654,"msg":"unknown"}`)
	if code != 987654 || hit || m.Message != "" {
		t.Fatalf("got (%d,%+v,%v) want (987654,zero,false)", code, m, hit)
	}
}

// error 帧附加：命中码时补 upstream_code + client_message；未命中且无 hint 时
// **逐字节原样返回**（零回归）。
func TestAttachDiagnosticsToErrorFrame(t *testing.T) {
	payload := `{"error":{"message":"{\"code\":6008,\"msg\":\"limit\"}","type":"upstream_error"}}`
	out := attachDiagnosticsToErrorFrame(payload, "")
	var obj struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("out 不是 JSON: %v (%s)", err, out)
	}
	if obj.Error["upstream_code"] != float64(6008) {
		t.Fatalf("upstream_code=%v want 6008 (%s)", obj.Error["upstream_code"], out)
	}
	if _, ok := obj.Error["client_message"]; !ok {
		t.Fatalf("client_message 缺失: %s", out)
	}
	if _, ok := obj.Error["gateway_hint"]; ok {
		t.Fatalf("hint 为空时不该带 gateway_hint: %s", out)
	}

	// 无码无 hint：原文逐字节不变。
	plain := `{"error":{"message":"boom","type":"upstream_error"}}`
	if got := attachDiagnosticsToErrorFrame(plain, ""); got != plain {
		t.Fatalf("无码无 hint 时应原样返回：%s", got)
	}
	// 非 JSON：原样返回。
	if got := attachDiagnosticsToErrorFrame("not json", "hint"); got != "not json" {
		t.Fatalf("非 JSON 应原样返回：%s", got)
	}
	// 只有 hint：保留既有语义（只加 gateway_hint）。
	honly := attachDiagnosticsToErrorFrame(plain, "retry later")
	if !strings.Contains(honly, `"gateway_hint":"retry later"`) {
		t.Fatalf("hint 未附加：%s", honly)
	}
}
