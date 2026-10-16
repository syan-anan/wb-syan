package clientprofile

import (
	"strings"
	"testing"
)

// noEnv 把本项目/参考实现用到的环境变量清空，避免宿主环境干扰合成结果。
func noEnv(t *testing.T) {
	t.Helper()
	for _, names := range EnvFor {
		for _, n := range names {
			t.Setenv(n, "")
		}
	}
}

// TestComposeLayerOrder 层的优先级：auto 探测优先、saved 保存优先。
// 这一条曾经写反过（saved 模式让探测胜出、auto 模式让保存胜出），后果是
// 「auto 模式下版本升级后自动跟上」这条承诺根本不成立，必须有回归测试钉住。
func TestComposeLayerOrder(t *testing.T) {
	noEnv(t)
	saved := map[string]any{"desktop_version": "5.0.0"}
	detected := map[string]any{"desktop_version": "9.9.9"}

	if got := toStr(Compose(saved, SourceAuto, detected)["desktop_version"]); got != "9.9.9" {
		t.Errorf("auto 模式应探测优先，实际 %s", got)
	}
	if got := toStr(Compose(saved, SourceSaved, detected)["desktop_version"]); got != "5.0.0" {
		t.Errorf("saved 模式应保存优先，实际 %s", got)
	}
}

// TestComposeHeadersMergedNotReplaced headers 逐层合并：只改一个头时其余默认头必须还在。
func TestComposeHeadersMergedNotReplaced(t *testing.T) {
	noEnv(t)
	saved := map[string]any{"headers": map[string]any{"Accept-Language": "en-US"}}
	eff := Parse(Compose(saved, SourceSaved, nil))
	if eff.ExtraHeaders["Accept-Language"] != "en-US" {
		t.Errorf("保存值未生效: %+v", eff.ExtraHeaders)
	}
	for _, k := range []string{"X-CodeBuddy-Request", "X-Requested-With", "X-Private-Data"} {
		if eff.ExtraHeaders[k] == "" {
			t.Errorf("默认风控头 %s 被整层替换掉了: %+v", k, eff.ExtraHeaders)
		}
	}
}

// TestUAAlwaysDerived UA 永远由版本号现拼：map 里塞一个 user_agent 不生效
// （否则会出现「UA 报旧版本、X-IDE-Version 报新版本」的错位）。
func TestUAAlwaysDerived(t *testing.T) {
	noEnv(t)
	saved := map[string]any{
		"desktop_version": "6.0.0",
		"cli_version":     "3.0.0",
		"user_agent":      "WorkBuddy/1.0.0 WorkBuddy/1.0.0 CLI/1.0.0",
	}
	eff := Parse(Compose(saved, SourceSaved, nil))
	want := "WorkBuddy/6.0.0 WorkBuddy/6.0.0 CLI/3.0.0"
	if eff.UserAgent != want {
		t.Fatalf("UA=%q want %q", eff.UserAgent, want)
	}
}

// TestNormalizeRejectsUserAgent 保存 UA 要明确被拒并给出正确做法。
func TestNormalizeRejectsUserAgent(t *testing.T) {
	clean, errs := Normalize(map[string]any{"user_agent": "x/1", "desktop_version": "6.0.0"})
	if _, kept := clean["user_agent"]; kept {
		t.Error("user_agent 不该被保留")
	}
	joined := strings.Join(errs, " | ")
	if !strings.Contains(joined, "WB2A_USER_AGENT") {
		t.Errorf("应提示正确做法，实际 %v", errs)
	}
	if clean["desktop_version"] != "6.0.0" {
		t.Errorf("同批次的合法字段应被保留: %+v", clean)
	}
}

// TestNormalizeRejectsBadTypes 非法类型/负数/超长都要被挡下并报告。
func TestNormalizeRejectsBadTypes(t *testing.T) {
	_, errs := Normalize(map[string]any{
		"cpu_cores":         -3,
		"os_version":        strings.Repeat("x", 600),
		"headers":           "not-a-map",
		"turing_channel_id": 7,
	})
	joined := strings.Join(errs, " | ")
	for _, want := range []string{"cpu_cores", "os_version", "headers"} {
		if !strings.Contains(joined, want) {
			t.Errorf("应报告 %s 的问题，实际 %v", want, errs)
		}
	}
}

// TestRiskHeadersAttributionWins 额外头不能覆盖四个归属头（否则一个手滑的配置就能
// 把 X-IDE-Name 改成风控头的值）。
func TestRiskHeadersAttributionWins(t *testing.T) {
	noEnv(t)
	saved := map[string]any{
		"ide_name": "WorkBuddy",
		"headers":  map[string]any{"X-IDE-Name": "evil", "Accept-Language": "en-US"},
	}
	h := Parse(Compose(saved, SourceSaved, nil)).RiskHeaders()
	if h["X-IDE-Name"] != "WorkBuddy" {
		t.Errorf("归属头被额外头覆盖了: %+v", h)
	}
	if h["X-Product"] != "WorkBuddy" {
		t.Errorf("X-Product 必须是部署形态而不是产品名: %+v", h)
	}
	if h["X-IDE-Version"] != "5.5.4" {
		t.Errorf("X-IDE-Version 应等于 desktop_version: %+v", h)
	}
	if h["Accept-Language"] != "en-US" {
		t.Errorf("额外头应被合并进来: %+v", h)
	}
}

// TestUAByRealm 平台段按域切换（送错会触发上游 403 code 11140）。
func TestUAByRealm(t *testing.T) {
	noEnv(t)
	eff := Parse(Compose(nil, SourceAuto, nil))
	if got := eff.UAByRealm(false); got != "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1" {
		t.Errorf("CN UA=%q", got)
	}
	if got := eff.UAByRealm(true); got != "WorkBuddy/5.5.4 WorkBuddy AI/5.5.4 CLI/2.137.1" {
		t.Errorf("global UA=%q", got)
	}
}

// TestEnvOverrideWins 环境变量永远最高优先级，且支持参考实现的 WORKBUDDY_ 前缀。
func TestEnvOverrideWins(t *testing.T) {
	noEnv(t)
	p := Defaults()
	applyEnv(p, func(k string) string {
		if k == "WORKBUDDY_DESKTOP_VERSION" {
			return "7.7.7"
		}
		if k == "WB2A_CPU_CORES" {
			return "8"
		}
		return ""
	})
	if toStr(p["desktop_version"]) != "7.7.7" {
		t.Errorf("环境变量未生效（参考实现前缀）: %v", p["desktop_version"])
	}
	if n, _ := intOrNone(p["cpu_cores"]); n != 8 {
		t.Errorf("整数键的环境变量覆盖失败: %v", p["cpu_cores"])
	}
}

// TestSourcesReporting 排障视图：每个键取自哪一层。
func TestSourcesReporting(t *testing.T) {
	saved := map[string]any{"desktop_version": "6.0.0"}
	src := Sources(saved, SourceSaved, nil, func(string) string { return "" })
	if src["desktop_version"] != "已保存" {
		t.Errorf("desktop_version 来源=%q", src["desktop_version"])
	}
	if src["ide_name"] != "内置兜底" {
		t.Errorf("ide_name 来源=%q", src["ide_name"])
	}
	srcEnv := Sources(saved, SourceSaved, nil, func(k string) string {
		if k == "WORKBUDDY_CLI_VERSION" {
			return "3.3.3"
		}
		return ""
	})
	if srcEnv["cli_version"] != "环境变量 WORKBUDDY_CLI_VERSION" {
		t.Errorf("cli_version 来源=%q", srcEnv["cli_version"])
	}
}

// TestHolderRoundTrip 持有者：保存 → 立即生效 → 读到的是一致视图。
func TestHolderRoundTrip(t *testing.T) {
	noEnv(t)
	h := NewHolder()
	if got := h.Effective().DesktopVersion; got != "5.5.4" {
		t.Fatalf("默认 desktop_version=%q", got)
	}
	h.Set(map[string]any{"desktop_version": "6.1.0", "cpu_cores": 4}, SourceSaved)
	eff := h.Effective()
	if eff.DesktopVersion != "6.1.0" || eff.CPUCores != 4 {
		t.Errorf("保存后未生效: %+v", eff)
	}
	if eff.UserAgent != "WorkBuddy/6.1.0 WorkBuddy/6.1.0 CLI/2.137.1" {
		t.Errorf("UA 未随版本现拼: %q", eff.UserAgent)
	}
	// 未知模式回落 auto（不把非法值写进持有者）。
	h.Set(nil, "bogus")
	if h.Source() != SourceAuto {
		t.Errorf("非法模式应回落 auto，实际 %q", h.Source())
	}
	// 快照是副本：改它不影响持有者。
	snap := h.Snapshot()
	snap["desktop_version"] = "tampered"
	if h.Effective().DesktopVersion != "5.5.4" {
		t.Errorf("快照被改成脏值并污染了持有者")
	}
}

// TestDetectEmptyOnServer 服务器上没有客户端可探测：探测层恒空，这是正确结果。
func TestDetectEmptyOnServer(t *testing.T) {
	if len(Detect()) != 0 {
		t.Fatalf("服务器探测应为空，实际 %v", Detect())
	}
	noEnv(t)
	// 探测为空时 auto 与 saved 结果一致（都靠保存 + 环境变量）。
	saved := map[string]any{"desktop_version": "6.0.0"}
	a := toStr(Compose(saved, SourceAuto, nil)["desktop_version"])
	b := toStr(Compose(saved, SourceSaved, nil)["desktop_version"])
	if a != b || a != "6.0.0" {
		t.Errorf("探测为空时两模式应一致: auto=%s saved=%s", a, b)
	}
}

// TestFingerprintVersionConsistency 指纹里的 ideVersion/extVersion 必须与
// X-IDE-Version 报同一个版本（三处同源，错位就是可识别的差异）。
func TestFingerprintVersionConsistency(t *testing.T) {
	noEnv(t)
	eff := Parse(Compose(map[string]any{"desktop_version": "6.2.0"}, SourceSaved, nil))
	fp := eff.Fingerprint("machine-1", "session-1")
	if fp["ideVersion"] != "6.2.0" || fp["extVersion"] != "6.2.0" {
		t.Errorf("指纹版本与实际版本不一致: %+v", fp)
	}
	if eff.RiskHeaders()["X-IDE-Version"] != "6.2.0" {
		t.Errorf("归属头版本不一致: %+v", eff.RiskHeaders())
	}
	for _, k := range []string{"timezone", "reportDelay", "product", "ideName", "ideType",
		"machineId", "sessionId", "extName", "os", "arch", "osVersion", "cpuCores", "memorySize"} {
		if _, ok := fp[k]; !ok {
			t.Errorf("指纹缺字段 %s: %+v", k, fp)
		}
	}
}
