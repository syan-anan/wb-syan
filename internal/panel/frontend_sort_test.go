package panel

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSSortAndKeySummary 账号池排序与密钥卡片摘要的**行为**验证：
// 在 node 里真实求值 app.js（复用 TestAppJSTopLevelSmoke 的 DOM 桩），再直接调用
// sortAccounts / keyLimitTags / quotaText / toLocalInput 断言输出。
//
// 为什么必须跑真实源码：这些是纯函数，Go 侧完全看不见；「账号池排序既不是添加时间
// 也不是国内/国际」就是后端 sort.Strings(uid) 与前端无排序叠加的产物，只有行为断言
// 才能把排序规则钉住。无 node 环境时跳过。
func TestAppJSSortAndKeySummary(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; sort/key summary test skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const sandbox = new Proxy({
  location: { hash: '#accounts' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval, clearInterval, setTimeout, clearTimeout,
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src, sandbox, { filename: 'app.js' });
vm.runInContext('this.__t = { sort: sortAccounts, tags: keyLimitTags, quota: quotaText, fmt: fmtUnixTime, local: toLocalInput };', sandbox);
const T = sandbox.__t;
const accs = [
  { uid: 'c', realm: 'global', added_at: 50 },
  { uid: 'a', realm: 'cn', added_at: 100 },
  { uid: 'b', realm: 'global', added_at: 200 },
  { uid: 'z', realm: 'cn' },
];
vm.runInContext('accSort = "added"', sandbox);
const added = T.sort(accs).map(a => a.uid).join(',');
vm.runInContext('accSort = "realm"', sandbox);
const realm = T.sort(accs).map(a => a.uid).join(',');
vm.runInContext('accSort = "bogus"', sandbox);
const bogus = T.sort(accs).map(a => a.uid).join(',');
const ts = 1893456000;
console.log(JSON.stringify({
  added, realm, bogus,
  orig: accs.map(a => a.uid).join(','),
  tags: T.tags({ accounts: ['u1', 'u2'], models: ['glm-5.3'], realms: ['cn'], time_start: '09:00', time_end: '18:00' }),
  tagsOpen: T.tags({}),
  quota: T.quota({ daily_limit: 100, used_today: 3, hourly_limit: 10, used_hour: 4 }),
  quotaOpen: T.quota({}),
  fmtOk: /^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/.test(T.fmt(ts)),
  localOk: /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(T.local(ts)),
  roundTrip: new Date(T.local(ts)).getTime() / 1000 === ts,
}));
// app.js 顶层 start() 的 setInterval 会让事件循环不退出：与 TestAppJSTopLevelSmoke
// 同判，成功路径显式 exit(0)（不 exit 的话 go test 会一直挂在 node 上）。
process.exit(0);`
	hf, err := os.CreateTemp(t.TempDir(), "sort-*.cjs")
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
		t.Fatalf("node 排序/摘要测试失败: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	want := `{"added":"z,c,a,b","realm":"z,a,c,b","bogus":"z,c,a,b","orig":"c,a,b,z",` +
		`"tags":["账号 2 个","模型 glm-5.3","域 国内","时段 09:00–18:00"],"tagsOpen":["账号不限"],` +
		`"quota":"今日 3/100 · 本小时 4/10","quotaOpen":"","fmtOk":true,"localOk":true,"roundTrip":true}`
	if got != want {
		t.Fatalf("排序/摘要结果不符:\n got=%s\nwant=%s", got, want)
	}
	if bytes.Contains(out, []byte("SMOKE FAIL")) {
		t.Fatalf("app.js 求值失败:\n%s", out)
	}
}
