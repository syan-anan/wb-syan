package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestPool(t *testing.T, base, key string) *OpenAICompatPool {
	t.Helper()
	pool, warn := NewOpenAICompatPool([]OpenAIProvider{{
		Name: "groq", BaseURL: base, APIKey: key, Models: []string{"llama-3.3-70b"},
	}})
	if len(warn) != 0 {
		t.Fatalf("warnings: %v", warn)
	}
	if pool == nil {
		t.Fatal("pool nil")
	}
	return pool
}

// 前缀拆分：只认已注册的 provider，未知前缀不吞。
func TestOpenAICompatSplit(t *testing.T) {
	pool := newTestPool(t, "https://example.invalid/v1", "")

	if prov, bare, ok := pool.Split("groq:llama-3.3-70b"); !ok || prov != "groq" || bare != "llama-3.3-70b" {
		t.Fatalf("split = %q %q %v", prov, bare, ok)
	}
	if _, _, ok := pool.Split("nope:model"); ok {
		t.Fatal("未注册前缀不应命中")
	}
	if _, _, ok := pool.Split("groq:"); ok {
		t.Fatal("空模型名不应命中")
	}
	if _, _, ok := pool.Split("llama-3.3-70b"); ok {
		t.Fatal("无前缀不应命中")
	}
}

// 保留前缀必须拒绝：qoder / cn / global 已有别的语义。
func TestOpenAICompatReservedNames(t *testing.T) {
	for _, name := range []string{"qoder", "cn", "global"} {
		pool, warn := NewOpenAICompatPool([]OpenAIProvider{{
			Name: name, BaseURL: "https://example.invalid/v1",
		}})
		if pool != nil {
			t.Fatalf("%s 应被拒绝", name)
		}
		if len(warn) == 0 {
			t.Fatalf("%s 被拒绝时应给出告警", name)
		}
	}
}

// 转发：路径拼 /chat/completions，带 Authorization，body 原样。
func TestOpenAICompatChatStream(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	pool := newTestPool(t, srv.URL+"/v1", "sk-test")
	rc, status, _, err := pool.ChatStream(context.Background(), "groq", []byte(`{"model":"llama-3.3-70b"}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer rc.Close()
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !strings.Contains(gotBody, "llama-3.3-70b") {
		t.Fatalf("body = %q", gotBody)
	}
}

// 上游错误体原样带回（handler 依赖它透出上游信息）。
func TestOpenAICompatErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer srv.Close()

	pool := newTestPool(t, srv.URL+"/v1", "")
	rc, status, body, err := pool.ChatStream(context.Background(), "groq", []byte(`{}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if rc != nil {
		rc.Close()
		t.Fatal("非 2xx 不应返回 reader")
	}
	if status != 429 || !strings.Contains(string(body), "slow down") {
		t.Fatalf("status=%d body=%s", status, body)
	}
}

// 模型列表：带 provider 前缀，未配模型表的给通配条目。
func TestOpenAICompatModels(t *testing.T) {
	pool, _ := NewOpenAICompatPool([]OpenAIProvider{
		{Name: "groq", BaseURL: "https://a/v1", Models: []string{"m1", "m2"}},
		{Name: "zeta", BaseURL: "https://b/v1"},
	})
	models := pool.Models()
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	if !ids["groq:m1"] || !ids["groq:m2"] || !ids["zeta:*"] {
		t.Fatalf("ids = %v", ids)
	}
}

// 健康检查：/models 2xx 通过，非 2xx 报错。
func TestOpenAICompatHealthCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	pool := newTestPool(t, srv.URL+"/v1", "")
	if err := pool.HealthCheck(context.Background(), "groq"); err != nil {
		t.Fatalf("health: %v", err)
	}
	if err := pool.HealthCheck(context.Background(), "missing"); err == nil {
		t.Fatal("未知 provider 应报错")
	}
}
