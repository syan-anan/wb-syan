// messages_test.go —— /v1/messages 接线端到端测试。
//
// 覆盖点：两条主出口（非流式整体翻译 / 流式逐帧翻译）都必须复用既有 chatCompletions
// 状态机（因此 fake upstream 与 pool 用的是 handler_test.go 里那套），以及错误路径
// 必须换成 Anthropic 形态（否则 Anthropic SDK 先报解析失败、真实原因被盖掉）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/upstream"
)

// sseErrFrame 上游 200 流里透传的错误帧（error-passthrough），后跟 [DONE]。
const sseErrFrame = "data: {\"error\":{\"message\":\"rate limited 6004\",\"type\":\"upstream_error\",\"code\":\"model_rate_limit\"}}\n\n" +
	"data: [DONE]\n\n"

// messagesHandler 起一个单账号池 + fake upstream 的 handler（Anthropic 入参由调用方给）。
func messagesHandler(t *testing.T, up *upstream.Client, cfg Config) *Handler {
	t.Helper()
	cfg.Pool = testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	cfg.Upstream = up
	return NewHandler(cfg)
}

func postMessages(h *Handler, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	if mutate != nil {
		mutate(req)
	}
	h.ServeHTTP(rec, req)
	return rec
}

// TestMessagesNonStreamTranslates 非流式：Anthropic 请求进、Anthropic 响应出，
// 且中间确实走了 chat 状态机（账号用量被记账）。
func TestMessagesNonStreamTranslates(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true
	})
	h := messagesHandler(t, up, Config{})
	rec := postMessages(h, `{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type=%q", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["type"] != "message" || resp["role"] != "assistant" {
		t.Errorf("envelope=%v", resp)
	}
	if resp["model"] != "glm-5.2" {
		t.Errorf("model=%v want 请求回显", resp["model"])
	}
	if resp["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", resp["stop_reason"])
	}
	blocks := resp["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "你好" {
		t.Errorf("content=%v", resp["content"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 1 || usage["output_tokens"].(float64) != 1 {
		t.Errorf("usage=%v", usage)
	}
	// 复用状态机的证据：记账发生在 chatCompletions 内部。
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok || st.TokenUsage.RequestCount != 1 {
		t.Errorf("用量未记账: %+v (ok=%v)", st.TokenUsage, ok)
	}
}

// TestMessagesStreamTranslates 流式：OpenAI SSE 必须被翻成 Anthropic 事件序列，
// 且不把 OpenAI 的 [DONE] 透给客户端。
func TestMessagesStreamTranslates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := messagesHandler(t, up, Config{})
	rec := postMessages(h, `{"model":"glm-5.2","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type=%q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"\"text\":\"你好\"",
		"event: content_block_stop",
		"event: message_delta",
		"\"stop_reason\":\"end_turn\"",
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("流里缺 %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("OpenAI 的 [DONE] 不该透给 Anthropic 客户端:\n%s", body)
	}
}

// TestMessagesStreamUpstreamErrorBecomesErrorEvent 流内上游错误帧 → Anthropic 的
// event: error；已失败的流不应再补 message_stop（假成功骨架）。
func TestMessagesStreamUpstreamErrorBecomesErrorEvent(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseErrFrame, true
	})
	h := messagesHandler(t, up, Config{})
	rec := postMessages(h, `{"model":"glm-5.2","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)

	body := rec.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("want event: error:\n%s", body)
	}
	if !strings.Contains(body, "rate limited 6004") {
		t.Errorf("错误原文应保留:\n%s", body)
	}
	if strings.Contains(body, "event: message_stop") {
		t.Errorf("失败的流不该补 message_stop:\n%s", body)
	}
}

// TestMessagesUpstreamErrorKeepsAnthropicShape 全账号失败 → 状态码与上游原文保留，
// 但错误体必须是 Anthropic 形状。
func TestMessagesUpstreamErrorKeepsAnthropicShape(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	h := messagesHandler(t, up, Config{})
	rec := postMessages(h, `{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp.Type != "error" || resp.Error.Type != "api_error" {
		t.Errorf("envelope=%s", rec.Body)
	}
	if resp.Error.Message == "" {
		t.Errorf("message 不该为空: %s", rec.Body)
	}
	// OpenAI 形状的 error.code 键不该在这层出现（那是 Chat 出口的契约）。
	if strings.Contains(rec.Body.String(), `"code"`) {
		t.Errorf("不该携带 OpenAI 的 error.code: %s", rec.Body)
	}
}

// TestMessagesAuthFailureAnthropicShape 鉴权失败也要是 Anthropic 形状
// （否则 SDK 报解析错，401 的真实原因被盖掉）。
func TestMessagesAuthFailureAnthropicShape(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := messagesHandler(t, up, Config{APIKey: "secret"})
	rec := postMessages(h, `{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &resp) != nil || resp.Type != "error" || resp.Error.Type != "authentication_error" {
		t.Errorf("body=%s", rec.Body)
	}
}

// TestMessagesAcceptsXApiKey Anthropic 客户端用 x-api-key 而非 Bearer。
func TestMessagesAcceptsXApiKey(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := messagesHandler(t, up, Config{APIKey: "secret"})
	rec := postMessages(h, `{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		func(r *http.Request) { r.Header.Set("X-Api-Key", "secret") })
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"type":"message"`) {
		t.Errorf("body=%s", rec.Body)
	}
}

// TestMessagesRejectsBadBody 畸形请求体应在网关就地 400，不进入选号。
func TestMessagesRejectsBadBody(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	h := messagesHandler(t, up, Config{})

	for _, tc := range []struct{ name, body string }{
		{"不是 JSON", `{"model":`},
		{"缺 messages", `{"model":"glm-5.2","max_tokens":64}`},
	} {
		rec := postMessages(h, tc.body, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d body=%s", tc.name, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), `"type":"error"`) {
			t.Errorf("%s: body=%s", tc.name, rec.Body)
		}
	}
	if calls != 0 {
		t.Errorf("畸形请求不该打上游: calls=%d", calls)
	}
}
