package panel

import (
	"strings"
	"testing"

	"github.com/syan-anan/wb-syan/internal/logfmt"
)

// TestRenderZhChatRow 锁死对话行的中文列：成功/失败两条都要能读出
// 账号、模型、结果、首字、输出、吞吐、耗时、扣分八件事。
func TestRenderZhChatRow(t *testing.T) {
	ok := "| #1671 | 16:06:52 | global:deepseek-v4.1-flash | stream | 200 | syan-anan(40d22172) | TTFB=3321ms   | tok=254  | 48.4tok/s   | total=5.2s | cr=0.0000   |"
	zh, hit := RenderZh(ok)
	if !hit {
		t.Fatalf("chat row not recognized: %q", ok)
	}
	for _, want := range []string{"对话·流式", "syan-anan", "global:deepseek-v4.1-flash", "成功 200", "首字 3.3s", "输出 254 字", "共 5.2s", "扣分 0.0000"} {
		if !strings.Contains(zh, want) {
			t.Errorf("chat row missing %q in:\n%s", want, zh)
		}
	}
	if strings.Contains(zh, "40d22172") {
		t.Errorf("uid8 不该出现在中文列（悬停/原文里才有）:\n%s", zh)
	}

	bad := "| #1669 | 16:04:26 | global:deepseek-v4.1-flash | stream | 503 | baiqiqi163@gmail.com(b09dc448) | TTFB=-        | tok=-    | -           | total=30.8s | cr=-        |"
	zh, hit = RenderZh(bad)
	if !hit {
		t.Fatalf("chat row not recognized: %q", bad)
	}
	for _, want := range []string{"失败 503", "首字 —", "输出 —", "扣分 —"} {
		if !strings.Contains(zh, want) {
			t.Errorf("failed chat row missing %q in:\n%s", want, zh)
		}
	}
}

// TestRenderZhSysRules 系统行逐条翻译；未命中的行必须回退（ok=false），
// 宁可显示英文也不能翻错。
func TestRenderZhSysRules(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{
			`WARN: [upstream] chat_stream acct=baiqian7777777(8be43080): upstream 400 client body={"code":11133,"msg":"Invalid request parameters","requestId":"x","extError":{"code":"model_param_invalid","message":"the request parameters were rejected by the model provider"}}`,
			[]string{"系统·警告", "上游拒绝", "baiqian7777777", "模型参数不合法", "11133"},
		},
		{
			`ERR: [upstream] chat_stream acct=baiqiqi163@gmail.com(b09dc448): transport error: Post "https://www.workbuddy.ai/v2/chat/completions": context canceled`,
			[]string{"系统·错误", "上游中断", "请求被取消"},
		},
		{
			`WARN: [server] rotate backoff aborted: ctx cancelled`,
			[]string{"系统·警告", "轮换退避中止"},
		},
		{
			`WARN: [pool] fallback_earliest_expiry acct=baiqian7777777(8be43080) until=2026-10-03T16:14:24+08:00 kind=soft`,
			[]string{"账号池回退", "baiqian7777777", "改用最早到期套餐"},
		},
		{
			`WARN: [server] stream acct=syan-anan(40d22172) model=deepseek-v4.1-flash: empty upstream stream (200+0 frames)`,
			[]string{"空响应", "上游 200 但 0 帧"},
		},
	}
	for _, c := range cases {
		zh, hit := RenderZh(c.raw)
		if !hit {
			t.Errorf("not translated: %q", c.raw)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(zh, w) {
				t.Errorf("missing %q in %q", w, zh)
			}
		}
	}

	if _, hit := RenderZh("something nobody translated 42"); hit {
		t.Errorf("unknown line must not be translated")
	}
}

// TestRenderZhTaskLine 任务行：前缀映射 + 短语词表。
func TestRenderZhTaskLine(t *testing.T) {
	cases := map[string][]string{
		"travel syan-anan(40d22172): skip (daily limit reached)":   {"任务·旅行", "syan-anan", "跳过（今日次数已用完）"},
		"streak-bonus 5c162cc9: 🎊 新手礼包 +100c":                      {"任务·连登", "🎊 新手礼包 +100 积分"},
		"travel syan-anan(40d22172): claim ok record=7 reward=300": {"领取成功", "记录 7", "300 积分"},
		"panel: 队列启动：6 项（并发 2）":                                    {"任务·面板", "队列启动"},
	}
	for raw, want := range cases {
		zh, hit := RenderZh(raw)
		if !hit {
			t.Errorf("not translated: %q", raw)
			continue
		}
		for _, w := range want {
			if !strings.Contains(zh, w) {
				t.Errorf("missing %q in %q", w, zh)
			}
		}
	}
}

// TestRenderZhWidthBudget 中文行必须能一行放下：日志区字号 18px 时，1920 窗口
// 下可用宽度约 1296px ≈ 129 个 ASCII 列（CJK 按 2 列算比实际更宽，偏保守），
// 再减掉面板前置的 "HH:MM:SS " 时间列。
func TestRenderZhWidthBudget(t *testing.T) {
	lines := []string{
		"| #1671 | 16:06:52 | global:deepseek-v4.1-flash | stream | 200 | syan-anan(40d22172) | TTFB=3321ms   | tok=254  | 48.4tok/s   | total=5.2s | cr=0.0000   |",
		"| #1669 | 16:04:26 | global:deepseek-v4.1-flash | stream | 503 | baiqiqi163@gmail.com(b09dc448) | TTFB=-        | tok=-    | -           | total=30.8s | cr=-        |",
		"| #1700 | 16:10:00 | cn:deepseek-v4.1-flash | stream | 200 | baiqian7777777777(8be43080) | TTFB=1204ms   | tok=1024 | 88.8tok/s   | total=11.5s | cr=12.5000  |",
		`WARN: [upstream] chat_stream acct=baiqian7777777(8be43080): upstream 400 client body={"code":11133,"msg":"Invalid request parameters","requestId":"x","extError":{"code":"model_param_invalid","message":"the request parameters were rejected by the model provider"}}`,
		"WARN: [pool] fallback_earliest_expiry acct=baiqian7777777(8be43080) until=2026-10-03T16:14:24+08:00 kind=soft",
	}
	const budget = 129 - 9 // 减掉面板前置的 "HH:MM:SS " 时间列
	for _, raw := range lines {
		zh, hit := RenderZh(raw)
		if !hit {
			t.Fatalf("not translated: %q", raw)
		}
		if w := logfmt.DisplayWidth(zh); w > budget {
			t.Errorf("line width %d > %d:\n%s", w, budget, zh)
		}
	}
}

// TestRenderZhColumnAlignment 账号长短不一时，模型列必须落在同一显示列——
// 否则一屏日志里列会参差，扫列就失去意义（超长昵称按列宽截断，完整值在悬停原文里）。
func TestRenderZhColumnAlignment(t *testing.T) {
	short := "| #1671 | 16:06:52 | global:deepseek-v4.1-flash | stream | 200 | syan-anan(40d22172) | TTFB=3321ms   | tok=254  | 48.4tok/s   | total=5.2s | cr=0.0000   |"
	long := "| #1669 | 16:04:26 | global:deepseek-v4.1-flash | stream | 503 | baiqiqi163@gmail.com(b09dc448) | TTFB=-        | tok=-    | -           | total=30.8s | cr=-        |"
	zs, ok1 := RenderZh(short)
	zl, ok2 := RenderZh(long)
	if !ok1 || !ok2 {
		t.Fatal("chat rows must be translated")
	}
	model := "global:deepseek-v4.1-flash"
	cs := logfmt.DisplayWidth(zs[:strings.Index(zs, model)])
	cl := logfmt.DisplayWidth(zl[:strings.Index(zl, model)])
	if cs != cl {
		t.Errorf("模型列起点不一致: %d vs %d\n%s\n%s", cs, cl, zs, zl)
	}
	if !strings.Contains(zl, "baiqiqi163@gm…") {
		t.Errorf("超长昵称应按列宽截断:\n%s", zl)
	}
}
