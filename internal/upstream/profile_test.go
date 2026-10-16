// profile_test.go 客户端参数档案接管出站请求头的行为锁定（Profile=nil 时零回归）。
package upstream

import (
	"net/http/httptest"
	"testing"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/clientprofile"
)

func profileClient(t *testing.T, saved map[string]any) *Client {
	t.Helper()
	h := clientprofile.NewHolder()
	h.Set(saved, clientprofile.SourceSaved)
	return &Client{Profile: h}
}

// TestProfileDrivesVersionAndUA 档案接管版本号与 UA：三处（UA / X-IDE-Version /
// 指纹）报同一个版本，不各说各话。
func TestProfileDrivesVersionAndUA(t *testing.T) {
	c := profileClient(t, map[string]any{
		"desktop_version": "6.6.6",
		"cli_version":     "9.9.9",
		"product":         "SaaS",
	})
	if got := c.clientVersion(); got != "6.6.6" {
		t.Errorf("clientVersion=%q", got)
	}
	if got := c.cliVersion(); got != "9.9.9" {
		t.Errorf("cliVersion=%q", got)
	}
	if got := c.userAgent(nil); got != "WorkBuddy/6.6.6 WorkBuddy/6.6.6 CLI/9.9.9" {
		t.Errorf("CN UA=%q", got)
	}
	globalAcct := &auth.Auth{UID: "u1"}
	if _, err := auth.BackfillRealmFor(globalAcct, "global"); err != nil {
		t.Fatalf("set realm: %v", err)
	}
	if got := c.userAgent(globalAcct); got != "WorkBuddy/6.6.6 WorkBuddy AI/6.6.6 CLI/9.9.9" {
		t.Errorf("global UA=%q", got)
	}
	if got := c.billingUA(); got != "WorkBuddy/6.6.6" {
		t.Errorf("billing UA=%q", got)
	}
}

// TestProfileDrivesAttribution 归属头随档案走，且额外风控头一并注入。
func TestProfileDrivesAttribution(t *testing.T) {
	c := profileClient(t, map[string]any{"desktop_version": "6.6.6", "product": "SaaS"})
	req := httptest.NewRequest("POST", "https://example.invalid/v1", nil)
	c.injectAttribution(req)
	if got := req.Header.Get("X-IDE-Version"); got != "6.6.6" {
		t.Errorf("X-IDE-Version=%q（必须与 UA 同版本）", got)
	}
	if got := req.Header.Get("X-IDE-Name"); got != "WorkBuddy" {
		t.Errorf("X-IDE-Name=%q", got)
	}
	if got := req.Header.Get("X-Product"); got != "SaaS" {
		t.Errorf("X-Product=%q（部署形态，不是产品名）", got)
	}
	// 额外风控头（官方客户端必带）来自档案的 headers 层。
	if got := req.Header.Get("X-Private-Data"); got != "false" {
		t.Errorf("X-Private-Data=%q", got)
	}
	if got := req.Header.Get("Accept-Language"); got != "zh-CN" {
		t.Errorf("Accept-Language=%q", got)
	}
}

// TestProfileSaaSEscapeHatch ClientName=SaaS 仍然还原旧的最小行为（只发 X-Product）。
func TestProfileSaaSEscapeHatch(t *testing.T) {
	c := profileClient(t, map[string]any{"desktop_version": "6.6.6"})
	c.ClientName = "SaaS"
	req := httptest.NewRequest("POST", "https://example.invalid/v1", nil)
	c.injectAttribution(req)
	if got := req.Header.Get("X-Product"); got != "SaaS" {
		t.Errorf("X-Product=%q", got)
	}
	if got := req.Header.Get("X-IDE-Name"); got != "" {
		t.Errorf("逃生门不该发 X-IDE-*，实际 %q", got)
	}
}

// TestProfileNilKeepsLegacy 未装配档案时逐字保持既有行为（零回归）。
func TestProfileNilKeepsLegacy(t *testing.T) {
	c := &Client{}
	if got := c.clientVersion(); got != defaultClientVersion {
		t.Errorf("clientVersion=%q", got)
	}
	if got := c.userAgent(nil); got != "WorkBuddy/"+defaultClientVersion+" WorkBuddy/"+defaultClientVersion+" CLI/"+defaultCliVersion {
		t.Errorf("UA=%q", got)
	}
	req := httptest.NewRequest("POST", "https://example.invalid/v1", nil)
	c.injectAttribution(req)
	if got := req.Header.Get("X-Product"); got != "WorkBuddy" {
		t.Errorf("旧行为 X-Product=%q", got)
	}
	if got := req.Header.Get("X-Private-Data"); got != "" {
		t.Errorf("未装配档案时不该凭空多出风控头，实际 %q", got)
	}
}
