package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSAgoTick 相对时间「每秒自走」的运行时闸门。
//
// 为什么需要：这段逻辑只在浏览器里跑，Go 侧不执行 JS。它有三个容易回归的点，
// 语法/冒烟测试都抓不到：
//   - agoAt 的边界（<1s 不能显示「0 秒前」；分钟/小时/天阈值不能错位）
//   - tickAgo 必须真的改写 [data-ago] 的文本，且文本没变时不重复写 DOM
//   - 后台标签页（document.hidden）必须直接跳过，别白烧 CPU
//
// 顺带锁 fmtInt：积分口径要精确数字（12,838），不允许 k/m 缩写。
// 无 node 的环境跳过。
func TestAppJSAgoTick(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; ago tick test skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const a0 = src.indexOf('function agoAt(');
const a1 = src.indexOf('function dur(sec) {');
const f0 = src.indexOf('function fmtInt(');
const f1 = src.indexOf('function fmtTok(');
if (a0 < 0 || a1 < 0 || f0 < 0 || f1 < 0) { console.log('BLOCK NOT FOUND'); process.exit(1); }

const RealDate = Date;
let FAKE = 1700000000000;
function D(...a) { return a.length ? new RealDate(...a) : new RealDate(FAKE); }
D.now = () => FAKE;
D.parse = RealDate.parse; D.UTC = RealDate.UTC; D.prototype = RealDate.prototype;

let writes = 0;
function mkEl(iso) {
  const el = { _iso: iso, _txt: '', getAttribute: k => (k === 'data-ago' ? el._iso : null) };
  Object.defineProperty(el, 'textContent', {
    get: () => el._txt,
    set: v => { writes++; el._txt = v; },
  });
  return el;
}
const els = [mkEl(new RealDate(FAKE - 3000).toISOString()),
             mkEl(new RealDate(FAKE - 65000).toISOString())];
const document = { hidden: false, querySelectorAll: sel => (sel === '[data-ago]' ? els : []) };

const ctx = { Date: D, Math, Number, String, isNaN, document, console };
ctx.window = ctx; ctx.globalThis = ctx;
vm.createContext(ctx);
vm.runInContext(src.slice(a0, a1) + src.slice(f0, f1) +
  '\nthis.__T = { agoAt, ago, absTime, tickAgo, fmtInt };', ctx);
const T = ctx.__T;

const bad = [];
const ok = (c, m) => { if (!c) bad.push(m); };
const iso = ms => new RealDate(FAKE - ms).toISOString();

ok(T.agoAt('0001-01-01T00:00:00Z', FAKE) === '\u2014', 'zero time -> \u2014');
ok(T.agoAt(iso(400), FAKE) === '\u521a\u521a', '0.4s -> \u521a\u521a, got ' + T.agoAt(iso(400), FAKE));
ok(T.agoAt(iso(1000), FAKE) === '1 \u79d2\u524d', '1s -> 1 \u79d2\u524d, got ' + T.agoAt(iso(1000), FAKE));
ok(T.agoAt(iso(3000), FAKE) === '3 \u79d2\u524d', '3s -> 3 \u79d2\u524d, got ' + T.agoAt(iso(3000), FAKE));
ok(T.agoAt(iso(59000), FAKE) === '59 \u79d2\u524d', '59s -> 59 \u79d2\u524d, got ' + T.agoAt(iso(59000), FAKE));
ok(T.agoAt(iso(60000), FAKE) === '1 \u5206\u949f\u524d', '60s -> 1 \u5206\u949f\u524d, got ' + T.agoAt(iso(60000), FAKE));
ok(T.agoAt(iso(3600000), FAKE) === '1 \u5c0f\u65f6\u524d', '1h -> 1 \u5c0f\u65f6\u524d');
ok(T.agoAt(iso(86400000), FAKE) === '1 \u5929\u524d', '1d -> 1 \u5929\u524d');
ok(T.agoAt(new RealDate(FAKE + 5000).toISOString(), FAKE) === '\u521a\u521a', 'future -> \u521a\u521a');
ok(T.absTime('0001-01-01T00:00:00Z') === '', 'absTime zero -> ""');
ok(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(T.absTime(iso(3000))), 'absTime 格式应为 YYYY-MM-DD HH:MM:SS');

writes = 0;
T.tickAgo();
ok(els[0].textContent === '3 \u79d2\u524d', 'tick el0 = 3 \u79d2\u524d, got ' + els[0].textContent);
ok(els[1].textContent === '1 \u5206\u949f\u524d', 'tick el1 = 1 \u5206\u949f\u524d, got ' + els[1].textContent);
const afterFirst = writes;
FAKE += 2000;
T.tickAgo();
ok(els[0].textContent === '5 \u79d2\u524d', 'tick el0 +2s = 5 \u79d2\u524d, got ' + els[0].textContent);
ok(writes > afterFirst, 'tick \u5728\u65f6\u95f4\u524d\u8fdb\u540e\u5e94\u91cd\u5199\u6587\u672c');
const beforeIdle = writes;
T.tickAgo();
ok(writes === beforeIdle, 'tick \u6587\u672c\u672a\u53d8\u65f6\u4e0d\u5e94\u91cd\u590d\u5199 DOM');
document.hidden = true;
FAKE += 5000;
T.tickAgo();
ok(els[0].textContent === '5 \u79d2\u524d', 'hidden tab \u5e94\u8df3\u8fc7, got ' + els[0].textContent);
document.hidden = false;

ok(T.fmtInt(12838) === '12,838', 'fmtInt 12838 = ' + T.fmtInt(12838));
ok(T.fmtInt(0) === '0', 'fmtInt 0 = ' + T.fmtInt(0));
ok(T.fmtInt(1234567) === '1,234,567', 'fmtInt 1234567 = ' + T.fmtInt(1234567));
ok(T.fmtInt(154.6) === '155', 'fmtInt \u56db\u820d\u4e94\u5165, got ' + T.fmtInt(154.6));
ok(!/[kKmMbB]/.test(T.fmtInt(12838)), 'fmtInt \u4e0d\u5f97\u51fa\u73b0 k/m/b \u7f29\u5199');

if (bad.length) { console.log('AGO FAIL:\n' + bad.join('\n')); process.exit(1); }
console.log('AGO OK');
process.exit(0);
`
	hf, err := os.CreateTemp(t.TempDir(), "ago-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	cmd := exec.Command(node, hf.Name(), "app.js")
	cmd.Dir = "." // 测试工作目录 = internal/panel
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("相对时间自走行为断言失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "AGO OK") {
		t.Fatalf("相对时间 harness 未通过:\n%s", out)
	}
}
