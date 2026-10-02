package server

import (
	"net/http"
	"strings"
	"testing"
)

// mkHdr 按 (名, 值) 成对追加构造请求头；同一个名字给两次就是「重复头」。
func mkHdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

func TestAgentClientLabel(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		want string
	}{
		{"claude-code 专用头", mkHdr("X-Claude-Code-Session-Id", "s1"), "claude-code"},
		{"codex 专用头", mkHdr("X-Codex-Window-Id", "w1"), "codex"},
		{"codex 开集前缀兜底", mkHdr("X-Codex-Future-Thing", "x"), "codex"},
		{"opencode 专用头", mkHdr("X-Opencode-Project", "p"), "opencode"},
		{"originator 值规则", mkHdr("Originator", "codex_cli_rs"), "codex"},
		{"cline 值规则", mkHdr("X-Client-Type", "cline-sdk"), "cline"},
		{"kilo 值规则", mkHdr("X-Title", "Kilo Code"), "kilo-code"},

		{"UA codex_cli_rs（真机）", mkHdr("User-Agent", "codex_cli_rs/0.153.4 (Windows 11; x64)"), "codex"},
		{"UA codex 旧写法", mkHdr("User-Agent", "codex/0.1"), "codex"},
		{"UA codex-desktop", mkHdr("User-Agent", "codex-desktop/1.0"), "codex-desktop"},
		{"UA claude-cli", mkHdr("User-Agent", "claude-cli/2.1.161 (external, cli)"), "claude-code"},
		{"UA claude-vscode", mkHdr("User-Agent", "Claude-VSCode/1.0"), "claude-code"},
		{"UA workbuddy 官方", mkHdr("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2"), "workbuddy"},
		{"UA deepseek-harness", mkHdr("User-Agent", "deepseek-harness/1.2 (+https://github.com/deepseek-ai/deepseek-harness)"), "deepseek-harness"},
		{"UA zcode", mkHdr("User-Agent", "zcode/0.9.1"), "zcode"},
		{"UA kimi", mkHdr("User-Agent", "Kimicli/0.5"), "kimi-cli"},
		{"UA gemini", mkHdr("User-Agent", "GeminiCLI/0.20.0/gemini-2.5-pro (linux; x64; cli)"), "gemini-cli"},
		{"UA cherry studio", mkHdr("User-Agent", "CherryStudio/1.4.0"), "cherry-studio"},
		{"UA cursor", mkHdr("User-Agent", "Cursor/0.48.7 (darwin arm64)"), "cursor"},

		{"UA 未知工具", mkHdr("User-Agent", "curl/8.5.0"), "未识别: curl"},
		{"无任何头", mkHdr(), "未识别: (无 UA)"},
		{"专用头优先于 UA", mkHdr("X-Claude-Code-Session-Id", "s1", "User-Agent", "codex_cli_rs/1"), "claude-code"},
		{"重复专用头不算", mkHdr("X-Codex-Window-Id", "w1", "X-Codex-Window-Id", "w2", "User-Agent", "curl/8.5.0"), "未识别: curl"},
		{"空值专用头不算", mkHdr("X-Codex-Window-Id", "   ", "User-Agent", "curl/8.5.0"), "未识别: curl"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := agentClientLabel(c.h); got != c.want {
				t.Fatalf("agentClientLabel() = %q, want %q", got, c.want)
			}
		})
	}
}

// 识别输入超 8KB 预算时整体放弃识别，不猜、也不让超大头撑爆用量桶。
func TestAgentClientLabelBudget(t *testing.T) {
	big := strings.Repeat("a", agentHeaderBudget+1)
	if got := agentClientLabel(mkHdr("User-Agent", big)); got != "未识别: (头超长)" {
		t.Fatalf("超预算 UA: got %q, want 未识别: (头超长)", got)
	}
	// 恰好等于预算：不算超，正常走识别流程。
	exact := strings.Repeat("a", agentHeaderBudget)
	if got := agentClientLabel(mkHdr("User-Agent", exact)); got == "未识别: (头超长)" {
		t.Fatalf("恰好等于预算不应判超长: got %q", got)
	}
	// 与识别无关的大头不占预算（否则随便一个 cookie 就能让识别失效）。
	h := mkHdr("User-Agent", "codex_cli_rs/1.0", "Cookie", strings.Repeat("b", agentHeaderBudget*2))
	if got := agentClientLabel(h); got != "codex" {
		t.Fatalf("无关大头不应影响识别: got %q, want codex", got)
	}
	// 参与识别的头加起来超预算 → 判超长（UA 4KB + 特征头 4KB + 1 字节）。
	h2 := mkHdr("User-Agent", strings.Repeat("c", 4096), "X-Codex-Window-Id", strings.Repeat("d", 4097))
	if got := agentClientLabel(h2); got != "未识别: (头超长)" {
		t.Fatalf("特征头累计超预算: got %q, want 未识别: (头超长)", got)
	}
}

func TestUAToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"curl/8.5.0", "curl"},
		{"MyTool 1.0", "mytool"},
		{"", "(无 UA)"},
		{"   ", "(无 UA)"},
		{"/weird", "/weird"},
		{strings.Repeat("x", 40), strings.Repeat("x", 32)},
	}
	for _, c := range cases {
		if got := uaToken(c.in); got != c.want {
			t.Fatalf("uaToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
