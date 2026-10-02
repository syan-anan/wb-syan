package server

import (
	"net/http"
	"strings"
)

// agentclient.go 入站请求的「编码 Agent 客户端」识别。
//
// 目的：面板「用量」视图按客户端下钻——这一列回答的是「这些 token 是 Codex 花的、
// 还是 Claude Code 花的、还是 WorkBuddy 官方客户端花的」。
//
// 识别规则移植并交叉比对自多处上游实现，再按本网关的实际链路做了修正：
//
//   - langfuse ai-gateway `ai-gateway/src/telemetry/context.rs`（原版设计：
//     专用头优先、User-Agent 前缀兜底、识别输入有字节预算、识别不到就不猜）；
//   - yolorouter `internal/gateway/agent_client.go`（Apache-2.0，同一套规则，
//     含 8KB 预算与「重复头视为不存在」的处理）；
//   - maximhq/bifrost `core/schemas/useragents.go`（Go 实现、有序匹配表；其注释
//     给出两条硬信息：`x-claude-code-*` 是随版本增长的开集；`x-parent-session-id`
//     会把子 agent 折叠到父 key，故刻意不参与识别）；
//   - yetone/magpie `internal/agent/*.go`、musistudio/claude-code-router
//     `request-log-store.ts`、tashfeenahmed/freellmapi `client-classifier.ts`
//     （三家的 UA 表，用于补齐国内/新兴客户端的 UA 前缀）。
//
// 本网关的修正（证据来自 CC Switch 源码 `src-tauri/src/proxy/forwarder.rs`）：
//
//   - CC Switch 出站头构造是「遍历客户端原始头、默认透传」，因此 `x-claude-code-*`
//     能到达本网关；但 `is_codex_client_fingerprint_header` 会在发往**第三方上游**
//     时把 `x-codex-*` / `x-stainless-*` / `x-oai-*` / `x-openai-*` 整族剥掉，
//     所以 Codex 只能靠 User-Agent / originator 兜底识别。
//   - Codex CLI 的真实 UA 前缀是 `codex_cli_rs/`（openai/codex
//     `codex-rs/login/src/auth/default_client.rs` 拼的是
//     `{originator}/{version} ({os}; {arch})`，originator 默认 `codex_cli_rs`；
//     CC Switch 源码注释里的实测值是 `codex_cli_rs/0.153.4`），不是上游表里写的
//     `codex/`；两者都保留，前者优先。
//   - Codex CLI 同时会带 `originator: codex_cli_rs` 头，这条比 UA 更可靠，优先用。
//   - WorkBuddy 官方客户端 UA 形如 `CLI/2.63.2 CodeBuddy/2.63.2`（本仓库
//     internal/panel/login.go 的 clientUA 常量即此形态）。
//
// 识别不到时**不猜**：返回 "未识别: <UA 首段>"，把原始 UA 的产品首段带进桶里，
// 这样面板上直接能看到「还有谁在打」，后续补映射表有据可依；首段已去掉版本号，
// 桶数量因此有界（不会每个 SDK 小版本裂一个桶）。

// agentHeaderBudget 识别输入的总字节上限（所有参与识别的头值之和，UA 在内）。
// 超过即整体放弃识别——正常客户端不会把 UA 或工具头塞到 8KB 以上，越界的输入
// 不值得归因，也不该让它撑爆用量桶。与上游两处实现的取值一致。
const agentHeaderBudget = 8 * 1024

// unrecognizedPrefix 未识别桶的前缀。带前缀是为了让面板上一眼区分
// 「识别到的客户端名」与「没识别到、只剩 UA 首段的兜底项」。
const unrecognizedPrefix = "未识别: "

// agentHeaderGroups 专用特征头，按组顺序判定：先到先得。
//
// 顺序固定（claude-code → codex → opencode）而不是按字母序，是为了让
// 「一个请求同时带了两个工具的头」这种异常情况有确定结果，不随 map 迭代抖动。
var agentHeaderGroups = []struct {
	client  string
	headers []string
}{
	{client: "claude-code", headers: []string{
		"x-claude-code-session-id",
		"x-claude-code-agent-id",
		"x-claude-code-parent-agent-id",
	}},
	{client: "codex", headers: []string{
		"x-codex-installation-id",
		"x-codex-window-id",
		"x-codex-parent-thread-id",
		"x-codex-turn-metadata",
	}},
	{client: "opencode", headers: []string{
		"x-opencode-project",
		"x-opencode-session",
		"x-opencode-request",
	}},
}

// agentHeaderPrefixGroups 开集特征头：只要出现该前缀的头就算命中。
// `x-claude-code-*` / `x-codex-*` / `x-opencode-*` 是随版本增长的开集（bifrost
// 注释引官方文档），枚举法会漏掉未来新增的头；上面枚举已知项之后，这里再兜一层。
// 放在 agentHeaderGroups 之后判定，保证「同请求带多族头」时结果由枚举表决定。
var agentHeaderPrefixGroups = []struct {
	prefix string
	client string
}{
	{"x-claude-code-", "claude-code"},
	{"x-codex-", "codex"},
	{"x-opencode-", "opencode"},
}

// agentHeaderValueRules 值匹配型特征头：头名 + 值前缀 → 客户端。
// 与「只看头在不在」的 agentHeaderGroups 区分开：这类头是通用头
// （originator / x-client-type / x-title），必须看值才能定性，否则会误伤。
var agentHeaderValueRules = []struct {
	header      string
	valuePrefix string
	client      string
}{
	{"originator", "codex_cli_rs", "codex"},
	{"x-client-type", "cline-sdk", "cline"},
	{"x-title", "kilo code", "kilo-code"},
}

// agentUAPrefixes User-Agent 前缀兜底表，按切片顺序匹配（更长的前缀必须排在前面，
// 否则 `claude-cli/` 会被更短的规则抢走）。
//
// 每个前缀都带版本斜杠（`claude-cli/` 而不是 `claude-cli`），这样 `codexa/1.0`
// 不会撞上 `codex/`、`MyCherryStudio/x` 不会撞上 `cherrystudio/`。
// 通用 SDK / 工具（curl、urllib、openai-python、node-fetch…）刻意不入表——
// 它们不是「编码 Agent 客户端」，落进「未识别」桶才是诚实的。
var agentUAPrefixes = []struct {
	prefix string
	client string
}{
	// Anthropic 系（更具体的排前面）
	{"claude-cli/", "claude-code"},
	{"claude-code/", "claude-code"},
	{"claude-vscode", "claude-code"},
	{"claude-desktop", "claude-desktop"},
	{"claude-cowork", "claude-cowork"},
	// OpenAI 系
	{"codex_cli_rs/", "codex"}, // Codex CLI 真实 UA（openai/codex + CC Switch 实测）
	{"codex-cli/", "codex"},
	{"codex-tui/", "codex"},
	{"codex_cli/", "codex"},
	{"openai-codex/", "codex"},
	{"codex-desktop/", "codex-desktop"},
	{"codex desktop/", "codex-desktop"},
	{"codex/", "codex"}, // 上游表里的旧写法，保留兜底
	// 其它编码 Agent
	{"opencode/", "opencode"},
	{"geminicli/", "gemini-cli"},
	{"gemini-cli/", "gemini-cli"},
	{"qwencode/", "qwen-code"},
	{"qwen-code/", "qwen-code"},
	{"qwen-cli/", "qwen-code"},
	{"charm-crush/", "crush"},
	{"crush/", "crush"},
	{"codewhale/", "codewhale"},
	{"cherrystudio/", "cherry-studio"},
	{"kilo-code/", "kilo-code"},
	{"kilocode/", "kilo-code"},
	{"kilo/", "kilo-code"},
	{"cline/", "cline"},
	{"roocode/", "roo-code"},
	{"roo-code/", "roo-code"},
	{"cursor/", "cursor"},
	{"windsurf/", "windsurf"},
	{"zed/", "zed"},
	{"aider/", "aider"},
	{"continue/", "continue"},
	{"copilot/", "copilot-cli"},
	{"github-copilot/", "copilot-cli"},
	{"goose/", "goose"},
	{"zcode/", "zcode"},
	{"workbuddy/", "workbuddy"},
	{"deepseek-harness/", "deepseek-harness"},
	{"dsh/", "deepseek-harness"},
	{"kimicli/", "kimi-cli"},
	{"kimi-cli/", "kimi-cli"},
	{"kimi-code-cli/", "kimi-cli"},
	{"minimax-code/", "minimax-code"},
	{"factory-cli/", "droid"},
	{"oh-my-pi/", "omp"},
	{"mimo-code/", "mimo-code"},
	{"mimo/", "mimo-code"},
	{"atomcode/", "atomcode"},
	{"openclaw/", "openclaw"},
	{"hermes-agent/", "hermes"},
	{"hermes-cli/", "hermes"},
	{"grok-shell/", "grok"},
	{"grok-pager/", "grok"},
	{"xai-grok-build/", "grok"},
	{"xai-grok-cli/", "grok"},
	{"grok-cli/", "grok"},
	{"muse-build/", "muse"},
	{"muse-code/", "muse"},
	{"alma/", "alma"},
	{"cindy/", "cindy"},
	{"hanaagent/", "hanako"},
	{"pi-coding-agent/", "pi"},
	{"pi/", "pi"},
}

// agentClientLabel 从入站请求头识别调用方客户端，返回写进用量桶的标签。
//
// 命中专用特征头 → 直接用该客户端名；否则按 UA 前缀表兜底；
// 都不命中 → "未识别: <UA 首段>"（UA 为空时 "未识别: (无 UA)"）。
// 识别输入超预算 → "未识别: (头超长)"。
func agentClientLabel(h http.Header) string {
	if agentHeaderBytes(h) > agentHeaderBudget {
		return unrecognizedPrefix + "(头超长)"
	}
	if c := agentClientFromHeaders(h); c != "" {
		return c
	}
	return unrecognizedPrefix + uaToken(h.Get("User-Agent"))
}

// agentClientFromHeaders 专用头优先、值规则次之、UA 兜底；识别不到返回 ""。
func agentClientFromHeaders(h http.Header) string {
	for _, g := range agentHeaderGroups {
		for _, name := range g.headers {
			if singleHeaderPresent(h, name) {
				return g.client
			}
		}
	}
	for _, g := range agentHeaderPrefixGroups {
		if anyHeaderWithPrefix(h, g.prefix) {
			return g.client
		}
	}
	for _, r := range agentHeaderValueRules {
		v := strings.ToLower(strings.TrimSpace(h.Get(r.header)))
		if v != "" && strings.HasPrefix(v, r.valuePrefix) {
			return r.client
		}
	}
	ua := strings.ToLower(strings.TrimSpace(h.Get("User-Agent")))
	if ua == "" {
		return ""
	}
	// WorkBuddy 官方客户端：`CLI/<ver> CodeBuddy/<ver>`。前缀 `cli/` 太通用，
	// 必须同时含 `codebuddy/` 才算，避免把任何自报 CLI 的东西都算成 WorkBuddy。
	if strings.HasPrefix(ua, "cli/") && strings.Contains(ua, "codebuddy/") {
		return "workbuddy"
	}
	for _, m := range agentUAPrefixes {
		if strings.HasPrefix(ua, m.prefix) {
			return m.client
		}
	}
	return ""
}

// singleHeaderPresent 判断 name 是否携带**唯一**一个非空值。
// 重复头（len != 1）不算：两个值之间无法确定哪个是工具签名，宁可归入未识别。
func singleHeaderPresent(h http.Header, name string) bool {
	v := h.Values(name)
	return len(v) == 1 && strings.TrimSpace(v[0]) != ""
}

// anyHeaderWithPrefix 是否存在以 prefix 开头、且只带一个非空值的头。
// 头名比较用小写（net/http 会把头名规范化成 CanonicalMIMEHeaderKey）。
func anyHeaderWithPrefix(h http.Header, prefix string) bool {
	for name, vs := range h {
		if !strings.HasPrefix(strings.ToLower(name), prefix) {
			continue
		}
		if len(vs) == 1 && strings.TrimSpace(vs[0]) != "" {
			return true
		}
	}
	return false
}

// uaToken 取 UA 的「产品首段」作为兜底标签：第一个 '/' 或空格之前的部分，小写。
// 去掉版本号是关键——否则每个 SDK 小版本都会裂成一个桶，长期把用量文件撑大。
func uaToken(ua string) string {
	ua = strings.ToLower(strings.TrimSpace(ua))
	if ua == "" {
		return "(无 UA)"
	}
	if i := strings.IndexAny(ua, "/ "); i > 0 {
		ua = ua[:i]
	}
	if r := []rune(ua); len(r) > 32 {
		ua = string(r[:32])
	}
	return ua
}

// agentHeaderBytes 参与识别的所有头值字节数之和（UA + 特征头 + 前缀头 + 值规则头）。
// 只统计识别真正会读的头，其它头再大也不计——预算是「识别输入」的预算。
// 按头名去重，同一个头不会被枚举表和前缀表重复计入。
func agentHeaderBytes(h http.Header) int {
	total := len(h.Get("User-Agent"))
	for name, vs := range h {
		if !agentHeaderParticipates(strings.ToLower(name)) {
			continue
		}
		for _, v := range vs {
			total += len(v)
		}
	}
	return total
}

// agentHeaderParticipates 判断某个（小写）头名是否参与识别。
func agentHeaderParticipates(ln string) bool {
	for _, g := range agentHeaderGroups {
		for _, n := range g.headers {
			if ln == n {
				return true
			}
		}
	}
	for _, g := range agentHeaderPrefixGroups {
		if strings.HasPrefix(ln, g.prefix) {
			return true
		}
	}
	for _, r := range agentHeaderValueRules {
		if ln == r.header {
			return true
		}
	}
	return false
}
