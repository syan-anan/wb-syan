package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/syan-anan/wb-syan/internal/upstream"
)

// rewriteQoderModel 只改 model，其余字段（含网关不认识的扩展字段）必须原样保留。
func TestRewriteQoderModelPreservesFields(t *testing.T) {
	in := []byte(`{"model":"qoder:gpt-5","messages":[{"role":"user","content":"hi"}],"temperature":0.3,"prediction":{"type":"content"}}`)
	out, err := rewriteQoderModel(in, "gpt-5")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["model"] != "gpt-5" {
		t.Fatalf("model = %v", m["model"])
	}
	if m["temperature"] != 0.3 {
		t.Fatalf("temperature 丢了: %v", m["temperature"])
	}
	if _, ok := m["prediction"]; !ok {
		t.Fatal("未知扩展字段 prediction 被丢掉")
	}
	if msgs, ok := m["messages"].([]any); !ok || len(msgs) != 1 {
		t.Fatalf("messages 被破坏: %v", m["messages"])
	}
}

// 非法 JSON 要报错而不是静默产出空体。
func TestRewriteQoderModelInvalidJSON(t *testing.T) {
	if _, err := rewriteQoderModel([]byte("not json"), "gpt-5"); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// 未配置 Qoder 时返回 501，且错误码明确指向配置项。
func TestQoderChatNotConfigured(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qoder:auto"}`))
	h.qoderChat(rec, req, []byte(`{"model":"qoder:auto"}`), "auto", "", false)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "qoder_not_configured") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// 空模型名要在本地拦掉，不要发给上游。
func TestQoderChatEmptyModel(t *testing.T) {
	h := &Handler{}
	h.cfg.Qoder = upstream.NewQoderPool([]upstream.QoderAccount{{PAT: "pt"}}, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	h.qoderChat(rec, req, []byte(`{"model":"qoder:"}`), "", "", false)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// qoderModelEntries 必须带 qoder: 前缀，且 owned_by 标记为 qoder。
func TestQoderModelEntriesPrefix(t *testing.T) {
	h := &Handler{}
	entries := h.qoderModelEntries()
	if len(entries) == 0 {
		t.Fatal("空列表")
	}
	for _, e := range entries {
		id, _ := e["id"].(string)
		// 双区前缀：qodercn: / qoderglobal:（qoder: 作为兼容）。任一即可。
		if !strings.HasPrefix(id, qoderCNPrefix) &&
			!strings.HasPrefix(id, qoderGlobalPrefix) &&
			!strings.HasPrefix(id, qoderModelPrefix) {
			t.Fatalf("id 缺前缀: %v", e["id"])
		}
		switch e["owned_by"] {
		case "qoder", "qoder-cn", "qoder-intl":
		default:
			t.Fatalf("owned_by = %v", e["owned_by"])
		}
	}
}
