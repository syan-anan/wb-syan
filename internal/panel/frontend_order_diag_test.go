package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSOrderAndDiagnose 手动排序与错误三层诊断的前端闸门。
//
// 为什么需要：两者都是「后端有字段、前端没渲染」的典型断点。排序如果只测
// 后端比较器，用户点完 ↑ 看不见账号动，功能等于没做；诊断面板如果少渲染
// 一层，用户看到的就还是「message=上游原文」的老样子。这里直接求值 app.js
// 的真实函数体，锁死三件事：
//   - sortAccounts 的第一档是 priority 降序（与池内选号同口径）
//   - renderDiagnose 三层齐全（网关判断 / 客户端解释 / 上游证据）
//   - 缺层时写「—」，不拿上游原文冒充客户端解释
//
// 无 node 的环境跳过。
func TestAppJSOrderAndDiagnose(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; order/diagnose test skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
function block(startMarker) {
  const a = src.indexOf(startMarker);
  if (a < 0) { console.log('BLOCK NOT FOUND: ' + startMarker); process.exit(1); }
  const b = src.indexOf(String.fromCharCode(10) + '}', a);
  if (b < 0) { console.log('BLOCK END NOT FOUND: ' + startMarker); process.exit(1); }
  return src.slice(a, b + 2);
}
const els = {};
function el(id) {
  if (!els[id]) els[id] = { id, textContent: '', innerHTML: '', value: '', onclick: null, disabled: false };
  return els[id];
}
const ctx = { Math, Number, String, isNaN, Object, Array, console, JSON, Date, RegExp, encodeURIComponent };
ctx.$ = el;
ctx.document = { getElementById: el, querySelector: () => null, querySelectorAll: () => [] };
ctx.window = ctx; ctx.globalThis = ctx;
vm.createContext(ctx);
const parts = [
  block('function esc('),
  block('function sortAccounts('),
  block('function renderDiagnose('),
];
vm.runInContext('var accSort = "added";' + parts.join(String.fromCharCode(10)) +
  String.fromCharCode(10) + 'this.__T = { sort: sortAccounts, diag: renderDiagnose, setSort: v => { accSort = v; } };', ctx);
const T = ctx.__T;

const bad = [];
const chk = (cond, msg) => { if (!cond) bad.push(msg); };

// 1) priority 是第一档：正数置顶、0 居中、负数沉底；同档回落到添加时间
const accs = [
  { uid: 'a', added_at: 10, priority: 0 },
  { uid: 'b', added_at: 20, priority: 5 },
  { uid: 'c', added_at: 30, priority: -2 },
  { uid: 'd', added_at: 40, priority: 9 },
];
const ord = T.sort(accs).map(a => a.uid).join(',');
chk(ord === 'd,b,a,c', 'priority 降序排序不符：' + ord);
chk(accs.map(a => a.uid).join(',') === 'a,b,c,d', 'sortAccounts 不能原地改数组');
// 全 0 时零回归：仍然是添加时间序
chk(T.sort([{ uid: 'y', added_at: 2 }, { uid: 'x', added_at: 1 }]).map(a => a.uid).join(',') === 'x,y',
  '无手动排序时应保持添加时间序');

// 2) 三层齐全
T.diag({ entries: [{
  ts: 1893456000000, account: 'u1@test', model: 'global:glm-5.3', status: 429,
  upstream_code: '42901', kind: 'rate_limit',
  gateway_hint: '全部账号都在冷却，稍后重试', client_message: '请求过于频繁',
  client_detail: '上游返回限流，减少并发或稍后重试', upstream_msg: 'rate limited by upstream',
}]});
let h = el('diagBody').innerHTML;
chk(h.indexOf('全部账号都在冷却') >= 0, '缺网关判断层');
chk(h.indexOf('请求过于频繁') >= 0, '缺客户端解释层');
chk(h.indexOf('rate limited by upstream') >= 0, '缺上游证据层');
chk(h.indexOf('42901') >= 0, '缺上游业务码');
chk(h.indexOf('u1@test') >= 0 && h.indexOf('global:glm-5.3') >= 0, '缺账号/模型上下文');
chk(h.indexOf('diagNote') < 0 && el('diagNote').textContent.indexOf('1') >= 0, '条目计数缺失：' + el('diagNote').textContent);

// 3) 缺层写「—」，不编造
T.diag({ entries: [{ ts: 1893456000000, status: 500, gateway_hint: '上游 500' }] });
h = el('diagBody').innerHTML;
chk(h.indexOf('码表未收录') >= 0, '无客户端解释时必须显式说明未收录');
chk(h.indexOf('— 网关未给出判断') < 0, '网关判断存在时不应写「—」');

// 4) 空列表给空态，不渲染表头行
T.diag({ entries: [] });
h = el('diagBody').innerHTML;
chk(h.indexOf('没有错误记录') >= 0, '空态缺失');

if (bad.length) { console.log('FAIL'); bad.forEach(m => console.log(' - ' + m)); process.exit(1); }
console.log('OK');`
	dir := t.TempDir()
	hp := dir + "/order_diag_harness.js"
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
