package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
)

// busyRec 记录假上游收到的每批请求（调用次数 + 每批模型数）。
type busyRec struct {
	mu      sync.Mutex
	calls   int
	batches []int
}

func (r *busyRec) record(n int) {
	r.mu.Lock()
	r.calls++
	r.batches = append(r.batches, n)
	r.mu.Unlock()
}

func (r *busyRec) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// newBusyServer 起一个假 /v2/chat/busy：校验方法与路径，按请求里的模型逐个回条目。
// status != 200 时直接回该状态码（错误路径用）；delay > 0 时先睡再回（超时路径用）。
func newBusyServer(t *testing.T, rec *busyRec, status int, delay time.Duration) *httptest.Server {
	t.Helper()
	levels := []string{"idle", "normal", "busy", "congested", "unavailable"}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/chat/busy" {
			t.Errorf("上游路径 = %q, want /v2/chat/busy", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("上游方法 = %q, want POST", r.Method)
		}
		var req struct {
			Models []string `json:"models"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解析请求体失败: %v", err)
		}
		rec.record(len(req.Models))
		if delay > 0 {
			time.Sleep(delay)
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":13801,"msg":"boom"}`))
			return
		}
		data := make([]map[string]any, 0, len(req.Models))
		for i, m := range req.Models {
			data = append(data, map[string]any{
				"model":      m,
				"busy_level": levels[i%len(levels)],
				"busy":       float64(i) / 10,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok", "data": data})
	}))
}

// busyTestClient 造一个指向假上游的 Client（账号 realm=cn → 走 ChatBaseCN）。
func busyTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := New()
	c.HTTP = srv.Client()
	c.ChatHTTP = srv.Client()
	c.ChatBaseCN = srv.URL
	return c
}

// 正常返回：去重 + 排序 + 信封解析 + Raw 保留。
func TestFetchModelBusyNormal(t *testing.T) {
	resetModelBusyCache()
	rec := &busyRec{}
	srv := newBusyServer(t, rec, http.StatusOK, 0)
	defer srv.Close()
	c := busyTestClient(t, srv)

	got, err := c.FetchModelBusy(&auth.Auth{UID: "u1"}, []string{"b-model", "a-model", "a-model"})
	if err != nil {
		t.Fatalf("FetchModelBusy: %v", err)
	}
	if rec.count() != 1 || len(rec.batches) != 1 || rec.batches[0] != 2 {
		t.Fatalf("调用 %d 次 / 批次 %v, want 1 次 / 2 个模型（去重后）", rec.count(), rec.batches)
	}
	if len(got) != 2 || got[0].Model != "a-model" || got[1].Model != "b-model" {
		t.Fatalf("结果 = %+v, want 排序后的 a-model / b-model", got)
	}
	if got[0].Level != "idle" || got[1].Level != "normal" {
		t.Fatalf("level = %q / %q, want idle / normal", got[0].Level, got[1].Level)
	}
	if got[1].Busy != 0.1 {
		t.Fatalf("busy = %v, want 0.1", got[1].Busy)
	}
	if len(got[0].Raw) == 0 {
		t.Fatal("Raw 应保留上游原始条目")
	}
}

// 超过 50 个自动分批：120 个 → 3 批，每批 ≤ 50，并集 120。
func TestFetchModelBusyBatchesOverFifty(t *testing.T) {
	resetModelBusyCache()
	rec := &busyRec{}
	srv := newBusyServer(t, rec, http.StatusOK, 0)
	defer srv.Close()
	c := busyTestClient(t, srv)

	models := make([]string, 0, 120)
	for i := 0; i < 120; i++ {
		models = append(models, fmt.Sprintf("m-%03d", i))
	}
	got, err := c.FetchModelBusy(&auth.Auth{UID: "u1"}, models)
	if err != nil {
		t.Fatalf("FetchModelBusy: %v", err)
	}
	if rec.count() != 3 {
		t.Fatalf("批次数 = %d, want 3（120 个模型 / 每批上限 %d）", rec.count(), modelBusyBatchMax)
	}
	for i, n := range rec.batches {
		if n > modelBusyBatchMax {
			t.Fatalf("第 %d 批 %d 个模型，超过上限 %d", i+1, n, modelBusyBatchMax)
		}
	}
	if len(rec.batches) != 3 || rec.batches[0] != 50 || rec.batches[1] != 50 || rec.batches[2] != 20 {
		t.Fatalf("分批形状 = %v, want [50 50 20]", rec.batches)
	}
	if len(got) != 120 {
		t.Fatalf("结果数 = %d, want 120", len(got))
	}
}

// 超时：把 modelBusyTimeout 压到 50ms，上游睡 400ms → 必须报错且返回 nil。
func TestFetchModelBusyTimeout(t *testing.T) {
	resetModelBusyCache()
	old := modelBusyTimeout
	modelBusyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { modelBusyTimeout = old })

	rec := &busyRec{}
	srv := newBusyServer(t, rec, http.StatusOK, 400*time.Millisecond)
	defer srv.Close()
	c := busyTestClient(t, srv)

	got, err := c.FetchModelBusy(&auth.Auth{UID: "u1"}, []string{"slow"})
	if err == nil {
		t.Fatalf("期望超时错误，实际返回 %+v", got)
	}
	if got != nil {
		t.Fatalf("超时应返回 nil，实际 %+v", got)
	}
}

// 上游报错：返回 (nil, err)，不 panic；失败不入缓存，下次仍打上游。
func TestFetchModelBusyUpstreamError(t *testing.T) {
	resetModelBusyCache()
	rec := &busyRec{}
	srv := newBusyServer(t, rec, http.StatusInternalServerError, 0)
	defer srv.Close()
	c := busyTestClient(t, srv)
	a := &auth.Auth{UID: "u1"}

	got, err := c.FetchModelBusy(a, []string{"m"})
	if err == nil || got != nil {
		t.Fatalf("上游 500 应返回 (nil, err)，实际 got=%+v err=%v", got, err)
	}
	if _, err := c.FetchModelBusyCached(a, []string{"m"}, time.Minute); err == nil {
		t.Fatal("FetchModelBusyCached 应把上游错误原样返回")
	}
	if rec.count() != 2 {
		t.Fatalf("调用次数 = %d, want 2（失败不缓存，第二次仍打上游）", rec.count())
	}
}

// 缓存：命中（含模型顺序无关）；过期后重新打上游。
func TestFetchModelBusyCachedHitAndExpire(t *testing.T) {
	resetModelBusyCache()
	rec := &busyRec{}
	srv := newBusyServer(t, rec, http.StatusOK, 0)
	defer srv.Close()
	c := busyTestClient(t, srv)
	a := &auth.Auth{UID: "u1"}
	const ttl = 60 * time.Millisecond

	if _, err := c.FetchModelBusyCached(a, []string{"m1", "m2"}, ttl); err != nil {
		t.Fatalf("首次: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("首次调用次数 = %d, want 1", rec.count())
	}
	// 同一组模型、不同顺序 → 同一缓存键（normalizeBusyModels 已排序）
	if _, err := c.FetchModelBusyCached(a, []string{"m2", "m1"}, ttl); err != nil {
		t.Fatalf("命中: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("缓存命中后调用次数 = %d, want 1", rec.count())
	}
	time.Sleep(120 * time.Millisecond)
	if _, err := c.FetchModelBusyCached(a, []string{"m1", "m2"}, ttl); err != nil {
		t.Fatalf("过期后: %v", err)
	}
	if rec.count() != 2 {
		t.Fatalf("过期后调用次数 = %d, want 2", rec.count())
	}
}

// 空输入不打上游。
func TestFetchModelBusyEmptyInputs(t *testing.T) {
	resetModelBusyCache()
	rec := &busyRec{}
	srv := newBusyServer(t, rec, http.StatusOK, 0)
	defer srv.Close()
	c := busyTestClient(t, srv)

	got, err := c.FetchModelBusy(nil, []string{"m"})
	if err != nil || got != nil {
		t.Fatalf("nil 账号: got=%+v err=%v", got, err)
	}
	got, err = c.FetchModelBusy(&auth.Auth{UID: "u1"}, nil)
	if err != nil || got != nil {
		t.Fatalf("空模型: got=%+v err=%v", got, err)
	}
	if rec.count() != 0 {
		t.Fatalf("空输入不应打上游，实际 %d 次", rec.count())
	}
}

// 单条解析：噪声条目丢弃、备用键生效、缺 busy_level 回落 unknown。
func TestParseBusyItemShapes(t *testing.T) {
	if _, ok := parseBusyItem(json.RawMessage(`{"busy_level":"busy"}`)); ok {
		t.Fatal("无模型名的条目应被丢弃")
	}
	if _, ok := parseBusyItem(json.RawMessage(`not-json`)); ok {
		t.Fatal("非法 JSON 应被丢弃")
	}
	got, ok := parseBusyItem(json.RawMessage(`{"model":"m","level":"CONGESTED"}`))
	if !ok || got.Model != "m" || got.Level != "congested" {
		t.Fatalf("备用键 level 应生效并小写归一, got=%+v ok=%v", got, ok)
	}
	got, ok = parseBusyItem(json.RawMessage(`{"model":"m"}`))
	if !ok || got.Level != "unknown" || got.Busy != 0 {
		t.Fatalf("缺 busy_level 应回落 unknown/0, got=%+v ok=%v", got, ok)
	}
	got, ok = parseBusyItem(json.RawMessage(`{"model":"m","busy_rate":0.42}`))
	if !ok || got.Busy != 0.42 {
		t.Fatalf("备用键 busy_rate 应生效, got=%+v ok=%v", got, ok)
	}
}
