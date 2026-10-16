package panel

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/pool"
	"github.com/syan-anan/wb-syan/internal/upstream"
	"github.com/syan-anan/wb-syan/internal/usage"
)

// 注意：每个用例用**不同的模型名**。upstream 侧的进程内缓存（modelBusyMemo）按
// realm|uid|模型集合 命中，跨用例复用同一个 uid 与模型名会让后一个用例读到前一个
// 用例的旧结果。不同模型名 = 不同缓存键 = 用例之间天然隔离。

// newPanelBusyServer 起一个假上游 /v2/chat/busy，返回服务器与调用计数函数。
func newPanelBusyServer(t *testing.T, status int) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		var req struct {
			Models []string `json:"models"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":13801,"msg":"boom"}`))
			return
		}
		data := make([]map[string]any, 0, len(req.Models))
		for _, m := range req.Models {
			data = append(data, map[string]any{"model": m, "busy_level": "busy", "busy": 0.5})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
	}))
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// newBusyTestPanel 组装面板：池里一个 CN 账号，Upstream 指向假上游。
func newBusyTestPanel(t *testing.T, srv *httptest.Server, usageRec *usage.Recorder) *Panel {
	t.Helper()
	up := upstream.New()
	if srv != nil {
		up.HTTP = srv.Client()
		up.ChatHTTP = srv.Client()
		up.ChatBaseCN = srv.URL
	}
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn"})
	return New(Config{Version: "test", APIKey: "test-key", Pool: pl, Upstream: up, Usage: usageRec})
}

// invokePanelHandler 直接调 handler（本文件不注册路由，见 modelbusy.go 顶部说明）。
func invokePanelHandler(h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

type busyResponse struct {
	OK     bool                 `json:"ok"`
	Busy   []upstream.ModelBusy `json:"busy"`
	Cached bool                 `json:"cached"`
}

func floatNear(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// 繁忙度端点：正常路径 + 面板缓存命中。
func TestPanelModelBusyEndpoint(t *testing.T) {
	resetPanelBusyCache()
	srv, calls := newPanelBusyServer(t, http.StatusOK)
	defer srv.Close()
	p := newBusyTestPanel(t, srv, nil)

	// 第一次：打上游，cached=false，realm 前缀被剥掉
	rec := invokePanelHandler(p.modelBusy, "/panel/api/model_busy?models=cn:mb-ok-1,cn:mb-ok-2")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var first busyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if !first.OK || first.Cached || len(first.Busy) != 2 {
		t.Fatalf("首次响应 = %+v, want ok=true cached=false busy=2", first)
	}
	if first.Busy[0].Model != "mb-ok-1" || first.Busy[1].Model != "mb-ok-2" {
		t.Fatalf("模型名应剥掉 realm 前缀并排序, got %q / %q", first.Busy[0].Model, first.Busy[1].Model)
	}
	if first.Busy[0].Level != "busy" {
		t.Fatalf("level = %q, want busy", first.Busy[0].Level)
	}
	if calls() != 1 {
		t.Fatalf("上游调用 = %d, want 1", calls())
	}

	// 第二次：命中面板缓存，不再打上游
	rec = invokePanelHandler(p.modelBusy, "/panel/api/model_busy?models=cn:mb-ok-2,cn:mb-ok-1")
	var second busyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if !second.OK || !second.Cached || len(second.Busy) != 2 {
		t.Fatalf("二次响应 = %+v, want ok=true cached=true busy=2", second)
	}
	if calls() != 1 {
		t.Fatalf("缓存命中后上游调用 = %d, want 1", calls())
	}
}

// 没有可用账号 / 没给模型 → 200 + 空数组，不是错误。
func TestPanelModelBusyEmptyCases(t *testing.T) {
	resetPanelBusyCache()
	up := upstream.New()
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pool.New(""), Upstream: up})

	for _, target := range []string{
		"/panel/api/model_busy?models=mb-empty-1", // 池里没有账号
		"/panel/api/model_busy",                   // 没给模型
	} {
		rec := invokePanelHandler(p.modelBusy, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: code = %d, want 200", target, rec.Code)
		}
		var got busyResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if !got.OK || got.Cached || len(got.Busy) != 0 {
			t.Fatalf("%s: 响应 = %+v, want ok=true cached=false busy=0", target, got)
		}
		if body := rec.Body.String(); !strings.Contains(body, `"busy":[]`) {
			t.Fatalf("%s: busy 必须是空数组而不是 null, body=%s", target, body)
		}
	}
}

// 上游报错 → 200 + 空数组，不 panic、不把故障传成 5xx。
func TestPanelModelBusyUpstreamError(t *testing.T) {
	resetPanelBusyCache()
	srv, _ := newPanelBusyServer(t, http.StatusInternalServerError)
	defer srv.Close()
	p := newBusyTestPanel(t, srv, nil)

	rec := invokePanelHandler(p.modelBusy, "/panel/api/model_busy?models=cn:mb-err-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got busyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || len(got.Busy) != 0 {
		t.Fatalf("响应 = %+v, want ok=true busy=0", got)
	}
}

// 抖动端点：分位数 / 标准差 / 变异系数；单样本（样本不足）分支；无延迟观测的模型不出现。
func TestPanelModelLatencyJitter(t *testing.T) {
	rec := usage.New("")
	now := time.Now()
	// jit-A：3 个分桶（不同 uid → 不同桶），均值 100 / 200 / 300 ms
	rec.Add(now, "cn", "uA", "jit-A", "codex", usage.Delta{LatencyMs: 100, HasLatency: true}, true)
	rec.Add(now, "cn", "uB", "jit-A", "codex", usage.Delta{LatencyMs: 200, HasLatency: true}, true)
	rec.Add(now, "cn", "uC", "jit-A", "codex", usage.Delta{LatencyMs: 300, HasLatency: true}, true)
	// jit-B：只有 1 个样本 → 样本不足分支
	rec.Add(now, "cn", "uA", "jit-B", "codex", usage.Delta{LatencyMs: 500, HasLatency: true}, true)
	// jit-C：有请求但没有延迟观测 → 不产样本，不出现在结果里
	rec.Add(now, "cn", "uA", "jit-C", "codex", usage.Delta{}, true)

	p := New(Config{Version: "test", APIKey: "test-key", Usage: rec})
	resp := invokePanelHandler(p.modelLatencyJitter, "/panel/api/model_latency_jitter?days=7")
	if resp.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200, body=%s", resp.Code, resp.Body.String())
	}
	var got struct {
		OK     bool          `json:"ok"`
		Days   int           `json:"days"`
		Models []modelJitter `json:"models"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Days != 7 {
		t.Fatalf("ok=%v days=%d, want true/7", got.OK, got.Days)
	}
	byModel := map[string]modelJitter{}
	for _, m := range got.Models {
		byModel[m.Model] = m
	}
	if _, exists := byModel["jit-C"]; exists {
		t.Fatal("没有延迟观测的模型不该出现在抖动结果里")
	}

	a, okA := byModel["jit-A"]
	if !okA {
		t.Fatal("缺少 jit-A")
	}
	// 100/200/300：均值 200，样本标准差 100，CV 0.5；p50 插值 = 200，p95 = 290
	if a.Samples != 3 || !floatNear(a.MeanMs, 200) || !floatNear(a.StdMs, 100) || !floatNear(a.CV, 0.5) {
		t.Fatalf("jit-A = %+v, want samples=3 mean=200 std=100 cv=0.5", a)
	}
	if !floatNear(a.P50Ms, 200) || !floatNear(a.P95Ms, 290) {
		t.Fatalf("jit-A 分位数 = p50 %v / p95 %v, want 200 / 290", a.P50Ms, a.P95Ms)
	}

	b, okB := byModel["jit-B"]
	if !okB {
		t.Fatal("缺少 jit-B")
	}
	// 样本不足：不出分位数（p50=p95=唯一均值），std/cv 记 0
	if b.Samples != 1 || !floatNear(b.MeanMs, 500) || !floatNear(b.P50Ms, 500) || !floatNear(b.P95Ms, 500) {
		t.Fatalf("jit-B = %+v, want samples=1 mean=p50=p95=500", b)
	}
	if b.StdMs != 0 || b.CV != 0 {
		t.Fatalf("jit-B std/cv = %v/%v, want 0/0（样本不足）", b.StdMs, b.CV)
	}

	// CV 降序：jit-A(0.5) 在 jit-B(0) 之前
	if len(got.Models) != 2 || got.Models[0].Model != "jit-A" {
		t.Fatalf("排序 = %v, want jit-A 在前（cv 降序）", got.Models)
	}
}

// 未接用量记录器 → 200 + 空数组。
func TestPanelModelLatencyJitterNoRecorder(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "test-key"})
	resp := invokePanelHandler(p.modelLatencyJitter, "/panel/api/model_latency_jitter")
	if resp.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", resp.Code)
	}
	var got struct {
		OK     bool          `json:"ok"`
		Models []modelJitter `json:"models"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || len(got.Models) != 0 {
		t.Fatalf("响应 = %+v, want ok=true models=0", got)
	}
}

// 参数解析：非法/缺省回落默认值，超限收敛到上限。
func TestParseLimitAndBusyModels(t *testing.T) {
	if got := parseLimit("", 7, 90); got != 7 {
		t.Fatalf("缺省 = %d, want 7", got)
	}
	if got := parseLimit("abc", 7, 90); got != 7 {
		t.Fatalf("非法 = %d, want 7", got)
	}
	if got := parseLimit("1000", 7, 90); got != 90 {
		t.Fatalf("超限 = %d, want 90", got)
	}
	if got := parseLimit("30", 7, 90); got != 30 {
		t.Fatalf("合法 = %d, want 30", got)
	}
	got := parseBusyModels(" cn:b , global:a , b ,, ")
	want := []string{"a", "b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parseBusyModels = %v, want %v", got, want)
	}
}
