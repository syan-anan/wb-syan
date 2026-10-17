package upstream

import "testing"

func TestIsDailyRequestLimit(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"code":6008,"msg":"daily request limit"}`, true},
		{`{"code": 6008}`, true},
		{`{"code":"6008"}`, true},
		{`{"code":6004,"msg":"将在 2026-10-17 22:00:00 UTC+8 重置"}`, false}, // 6004 是模型级，不是日请求
		{`{"code":11140,"msg":"x"}`, false},
		{``, false},
	}
	for _, c := range cases {
		if got := IsDailyRequestLimit(c.body); got != c.want {
			t.Fatalf("IsDailyRequestLimit(%s)=%v want %v", c.body, got, c.want)
		}
	}
}
