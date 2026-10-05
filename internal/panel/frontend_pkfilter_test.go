package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSPackagesFilter 积分构成页「域 / 账号」筛选的运行时行为闸门。
//
// 与 TestAppJSUsageDimFilter 同一思路：这段逻辑只在浏览器里生效，Go 侧不执行
// JS；语法与顶层求值冒烟都抓不到「点了没反应 / 选项集合不跟着账号刷新重建 /
// 账号被移除后选择集残留导致空表」这类回归。这里用最小 DOM 桩跑真实源码
// （从 app.js 切出通用多选 + 筛选段，不复制实现），断言：
//   - 初始 = 全部（两个按钮都是「全部」，选项集合来自本次全量账号）
//   - 勾一个域 → 按钮文字变成该域名；勾两个 → 变成个数
//   - 点「全部」清空选择集
//   - 账号项按昵称排序、标签带域；勾一个 → 按钮显示昵称
//   - 账号消失后选择集被清掉（不留「筛了个不存在的号」）
//
// renderPackages 用桩替代（真实实现不在切出的片段里）：桩只做一件事——把
// 账号交给 pkFilterSync，从而复现「点击 → pkRender → renderPackages →
// pkFilterSync 更新按钮文字」这条真实链路。
//
// 无 node 的环境跳过。
func TestAppJSPackagesFilter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; packages filter test skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('/* \u2500\u2500 \u901a\u7528\u81ea\u7ed8\u591a\u9009\u4e0b\u62c9');
const end = src.indexOf('pkFilterInit();');
if (start < 0 || end < 0) { console.log('BLOCK NOT FOUND'); process.exit(1); }
const block = src.slice(start, end + 'pkFilterInit();'.length);

const created = [];
function mkEl(tag, id) {
  const el = { tagName: tag, id: id || '', children: [], hidden: false, parentNode: null,
    dataset: {}, _cls: new Set(), textContent: '', title: '', style: {}, attrs: {},
    offsetWidth: 120, offsetHeight: 100, onclick: null, _html: '' };
  el.classList = {
    add: c => el._cls.add(c),
    remove: c => el._cls.delete(c),
    contains: c => el._cls.has(c),
    toggle: (c, on) => { if (on === undefined) { el._cls.has(c) ? el._cls.delete(c) : el._cls.add(c); } else if (on) el._cls.add(c); else el._cls.delete(c); },
  };
  // className 必须和 classList 共用同一个集合：实现里是用 className 拼的，
  // 桩若只认 classList.add，就永远看不到 'on'。
  Object.defineProperty(el, 'className', {
    get() { return [...el._cls].join(' '); },
    set(v) { el._cls.clear(); String(v).split(/\s+/).forEach(c => { if (c) el._cls.add(c); }); },
  });
  Object.defineProperty(el, 'innerHTML', {
    get() { return el._html; },
    set(v) { el._html = v; if (v === '') el.children.length = 0; },
  });
  el.setAttribute = (k, v) => { el.attrs[k] = v; };
  el.getAttribute = k => el.attrs[k];
  el.appendChild = c => { el.children.push(c); c.parentNode = el; return c; };
  el.getBoundingClientRect = () => ({ left: 10, top: 10, bottom: 30, right: 110, width: 100, height: 20 });
  created.push(el);
  return el;
}

const realmBtn = mkEl('button', 'pkRealmBtn');
const acctBtn = mkEl('button', 'pkAcctBtn');
const document = {
  body: mkEl('body'),
  getElementById: id => (id === 'pkRealmBtn' ? realmBtn : id === 'pkAcctBtn' ? acctBtn : null),
  createElement: t => mkEl(t),
  querySelector: () => null,
  querySelectorAll: () => [],
  addEventListener: () => {},
};
const SY_COMBO_CLOSERS = [];
function syComboCloseAll(except) {
  for (let i = SY_COMBO_CLOSERS.length - 1; i >= 0; i--) if (SY_COMBO_CLOSERS[i] !== except) SY_COMBO_CLOSERS[i]();
}
const all = [
  { uid: 'u1', nickname: 'syan', realm: 'cn' },
  { uid: 'u2', nickname: 'Ali', realm: 'global' },
  { uid: 'u3', nickname: 'bob', realm: 'cn' },
];
const ctx = { document, SY_COMBO_CLOSERS, syComboCloseAll, $: document.getElementById,
  requestAnimationFrame: fn => fn(), innerWidth: 1200, innerHeight: 800, console, Math, Set, Array, Object, Number, String,
  ALL: all };
ctx.window = ctx; ctx.globalThis = ctx;
vm.createContext(ctx);
vm.runInContext(
  'const PK_DEFAULT_DETAIL_LIMIT = 5;\n' +
  'function renderPackages(d) { pkFilterSync((d && d.accounts) || []); }\n' +
  block +
  '\nthis.__T = { pkRealmSel, pkAcctSel, pkFilterSync, realmMenu: document.body.children[0],' +
  ' acctMenu: document.body.children[1], realmBtn: $("pkRealmBtn"), acctBtn: $("pkAcctBtn"),' +
  ' seed: () => { pkLastData = { accounts: ALL }; } };', ctx);
const T = ctx.__T;
T.seed();
T.pkFilterSync(all);

let bad = [];
const ok = (cond, msg) => { if (!cond) bad.push(msg); };
const labels = menu => menu.children.map(c => c.textContent);

ok(T.realmMenu.children.length === 3, 'realm rows = 3, got ' + T.realmMenu.children.length + ' ' + JSON.stringify(labels(T.realmMenu)));
ok(JSON.stringify(labels(T.realmMenu)) === JSON.stringify(['\u5168\u90e8', '\u56fd\u5185', '\u56fd\u9645']), 'realm labels, got ' + JSON.stringify(labels(T.realmMenu)));
ok(T.acctMenu.children.length === 4, 'acct rows = 4, got ' + T.acctMenu.children.length + ' ' + JSON.stringify(labels(T.acctMenu)));
ok(JSON.stringify(labels(T.acctMenu)) === JSON.stringify(['\u5168\u90e8', 'Ali\uff08\u56fd\u9645\uff09', 'bob\uff08\u56fd\u5185\uff09', 'syan\uff08\u56fd\u5185\uff09']), 'acct labels, got ' + JSON.stringify(labels(T.acctMenu)));
ok(T.pkRealmSel.size === 0 && T.pkAcctSel.size === 0, 'default = no filter');
ok(T.realmMenu.children[0].classList.contains('on'), 'realm \u5168\u90e8 highlighted');
ok(T.realmBtn.textContent === '\u57df\uff1a\u5168\u90e8', 'realm btn default, got ' + T.realmBtn.textContent);
ok(T.acctBtn.textContent === '\u8d26\u53f7\uff1a\u5168\u90e8', 'acct btn default, got ' + T.acctBtn.textContent);

T.realmMenu.children[1].onclick();
ok(T.pkRealmSel.size === 1 && T.pkRealmSel.has('cn'), 'realm cn selected');
ok(T.realmBtn.textContent === '\u57df\uff1a\u56fd\u5185', 'realm btn after cn, got ' + T.realmBtn.textContent);
ok(!T.realmMenu.children[0].classList.contains('on'), 'realm \u5168\u90e8 unhighlighted');
ok(T.realmMenu.children[1].classList.contains('on'), 'realm \u56fd\u5185 highlighted');

T.realmMenu.children[2].onclick();
ok(T.pkRealmSel.size === 2, 'realm both selected');
ok(T.realmBtn.textContent === '\u57df\uff1a2 \u4e2a', 'realm btn after two, got ' + T.realmBtn.textContent);

T.realmMenu.children[0].onclick();
ok(T.pkRealmSel.size === 0, 'realm cleared by \u5168\u90e8');
ok(T.realmBtn.textContent === '\u57df\uff1a\u5168\u90e8', 'realm btn restored, got ' + T.realmBtn.textContent);

T.acctMenu.children[3].onclick();
ok(T.pkAcctSel.size === 1 && T.pkAcctSel.has('u1'), 'acct u1 selected');
ok(T.acctBtn.textContent === '\u8d26\u53f7\uff1asyan', 'acct btn single, got ' + T.acctBtn.textContent);
T.acctMenu.children[1].onclick();
ok(T.pkAcctSel.size === 2, 'acct two selected');
ok(T.acctBtn.textContent === '\u8d26\u53f7\uff1a2 \u4e2a', 'acct btn two, got ' + T.acctBtn.textContent);

// 账号被移除：选择集里的 u1/u3 必须被清掉，只剩还在的 u2。
T.pkFilterSync([{ uid: 'u2', nickname: 'Ali', realm: 'global' }]);
ok(T.pkAcctSel.size === 1 && T.pkAcctSel.has('u2'), 'stale uids pruned, got ' + JSON.stringify([...T.pkAcctSel]));
ok(T.acctBtn.textContent === '\u8d26\u53f7\uff1aAli', 'acct btn after prune, got ' + T.acctBtn.textContent);
ok(T.acctMenu.children.length === 2, 'acct rows after prune = 2, got ' + T.acctMenu.children.length);

// 下拉开关
T.realmBtn.onclick({ preventDefault() {} });
ok(T.realmMenu.classList.contains('on') && T.realmBtn.attrs['aria-expanded'] === 'true', 'menu open state');
ok(T.realmMenu.style.left === '10px' && T.realmMenu.style.top === '36px', 'menu placed');
T.realmBtn.onclick({ preventDefault() {} });
ok(!T.realmMenu.classList.contains('on') && T.realmBtn.attrs['aria-expanded'] === 'false', 'menu closed state');

if (bad.length) { console.log('PKFILTER FAIL:\n' + bad.join('\n')); process.exit(1); }
console.log('PKFILTER OK');
process.exit(0);
`
	hf, err := os.CreateTemp(t.TempDir(), "pkfilter-*.cjs")
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
		t.Fatalf("积分构成筛选行为断言失败: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PKFILTER OK") {
		t.Fatalf("积分构成筛选 harness 未通过:\n%s", out)
	}
}
