package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestParseSemver 版本串解析：容忍 v 前缀 / 预发布后缀 / 位数不齐；解析不出来
// 必须 ok=false（宁可判"不更新"，也不瞎报）。
func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"v1.17.0", [3]int{1, 17, 0}, true},
		{"1.17.0-panel", [3]int{1, 17, 0}, true},
		{"V2.0", [3]int{2, 0, 0}, true},
		{" 1.2.3 ", [3]int{1, 2, 3}, true},
		{"1.2.3.4", [3]int{}, false},
		{"v1.x.0", [3]int{}, false},
		{"", [3]int{}, false},
		{"v", [3]int{}, false},
		{"-rc1", [3]int{}, false},
	}
	for _, c := range cases {
		got, ok := parseSemver(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseSemver(%q) = %v,%v 期望 %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestVersionNewer 只报"确实更新了"；同版本、更旧、任一侧解析失败都返回 false。
func TestVersionNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.18.0", "1.17.0-panel", true},
		{"v1.17.1", "1.17.0-panel", true},
		{"v2.0.0", "1.17.0-panel", true},
		{"v1.17.0", "1.17.0-panel", false},
		{"v1.16.9", "1.17.0-panel", false},
		{"v1.17.0", "v1.17.0", false},
		{"nightly", "1.17.0-panel", false},
		{"v1.18.0", "dev", false},
		{"", "1.17.0-panel", false},
	}
	for _, c := range cases {
		if got := versionNewer(c.latest, c.current); got != c.want {
			t.Errorf("versionNewer(%q,%q) = %v 期望 %v", c.latest, c.current, got, c.want)
		}
	}
}

// updateStub 假 GitHub：releases/latest 与 tags 两个端点，各自可指定状态码。
// 两个端点都计入 hits —— 用来断言"Release 命中时不该回落 tags"。
func updateStub(t *testing.T, relStatus int, tagName string, tags []string, tagsStatus int) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if relStatus != http.StatusOK {
			w.WriteHeader(relStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tagName, "html_url": "https://example.test/rel"})
	})
	mux.HandleFunc("/repos/o/r/tags", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if tagsStatus != http.StatusOK {
			w.WriteHeader(tagsStatus)
			return
		}
		out := make([]map[string]string, 0, len(tags))
		for _, s := range tags {
			out = append(out, map[string]string{"name": s})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestUpdaterRefreshRelease Release 存在：取 tag_name，与本地版本比对出 has_update。
func TestUpdaterRefreshRelease(t *testing.T) {
	srv, hits := updateStub(t, http.StatusOK, "v1.18.0", nil, http.StatusOK)
	u := newUpdater(true, "1.17.0-panel", "o/r")
	u.base = srv.URL
	u.refresh()

	info := u.snapshot()
	if !info.Enabled || !info.HasUpdate || info.Latest != "v1.18.0" {
		t.Fatalf("info=%+v", info)
	}
	if info.URL != "https://example.test/rel" {
		t.Errorf("url=%q", info.URL)
	}
	if info.CheckedAt == "" || info.Error != "" {
		t.Errorf("checked_at=%q err=%q", info.CheckedAt, info.Error)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("Release 命中时不该回落 tags：hits=%d", n)
	}
}

// TestUpdaterFallsBackToTags 仓库还没发过 Release（404）→ 回落 tags 第一条。
func TestUpdaterFallsBackToTags(t *testing.T) {
	srv, hits := updateStub(t, http.StatusNotFound, "", []string{"v1.19.0", "v1.18.0"}, http.StatusOK)
	u := newUpdater(true, "1.17.0-panel", "o/r")
	u.base = srv.URL
	u.refresh()

	info := u.snapshot()
	if info.Latest != "v1.19.0" || !info.HasUpdate {
		t.Fatalf("info=%+v", info)
	}
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Errorf("应当两个端点各打一次：hits=%d", n)
	}
}

// TestUpdaterNoReleaseNoTags 两个端点都没有可用数据：Latest 为空、不报更新、
// 也不写错误（空仓库是正常状态，不是故障）。
func TestUpdaterNoReleaseNoTags(t *testing.T) {
	srv, _ := updateStub(t, http.StatusNotFound, "", nil, http.StatusOK)
	u := newUpdater(true, "1.17.0-panel", "o/r")
	u.base = srv.URL
	u.refresh()

	info := u.snapshot()
	if info.Latest != "" || info.HasUpdate || info.Error != "" {
		t.Fatalf("info=%+v", info)
	}
}

// TestUpdaterDisabledNeverCalls 关掉检查时一个请求都不发（离线/内网部署的硬约束）。
func TestUpdaterDisabledNeverCalls(t *testing.T) {
	srv, hits := updateStub(t, http.StatusOK, "v9.9.9", nil, http.StatusOK)
	u := newUpdater(false, "1.17.0-panel", "o/r")
	u.base = srv.URL
	u.refresh()
	u.run(context.Background())

	info := u.snapshot()
	if info.Enabled {
		t.Errorf("关闭时 enabled 应为 false：%+v", info)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("关闭时不该发任何请求：hits=%d", n)
	}
}

// TestUpdaterHTTPErrorKeepsSilent 网络/限流失败只记 error，不报更新、不 panic。
func TestUpdaterHTTPErrorKeepsSilent(t *testing.T) {
	srv, _ := updateStub(t, http.StatusForbidden, "", nil, http.StatusForbidden)
	u := newUpdater(true, "1.17.0-panel", "o/r")
	u.base = srv.URL
	u.refresh()

	info := u.snapshot()
	if info.HasUpdate || info.Latest != "" {
		t.Fatalf("失败时不该报更新：%+v", info)
	}
	if info.Error == "" {
		t.Errorf("失败原因应记进 error 便于排障：%+v", info)
	}
}

// TestPanelUpdateEndpoint 面板端点：默认（未开启）返回 enabled=false；
// 开启后返回服务端缓存快照，且带鉴权（未带凭据 401）。
func TestPanelUpdateEndpoint(t *testing.T) {
	// UpdateCheck 零值 = 关闭：不鉴权的面板（无 APIKey）也能读，只是 enabled=false。
	p := New(Config{Version: "test"})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/update", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var info updateInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Enabled {
		t.Errorf("未开启更新检查时 enabled 应为 false：%+v", info)
	}

	srv, _ := updateStub(t, http.StatusOK, "v2.0.0", nil, http.StatusOK)
	p2 := New(Config{Version: "1.0.0", APIKey: "k", UpdateCheck: true, UpdateRepo: "o/r"})
	p2.upd.base = srv.URL
	p2.upd.refresh()

	req := httptest.NewRequest("GET", "/panel/api/update", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec = httptest.NewRecorder()
	p2.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if !info.Enabled || !info.HasUpdate || info.Latest != "v2.0.0" {
		t.Fatalf("info=%+v", info)
	}

	// 无凭据：withAuth 拦下（401）。
	rec = httptest.NewRecorder()
	p2.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/update", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("无凭据应 401，得到 %d", rec.Code)
	}
}
