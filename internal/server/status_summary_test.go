package server

import (
	"testing"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/upstream"
)

// StatusSummary 是面板「上游与路由」卡的唯一数据源（走面板凭据）。
// 这张卡的账号数取自条目自己的 accounts：条目不带这个字段时，前端会退回
// 顶层 total，而 StatusSummary 没有 total —— 有号的池子被显示成 0，
// 比空白更难发现。这里把这个契约钉死。
func TestStatusSummaryCarriesPoolAccountCount(t *testing.T) {
	h := NewHandler(Config{
		Pool: testPoolWith(
			&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
			&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		),
		Upstream: upstream.New(),
	})

	s := h.StatusSummary()
	ups, ok := s["upstreams"].([]map[string]any)
	if !ok || len(ups) == 0 {
		t.Fatalf("upstreams 缺失或为空：%#v", s["upstreams"])
	}
	var wb map[string]any
	for _, u := range ups {
		if u["name"] == "workbuddy" {
			wb = u
		}
	}
	if wb == nil {
		t.Fatal("upstreams 里没有 workbuddy 条目")
	}
	if got := wb["accounts"]; got != 2 {
		t.Fatalf("workbuddy 的 accounts 应为池内账号数 2，实得 %#v", got)
	}
	if _, ok := s["routing"].(map[string]any); !ok {
		t.Fatalf("routing 字段缺失或类型不对：%#v", s["routing"])
	}
}
