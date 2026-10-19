package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// /panel/api/status 是凭据边界的护栏：面板凭据（panel.password）与网关
// api_key 配成不同值是生产常态，这份上游拓扑必须由面板凭据就能读到。
// 未注入时回 501 而不是回空对象 —— 空对象会让前端显示成「没配路由」，那是假消息。

func TestPanelStatusNotWired(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, authReq(httptest.NewRequest(http.MethodGet, "/panel/api/status", nil)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("未注入 StatusSummary 应 501，得到 %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPanelStatusSummaryPassthrough(t *testing.T) {
	p := New(Config{
		Version: "test",
		APIKey:  "k",
		StatusSummary: func() map[string]any {
			return map[string]any{
				"upstreams": []map[string]any{{"name": "workbuddy", "label": "WorkBuddy"}},
				"routing":   map[string]any{"enabled": true, "aliases": map[string]string{"fast": "deepseek-v4.1-flash"}},
			}
		},
	})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, authReq(httptest.NewRequest(http.MethodGet, "/panel/api/status", nil)))
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应必须是 JSON：%v（body=%s）", err, rec.Body.String())
	}
	if _, ok := got["upstreams"]; !ok {
		t.Fatalf("缺 upstreams：%v", got)
	}
	rt, ok := got["routing"].(map[string]any)
	if !ok || rt["enabled"] != true {
		t.Fatalf("routing 丢失或变形：%v", got["routing"])
	}
	// 无凭据必须挡住：这张卡读的是上游拓扑，不能对公网开放。
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/panel/api/status", nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401，得到 %d", rec2.Code)
	}
}
