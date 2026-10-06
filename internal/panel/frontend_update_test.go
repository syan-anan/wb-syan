package panel

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSUpdateChip 锁死侧栏「有新版本」提示的渲染分支：只有 enabled + latest
// 且 has_update 为真时才显示胶囊；其余（关闭检查、没取到版本、已是最新）一律
// 隐藏并关掉浮层。这条链路上没有 Go 代码兜底，错了就是"永远不提示"或"永远提示"。
func TestAppJSUpdateChip(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; update chip test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let updInfo = null');
const end = src.indexOf("$('navUpdChip').onclick");
if (start < 0 || end < 0) throw new Error('update chip block not found');
const els = {};
for (const id of ['navUpdRow','navUpdChip','updLatest','updCurrent','updLink','updPop']) {
  els[id] = { hidden: false, textContent: '', title: '', href: '#' };
}
const ctx = { $: id => els[id] };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderUpdate = renderUpdate;', ctx);
const snap = () => ({
  chipHidden: els.navUpdChip.hidden, pop: els.updPop.hidden, chip: els.navUpdChip.textContent,
  title: els.navUpdChip.title, latest: els.updLatest.textContent,
  current: els.updCurrent.textContent, href: els.updLink.href,
});
const out = {};
ctx.renderUpdate({ enabled: true, current: '1.17.0-panel', latest: 'v1.18.0', has_update: true, url: 'https://example.test/tag' });
out.new = snap();
ctx.renderUpdate({ enabled: true, current: '1.17.0-panel', latest: 'v1.17.0', has_update: false });
out.uptodate = snap();
ctx.renderUpdate({ enabled: false });
out.disabled = snap();
ctx.renderUpdate({ enabled: true, current: '1.17.0-panel' });
out.empty = snap();
process.stdout.write(JSON.stringify(out));`
	f, err := os.CreateTemp(t.TempDir(), "updchip-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("update chip node test failed: %v\n%s", err, out)
	}
	const want = `{"new":{"chipHidden":false,"pop":false,"chip":"有新版本 v1.18.0","title":"当前 v1.17.0-panel → 最新 v1.18.0（点开看升级命令）","latest":"v1.18.0","current":"当前 v1.17.0-panel","href":"https://example.test/tag"},"uptodate":{"chipHidden":true,"pop":true,"chip":"有新版本 v1.18.0","title":"当前 v1.17.0-panel → 最新 v1.18.0（点开看升级命令）","latest":"v1.18.0","current":"当前 v1.17.0-panel","href":"https://example.test/tag"},"disabled":{"chipHidden":true,"pop":true,"chip":"有新版本 v1.18.0","title":"当前 v1.17.0-panel → 最新 v1.18.0（点开看升级命令）","latest":"v1.18.0","current":"当前 v1.17.0-panel","href":"https://example.test/tag"},"empty":{"chipHidden":true,"pop":true,"chip":"有新版本 v1.18.0","title":"当前 v1.17.0-panel → 最新 v1.18.0（点开看升级命令）","latest":"v1.18.0","current":"当前 v1.17.0-panel","href":"https://example.test/tag"}}`
	if !bytes.Equal([]byte(strings.TrimSpace(string(out))), []byte(want)) {
		t.Fatalf("update chip=%s\nwant %s", out, want)
	}
}
