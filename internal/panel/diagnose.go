// diagnose.go 面板「错误诊断」视图：把日志环里的上游错误行还原成**三层诊断**。
//
// 三层与 API 响应同源，不另造一套口径：
//  1. 本网关判断   —— GatewayHint(kind, 原文)，与 error.gateway_hint 一致；
//  2. 客户端解释   —— ClientMessage(业务码)，与 error.client_message 一致；
//  3. 上游实际证据 —— 日志里的 upstream <status> + body 原文，与 error.message 同源。
//
// 为什么从日志行解析、而不是另建一路事件缓冲：日志已经是错误的唯一落地点
// （成功请求不写这类行），再开缓冲会出现「日志有、诊断没有」的窗口；解析失败
// 就跳过，不影响日志视图本身。解析只用宽松正则 + 码表，不做完整语法分析——
// 它是排障辅助，不是权威解析器。
package panel

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/syan-anan/wb-syan/internal/upstream"
)

var (
	// diagUpstreamRe 上游错误行：`... upstream 400 client body={...}`。
	// 第二段是 Classify 的 kindSlug（client/server/rate/…），仅作展示线索。
	diagUpstreamRe = regexp.MustCompile(`upstream (\d{3}) ([a-z_]+) body=`)
	diagAcctRe     = regexp.MustCompile(`acct=([^ ()]+)`)
	diagModelRe    = regexp.MustCompile(`model=([^ ,)]+)`)
	diagLevelRe    = regexp.MustCompile(`^(WARN|ERROR|INFO):`)
)

// DiagnoseEntry 一条上游错误的三层诊断。
type DiagnoseEntry struct {
	TS      time.Time `json:"ts"`
	Channel string    `json:"ch"`
	Level   string    `json:"level,omitempty"`
	Account string    `json:"account,omitempty"`
	Model   string    `json:"model,omitempty"`
	// 第三层（前半）：上游 HTTP 状态与 body 原文。
	Status      int    `json:"status"`
	UpstreamMsg string `json:"upstream_msg"`
	// 第一层：本网关判断（kind + 英文 hint）。
	Kind        string `json:"kind,omitempty"`
	GatewayHint string `json:"gateway_hint,omitempty"`
	// 第二层：客户端解释（业务码 + 中文文案 + 官方说明）。
	UpstreamCode  int    `json:"upstream_code,omitempty"`
	ClientMessage string `json:"client_message,omitempty"`
	ClientDetail  string `json:"client_detail,omitempty"`
	// Raw 原始日志行（三层都判不出时的兜底，也是「看证据」的入口）。
	Raw string `json:"raw"`
}

// parseDiagnose 从一条日志行还原三层诊断；不是上游错误行 → ok=false。
//
// 业务码与文案从 body 里解（body 可能是嵌套 JSON 字符串，ExtractCode 已容错）；
// gateway_hint 用**空请求上下文**重算——日志里没有「是否带图 / 模型能力」这些
// 请求侧信息，故 11133 家族会退到中性参数 hint（与 API 响应的精确 hint 相比
// 可能少一句「换模型」，但不会给错指向）。
func parseDiagnose(e LogEntry) (DiagnoseEntry, bool) {
	m := diagUpstreamRe.FindStringSubmatch(e.Text)
	if m == nil {
		return DiagnoseEntry{}, false
	}
	status, err := strconv.Atoi(m[1])
	if err != nil {
		return DiagnoseEntry{}, false
	}
	body := strings.TrimSpace(e.Text[strings.Index(e.Text, m[0])+len(m[0]):])
	d := DiagnoseEntry{
		TS:          e.TS,
		Channel:     e.Ch,
		Status:      status,
		UpstreamMsg: body,
		Raw:         e.Text,
	}
	if lv := diagLevelRe.FindStringSubmatch(e.Text); lv != nil {
		d.Level = lv[1]
	}
	if a := diagAcctRe.FindStringSubmatch(e.Text); a != nil {
		d.Account = a[1]
	}
	if mm := diagModelRe.FindStringSubmatch(e.Text); mm != nil {
		d.Model = mm[1]
	}
	if kind := upstream.Classify(status, body); kind != upstream.ErrNone {
		d.Kind = kind.String()
		d.GatewayHint = upstream.GatewayHint(kind, body, upstream.HintContext{})
	}
	if code, cm, hit := upstream.ExplainBody(body); code != 0 {
		d.UpstreamCode = code
		if hit {
			d.ClientMessage = cm.Message
			d.ClientDetail = cm.Detail
		}
	}
	return d, true
}

// diagnoseHandler 返回最近的上游错误诊断（新→旧）。
// limit 用小整数：这是排障视图，不是审计日志；旧条目仍在日志视图里可查。
func (p *Panel) diagnoseHandler(w http.ResponseWriter, r *http.Request) {
	const limit = 100
	entries := p.logs.Snapshot()
	out := make([]DiagnoseEntry, 0, limit)
	for i := len(entries) - 1; i >= 0 && len(out) < limit; i-- {
		if d, ok := parseDiagnose(entries[i]); ok {
			out = append(out, d)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "total": len(out)})
}
