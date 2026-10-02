package panel

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 主题层必须能作为同源样式表取到（否则 index.html 的 <link> 404，面板退回旧外观）。
func TestThemeCSSServed(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/theme.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Errorf("Content-Type=%q want text/css", ct)
	}
	body := rec.Body.String()
	for _, must := range []string{"--glass", "@font-face", ".keycard", ".shell"} {
		if !strings.Contains(body, must) {
			t.Errorf("theme.css missing %q", must)
		}
	}
}

// 字体必须能取到且是合法 woff2：字体取不到时浏览器静默回退，只有肉眼能看出，
// 所以把"文件在 + 类型对 + 魔数对 + 大小对"前移到 CI。
func TestFontServed(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/fonts/syan-round.woff2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "font/woff2" {
		t.Errorf("Content-Type=%q want font/woff2", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control=%q want immutable（5MB 字体不该每次重下）", cc)
	}
	b := rec.Body.Bytes()
	if !bytes.HasPrefix(b, []byte("wOF2")) {
		t.Fatalf("不是 woff2 魔数，前 4 字节 = %q", b[:min(4, len(b))])
	}
	if len(b) < 1_000_000 {
		t.Fatalf("字体体积异常：%d 字节", len(b))
	}
}

// index.html 必须在**内联样式之后**引用主题层，且该层只加样式不改结构。
func TestIndexReferencesThemeAfterInlineStyle(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	link := `<link rel="stylesheet" href="theme.css">`
	iLink := strings.Index(body, link)
	if iLink < 0 {
		t.Fatal("index.html 缺少 theme.css 的 <link>")
	}
	iStyle := strings.Index(body, "</style>")
	if iStyle < 0 || iStyle > iLink {
		t.Fatal("theme.css 必须在内联 <style> 之后加载，否则覆盖不生效")
	}
	// 反例保护：主题层是纯样式，不得引入内联脚本
	if strings.Contains(body, "<script>\n") || strings.Contains(body, "<script> ") {
		t.Error("index.html 不得出现内联 <script>（CSP 会拦掉，页面白屏）")
	}
}

// 字体来自本服务，CSP 必须开口 font-src，否则 @font-face 被拦。
func TestCSPAllowsSelfFonts(t *testing.T) {
	if !strings.Contains(csp, "font-src 'self'") {
		t.Fatalf("CSP 缺少 font-src 'self'，字体将被拦截: %s", csp)
	}
	// default-src 'none' 仍然保留：其它未开口的来源一律禁止
	if !strings.HasPrefix(csp, "default-src 'none'") {
		t.Errorf("CSP 必须以 default-src 'none' 开头: %s", csp)
	}
}