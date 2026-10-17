package server

import (
	"strings"
	"testing"
	"time"
)

// 别名展开：单级、多级、未命中三种基本形态。
func TestRouteExpand(t *testing.T) {
	rt := NewRouteTable(map[string]string{
		"fast":  "cn:glm-5.2",
		"smart": "fast",
	}, nil, nil)

	got, err := rt.Expand("fast")
	if err != nil || got != "cn:glm-5.2" {
		t.Fatalf("Expand(fast) = %q, %v", got, err)
	}
	// 多级：smart → fast → cn:glm-5.2
	got, err = rt.Expand("smart")
	if err != nil || got != "cn:glm-5.2" {
		t.Fatalf("Expand(smart) = %q, %v", got, err)
	}
	got, err = rt.Expand("unknown")
	if err != nil || got != "unknown" {
		t.Fatalf("Expand(unknown) = %q, %v", got, err)
	}
}

// 环引用必须报错，不能静默返回一个环里的值。
func TestRouteExpandCycle(t *testing.T) {
	rt := NewRouteTable(map[string]string{
		"a": "b",
		"b": "a",
	}, nil, nil)
	if _, err := rt.Expand("a"); err == nil {
		t.Fatal("环引用应报错")
	}
}

// 降级链：全名命中、裸名命中、去重、含自身不重复。
func TestRouteChain(t *testing.T) {
	rt := NewRouteTable(nil, map[string][]string{
		"cn:deepseek": {"cn:glm-5.2", "cn:deepseek", "qoder:auto"},
		"glm-5.2":    {"cn:qwen3"},
	}, nil)

	chain := rt.Chain("cn:deepseek")
	want := []string{"cn:deepseek", "cn:glm-5.2", "qoder:auto"}
	if len(chain) != len(want) {
		t.Fatalf("chain = %v, want %v", chain, want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Fatalf("chain[%d] = %q, want %q (chain=%v)", i, chain[i], want[i], chain)
		}
	}

	// 裸名命中的情况：请求 cn:glm-5.2 时，裸名 "glm-5.2" 的降级也并入。
	chain = rt.Chain("cn:glm-5.2")
	if len(chain) != 2 || chain[1] != "cn:qwen3" {
		t.Fatalf("裸名降级未并入: %v", chain)
	}
}

// 时段闸门：普通区间、跨零点、未配置（恒允许）。
func TestRouteAllowedNow(t *testing.T) {
	ranges, err := ParseHourRanges([]string{"22:00-06:00", "09:00-12:00"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rt := NewRouteTable(nil, nil, map[string][]HourRange{"qoder": ranges})

	at := func(h, m int) time.Time {
		return time.Date(2026, 10, 17, h, m, 0, 0, time.Local)
	}
	cases := []struct {
		t    time.Time
		want bool
	}{
		{at(23, 0), true},  // 跨零点前半段
		{at(2, 0), true},   // 跨零点后半段
		{at(6, 0), false},  // 终点是开区间
		{at(10, 0), true},  // 第二段
		{at(13, 0), false}, // 两段之间
		{at(21, 59), false},
		{at(22, 0), true},
	}
	for _, c := range cases {
		if got := rt.AllowedNow("qoder", c.t); got != c.want {
			t.Errorf("AllowedNow(%s) = %v, want %v", c.t.Format("15:04"), got, c.want)
		}
	}
	// 未配置时段的上游恒允许。
	if !rt.AllowedNow("workbuddy", at(13, 0)) {
		t.Error("未配置时段的上游应恒允许")
	}
}

// 时段格式错误必须在解析期报错。
func TestParseHourRangesInvalid(t *testing.T) {
	for _, bad := range [][]string{{"22:00"}, {"aa-bb"}, {"22:00-"}, {"-06:00"}} {
		if _, err := ParseHourRanges(bad); err == nil {
			t.Errorf("非法时段 %v 应报错", bad)
		}
	}
}

// FilterChain 只过滤降级候选，首元素（用户显式请求）永远保留。
func TestRouteFilterChain(t *testing.T) {
	ranges, _ := ParseHourRanges([]string{"00:00-00:01"}) // 几乎全天关闭
	rt := NewRouteTable(nil, nil, map[string][]HourRange{"qoder": ranges})

	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.Local)
	chain := rt.FilterChain([]string{"cn:a", "qoder:auto", "cn:b"}, now)
	want := []string{"cn:a", "cn:b"}
	if len(chain) != len(want) {
		t.Fatalf("chain = %v, want %v", chain, want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Fatalf("chain[%d] = %q, want %q", i, chain[i], want[i])
		}
	}
}

// upstreamOf：前缀判定。
func TestUpstreamOf(t *testing.T) {
	cases := map[string]string{
		"qoder:auto":     "qoder",
		"cn:glm-5.2":     "workbuddy",
		"global:claude":  "workbuddy",
		"glm-5.2":        "workbuddy",
	}
	for in, want := range cases {
		if got := upstreamOf(in); got != want {
			t.Errorf("upstreamOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// 缓存键：同一会话键必须稳定，不同会话键必须不同。
func TestQoderCacheKeyStable(t *testing.T) {
	a := qoderCacheKey("sess:abc")
	b := qoderCacheKey("sess:abc")
	c := qoderCacheKey("sess:xyz")
	if a != b {
		t.Fatalf("同一会话键产出不同缓存键: %q vs %q", a, b)
	}
	if a == c {
		t.Fatalf("不同会话键产出相同缓存键: %q", a)
	}
	if !strings.HasPrefix(a, "wb2a-q-") {
		t.Fatalf("缓存键缺前缀: %q", a)
	}
}

// 注入缓存键：客户端已有则不动；没有则补上。
func TestInjectQoderCacheKey(t *testing.T) {
	body := []byte(`{"model":"auto","messages":[]}`)
	out := injectQoderCacheKey(body, "sess:abc")
	if !strings.Contains(string(out), "prompt_cache_key") {
		t.Fatalf("未注入 prompt_cache_key: %s", out)
	}

	own := []byte(`{"model":"auto","prompt_cache_key":"client-key"}`)
	out = injectQoderCacheKey(own, "sess:abc")
	if !strings.Contains(string(out), "client-key") {
		t.Fatalf("覆盖了客户端自带的 key: %s", out)
	}
}
