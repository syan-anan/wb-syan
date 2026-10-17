package panel

import (
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSKeyLimits 锁死密钥表单的两段纯前端逻辑：
//   - parseModelDaily：只接受「模型=正整数」，脏输入丢弃而不是变成 NaN；
//   - keyLimitTags / quotaText：并发与模型日限必须出现在卡片摘要里，
//     否则后端加了能力、面板却看不见（等于没做）。
func TestAppJSKeyLimits(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; key-limit UI test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2] || process.argv[1], 'utf8');
const start = src.indexOf('function keyLimitTags');
const end = src.indexOf('function renderKeys');
if (start < 0 || end < 0) throw new Error('key-limit block not found');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.parseModelDaily = parseModelDaily;' +
  'this.modelDailyText = modelDailyText; this.keyLimitTags = keyLimitTags; this.quotaText = quotaText;', ctx);
const md = ctx.parseModelDaily('glm-5.2=100, qoder:*=50，bad, x=0, y=-3');
const back = ctx.parseModelDaily(ctx.modelDailyText(md));
const tags = ctx.keyLimitTags({ max_concurrent: 2, model_daily: md, accounts: [] });
const q = ctx.quotaText({ max_concurrent: 2, in_flight: 1, model_daily: { 'glm-5.2': 100 }, model_used: { 'glm-5.2': 3 } });
console.log(JSON.stringify({ md, back, tags, q }));`
	out, err := exec.Command(node, "-e", script, "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	got := string(out)
	want := `{"md":{"glm-5.2":100,"qoder:*":50},"back":{"glm-5.2":100,"qoder:*":50},"tags":["账号不限","并发 ≤ 2","模型日限 2 条"],"`
	if len(got) < len(want) || got[:len(want)] != want {
		t.Fatalf("unexpected output:\n got %s\nwant prefix %s", got, want)
	}
	if !strings.Contains(got, `在途 1/2`) || !strings.Contains(got, `glm-5.2 3/100`) {
		t.Fatalf("quota text missing concurrency/model usage: %s", got)
	}
}
