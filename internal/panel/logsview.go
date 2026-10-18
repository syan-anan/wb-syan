// logsview.go 运行日志视图：结果统计 + 服务端筛选 + 逐行明细 + 脱敏导出。
//
// 为什么分类放服务端：环里既有对话表格行（每请求一行、自带状态码），也有
// 任务/系统行（自由文本）。前端只拿到整行文本，靠它猜结果既不稳定也没法统计
// 全环——「这批里失败几行」要先把 500 行全下载再数。判定集中在这里，页面上的
// 数字与导出文件用同一份规则，不会打架。
//
// 兼容性：/panel/api/logs 的响应仍是 {entries:[{ts,ch,text,zhCols}]}，新字段只加
// 不改；筛选参数全部可选，不带参数时等于旧快照。
package panel

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 日志结果档位（与面板筛选 chips 一一对应）。
const (
	LogOK   = "ok"   // 成功：对话行 2xx
	LogWarn = "warn" // 需留意：429 / 冷却 / 降级 / WARN 前缀
	LogErr  = "err"  // 失败：非 2xx 对话行、上游错误行、ERROR 前缀
	LogInfo = "info" // 其它：常规流水
)

// logSeverity 档位映射成可比较的严重度（一行有多个信号时取最严重的）。
func logSeverity(res string) int {
	switch res {
	case LogErr:
		return 3
	case LogWarn:
		return 2
	case LogOK:
		return 1
	}
	return 0
}

// logWorst 返回两个档位中更严重的那个。
func logWorst(a, b string) string {
	if logSeverity(b) > logSeverity(a) {
		return b
	}
	return a
}

// logErrPhrases / logWarnPhrases：没命中表格行也没命中错误诊断行时的文本兜底。
// 只收「出现即基本可判定」的词——宁可少判，误判会把排障视线从真问题上带走。
// 计数为 0 的统计（"成功 12 失败 0"）由 logCountZero 排除，不算失败。
var (
	logErrPhrases  = []string{"失败", "错误", "拒绝", "中断", "不可用", "超时", "异常", "❌"}
	logWarnPhrases = []string{"限流", "冷却", "熔断", "降权", "降级", "警告", "被封", "禁用", "⚠"}
)

var (
	logZeroBeforeRe = regexp.MustCompile(`0(?:\.0+)?\s*$`)
	logZeroAfterRe  = regexp.MustCompile(`^\s*(?:率|数|量)?\s*0(?:\.0+)?(?:%|\s|$)`)
)

// logCountZero 短语出现在 text[i:j] 时，判断它是否只是「计数为 0」的统计串。
func logCountZero(text string, i, j int) bool {
	if logZeroBeforeRe.MatchString(text[:i]) {
		return true
	}
	return logZeroAfterRe.MatchString(text[j:])
}

// logHasPhrase 行内是否出现任一短语（跳过 0 值计数）。
func logHasPhrase(text string, phrases []string) bool {
	for _, ph := range phrases {
		from := 0
		for {
			i := strings.Index(text[from:], ph)
			if i < 0 {
				break
			}
			i += from
			if !logCountZero(text, i, i+len(ph)) {
				return true
			}
			from = i + len(ph)
		}
	}
	return false
}

// logRowInfo 一条日志的分类结果。
type logRowInfo struct {
	Res     string
	Status  int
	Account string
	Model   string
}

// classifyLogRow 给一条日志定档。优先级：对话表格行 > 行首级别 > 上游错误诊断 >
// 文本短语。表格行看状态码（每请求一行的权威口径）；诊断行按上游 HTTP 状态，
// 其中 429 记「警告」而不是「失败」——限流是容量信号，不是这一行的错。
func classifyLogRow(e LogEntry) logRowInfo {
	if m := zhChatRowRe.FindStringSubmatch(e.Text); m != nil {
		code, _ := strconv.Atoi(m[5])
		res := LogErr
		if code >= 200 && code < 300 {
			res = LogOK
		} else if code == http.StatusTooManyRequests {
			res = LogWarn // 与诊断行同口径：限流记警告，不是失败
		}
		return logRowInfo{Res: res, Status: code, Account: zhAccount(m[6]), Model: m[3]}
	}
	info := logRowInfo{Res: LogInfo}
	if lv := diagLevelRe.FindStringSubmatch(e.Text); lv != nil {
		switch lv[1] {
		case "ERROR":
			info.Res = logWorst(info.Res, LogErr)
		case "WARN":
			info.Res = logWorst(info.Res, LogWarn)
		}
	}
	if d, ok := parseDiagnose(e); ok {
		info.Status = d.Status
		if d.Account != "" {
			info.Account = zhAccount(d.Account)
		}
		if d.Model != "" && info.Model == "" {
			info.Model = d.Model
		}
		res := LogErr
		if d.Status == http.StatusTooManyRequests {
			res = LogWarn
		}
		info.Res = logWorst(info.Res, res)
	}
	if info.Res == LogInfo {
		if logHasPhrase(e.Text, logErrPhrases) {
			info.Res = LogErr
		} else if logHasPhrase(e.Text, logWarnPhrases) {
			info.Res = LogWarn
		}
	}
	return info
}

// logKV 明细面板里的一行键值。
type logKV struct {
	K string `json:"k"`
	V string `json:"v"`
}

// logDetailLimit 明细中长字段（原文 / 上游 body）的截断宽度（按字符计）。
const logDetailLimit = 600

// logClipText 按字符截断（中文按字计，不按字节切，避免截出半个字）。
func logClipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// logChannelName / logResultName 内部枚举值转中文（导出与明细共用）。
func logChannelName(ch string) string {
	switch ch {
	case ChChat:
		return "对话"
	case ChTask:
		return "任务"
	case ChSys:
		return "系统"
	}
	return ch
}

func logResultName(res string) string {
	switch res {
	case LogOK:
		return "成功"
	case LogWarn:
		return "警告"
	case LogErr:
		return "失败"
	}
	return "其它"
}

// logDetail 一行的可展开明细。对话行摊平 11 个字段（不让前端再解析原文）；
// 其它行给三层诊断的三段（本网关判断 / 客户端解释 / 上游原文），与 API 响应同
// 口径；两者都不命中时退回整行原文。
func logDetail(e LogEntry, info logRowInfo) []logKV {
	out := make([]logKV, 0, 14)
	add := func(k, v string) {
		if strings.TrimSpace(v) == "" {
			return
		}
		out = append(out, logKV{K: k, V: v})
	}
	add("频道", logChannelName(e.Ch))
	add("结果", logResultName(info.Res))
	if m := zhChatRowRe.FindStringSubmatch(e.Text); m != nil {
		mode := "流式"
		if m[4] == "sync" {
			mode = "同步"
		}
		add("序号", "#"+m[1])
		add("请求时间", m[2])
		add("模型", m[3])
		add("方式", mode)
		add("状态", m[5])
		add("账号", zhAccount(m[6]))
		add("首字", zhDur(m[7]))
		add("输出", zhTok(m[8]))
		add("吞吐", zhRate(m[9]))
		add("总耗时", m[10]+"s")
		add("扣分", zhCredit(m[11]))
		return out
	}
	if d, ok := parseDiagnose(e); ok {
		add("HTTP 状态", strconv.Itoa(d.Status))
		add("账号", zhAccount(d.Account))
		add("模型", d.Model)
		add("上游错误类", d.Kind)
		add("本网关判断", d.GatewayHint)
		add("客户端解释", d.ClientMessage)
		add("客户端详情", d.ClientDetail)
		if d.UpstreamCode != 0 {
			add("上游业务码", strconv.Itoa(d.UpstreamCode))
		}
		add("上游原文", logClipText(d.UpstreamMsg, logDetailLimit))
		return out
	}
	add("内容", logClipText(e.Text, logDetailLimit))
	return out
}

// logViewRow 下发给面板的一行。旧字段名（ts/ch/text/zhCols）保持不变，
// res/status/account/model/detail 是新增项。
type logViewRow struct {
	TS      time.Time `json:"ts"`
	Ch      string    `json:"ch"`
	Text    string    `json:"text"`
	ZhCols  []string  `json:"zhCols,omitempty"`
	Res     string    `json:"res"`
	Status  int       `json:"status,omitempty"`
	Account string    `json:"account,omitempty"`
	Model   string    `json:"model,omitempty"`
	Detail  []logKV   `json:"detail,omitempty"`
}

// logStats 全环统计，不受筛选影响：页脚要同时说「环里一共多少」和「筛出多少」，
// 两者一样时用户没法判断是筛没了还是真没日志。
type logStats struct {
	Total     int            `json:"total"`
	OK        int            `json:"ok"`
	Warn      int            `json:"warn"`
	Err       int            `json:"err"`
	Info      int            `json:"info"`
	ByChannel map[string]int `json:"byChannel"`
	First     string         `json:"first,omitempty"`
	Last      string         `json:"last,omitempty"`
}

// logFilter 面板筛选条件（全部来自 query，缺省即不过滤）。
type logFilter struct {
	Channel string `json:"ch"`
	Res     string `json:"res"`
	Account string `json:"account,omitempty"`
	Model   string `json:"model,omitempty"`
	Query   string `json:"q,omitempty"`
	Limit   int    `json:"limit"`
}

// defaultLogLimit 不带 limit 时的行数上限（= 环容量，等于旧行为）。
const defaultLogLimit = 500

// maxLogLimit 服务端硬上限（环容量当前是 500，留余量给以后调大容量）。
const maxLogLimit = 5000

// parseLogFilter 从 query 解析筛选条件；非法值退回默认，不报错。
func parseLogFilter(r *http.Request) logFilter {
	q := r.URL.Query()
	f := logFilter{
		Channel: strings.TrimSpace(q.Get("ch")),
		Res:     strings.TrimSpace(q.Get("result")),
		Account: strings.ToLower(strings.TrimSpace(q.Get("account"))),
		Model:   strings.ToLower(strings.TrimSpace(q.Get("model"))),
		Query:   strings.ToLower(strings.TrimSpace(q.Get("q"))),
		Limit:   defaultLogLimit,
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			f.Limit = n
		}
	}
	if f.Limit > maxLogLimit {
		f.Limit = maxLogLimit
	}
	return f
}

// match 已给的条件取 AND；空串与 "all" 视为不过滤。
func (f logFilter) match(e LogEntry, info logRowInfo) bool {
	if f.Channel != "" && f.Channel != "all" && e.Ch != f.Channel {
		return false
	}
	if f.Res != "" && f.Res != "all" && info.Res != f.Res {
		return false
	}
	if f.Account != "" && !strings.Contains(strings.ToLower(info.Account), f.Account) {
		return false
	}
	if f.Model != "" && !strings.Contains(strings.ToLower(info.Model), f.Model) {
		return false
	}
	if f.Query != "" && !strings.Contains(strings.ToLower(e.Text), f.Query) &&
		!strings.Contains(strings.ToLower(strings.Join(e.ZhCols, " ")), f.Query) {
		return false
	}
	return true
}

// buildLogView 统计走全环，筛选后按 limit 截尾（保留最新）。
func buildLogView(entries []LogEntry, f logFilter) (rows []logViewRow, stats logStats, truncated bool) {
	stats.ByChannel = map[string]int{}
	infos := make([]logRowInfo, len(entries))
	for i, e := range entries {
		info := classifyLogRow(e)
		infos[i] = info
		stats.Total++
		stats.ByChannel[e.Ch]++
		switch info.Res {
		case LogOK:
			stats.OK++
		case LogWarn:
			stats.Warn++
		case LogErr:
			stats.Err++
		default:
			stats.Info++
		}
	}
	if len(entries) > 0 {
		stats.First = entries[0].TS.Format("2006-01-02 15:04:05")
		stats.Last = entries[len(entries)-1].TS.Format("2006-01-02 15:04:05")
	}
	matched := make([]int, 0, len(entries))
	for i, e := range entries {
		if f.match(e, infos[i]) {
			matched = append(matched, i)
		}
	}
	if f.Limit > 0 && len(matched) > f.Limit {
		matched = matched[len(matched)-f.Limit:]
		truncated = true
	}
	rows = make([]logViewRow, 0, len(matched))
	for _, i := range matched {
		e, info := entries[i], infos[i]
		rows = append(rows, logViewRow{
			TS: e.TS, Ch: e.Ch, Text: e.Text, ZhCols: e.ZhCols,
			Res: info.Res, Status: info.Status, Account: info.Account, Model: info.Model,
			Detail: logDetail(e, info),
		})
	}
	return rows, stats, truncated
}

// ---------------------------------------------------------------------------
// HTTP 处理
// ---------------------------------------------------------------------------

// logsHandler 日志视图：全环统计 + 服务端筛选 + 逐行明细（时间升序，最新在末）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	f := parseLogFilter(r)
	rows, stats, truncated := buildLogView(p.logs.Snapshot(), f)
	writeJSON(w, http.StatusOK, map[string]any{
		"entries":   rows,
		"stats":     stats,
		"filter":    f,
		"filtered":  len(rows),
		"truncated": truncated,
	})
}

// logsExportHandler 按当前筛选导出日志。format=json 给机器读，默认纯文本给人读。
// 不带 limit 时导出全部命中行（上限 maxLogLimit），不受页面默认 500 行约束。
// 导出前统一脱敏：页面上看的是原文，落到文件里的是可以发给别人看的版本。
func (p *Panel) logsExportHandler(w http.ResponseWriter, r *http.Request) {
	f := parseLogFilter(r)
	if r.URL.Query().Get("limit") == "" {
		f.Limit = maxLogLimit
	}
	rows, stats, truncated := buildLogView(p.logs.Snapshot(), f)
	if r.URL.Query().Get("format") == "json" {
		clean := make([]logViewRow, len(rows))
		for i, rw := range rows {
			clean[i] = rw
			clean[i].Text = logSanitize(rw.Text)
			if len(rw.Detail) > 0 {
				clean[i].Detail = make([]logKV, len(rw.Detail))
				for j, kv := range rw.Detail {
					kv.V = logSanitize(kv.V)
					clean[i].Detail[j] = kv
				}
			}
		}
		w.Header().Set("Content-Disposition", "attachment; filename=\"wb-syan-logs.json\"")
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{
			"version":    p.cfg.Version,
			"exportedAt": time.Now().Format("2006-01-02 15:04:05"),
			"stats":      stats,
			"filter":     f,
			"entries":    clean,
			"truncated":  truncated,
		})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"wb-syan-logs.txt\"")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(buildLogExport(rows, stats, f, truncated, p.cfg.Version)))
}

// logFilterText 筛选条件的可读一行：导出文件里要能看出这份日志是按什么条件
// 捞出来的，否则「只有 12 行」会被当成「只有 12 行日志」。
func logFilterText(f logFilter) string {
	pick := func(s, def string) string {
		if s == "" {
			return def
		}
		return s
	}
	return "频道=" + pick(logChannelName(f.Channel), "全部") +
		" 结果=" + pick(logResultName(f.Res), "全部") +
		" 账号=" + pick(f.Account, "-") +
		" 模型=" + pick(f.Model, "-") +
		" 关键词=" + pick(f.Query, "-") +
		" 行数上限=" + strconv.Itoa(f.Limit)
}

// buildLogExport 导出正文：注释头（版本 + 窗口 + 全环统计 + 筛选条件 + 脱敏声明）
// 再接逐行 `[时间] [频道] [结果] 原文`。行格式刻意保持单行，方便 grep。
func buildLogExport(rows []logViewRow, stats logStats, f logFilter, truncated bool, version string) string {
	var b strings.Builder
	b.WriteString("# wb-syan 运行日志导出\n")
	b.WriteString("# 版本: " + version + "\n")
	b.WriteString("# 导出时间: " + time.Now().Format("2006-01-02 15:04:05") + "\n")
	if stats.Total > 0 {
		b.WriteString("# 日志窗口: " + stats.First + " ~ " + stats.Last +
			"（环内 " + strconv.Itoa(stats.Total) + " 行）\n")
	} else {
		b.WriteString("# 日志窗口: 环内无日志\n")
	}
	b.WriteString("# 全环统计: 成功 " + strconv.Itoa(stats.OK) +
		" 警告 " + strconv.Itoa(stats.Warn) +
		" 失败 " + strconv.Itoa(stats.Err) +
		" 其它 " + strconv.Itoa(stats.Info) + "\n")
	b.WriteString("# 筛选条件: " + logFilterText(f) + "\n")
	hint := "# 说明: 已脱敏（Bearer / sk- 密钥 / token / 密码 / URL 凭据 / 长十六进制串）；行格式 [时间] [频道] [结果] 原文"
	if truncated {
		hint += "（命中行数超过上限，仅导出最近 " + strconv.Itoa(len(rows)) + " 行）"
	}
	b.WriteString(hint + "\n")
	b.WriteString("\n")
	for _, rw := range rows {
		b.WriteString("[" + rw.TS.Format("2006-01-02 15:04:05") + "] [")
		b.WriteString(logChannelName(rw.Ch) + "] [")
		b.WriteString(logResultName(rw.Res) + "] ")
		b.WriteString(logSanitize(rw.Text) + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// 导出脱敏
// ---------------------------------------------------------------------------

// logMasked 掩码填充。8 个星号：一眼看出是被抹掉的，而不是原值长这样。
const logMasked = "********"

var (
	// logSanAuthHeaderRe 请求头式凭据：值前可带 bearer/basic/token 方案词（一并跳过再抹）。
	logSanAuthHeaderRe = regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|x-api-key|api-key)([[:space:]]*[:=][[:space:]]*)((?:(?:bearer|basic|token)[[:space:]]+)?)([^\s,;\x27\x22]+)`)
	// logSanBearerRe 裸 "Bearer xxx"（日志里常只带方案词，不带键名）。
	logSanBearerRe = regexp.MustCompile(`(?i)(\bbearer[[:space:]]+)([A-Za-z0-9._~+\/=:-]{6,})`)
	// logSanJSONRe JSON 里的凭据字段：组 1 含键名与值的开引号，组 3 是闭引号，
	// 替换时两边原样带回，只抹中间的值——键名留着，排障才知道是哪个字段被抹了。
	logSanJSONRe = regexp.MustCompile(`(?i)("(?:access_token|refresh_token|id_token|api_key|apikey|secret|password|cookie|token)"[[:space:]]*:[[:space:]]*")([^"]*)(")`)
	// logSanURLRe URL userinfo（scheme://user:pass@host 里的 pass）。
	logSanURLRe = regexp.MustCompile(`(?i)(://[^/?#\s:]+:)([^/?#\s@]+)(@)`)
	// logSanKeyRe 网关/上游密钥形态（sk- 开头 + 至少 8 位）。
	logSanKeyRe = regexp.MustCompile(`sk-[A-Za-z0-9_\-]{8,}`)
	// logSanHexRe 24 位及以上的十六进制串（主密钥/UID/设备指纹都长这样）。
	logSanHexRe = regexp.MustCompile(`[0-9a-fA-F]{24,}`)
)

// logReplaceGroups 用子组重写每个匹配（RE2 没有后向引用，替换函数里手拼）。
func logReplaceGroups(re *regexp.Regexp, s string, keep func(m []string) string) string {
	return re.ReplaceAllStringFunc(s, func(full string) string {
		m := re.FindStringSubmatch(full)
		if m == nil {
			return full
		}
		return keep(m)
	})
}

// logSanitize 抹掉日志行里的凭据。只替换「值」，不删行、不改结构——排障要看的
// 正是失败的那一行。顺序：先带键名的（头 / JSON / URL），再裸形态的（sk- 串、
// 长十六进制串），避免先把值打碎导致带键名的规则匹配不上。
func logSanitize(s string) string {
	s = logReplaceGroups(logSanAuthHeaderRe, s, func(m []string) string { return m[1] + m[2] + m[3] + logMasked })
	s = logReplaceGroups(logSanBearerRe, s, func(m []string) string { return m[1] + logMasked })
	s = logReplaceGroups(logSanJSONRe, s, func(m []string) string { return m[1] + logMasked + m[3] })
	s = logReplaceGroups(logSanURLRe, s, func(m []string) string { return m[1] + logMasked + m[3] })
	s = logSanKeyRe.ReplaceAllString(s, logMasked)
	s = logSanHexRe.ReplaceAllString(s, logMasked)
	return s
}
