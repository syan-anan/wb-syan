// growth_test.go 面板成长中心接口的边界：缺 uid / 未知 uid / 空缓存概览。
//
// 真正的快照内容由 internal/upstream 的测试覆盖（投影白名单 + 单段失败隔离）；
// 这里只锁面板这一层：参数校验、未知账号、以及"概览只看缓存不打上游"这条约定。
package panel

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/pool"
	"github.com/syan-anan/wb-syan/internal/upstream"
)

func growthHarness(t *testing.T) *Panel {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	return New(Config{Version: "test", APIKey: "test-key", Pool: p, Upstream: upstream.New()})
}

// TestGrowthRequiresUID 缺 uid 直接 400（不打上游）。
func TestGrowthRequiresUID(t *testing.T) {
	p := growthHarness(t)
	rec := panelReq(p, "GET", "/panel/api/growth", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestGrowthUnknownUID 未知账号 404（不是 500，也不是空快照）。
func TestGrowthUnknownUID(t *testing.T) {
	p := growthHarness(t)
	rec := panelReq(p, "GET", "/panel/api/growth?uid=nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestGrowthOverviewReadsCacheOnly 概览只回缓存：空缓存也要 200 + 空列表，
// 绝不因为"没有缓存"去打 90 个上游请求。
func TestGrowthOverviewReadsCacheOnly(t *testing.T) {
	p := growthHarness(t)
	rec := panelReq(p, "GET", "/panel/api/growth/overview", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		OK     bool             `json:"ok"`
		Cached []map[string]any `json:"cached"`
		TTLSec int              `json:"ttl_sec"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || !out.OK {
		t.Fatalf("响应不对: %v %s", err, rec.Body)
	}
	if len(out.Cached) != 0 {
		t.Errorf("空缓存应回空列表: %+v", out.Cached)
	}
	if out.TTLSec <= 0 {
		t.Errorf("应回缓存 TTL: %d", out.TTLSec)
	}
}
