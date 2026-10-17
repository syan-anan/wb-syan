// messages.go —— POST /v1/messages 路由接线（Anthropic Messages 兼容入口）。
//
// anthropic.go 只做协议翻译（纯函数、不注册路由）。本文件负责把它挂上 Handler：
//
//	请求侧  Anthropic body --AnthropicToChat--> Chat body --> 既有 chatCompletions
//	响应侧  既有 chatCompletions --anthropicWriter--> Anthropic 响应
//
// 为什么不另起一条转发链路：chatCompletions 里是 ~500 行调度状态机（选号 / 粘性 /
// 在途租约 / 轮转 / 降级重试 / gateway_hint / 用量记账），复制一份必然与主线漂移。
// 它对外只有三条出口——writeJSON（非流式）、writeOpenAIError*（错误）、
// upstream.StreamHint（SSE）——全部经过 http.ResponseWriter，所以这一层就是唯一
// 需要接的缝：包装 ResponseWriter，翻译只发生在这里。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/syan-anan/wb-syan/internal/keys"

	"github.com/syan-anan/wb-syan/internal/upstream"
)

// ---------------------------------------------------------------------------
// 错误体：OpenAI 形状 -> Anthropic 形状
// ---------------------------------------------------------------------------

// anthropicErrorType 把 HTTP 状态码映射成 Anthropic 的错误类型枚举。
func anthropicErrorType(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "api_error"
	}
	return "invalid_request_error"
}

// anthropicErrorBody 组装 Anthropic 形态的错误体。
// Anthropic 客户端只解 {type:"error", error:{type, message}}；把 OpenAI 的
// {error:{message, code, type}} 原样送过去，SDK 会先报解析失败，真正的 401/403
// 原因反而被盖掉。
func anthropicErrorBody(status int, msg string) []byte {
	return anthropicErrorBodyHint(status, msg, "")
}

// anthropicErrorBodyHint 同 anthropicErrorBody，另在 error 对象上附加
// gateway_hint。与 OpenAI 侧 writeOpenAIErrorHint 同语义：并列补充、绝不替换
// message。Anthropic 客户端只读 type/message，多余键被忽略——所以这里是把网关
// 提示透出去的唯一位点（不把 hint 拼进 message，免得污染上游原文）。
func anthropicErrorBodyHint(status int, msg, hint string) []byte {
	e := map[string]any{
		"type":    anthropicErrorType(status),
		"message": msg,
	}
	// 三层诊断的第二层（与 OpenAI 侧 writeOpenAIErrorHint 同口径）：业务码 +
	// 客户端中文解释。Anthropic 客户端只读 type/message，多余键被忽略。
	if n, cm, hit := upstream.ExplainBody(msg); n != 0 {
		e["upstream_code"] = n
		if hit {
			e["client_message"] = cm.Message
			if cm.Detail != "" {
				e["client_detail"] = cm.Detail
			}
		}
	}
	if hint != "" {
		e["gateway_hint"] = hint
	}
	raw, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": e,
	})
	if err != nil {
		return []byte("{\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"internal error\"}}")
	}
	return raw
}

// writeAnthropicError 写一条 Anthropic 形态的错误响应（网关自身产生的错误）。
func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(anthropicErrorBody(status, msg))
}

// openAIErrorParts 从 OpenAI 形态的错误体里取出 message 与 gateway_hint。
// 取不到（非 JSON / 无对应键）返回空串，由调用方决定兜底文案——不在这里编造上游文案。
func openAIErrorParts(body []byte) (msg, hint string) {
	var e struct {
		Error struct {
			Message     string `json:"message"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return "", ""
	}
	return strings.TrimSpace(e.Error.Message), strings.TrimSpace(e.Error.GatewayHint)
}

// anthropicStreamError 判断一帧 Chat SSE 是不是错误帧，是则取出可读 message。
// 覆盖两种来源：上游错误透传帧、网关空流兜底帧——两者都带顶层 error 对象。
func anthropicStreamError(data string) (string, bool) {
	var probe struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(data), &probe) != nil || probe.Error == nil {
		return "", false
	}
	if m := strings.TrimSpace(probe.Error.Message); m != "" {
		return m, true
	}
	return "upstream error", true
}

// ---------------------------------------------------------------------------
// 响应侧：http.ResponseWriter 包装
// ---------------------------------------------------------------------------

// anthropicWriter 把 chatCompletions 写出的 OpenAI 形态响应就地翻成 Anthropic 形态。
//
// 按 Content-Type 分流，与 chatCompletions 的实际分支一一对应：
//   - text/event-stream（upstream.StreamHint）→ 逐帧翻成 Anthropic 事件流；
//   - application/json（writeJSON / writeOpenAIError*）→ 缓冲到 finish() 再整体翻译
//     （非流式必须整体可见才能算 stop_reason 与 usage，逐块翻没有意义）。
//
// 状态码与响应头延迟到分流确定后再写：Anthropic 与 OpenAI 的错误体形状不同，
// 必须等拿到 body 才能决定写什么。
type anthropicWriter struct {
	http.ResponseWriter
	model string

	code   int
	buf    bytes.Buffer
	state  *AnthropicStreamState
	opened bool // 流式响应头已写出
	broken bool // 流内已发 error 事件，后续帧不再翻译
}

// Header 直接透出底层 header：StreamHint 要在第一帧之前设置 Content-Type，
// 本包装器正是靠它分流，不能拦。
func (a *anthropicWriter) Header() http.Header { return a.ResponseWriter.Header() }

func (a *anthropicWriter) WriteHeader(code int) {
	if a.code == 0 {
		a.code = code
	}
}

func (a *anthropicWriter) Write(b []byte) (int, error) {
	if strings.HasPrefix(a.Header().Get("Content-Type"), "text/event-stream") {
		return a.writeStream(b)
	}
	return a.buf.Write(b)
}

// Flush 仅在流式头已写出后向下透传：非流式响应还在缓冲，提前 flush 会把底层
// writer 的隐式 200 钉死，finish() 就再也改不了状态码。
func (a *anthropicWriter) Flush() {
	if !a.opened {
		return
	}
	if fl, ok := a.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// open 写出流式响应头（幂等）。状态码取 chatCompletions 记下的值，缺省 200。
func (a *anthropicWriter) open() {
	if a.opened {
		return
	}
	code := a.code
	if code == 0 {
		code = http.StatusOK
	}
	a.ResponseWriter.WriteHeader(code)
	a.opened = true
}

// writeStream 处理一个上游 SSE 分片。StreamHint 每次 Write 恰好写一帧
// （"data: <payload>\n\n"）或一行注释，因此分片边界就是帧边界，无需再拼缓冲。
func (a *anthropicWriter) writeStream(b []byte) (int, error) {
	a.open()
	if a.broken {
		return len(b), nil
	}
	if a.state == nil {
		a.state = &AnthropicStreamState{Model: a.model}
	}
	for _, data := range anthropicSSEDataLines(b) {
		switch {
		case data == "":
			// 空行 / 注释 / 心跳：Anthropic 流里没有等价物，吞掉。
		case data == "[DONE]":
			if err := AnthropicSSEFromChat([]byte("data: [DONE]\n\n"), a.ResponseWriter, a.state); err != nil {
				return 0, err
			}
		default:
			if msg, ok := anthropicStreamError(data); ok {
				a.writeStreamError(msg)
				continue
			}
			if err := AnthropicSSEFromChat([]byte("data: "+data+"\n\n"), a.ResponseWriter, a.state); err != nil {
				return 0, err
			}
		}
	}
	a.Flush()
	return len(b), nil
}

// writeStreamError 把流内错误发成 Anthropic 的 error 事件。
// 与 OpenAI 侧不同：Anthropic 流中途出错时 HTTP 头早已发出（200），只能在流里
// 发一条 event: error，客户端据此中止——所以这里同时置 broken，后续的 [DONE]
// 不再补 message_stop（已失败的流不该再收一副"正常结束"的骨架）。
func (a *anthropicWriter) writeStreamError(msg string) {
	a.broken = true
	raw, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": msg},
	})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(a.ResponseWriter, "event: error\ndata: %s\n\n", raw)
	a.Flush()
}

// finish 收尾：流式补骨架，非流式整体翻译。由 messages() 在 chatCompletions
// 返回后调用——chatCompletions 的每个出口都会写出响应，所以这里必然有内容可翻。
func (a *anthropicWriter) finish() {
	if a.opened {
		if a.broken || a.state == nil || a.state.finished {
			return
		}
		// 上游漏发收尾帧（只有 [DONE] 或干脆断流）时兜底补骨，客户端不至于空等。
		_ = anthropicFinishStream(a.ResponseWriter, a.state)
		return
	}
	code := a.code
	if code == 0 {
		code = http.StatusOK
	}
	body := a.buf.Bytes()
	if code == http.StatusOK {
		if out, err := ChatToAnthropic(body, a.model); err == nil {
			a.Header().Set("Content-Type", "application/json")
			a.ResponseWriter.WriteHeader(code)
			_, _ = a.ResponseWriter.Write(out)
			return
		}
		// 翻译失败（上游 200 但不是合法 Chat 体）：原样透传，客户端至少能看到真实
		// 原因，而不是网关伪造的 500。
		a.ResponseWriter.WriteHeader(code)
		_, _ = a.ResponseWriter.Write(body)
		return
	}
	msg, hint := openAIErrorParts(body)
	if msg == "" {
		if msg = http.StatusText(code); msg == "" {
			msg = "upstream error"
		}
	}
	a.Header().Set("Content-Type", "application/json")
	a.ResponseWriter.WriteHeader(code)
	_, _ = a.ResponseWriter.Write(anthropicErrorBodyHint(code, msg, hint))
}

// ---------------------------------------------------------------------------
// 路由
// ---------------------------------------------------------------------------

// anthropicKeyHeader 把 Anthropic 客户端惯用的 x-api-key 折进 Authorization，
// 让既有的 keys.Store.Resolve 一条路径同时吃两种头（Anthropic SDK / Claude Code
// 走 x-api-key，OpenAI 风格客户端走 Bearer）。已有 Bearer 时不覆盖。
func anthropicKeyHeader(r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return
	}
	if k := strings.TrimSpace(r.Header.Get("X-Api-Key")); k != "" {
		r.Header.Set("Authorization", "Bearer "+k)
	}
}

// withAuthAnthropic 同 withAuth，但鉴权失败写 Anthropic 形态的错误体。
func (h *Handler) withAuthAnthropic(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		snap := h.loadLive()
		store := snap.Keys
		if store == nil {
			store = keys.New(snap.APIKey, nil)
		}
		anthropicKeyHeader(r)
		id, ok := store.Resolve(r)
		if !ok {
			writeAnthropicError(w, http.StatusUnauthorized, "missing or invalid API key")
			return
		}
		next(w, r.WithContext(keys.WithIdentity(r.Context(), id)))
	}
}

// messages POST /v1/messages —— Anthropic Messages 兼容入口。
//
// 只做三件事：读 body → AnthropicToChat → 交给 chatCompletions，响应侧包一层
// anthropicWriter。选号、轮转、降级、粘性、记账全部复用既有状态机。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var areq AnthropicRequest
	if err := json.Unmarshal(raw, &areq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if len(areq.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "messages: field required")
		return
	}
	chatBody, err := AnthropicToChat(&areq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(chatBody))
	r.ContentLength = int64(len(chatBody))

	aw := &anthropicWriter{ResponseWriter: w, model: areq.Model}
	if areq.Stream {
		aw.state = &AnthropicStreamState{Model: areq.Model}
	}
	h.chatCompletions(aw, r)
	aw.finish()
}
