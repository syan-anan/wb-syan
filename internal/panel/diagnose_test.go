package panel

import (
	"testing"
	"time"
)

// 三层诊断：从日志行还原「网关判断 / 客户端解释 / 上游证据」。
//
// 这里锁的是**解析口径**：一旦日志格式微调而正则没跟上，面板的诊断视图会
// 静默变空（前端只会显示「暂无」），编译和语法检查都抓不到。
func TestParseDiagnose(t *testing.T) {
	ts := time.Date(2026, 10, 17, 10, 0, 0, 0, time.Local)
	e := LogEntry{
		TS:   ts,
		Ch:   ChSys,
		Text: `WARN: [upstream] chat_stream acct=baiqian7777777(8be43080): upstream 400 client body={"code":11133,"msg":"Invalid request parameters","requestId":"x"}`,
	}
	d, ok := parseDiagnose(e)
	if !ok {
		t.Fatalf("应识别为上游错误行")
	}
	if d.Status != 400 || d.UpstreamCode != 11133 {
		t.Fatalf("status=%d code=%d want 400/11133", d.Status, d.UpstreamCode)
	}
	if d.ClientMessage == "" {
		t.Fatalf("客户端解释为空（11133 应在码表里）")
	}
	if d.GatewayHint == "" {
		t.Fatalf("网关判断为空（11133 应产出中性参数 hint）")
	}
	if d.Account != "baiqian7777777" {
		t.Fatalf("account=%q", d.Account)
	}
	if d.Raw == "" || d.UpstreamMsg == "" {
		t.Fatalf("原文/上游证据不能为空")
	}
}

// 码表未收录的业务码：只带 upstream_code，不编造中文。
func TestParseDiagnoseUncoveredCode(t *testing.T) {
	e := LogEntry{TS: time.Now(), Ch: ChSys,
		Text: `ERROR: [upstream] chat acct=u1: upstream 400 client body={"code":987654,"msg":"weird"}`}
	d, ok := parseDiagnose(e)
	if !ok {
		t.Fatal("应识别")
	}
	if d.UpstreamCode != 987654 {
		t.Fatalf("code=%d", d.UpstreamCode)
	}
	if d.ClientMessage != "" || d.ClientDetail != "" {
		t.Fatalf("未收录的码不该有中文解释：%+v", d)
	}
}

// 普通日志行（无 upstream 错误形态）不产生诊断条目——否则日志视图会被噪声灌满。
func TestParseDiagnoseIgnoresNormalLines(t *testing.T) {
	for _, text := range []string{
		`pool: state.json 落盘失败: disk full`,
		`panel: order uid=u1 dir=up priority=1`,
		`| #12 | 200 | deepseek-v4 | 1234 tok |`,
	} {
		if _, ok := parseDiagnose(LogEntry{Text: text}); ok {
			t.Fatalf("不该识别为错误行：%s", text)
		}
	}
}
