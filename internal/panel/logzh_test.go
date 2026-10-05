package panel

import (
	"strings"
	"testing"
)

// srEm 估算 SyanRound 下的显示宽度（单位 em）：CJK 记 1.0，符号（✅ ❌ ⚠ 等回退到
// emoji 字体的）记 1.4，其余记 0.5。
//
// 为什么不用 logfmt.DisplayWidth：那套口径假设等宽（CJK=2 列），而 SyanRound 是
// 比例字体——"i" 只有 4.4px、空格 10.55px、"7" 9.58px。列宽是按这套保守估算定
// 出来的（见 index.html 的 #logBox .lng.c10），测试用它守住"一行放得下"。
func srEm(s string) float64 {
	w := 0.0
	for _, r := range s {
		switch {
		case r >= 0x2e80:
			w += 1.0
		case r >= 0x2600 && r <= 0x27bf:
			w += 1.4
		default:
			w += 0.5
		}
	}
	return w
}

// zhColCaps 各列的设计宽度（em），必须与 index.html 的 .lng.c10 / .lng.c4 一致。
// 第一项是时间列（面板渲染时补的 HH:MM:SS）。
var zhColCaps = []float64{4.4, 5.2, 8.4, 13.4, 2.2, 5.4, 6.9, 8.2, 4.8, 6.9}

const zhGapEm = 0.55 // 列间距
const zhLineBudgetEm = 81.7 // 1863 窗口下日志区可用 1471px ÷ 18px

// TestRenderZhChatCols 锁死对话行的中文列：成功/失败两条都要能读出账号、模型、
// 结果、首字、输出、吞吐、耗时、扣分九件事。状态列是图标，具体 HTTP 码只在
// 悬停的原文里。
func TestRenderZhChatCols(t *testing.T) {
	ok := "| #1671 | 16:06:52 | global:deepseek-v4.1-flash | stream | 200 | syan-anan(40d22172) | TTFB=3321ms   | tok=254  | 48.4tok/s   | total=5.2s | cr=0.0000   |"
	cols, hit := RenderZhCols(ok)
	if !hit {
		t.Fatalf("chat row not recognized: %q", ok)
	}
	if len(cols) != 9 {
		t.Fatalf("对话行应拆成 9 列，得到 %d：%q", len(cols), cols)
	}
	for i, want := range []string{"对话·流式", "syan-anan", "global:deepseek-v4.1-flash", "✅", "首字 3.3s", "输出 254tok", "吞吐 48.4tok/s", "共 5.2s", "扣分 0.0000"} {
		if cols[i] != want {
			t.Errorf("第 %d 列 = %q，期望 %q", i, cols[i], want)
		}
	}

	bad := "| #1669 | 16:04:26 | global:deepseek-v4.1-flash | stream | 503 | baiqiqi163@gmail.com(b09dc448) | TTFB=-        | tok=-    | -           | total=30.8s | cr=-        |"
	cols, hit = RenderZhCols(bad)
	if !hit {
		t.Fatalf("chat row not recognized: %q", bad)
	}
	for i, want := range []string{"❌", "首字 —", "输出 —", "吞吐 —", "扣分 —"} {
		got := cols[[]int{3, 4, 5, 6, 8}[i]]
		if got != want {
			t.Errorf("第 %d 列 = %q，期望 %q", i, got, want)
		}
	}
	if strings.Contains(cols[1], "40d22172") || strings.Contains(strings.Join(cols, " "), "b09dc448") {
		t.Errorf("uid8 不该出现在中文列（悬停/原文里才有）：%q", cols)
	}
}

// TestRenderZhSysRules 系统行逐条翻译；未命中的行必须回退（ok=false），
// 宁可显示英文也不能翻错。系统行的账号列恒为空，内容列与任务行同一列。
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
		{
			`reasoning_effort downgraded model=deepseek-v4.1-flash max -> high`,
			[]string{"系统·提示", "思考档位降级", "max → high"},
		},
	}
	for _, c := range cases {
		cols, hit := RenderZhCols(c.raw)
		if !hit {
			t.Errorf("not translated: %q", c.raw)
			continue
		}
		if len(cols) != 3 {
			t.Errorf("系统行应拆成 3 列，得到 %d：%q", len(cols), cols)
			continue
		}
		if cols[1] != "" {
			t.Errorf("系统行账号列应为空：%q", cols)
		}
		joined := cols[0] + " " + cols[2]
		for _, w := range c.want {
			if !strings.Contains(joined, w) {
				t.Errorf("missing %q in %q", w, joined)
			}
		}
	}

	if _, hit := RenderZhCols("something nobody translated 42"); hit {
		t.Errorf("unknown line must not be translated")
	}
}

// TestRenderZhTaskCols 任务行：前缀映射 + 短语词表，三段结构。
func TestRenderZhTaskCols(t *testing.T) {
	cases := map[string][]string{
		"travel syan-anan(40d22172): skip (daily limit reached)":   {"任务·旅行", "syan-anan", "跳过（今日次数已用完）"},
		"streak-bonus 5c162cc9: 🎊 新手礼包 +100c":                      {"任务·连登", "5c162cc9", "🎊 新手礼包 +100 积分"},
		"travel syan-anan(40d22172): claim ok record=7 reward=300": {"任务·旅行", "syan-anan", "领取成功（记录 7，奖励 300 积分）"},
		"panel: 队列启动：6 项（并发 2）":                                    {"任务·面板", "", "队列启动：6 项（并发 2）"},
	}
	for raw, want := range cases {
		cols, hit := RenderZhCols(raw)
		if !hit {
			t.Errorf("not translated: %q", raw)
			continue
		}
		if len(cols) != 3 {
			t.Errorf("任务行应拆成 3 列，得到 %d：%q", len(cols), cols)
			continue
		}
		for i, w := range want {
			if cols[i] != w {
				t.Errorf("第 %d 列 = %q，期望 %q", i, cols[i], w)
			}
		}
	}
}

// TestRenderZhColWidthBudget 每一列都必须放得进它的网格列宽，整行也必须放得下：
// 1863 窗口下日志区可用 1471px，18px 字号折合 81.7em；列宽 + 列间距不能超。
// 任何一列偷偷长胖（模型名变长、账号被加宽）都会先在这里红。
func TestRenderZhColWidthBudget(t *testing.T) {
	lines := []string{
		"| #1671 | 16:06:52 | global:deepseek-v4.1-flash | stream | 200 | syan-anan(40d22172) | TTFB=3321ms   | tok=254  | 48.4tok/s   | total=5.2s | cr=0.0000   |",
		"| #1669 | 16:04:26 | global:deepseek-v4.1-flash | stream | 503 | baiqiqi163@gmail.com(b09dc448) | TTFB=-        | tok=-    | -           | total=30.8s | cr=-        |",
		"| #1700 | 16:10:00 | cn:deepseek-v4.1-flash | stream | 200 | baiqian7777777777(8be43080) | TTFB=1204ms   | tok=4405 | 88.8tok/s   | total=53.0s | cr=12.5000  |",
		`WARN: [upstream] chat_stream acct=baiqian7777777(8be43080): upstream 400 client body={"code":11133,"msg":"Invalid request parameters","requestId":"x","extError":{"code":"model_param_invalid","message":"the request parameters were rejected by the model provider"}}`,
		"WARN: [pool] fallback_earliest_expiry acct=baiqian7777777(8be43080) until=2026-10-03T16:14:24+08:00 kind=soft",
	}
	total := 0.0
	for _, c := range zhColCaps {
		total += c
	}
	total += zhGapEm * float64(len(zhColCaps)-1)
	if total > zhLineBudgetEm {
		t.Fatalf("列宽合计 %.1fem 超过可用 %.1fem", total, zhLineBudgetEm)
	}
	for _, raw := range lines {
		cols, hit := RenderZhCols(raw)
		if !hit {
			t.Fatalf("not translated: %q", raw)
		}
		if got := srEm("16:06:52"); got > zhColCaps[0] {
			t.Errorf("时间列 %.1fem 超过 %.1fem", got, zhColCaps[0])
		}
		for i, c := range cols {
			if len(cols) == 3 && i == 2 {
				continue // 任务/系统行的内容列是 1fr，不设上限
			}
			cap := zhColCaps[i+1]
			if got := srEm(c); got > cap {
				t.Errorf("第 %d 列 %.1fem 超过列宽 %.1fem：%q", i, got, cap, c)
			}
		}
	}
}

// TestRenderZhAcctClip 账号/模型超宽时按列宽截断（完整值在悬停的原文里）——
// 网格列宽是死的，不截断就只能靠省略号，扫列会失去意义。
func TestRenderZhAcctClip(t *testing.T) {
	long := "| #1669 | 16:04:26 | global:deepseek-v4.1-flash-with-a-very-long-name | stream | 200 | baiqiqi163@gmail.com(b09dc448) | TTFB=-        | tok=-    | -           | total=30.8s | cr=-        |"
	cols, hit := RenderZhCols(long)
	if !hit {
		t.Fatal("chat row must be translated")
	}
	if !strings.HasSuffix(cols[1], "…") {
		t.Errorf("超长昵称应按列宽截断：%q", cols[1])
	}
	if !strings.HasSuffix(cols[2], "…") {
		t.Errorf("超长模型名应按列宽截断：%q", cols[2])
	}
}
