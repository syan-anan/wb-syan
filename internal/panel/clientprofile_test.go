// clientprofile_test.go 面板「客户端参数」接口的端到端校验。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/syan-anan/wb-syan/internal/clientprofile"
)

// profileHarness 装配一个只依赖持有者的面板，并记录最后一次落盘的档案
// （模拟 main 注入的 SaveClientProfile：写 config.json + 热更新持有者）。
func profileHarness(t *testing.T) (*Panel, *clientprofile.Holder, *map[string]any) {
	t.Helper()
	holder := clientprofile.NewHolder()
	last := map[string]any{}
	p := New(Config{
		Version:       "test",
		APIKey:        "test-key",
		ClientProfile: holder,
		SaveClientProfile: func(profile map[string]any, source string) error {
			last = profile
			holder.Set(profile, source)
			return nil
		},
	})
	return p, holder, &last
}

func decodeProfile(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v %s", err, rec.Body.String())
	}
	return out
}

// TestClientProfileAPIRoundTrip 读取 → 保存 → 合并保存 → 重置。
func TestClientProfileAPIRoundTrip(t *testing.T) {
	p, holder, last := profileHarness(t)

	rec := panelReq(p, "GET", "/panel/api/client_profile", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body)
	}
	view := decodeProfile(t, rec)
	if view["source"] != clientprofile.SourceAuto {
		t.Errorf("默认模式应为 auto: %v", view["source"])
	}
	if _, ok := view["defaults"].(map[string]any); !ok {
		t.Errorf("应返回内置兜底: %v", view)
	}

	// 保存：只改版本号 + 切到 saved 模式。
	rec = panelReq(p, "POST", "/panel/api/client_profile",
		`{"profile":{"desktop_version":"6.0.0"},"source":"saved"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST code=%d body=%s", rec.Code, rec.Body)
	}
	if got, _ := (*last)["desktop_version"].(string); got != "6.0.0" {
		t.Errorf("未落盘: %+v", *last)
	}
	if holder.Effective().DesktopVersion != "6.0.0" {
		t.Errorf("未热生效: %+v", holder.Effective())
	}
	if holder.Effective().UserAgent != "WorkBuddy/6.0.0 WorkBuddy/6.0.0 CLI/2.137.1" {
		t.Errorf("UA 未随版本现拼: %q", holder.Effective().UserAgent)
	}

	// 合并保存：只改 CLI 版本，desktop_version 必须还在。
	rec = panelReq(p, "POST", "/panel/api/client_profile", `{"profile":{"cli_version":"3.3.3"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("merge POST code=%d body=%s", rec.Code, rec.Body)
	}
	if got, _ := (*last)["desktop_version"].(string); got != "6.0.0" {
		t.Errorf("合并保存丢了已有键: %+v", *last)
	}
	if holder.Effective().CLIVersion != "3.3.3" {
		t.Errorf("CLI 版本未生效: %+v", holder.Effective())
	}

	// headers 也是合并语义。
	rec = panelReq(p, "POST", "/panel/api/client_profile",
		`{"profile":{"headers":{"Accept-Language":"en-US"}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("headers POST code=%d body=%s", rec.Code, rec.Body)
	}
	hdr := holder.Effective().ExtraHeaders
	if hdr["Accept-Language"] != "en-US" || hdr["X-Private-Data"] != "false" {
		t.Errorf("headers 未按合并语义处理: %+v", hdr)
	}

	// 重置：回到纯兜底。
	rec = panelReq(p, "POST", "/panel/api/client_profile/reset", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reset code=%d body=%s", rec.Code, rec.Body)
	}
	if len(holder.Saved()) != 0 {
		t.Errorf("重置后不该还有保存值: %+v", holder.Saved())
	}
	if holder.Effective().DesktopVersion != clientprofile.Defaults()["desktop_version"] {
		t.Errorf("重置后未回到兜底: %+v", holder.Effective().DesktopVersion)
	}
	_ = last
}

// TestClientProfileAPIValidation 非法值要报错且不落盘。
func TestClientProfileAPIValidation(t *testing.T) {
	p, holder, _ := profileHarness(t)
	rec := panelReq(p, "POST", "/panel/api/client_profile",
		`{"profile":{"cpu_cores":-5,"user_agent":"x/1"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("全非法应 400，实际 %d %s", rec.Code, rec.Body)
	}
	if len(holder.Saved()) != 0 {
		t.Errorf("校验失败不该落盘: %+v", holder.Saved())
	}
	var out struct {
		Errors []string `json:"errors"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	joined := ""
	for _, e := range out.Errors {
		joined += e + " | "
	}
	if !strings.Contains(joined, "cpu_cores") || !strings.Contains(joined, "user_agent") {
		t.Errorf("错误信息不完整: %v", out.Errors)
	}
}

// TestClientProfileDetectOnServer 服务器上探测恒为空，但要如实说明原因，
// 否则运维会一直点「探测」等一个永远不会有的结果。
func TestClientProfileDetectOnServer(t *testing.T) {
	p, _, _ := profileHarness(t)
	rec := panelReq(p, "POST", "/panel/api/client_profile/detect", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detect code=%d body=%s", rec.Code, rec.Body)
	}
	view := decodeProfile(t, rec)
	if got := view["note"].(string); got == "" {
		t.Errorf("探测为空时必须给出说明: %v", view)
	}
}
