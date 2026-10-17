package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// PAT 换 job token：请求体必须是 {"personal_token": ...}，响应里的 token 要落进内存。
func TestQoderExchangePAT(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/jobToken/exchange" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"token":"jt-1","refresh_token":"jrt-1","expires_at":"2999-01-01T00:00:00Z","refresh_token_expires_at":"2999-01-02T00:00:00Z"}`))
	}))
	defer srv.Close()

	c := NewQoderClient("pt-test", "")
	c.OpenAPI = srv.URL

	tok, err := c.ExchangePAT(context.Background())
	if err != nil {
		t.Fatalf("ExchangePAT: %v", err)
	}
	if tok.Token != "jt-1" || tok.RefreshToken != "jrt-1" {
		t.Fatalf("token = %+v", tok)
	}
	if gotBody["personal_token"] != "pt-test" {
		t.Fatalf("body = %+v", gotBody)
	}
}

// 未配置 PAT 时 ExchangePAT 必须直接报错，不发任何请求。
func TestQoderExchangeWithoutPAT(t *testing.T) {
	c := NewQoderClient("", "")
	if _, err := c.ExchangePAT(context.Background()); err == nil {
		t.Fatal("空 PAT 应报错")
	}
}

// Token 缓存：未过期时第二次调用不再打网络（用调用计数验证）。
func TestQoderTokenCached(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"token":"jt-1","expires_at":"2999-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	c := NewQoderClient("pt", "")
	c.OpenAPI = srv.URL
	for i := 0; i < 3; i++ {
		if _, err := c.Token(context.Background()); err != nil {
			t.Fatalf("Token: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("exchange 调用次数 = %d, want 1（应命中内存缓存）", calls)
	}
}

// 快过期时走 refresh 而不是重新 exchange（PAT 是最贵的一步，不该反复用）。
func TestQoderTokenRefreshBeforeExchange(t *testing.T) {
	var exchangeCalls, refreshCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobToken/exchange":
			exchangeCalls++
			_, _ = w.Write([]byte(`{"token":"jt-old","refresh_token":"jrt-1","expires_at":"2000-01-01T00:00:00Z","refresh_token_expires_at":"2999-01-01T00:00:00Z"}`))
		case "/api/v1/jobToken/refresh":
			refreshCalls++
			_, _ = w.Write([]byte(`{"token":"jt-new","refresh_token":"jrt-2","expires_at":"2999-01-01T00:00:00Z"}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := NewQoderClient("pt", "")
	c.OpenAPI = srv.URL

	// 第一次：exchange，拿到一个已过期的 token。
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	// 第二次：已过期，但 refresh 仍有效 → 应该走 refresh。
	tok, err := c.Token(context.Background())
	if err != nil {
		t.Fatalf("second Token: %v", err)
	}
	if tok != "jt-new" {
		t.Fatalf("token = %q, want jt-new", tok)
	}
	if exchangeCalls != 1 || refreshCalls != 1 {
		t.Fatalf("exchange=%d refresh=%d, want 1/1", exchangeCalls, refreshCalls)
	}
}

// ChatStream 必须带上 Authorization: Bearer <jt> 与 cosy-* 指纹头。
func TestQoderChatStreamHeaders(t *testing.T) {
	var gotAuth, gotCosyVersion, gotClientType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		gotCosyVersion = r.Header.Get("cosy-version")
		gotClientType = r.Header.Get("cosy-clienttype")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := NewQoderClient("pt", "")
	c.OpenAPI = srv.URL
	c.ModelBase = srv.URL
	// 预置 token，避免走 exchange。
	c.tok = &QoderToken{Token: "jt-x", ExpiresAt: "2999-01-01T00:00:00Z"}

	rc, status, _, err := c.ChatStream(context.Background(), []byte(`{"model":"auto","messages":[]}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if gotAuth != "Bearer jt-x" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotCosyVersion != QoderCLIVersion || gotClientType != "10" {
		t.Fatalf("cosy 头 = %q / %q", gotCosyVersion, gotClientType)
	}
	if gotBody["model"] != "auto" {
		t.Fatalf("body model = %v", gotBody["model"])
	}
}

// 非 2xx 时把上游错误体原样带回（handler 依赖它透出 request_id）。
func TestQoderChatStreamErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer srv.Close()

	c := NewQoderClient("pt", "")
	c.OpenAPI = srv.URL
	c.ModelBase = srv.URL
	c.tok = &QoderToken{Token: "jt", ExpiresAt: "2999-01-01T00:00:00Z"}

	rc, status, body, err := c.ChatStream(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if rc != nil {
		rc.Close()
		t.Fatal("非 2xx 不应返回 reader")
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(string(body), "rate limited") {
		t.Fatalf("body = %s", body)
	}
}

// 落盘 → 新实例恢复：重启不该重新 exchange。
func TestQoderTokenPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qoder_token.json")
	c := NewQoderClient("pt", path)
	c.tok = &QoderToken{Token: "jt-disk", RefreshToken: "jrt", ExpiresAt: time.Now().Add(24 * time.Hour).Format(time.RFC3339)}
	c.saveToken(c.tok)

	c2 := NewQoderClient("pt", path)
	got, err := c2.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "jt-disk" {
		t.Fatalf("token = %q", got)
	}
}

// QoderModels 必须至少包含 auto（服务端路由入口），且 ID 不重复。
func TestQoderModelsShape(t *testing.T) {
	models := QoderModels()
	if len(models) == 0 {
		t.Fatal("模型列表为空")
	}
	seen := map[string]bool{}
	hasAuto := false
	for _, m := range models {
		if m.ID == "" || m.Label == "" {
			t.Fatalf("条目字段缺失: %+v", m)
		}
		if seen[m.ID] {
			t.Fatalf("重复 ID %q", m.ID)
		}
		seen[m.ID] = true
		if m.ID == "auto" {
			hasAuto = true
		}
	}
	if !hasAuto {
		t.Fatal("缺少 auto 模型")
	}
}

// QoderModels 返回的切片不能被调用方改坏内部状态。
func TestQoderModelsIsCopy(t *testing.T) {
	a := QoderModels()
	a[0].ID = "mutated"
	b := QoderModels()
	if b[0].ID == "mutated" {
		t.Fatal("模型列表被外部修改后污染了下一次调用")
	}
}

// 保证 io 被使用（ChatStream 返回 io.ReadCloser 的编译期契约）。
var _ io.ReadCloser = io.NopCloser(strings.NewReader(""))
