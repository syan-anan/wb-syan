package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSModelStatStrip 模型可用率条（mdStatOf / mdStatStrip / sizeTag）的运行时闸门。
//
// 为什么需要：这段只在浏览器里跑，Go 侧不执行 JS。几个容易回归的点语法检查抓不到：
//   - mdStatOf 的三档查表（全名 → 去 realm 前缀 → 补前缀）：用量桶记的是
//     「调用时填的字符串」，带不带前缀都可能，查不到就整条显示「—」
//   - 没有观测的模型必须整条「—」，绝不能伪造 100%
//   - 统计档位：只渲染所选窗口（today / 30d）的那一对次数，四个数字不能同时出现
//   - 状态格的配色阈值（全成 / 有失败 / 失败过半 / 无观测）与百分比
//   - sizeTag 的压缩口径（1M / 192K）：上下文与最大输出已改成角标，压缩错了会
//     把 1000K 这种别扭的写法印到界面上
//
// 无 node 的环境跳过。
func TestAppJSModelStatStrip(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model stat strip test skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
function block(startMarker) {
  const a = src.indexOf(startMarker);
  if (a < 0) { console.log('BLOCK NOT FOUND: ' + startMarker); process.exit(1); }
  const b = src.indexOf('\n}', a);   // 顶层函数以行首 } 收尾
  if (b < 0) { console.log('BLOCK END NOT FOUND: ' + startMarker); process.exit(1); }
  return src.slice(a, b + 2);
}
const parts = [
  block('function esc('),
  block('function mdStatOf('),
  block('function mdStatStrip('),
  block('function sizeTag('),
  block('function fmtInt('),
  block('function fmtMs('),
];
const ctx = { Math, Number, String, isNaN, Object, Array, console };
ctx.window = ctx; ctx.globalThis = ctx;
vm.createContext(ctx);
vm.runInContext(parts.join('\n') +
  '\nconst fmtRate = r => r ? Number(r).toFixed(1) + " tok/s" : "—";' +
  '\nthis.__T = { mdStatOf, mdStatStrip, sizeTag };', ctx);
const T = ctx.__T;

const bad = [];
const chk = (cond, msg) => { if (!cond) bad.push(msg); };

// 1) 没有观测 → 整条「—」，不能出现 100%
ctx.mdStats = {};
let h = T.mdStatStrip({ id: 'global:foo' }, 'today');
let dash = (h.match(/—/g) || []).length;
chk(dash === 7, '无观测时应出 7 个「—」（调用/成功/状态/延迟/吞吐/消耗/明细），实得 ' + dash);
chk(h.indexOf('100.0%') < 0, '无观测时不能伪造 100%');
chk(h.indexOf('mdst') >= 0, '无观测时也要有 .mdst 容器');
chk(h.indexOf('今日调用') >= 0, 'today 档应显示「今日调用」');
h = T.mdStatStrip({ id: 'global:foo' }, '30d');
chk(h.indexOf('30 天调用') >= 0, '30d 档应显示「30 天调用」');
chk(h.indexOf('今日调用') < 0, '30d 档不应再出现「今日调用」');

// 2) 三档查表
ctx.mdStats = { 'global:foo': { model: 'global:foo', today_req: 1, today_err: 0,
  window_req: 1, window_err: 0, slots: [] } };
chk(T.mdStatOf('global:foo') !== null, '全名应命中');
chk(T.mdStatOf('foo') !== null, '去前缀应命中');
ctx.mdStats = { foo: { model: 'foo', today_req: 1, today_err: 0, window_req: 1, window_err: 0, slots: [] } };
chk(T.mdStatOf('global:foo') !== null, '补前缀应命中');
chk(T.mdStatOf('') === null, '空 id 应返回 null');

// 3) 数值与百分比：档位只渲染一对次数，另一对不得同屏
ctx.mdStats = { m: {
  model: 'm', today_req: 4027, today_err: 69,
  window_req: 88621, window_err: 3635,
  avg_latency_ms: 17100, avg_tokens_per_second: 205.5,
  slots: [
    { t: '2026-10-03T10', req: 0, err: 0 },
    { t: '2026-10-03T11', req: 10, err: 0 },
    { t: '2026-10-03T12', req: 10, err: 1 },
    { t: '2026-10-03T13', req: 10, err: 9 },
  ] } };
h = T.mdStatStrip({ id: 'm' }, 'today');
chk(h.indexOf('4,027') >= 0, '今日调用应千分位 4,027，实得 ' + h.slice(0, 400));
chk(h.indexOf('3,958') >= 0, '今日成功应为 3,958');
chk(h.indexOf('mds-k">30 天调用') < 0, 'today 档不应渲染近 30 天调用格（省横向空间）');
chk(h.indexOf('mds-k">30 天成功') < 0, 'today 档不应渲染近 30 天成功格');
chk(h.indexOf('95.9%') >= 0, '可用率应为 95.9%，实得 ' + h.slice(0, 600));
chk(h.indexOf('17.10s') >= 0, '延迟应格式化为 17.10s');
chk(h.indexOf('205.5 tok/s') >= 0, '吞吐应为 205.5 tok/s');
chk(h.indexOf('mdsg void') >= 0, '无观测格应为 void');
chk(h.indexOf('mdsg ok') >= 0, '全成格应为 ok');
chk(h.indexOf('mdsg warn') >= 0, '1/10 失败格应为 warn');
chk(h.indexOf('mdsg bad') >= 0, '9/10 失败格应为 bad');
chk(h.indexOf('近 30 天 88,621 次') >= 0, '另一档应进状态格 tooltip 备查');
chk((h.match(/class="mdst"/g) || []).length === 1, '只应有一个 .mdst 容器（一行）');

h = T.mdStatStrip({ id: 'm' }, '30d');
chk(h.indexOf('88,621') >= 0, '30d 档应显示近 30 天调用 88,621');
chk(h.indexOf('84,986') >= 0, '30d 档应显示近 30 天成功 84,986');
chk(h.indexOf('mds-k">今日调用') < 0, '30d 档不应渲染今日调用格');
chk(h.indexOf('今日 4,027 次') >= 0, '今日口径应进 tooltip 备查');

// 4) 可用率 < 90% 时百分比染红
ctx.mdStats = { bad: { model: 'bad', today_req: 10, today_err: 9,
  window_req: 100, window_err: 30, avg_latency_ms: 0, avg_tokens_per_second: 0,
  slots: [{ t: '2026-10-03T13', req: 1, err: 1 }] } };
h = T.mdStatStrip({ id: 'bad' }, 'today');
chk(h.indexOf('mds-v bad') >= 0, '低于 90% 的可用率应加 bad 类');
chk(h.indexOf('70.0%') >= 0, '可用率应为 70.0%');
chk(h.indexOf('>—<') >= 0, '没有延迟/吞吐样本时应显示「—」');

// 5) 用量口径四态：未知 / 免费 / 有消耗 / token 未记录与部分记录
ctx.mdStats = { u: { model: 'u', today_req: 1, today_err: 0, window_req: 1, window_err: 0,
  slots: [], credits: 0, credit_samples: 0, tokens_recorded: 0, tokens_missing: 1 } };
h = T.mdStatStrip({ id: 'u' }, 'today');
chk(h.indexOf('未知') >= 0, '无成本观测应显示「未知」');
chk(h.indexOf('未记录') >= 0, '无 token 观测应显示「未记录」');
chk(h.indexOf('免费') < 0, '无成本观测不能当成免费');
chk(h.indexOf('0 cr') < 0, '无成本观测不能显示 0 cr');
ctx.mdStats = { u: { model: 'u', today_req: 2, today_err: 0, window_req: 2, window_err: 0,
  slots: [], credits: 0, credit_samples: 3, tokens_recorded: 2, tokens_missing: 0 } };
h = T.mdStatStrip({ id: 'u' }, 'today');
chk(h.indexOf('免费') >= 0, '实测 0 消耗应显示「免费」');
chk(h.indexOf('2') >= 0, 'token 明细应显示记录条数');
ctx.mdStats = { u: { model: 'u', today_req: 2, today_err: 0, window_req: 2, window_err: 0,
  slots: [], credits: 12.5, credit_samples: 4, tokens_recorded: 2, tokens_missing: 1 } };
h = T.mdStatStrip({ id: 'u' }, 'today');
chk(h.indexOf('12.5 cr') >= 0, '实测消耗应显示 12.5 cr，实得 ' + h.slice(0, 800));
chk(h.indexOf('2/3') >= 0, '部分记录应显示 已记录/总数');

// 6) 上下文 / 最大输出角标压缩
chk(T.sizeTag(1000000) === '1M', '1000000 应压成 1M，实得 ' + T.sizeTag(1000000));
chk(T.sizeTag(1000000 * 2) === '2M', '2000000 应压成 2M');
chk(T.sizeTag(192000) === '192K', '192000 应压成 192K，实得 ' + T.sizeTag(192000));
chk(T.sizeTag(131072) === '131K', '131072 应压成 131K');
chk(T.sizeTag(50000) === '50K', '50000 应压成 50K');
chk(T.sizeTag(0) === '—', '0 应显示 —');

if (bad.length) { console.log('FAIL'); bad.forEach(m => console.log(' - ' + m)); process.exit(1); }
console.log('OK');`
	dir := t.TempDir()
	hp := dir + "/mdstats_harness.js"
	if err := os.WriteFile(hp, []byte(harness), 0o600); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	cmd := exec.Command(node, hp, "app.js")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("harness output: %s", out)
	}
}
