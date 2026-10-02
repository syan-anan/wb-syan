package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSUsageDimFilter 用量页「维度多选」的运行时行为闸门。
//
// 为什么需要：这段逻辑只在浏览器里生效，Go 侧不执行 JS；语法与顶层求值冒烟
// （TestAppJSSyntax / TestAppJSTopLevelSmoke）都抓不到「点了没反应 / 隐藏错表 /
// 取消到空页」这类行为回归。这里用最小 DOM 桩跑真实源码（从 app.js 切出
// usDim 段，不复制实现），断言用户约定的四条语义：
//   - 默认全选 = 「全部」（四张分表都显示）
//   - 取消一项只隐藏对应的那张表，按钮文字跟着变
//   - 取消到只剩一项时不再生效（不会整页空掉）
//   - 点「全部」恢复全选
//
// 无 node 的环境跳过。
func TestAppJSUsageDimFilter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; usage dim filter test skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('/* \u2500\u2500 \u7528\u91cf\u7ef4\u5ea6\u591a\u9009');
const end = src.indexOf('usDimInit();');
if (start < 0 || end < 0) { console.log('BLOCK NOT FOUND'); process.exit(1); }
const block = src.slice(start, end + 'usDimInit();'.length);

const created = [];
function mkEl(tag, id) {
  const el = { tagName: tag, id: id || '', children: [], hidden: false, parentNode: null,
    dataset: {}, _cls: new Set(), textContent: '', title: '', style: {}, attrs: {},
    offsetWidth: 120, offsetHeight: 100, onclick: null };
  el.classList = {
    add: c => el._cls.add(c),
    remove: c => el._cls.delete(c),
    contains: c => el._cls.has(c),
    toggle: (c, on) => { if (on === undefined) { el._cls.has(c) ? el._cls.delete(c) : el._cls.add(c); } else if (on) el._cls.add(c); else el._cls.delete(c); },
  };
  el.setAttribute = (k, v) => { el.attrs[k] = v; };
  el.getAttribute = k => el.attrs[k];
  el.appendChild = c => { el.children.push(c); c.parentNode = el; return c; };
  el.getBoundingClientRect = () => ({ left: 10, top: 10, bottom: 30, right: 110, width: 100, height: 20 });
  created.push(el);
  return el;
}

const boxes = {};
for (const id of ['usAccBox', 'usModelBox', 'usRealmBox', 'usClientBox']) boxes[id] = mkEl('div', id);
const btn = mkEl('button', 'usDimBtn');
const document = {
  body: mkEl('body'),
  getElementById: id => (id === 'usDimBtn' ? btn : boxes[id] || null),
  createElement: t => mkEl(t),
  querySelector: sel => {
    const m = /^\.sy-combo-item\[data-dim="([^"]+)"\]$/.exec(sel);
    return m ? (created.find(e => e.dataset && e.dataset.dim === m[1]) || null) : null;
  },
  querySelectorAll: () => [],
  addEventListener: () => {},
};
const SY_COMBO_CLOSERS = [];
function syComboCloseAll(except) {
  for (let i = SY_COMBO_CLOSERS.length - 1; i >= 0; i--) if (SY_COMBO_CLOSERS[i] !== except) SY_COMBO_CLOSERS[i]();
}
const ctx = { document, SY_COMBO_CLOSERS, syComboCloseAll, $: document.getElementById,
  requestAnimationFrame: fn => fn(), innerWidth: 1200, innerHeight: 800, console, Math, Set, Array, Object };
ctx.window = ctx; ctx.globalThis = ctx;
vm.createContext(ctx);
vm.runInContext(block + '\nthis.__T = { US_DIMS, usDimOn, menu: document.body.children[0], btn: $("usDimBtn") };', ctx);
const T = ctx.__T;

let bad = [];
const ok = (cond, msg) => { if (!cond) bad.push(msg); };
const rowOf = k => T.menu.children.find(c => c.dataset.dim === k);
const allShown = () => T.US_DIMS.every(d => boxes[d.box].hidden === false);

ok(T.US_DIMS.length === 4, 'dim count');
ok(T.menu.children.length === 5, 'menu rows = 5');
ok(T.usDimOn.size === 4 && allShown(), 'default = all shown');
ok(T.btn.textContent === '\u5168\u90e8', 'default label = \u5168\u90e8, got ' + T.btn.textContent);
ok(rowOf('__all').classList.contains('on'), 'row \u5168\u90e8 highlighted');

rowOf('model').onclick();
ok(boxes.usModelBox.hidden === true, 'model box hidden');
ok(boxes.usAccBox.hidden === false && boxes.usRealmBox.hidden === false && boxes.usClientBox.hidden === false, 'other boxes stay shown');
ok(T.btn.textContent === '\u6309\u8d26\u53f7\u3001\u6309\u57df\u3001\u6309\u5ba2\u6237\u7aef', 'label after unchecking model, got ' + T.btn.textContent);
ok(!rowOf('__all').classList.contains('on'), 'row \u5168\u90e8 unhighlighted');

rowOf('client').onclick();
ok(boxes.usClientBox.hidden === true, 'client box hidden');
rowOf('realm').onclick();
ok(T.usDimOn.size === 1, 'one dim left');
rowOf('account').onclick();
ok(T.usDimOn.size === 1 && boxes.usAccBox.hidden === false, 'last dim cannot be unchecked');

rowOf('__all').onclick();
ok(T.usDimOn.size === 4 && allShown(), 'restored to all');
ok(T.btn.textContent === '\u5168\u90e8', 'label restored');

btn.onclick({ preventDefault() {} });
ok(T.menu.classList.contains('on') && btn.attrs['aria-expanded'] === 'true', 'open state');
ok(T.menu.style.left === '10px' && T.menu.style.top === '36px', 'menu placed');
btn.onclick({ preventDefault() {} });
ok(!T.menu.classList.contains('on') && btn.attrs['aria-expanded'] === 'false', 'closed state');

if (bad.length) { console.log('DIM FAIL:\n' + bad.join('\n')); process.exit(1); }
console.log('DIM OK');
process.exit(0);
`
	hf, err := os.CreateTemp(t.TempDir(), "dim-*.cjs")
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
		t.Fatalf("用量维度多选行为断言失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "DIM OK") {
		t.Fatalf("用量维度多选 harness 未通过:\n%s", out)
	}
}
