// logzh.go 运行日志的中文渲染层：把 ring 里的原始行渲染成中文表格行。
//
// 分工：原文（LogEntry.Text）保持不动，中文只作为展示副本（LogEntry.ZhCols）随
// 快照下发；面板在「中文 / 原文」之间切换，鼠标悬停任意一行看原文。stdout 与
// docker logs 完全不受影响，排障时 grep 原文的习惯不变。
//
// 三条渲染路径，与 ring 的频道归类一一对应：
//   - 对话：server/logging.go 的固定表格行（| #seq | 时间 | 模型 | ... |），
//     字段全部结构化，重排成中文列；
//
// 输出是「列」而不是拼好的整行：面板按固定列宽的网格排版（SyanRound 不是等宽
// 字体，空格填充对齐不了），列宽定义在 internal/panel/index.html 的
// #logBox .lng 上，本文件的 zh*Width 是同一套数字的来源。
//   - 任务：scheduler 的 "<task> <标签>: <短语>" 行，前缀映射成中文任务名，
//     短语走 zhPhrases 词表；
//   - 系统：upstream/pool/server 的 WARN/ERR 行，按 zhSysRules 逐条翻译，
//     未命中规则的行交给 zhSysPhrases 做词表替换，再不行返回 ok=false 让
//     调用方回退原文——宁可显示英文，也不要翻译成错的。
package panel

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/syan-anan/wb-syan/internal/logfmt"
)

// 中文表格行的列宽（显示列宽：CJK 记 2 列，与 logfmt.Pad 同口径）。
//
// 取值原则是"刚好容纳该列最长值"。这些数字是面板网格列宽的来源
// （internal/panel/index.html 的 #logBox .lng.c10），改这里要同步改那边。
// 只有 zhAcctWidth / zhModelWidth 在服务端真正参与截断（zhClip），其余是文档。
const (
	zhTypeWidth  = 10 // 对话·流式 / 系统·警告 / 任务·连登奖励
	zhAcctWidth  = 14 // 昵称（uid8 留在原文与悬停里，中文列只放认人的昵称）
	zhModelWidth = 26 // realm 前缀 + 最长模型名
	zhStatWidth  = 2  // ✅ / ❌（具体状态码在悬停的原文里）
	zhTTFBWidth  = 11 // 首字 3.3s
	zhTokWidth   = 12 // 输出 254 字
	zhRateWidth  = 16 // 吞吐 548.6 字/s（解码速率口径：输出 ÷（总耗时 − 首字））
	zhTotalWidth = 9  // 共 5.2s
	zhCrWidth    = 10 // 扣分 0.0000
)

// labelRe 匹配 logfmt.Label 产出的 "昵称(uid8)"。
var labelRe = regexp.MustCompile(`^(.*)\(([0-9A-Za-z]{1,8})\)$`)

// zhAccount 从 "昵称(uid8)" 取昵称；没有昵称时退回 uid8。
func zhAccount(s string) string {
	s = strings.TrimSpace(s)
	if m := labelRe.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[1]) != "" {
		return strings.TrimSpace(m[1])
	}
	return s
}

// zhClip 按显示列宽截断（超出补省略号）。表格列宽固定时用：账号/模型一旦
// 超宽就会把后面所有列推歪，一行对齐比多显示几个字符重要——完整值在悬停的
// 原文和「原文」模式里都在。
func zhClip(s string, width int) string {
	if logfmt.DisplayWidth(s) <= width {
		return s
	}
	out, w := make([]rune, 0, len(s)), 0
	for _, r := range s {
		rw := logfmt.DisplayWidth(string(r))
		if w+rw > width-1 {
			break
		}
		out = append(out, r)
		w += rw
	}
	return string(out) + "…"
}

// zhChatRowRe 解析 server/logging.go logChatRow 的固定行。
// 各字段已被 logfmt.Pad 右补空格，用 \s* 吸收；账号含空格（中文昵称）故非贪婪。
var zhChatRowRe = regexp.MustCompile(
	`^\|\s*#(\d+)\s*\|\s*([0-9:]{8})\s*\|\s*(\S+)\s*\|\s*(\w+)\s*\|\s*(\d{3})\s*\|\s*(.+?)\s*\|\s*` +
		`TTFB=(\S+)\s*\|\s*tok=(\S+)\s*\|\s*(\S+)\s*\|\s*total=([0-9.]+)s\s*\|\s*cr=(\S+)\s*\|$`)

// zhDur 把 logChatRow 的 TTFB 字段（"-" / "1234ms"）转成中文列值。
func zhDur(s string) string {
	if s == "-" || s == "" {
		return "—"
	}
	ms, err := strconv.Atoi(strings.TrimSuffix(s, "ms"))
	if err != nil {
		return s
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// zhTok 输出字数：usage 缺失（"-"）显示破折号，不写 0。
func zhTok(s string) string {
	if s == "-" || s == "" {
		return "—"
	}
	return s + " 字"
}

// zhRate 吞吐列：原文 "48.4tok/s" -> "48.4 字/s"；没有观测（"-"）显示破折号。
// 数值是解码速率（输出 token ÷（总耗时 − 首字），对齐 DeepSeek Harness 与
// Artificial Analysis 的 Output Speed），标签沿用「吞吐」——同行的叫法。
func zhRate(s string) string {
	if s == "-" || s == "" {
		return "—"
	}
	return strings.TrimSuffix(s, "tok/s") + " 字/s"
}

// zhCredit 扣费积分："-" 表示没有成本观测，与"观测到 0"（免费模型）区分开。
func zhCredit(s string) string {
	if s == "-" || s == "" {
		return "—"
	}
	return s
}

// zhStatus 把 HTTP 状态码压成一枚图标：2xx ✅，其余（429 限流 / 4xx 被拒 /
// 5xx 上游失败）一律 ❌。图标只占 2 列，比"成功 200"省下 7 列——腾出来的宽度
// 正好把吞吐列装回来，一行仍然放得下。具体码在悬停的原文与「原文」模式里都在。
func zhStatus(code string) string {
	n, err := strconv.Atoi(code)
	if err != nil {
		return "❓"
	}
	if n >= 200 && n < 300 {
		return "✅"
	}
	return "❌"
}

// zhChatCols 把对话表格行拆成中文列：类型 / 账号 / 模型 / 状态 / 首字 / 输出 /
// 吞吐 / 耗时 / 扣分。不填充空格——面板按固定列宽的网格排版，填充由展示层做
// （SyanRound 不是等宽字体，空格填充在它身上必然错列）。
func zhChatCols(m []string) []string {
	mode := "流式"
	if m[4] == "sync" {
		mode = "同步"
	}
	return []string{
		"对话·" + mode,
		zhClip(zhAccount(m[6]), zhAcctWidth),
		zhClip(m[3], zhModelWidth),
		zhStatus(m[5]),
		"首字 " + zhDur(m[7]),
		"输出 " + zhTok(m[8]),
		"吞吐 " + zhRate(m[9]),
		"共 " + m[10] + "s",
		"扣分 " + zhCredit(m[11]),
	}
}

// zhReason 上游 extError.code → 中文原因。未收录的码返回空串，由调用方
// 退回上游原文 msg——不猜。
var zhReason = map[string]string{
	"model_param_invalid":   "模型参数不合法",
	"invalid_request_error": "请求不合法",
	"rate_limit_exceeded":   "触发上游限流",
	"insufficient_credits":  "积分不足",
	"account_banned":        "账号被封禁",
	"session_expired":       "登录态过期",
}

// zhUpstreamBody 把上游错误 body（JSON）压成一句中文原因。
func zhUpstreamBody(body string) string {
	var o struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Ext  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"extError"`
	}
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		return logfmt.Truncate(body, 100)
	}
	reason := zhReason[o.Ext.Code]
	if reason == "" {
		reason = o.Ext.Message
	}
	if reason == "" {
		reason = o.Msg
	}
	if reason == "" {
		reason = "上游未给原因"
	}
	if o.Ext.Code != "" {
		return fmt.Sprintf("%s · %d %s", reason, o.Code, o.Ext.Code)
	}
	return fmt.Sprintf("%s · %d", reason, o.Code)
}

// zhErrText 常见 Go/HTTP 错误原文 → 中文；未收录原样保留。
func zhErrText(s string) string {
	s = strings.TrimSpace(s)
	switch s {
	case "context canceled":
		return "请求被取消"
	case "context deadline exceeded":
		return "请求超时"
	case "EOF":
		return "上游连接被断开"
	}
	return s
}

// zhSysRule 一条系统行翻译规则：正则捕获组 → 中文渲染。
type zhSysRule struct {
	re *regexp.Regexp
	fn func(m []string) string
}

// zhSysRules 系统行规则表（按序匹配，命中即停）。
var zhSysRules = []zhSysRule{
	// 上游 4xx/5xx 拒绝：body 里带 extError.code，翻成一句原因。
	{regexp.MustCompile(`^\[upstream\] chat_stream acct=(\S+): upstream (\d+) client body=(.*)$`),
		func(m []string) string {
			return "上游拒绝 · 账号 " + zhAccount(m[1]) + " · 上游返回 " + m[2] + "（" + zhUpstreamBody(m[3]) + "）"
		}},
	// 传输层错误（连接被取消 / 超时 / 连接重置）。
	{regexp.MustCompile(`^\[upstream\] chat_stream acct=(\S+): transport error: Post "([^"]*)": (.*)$`),
		func(m []string) string {
			return "上游中断 · 账号 " + zhAccount(m[1]) + " · " + zhErrText(m[3])
		}},
	{regexp.MustCompile(`^\[upstream\] chat_stream acct=(\S+): read body: (.*)$`),
		func(m []string) string {
			return "上游中断 · 账号 " + zhAccount(m[1]) + " · 读取响应失败：" + zhErrText(m[2])
		}},
	// 思考档位被上游降级：面板请求的 max 没被接受，静默变 high。
	{regexp.MustCompile(`^reasoning_effort downgraded model=(\S+) (\S+) -> (\S+)$`),
		func(m []string) string {
			return "思考档位降级 · 模型 " + m[1] + " · " + m[2] + " → " + m[3]
		}},
	// 上游 200 但零帧：客户端会看到空回复，必须显眼。
	{regexp.MustCompile(`^\[server\] stream acct=(\S+) model=(\S+): empty upstream stream \(200\+0 frames\)$`),
		func(m []string) string {
			return "空响应 · 账号 " + zhAccount(m[1]) + " · 模型 " + m[2] + " · 上游 200 但 0 帧"
		}},
	// 账号轮换被请求取消打断（客户端主动断开时常见，不是故障）。
	{regexp.MustCompile(`^\[server\] rotate backoff aborted: ctx cancelled$`),
		func(m []string) string { return "轮换退避中止 · 客户端已断开" }},
	// 账号池回退：套餐用尽/到期时改用最早到期的号。
	{regexp.MustCompile(`^\[pool\] fallback_earliest_expiry acct=(\S+) until=(\S+) kind=(\S+)$`),
		func(m []string) string {
			kind := "软性回退"
			if m[3] == "hard" {
				kind = "强制回退"
			}
			return "账号池回退 · " + zhAccount(m[1]) + " · 改用最早到期套餐（截止 " + zhShortTime(m[2]) + " · " + kind + "）"
		}},
	// WAF 级别的 IP 封锁：多个账号同时被 403，暂停轮换。
	{regexp.MustCompile(`^\[server\] waf ip-level block: (\d+) accounts hit waf 403 within (\S+), rotate fail-fast until (\S+)$`),
		func(m []string) string {
			return "WAF 封锁 · " + m[1] + " 个账号被上游 403 · 暂停轮换到 " + zhShortTime(m[3])
		}},
	// 凭据保存失败：本次请求用的号可能因此掉线。
	{regexp.MustCompile(`^\[server\] chat refresh acct=(\S+): save auth failed: (.*)$`),
		func(m []string) string {
			return "凭据保存失败 · 账号 " + zhAccount(m[1]) + " · " + m[2]
		}},
	// 免费额度结束，开始按量计费。
	{regexp.MustCompile(`^\[pool\] model (\S+) on uid (\S+): free tier ended, now ([\d.]+) credits/1k$`),
		func(m []string) string {
			return "免费额度结束 · 模型 " + m[1] + " · 账号 " + m[2] + " · 现在 " + m[3] + " 积分/千字"
		}},
	// 成本探测（每个模型挑一个号试价）。
	{regexp.MustCompile(`^\[pool\] cost explore model=(\S+) realm="?(\w+)"? acct=(\S+) window=(\S+)$`),
		func(m []string) string {
			return "成本探测 · 模型 " + m[1] + " · 域 " + zhRealm(m[2]) + " · 账号 " + zhAccount(m[3]) + " · 窗口 " + m[4]
		}},
}

// zhSysPhrases 未命中规则时的兜底词表（逐条替换）。
var zhSysPhrases = []struct {
	re  *regexp.Regexp
	rep string
}{
	{regexp.MustCompile(`^\[upstream\] fetch models: (.*)$`), "获取模型列表 · $1"},
	{regexp.MustCompile(`^\[upstream\] global models: (.*)$`), "国际模型列表 · $1"},
	{regexp.MustCompile(`^\[upstream\] model\.json (.*)$`), "模型缓存 · $1"},
	{regexp.MustCompile(`^\[upstream\] models\.dev fetch failed \(silent fallback to 1M\): (.*)$`), "模型元数据获取失败 · 静默回退到 1M 上下文（$1）"},
	{regexp.MustCompile(`^\[upstream\] models\.dev fetch status (\d+) \(silent fallback to 1M\)$`), "模型元数据获取失败 · 上游返回 $1 · 静默回退到 1M 上下文"},
	{regexp.MustCompile(`^\[upstream\] models\.dev (.*)$`), "模型元数据 · $1"},
	{regexp.MustCompile(`^\[pool\] (.*)$`), "账号池 · $1"},
	{regexp.MustCompile(`^\[server\] (.*)$`), "服务 · $1"},
	{regexp.MustCompile(`^\[auth\] (.*)$`), "登录 · $1"},
	{regexp.MustCompile(`^\[upstream\] (.*)$`), "上游 · $1"},
}

// zhRealm 域标识 → 中文。
func zhRealm(r string) string {
	switch r {
	case "cn":
		return "国内"
	case "global":
		return "国际"
	}
	return r
}

// zhShortTime 把 RFC3339 时间压成 "今天 16:14" / "10-04 16:14"。
// 只做本地时区显示；解析失败时原样返回（宁可难看也别错）。
func zhShortTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	t = t.Local()
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return "今天 " + t.Format("15:04")
	}
	if t.Year() == now.Year() && t.YearDay() == now.YearDay()+1 {
		return "明天 " + t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// zhTaskPrefixes 调度任务行前缀 → 中文任务名（与 ring.go taskPrefixes 同源）。
var zhTaskPrefixes = []struct{ pre, name string }{
	{"school ", "学堂"}, {"streak-bonus ", "连登"}, {"travel ", "旅行"},
	{"blackcat ", "黑猫"}, {"lottery ", "抽奖"}, {"checkin ", "签到"},
	{"activity ", "活动"}, {"keepalive ", "保活"}, {"balance ", "余额"},
	{"user-resource ", "资源"},
}

// zhPhrases 任务行短语词表（作用于 "<标签>: " 之后的尾部）。
var zhPhrases = []struct {
	re  *regexp.Regexp
	rep string
}{
	{regexp.MustCompile(`^skip \(daily limit reached\)$`), "跳过（今日次数已用完）"},
	{regexp.MustCompile(`^skip \(traveling record=(\d+)\)$`), "跳过（旅行进行中，记录 $1）"},
	{regexp.MustCompile(`^skip \(unknown state "([^"]*)"\)$`), "跳过（未知状态 $1）"},
	{regexp.MustCompile(`^depart ok location=(\d+)$`), "出发成功（地点 $1）"},
	{regexp.MustCompile(`^claim ok record=(\d+) reward=(\d+)$`), "领取成功（记录 $1，奖励 $2 积分）"},
	{regexp.MustCompile(`^claim skipped \(arrived but no record_id\)$`), "跳过领取（已到达但没有记录 ID）"},
	{regexp.MustCompile(`^claim record=(\d+): (.*)$`), "领取记录 $1 失败：$2"},
	{regexp.MustCompile(`^adopt ok \(\+(\d+) credits\)$`), "领养成功（+$1 积分）"},
	{regexp.MustCompile(`^adopt skipped \(conversation threshold not reached, retry tomorrow\)$`), "跳过领养（对话数未达标，明天再试）"},
	{regexp.MustCompile(`^adopt preflight report: (.*)$`), "领养预检：$1"},
	{regexp.MustCompile(`^adopt: (.*)$`), "领养失败：$1"},
	{regexp.MustCompile(`^agreement: (.*)$`), "协议：$1"},
	{regexp.MustCompile(`^depart: (.*)$`), "出发失败：$1"},
	{regexp.MustCompile(`^buddy-info: (.*)$`), "伙伴信息：$1"},
	{regexp.MustCompile(`^status: (.*)$`), "状态：$1"},
	{regexp.MustCompile(`^save: (.*)$`), "保存失败：$1"},
	{regexp.MustCompile(`^streak check failed \(report OK\): (.*)$`), "连登校验失败（上报成功）：$1"},
	{regexp.MustCompile(`^report OK but streak\.days=0 \(silent drop\?\)$`), "上报成功但连登天数为 0（疑似静默丢弃）"},
	{regexp.MustCompile(`^streak days=(\d+)$`), "连登 $1 天"},
	{regexp.MustCompile(`^lottery summary: (.*)$`), "抽奖汇总：$1"},
	{regexp.MustCompile(`^draw: (.*)$`), "抽奖失败：$1"},
	{regexp.MustCompile(`^redeem (\S+): (.*)$`), "兑换 $1 失败：$2"},
	{regexp.MustCompile(`^(\d+)/(\d+) 完成，中断: (.*)$`), "$1/$2 完成，中断：$3"},
	{regexp.MustCompile(`🎊 新手礼包 \+(\d+)c$`), "🎊 新手礼包 +$1 积分"},
	{regexp.MustCompile(`🎊 补偿领取 \+(\d+)c$`), "🎊 补偿领取 +$1 积分"},
}

// zhPhrase 应用短语词表；未命中返回原样（不猜意思）。
func zhPhrase(tail string) string {
	for _, p := range zhPhrases {
		if p.re.MatchString(tail) {
			return p.re.ReplaceAllString(tail, p.rep)
		}
	}
	return tail
}

// zhTaskCols 拆调度任务行："travel 昵称(uid8): skip (daily limit reached)"
// → ["任务·旅行", "昵称", "跳过（今日次数已用完）"]。三段结构（类型 / 账号 /
// 内容）与系统行共用同一套网格，账号列为空时内容列仍落在同一列。
func zhTaskCols(raw string) ([]string, bool) {
	for _, p := range zhTaskPrefixes {
		if !strings.HasPrefix(raw, p.pre) {
			continue
		}
		rest := strings.TrimPrefix(raw, p.pre)
		label, tail, ok := strings.Cut(rest, ": ")
		if !ok {
			return []string{"任务·" + p.name, "", rest}, true
		}
		return []string{"任务·" + p.name, zhClip(zhAccount(label), zhAcctWidth), zhPhrase(tail)}, true
	}
	// 面板自身的任务动作行（panel: 队列启动：6 项（并发 2））已经是中文，只补类型列。
	if strings.HasPrefix(raw, "panel: ") {
		return []string{"任务·面板", "", strings.TrimPrefix(raw, "panel: ")}, true
	}
	return nil, false
}

// zhLevel 取行首级别（WARN:/ERR:/无）。
func zhLevel(raw string) (level, rest string) {
	switch {
	case strings.HasPrefix(raw, "WARN: "):
		return "警告", strings.TrimPrefix(raw, "WARN: ")
	case strings.HasPrefix(raw, "ERR: "):
		return "错误", strings.TrimPrefix(raw, "ERR: ")
	}
	return "提示", raw
}

// zhSysCols 拆系统行（账号列恒为空，内容列与任务行对齐）；ok=false 表示没有
// 规则命中，调用方回退原文。
func zhSysCols(raw string) ([]string, bool) {
	level, rest := zhLevel(raw)
	for _, r := range zhSysRules {
		if m := r.re.FindStringSubmatch(rest); m != nil {
			return []string{"系统·" + level, "", r.fn(m)}, true
		}
	}
	for _, p := range zhSysPhrases {
		if p.re.MatchString(rest) {
			return []string{"系统·" + level, "", p.re.ReplaceAllString(rest, p.rep)}, true
		}
	}
	return nil, false
}

// RenderZhCols 把一条原始日志行拆成中文列；ok=false 时调用方回退原文。
//
// 返回列而不是拼好的整行，是因为面板要按固定列宽的网格排版：SyanRound 不是
// 等宽字体（"i" 4.4px、空格 10.55px、"7" 9.58px），用空格填充对齐必然错列。
// 列的语义按行类型定：
//   - 对话 9 列：类型 / 账号 / 模型 / 状态 / 首字 / 输出 / 吞吐 / 耗时 / 扣分
//   - 任务 3 列：类型 / 账号 / 内容
//   - 系统 3 列：类型 / 空 / 内容
func RenderZhCols(raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	if m := zhChatRowRe.FindStringSubmatch(raw); m != nil {
		return zhChatCols(m), true
	}
	if cols, ok := zhTaskCols(raw); ok {
		return cols, true
	}
	if cols, ok := zhSysCols(raw); ok {
		return cols, true
	}
	return nil, false
}
