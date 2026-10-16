// responses.go —— OpenAI Responses API（POST /v1/responses）适配层。
//
// 背景：Codex CLI 这类客户端只说 Responses，上游只认 Chat Completions。本文件在两个
// 协议之间做纯数据翻译，并把 Responses 请求**投影**成 Chat messages（input 项 →
// messages：message / function_call / function_call_output 各自落位），让 Responses
// 客户端复用既有那条转发链路（选号 / 粘性 / 轮转 / 降级 / 记账一条不落）。
//
// 事件契约（这一节是踩坑换来的，改之前先读完）：
// Responses 规范里若干事件的字段是**必填**的，主流客户端用严格 schema 校验每个 SSE
// 事件，校验不通过时**逐条丢弃该事件**而不是中断整条流 —— 故障表现是静默丢数据。
// 两个真实症状都是「思考完就断、没有正文」：
//
//  1. response.output_text.delta 少了必填的 item_id → 每条正文 delta 被丢弃；
//     reasoning 事件字段齐全所以思考能显示，最终只剩空回复。
//  2. response.in_progress / response.content_part.* / *.done 属于可选扩展，并非所有
//     客户端都实现 → 发了会被判非法，白白污染事件流。
//
// 所以本层只发「规范的必需事件子集」，字段齐全且不依赖可选扩展（与参考实现的
// EMIT_EXTENDED_EVENTS=False 默认档一致）。
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// responsesAllowedEvents 允许发给客户端的事件类型（规范的必需子集）。
var responsesAllowedEvents = map[string]bool{
	"response.created":                       true,
	"response.output_item.added":             true,
	"response.output_item.done":              true,
	"response.output_text.delta":             true,
	"response.reasoning_summary_part.added":  true,
	"response.reasoning_summary_text.delta":  true,
	"response.reasoning_summary_part.done":   true,
	"response.function_call_arguments.delta": true,
	"response.completed":                     true,
	"response.incomplete":                    true,
	"response.failed":                        true,
}

// randID 生成 Responses 风格的对象 id（前缀 + 24 位十六进制）。
func randID(prefix string) string {
	b := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return prefix + fmt.Sprintf("%024x", time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// 请求：Responses -> Chat
// ---------------------------------------------------------------------------

// ResponsesRequest Responses API 请求体（只解我们翻译需要的字段）。
type ResponsesRequest struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions,omitempty"`
	Input           json.RawMessage `json:"input,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	MaxTokens       int             `json:"max_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stop            []string        `json:"stop,omitempty"`
	Seed            *int            `json:"seed,omitempty"`
	Tools           json.RawMessage `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	Reasoning       json.RawMessage `json:"reasoning,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
}

// responsesChatMsg 本层要写出的 Chat 消息（字段顺序稳定，便于 diff 与测试）。
type responsesChatMsg struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCalls  []responsesCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type responsesCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type responsesChatRequest struct {
	Model           string             `json:"model,omitempty"`
	Messages        []responsesChatMsg `json:"messages"`
	Stream          bool               `json:"stream,omitempty"`
	MaxTokens       int                `json:"max_tokens,omitempty"`
	Temperature     *float64           `json:"temperature,omitempty"`
	TopP            *float64           `json:"top_p,omitempty"`
	Stop            []string           `json:"stop,omitempty"`
	Seed            *int               `json:"seed,omitempty"`
	Tools           json.RawMessage    `json:"tools,omitempty"`
	ToolChoice      json.RawMessage    `json:"tool_choice,omitempty"`
	ReasoningEffort string             `json:"reasoning_effort,omitempty"`
}

// responsesInputItem input 数组里的一项（字段全可选，未知类型静默忽略）。
type responsesInputItem struct {
	Type      string          `json:"type,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

// ResponsesToChat 把 Responses 请求投影成 Chat Completions 请求体。
//
// 语义定死在这里（下游无人可问）：
//   - instructions → 置顶 system 消息；
//   - input 字符串 → 单条 user 消息；数组 → 按项投影（见 appendInputItems）；
//   - reasoning:{effort} → 扁平 reasoning_effort（不映射的话走 Responses 的客户端档位
//     被静默丢弃、上游不吐思维链）；
//   - max_output_tokens → max_tokens；
//   - tools 由 Responses 的扁平行改成 Chat 的 function 嵌套行；
//   - previous_response_id / store / metadata / include 忽略：本网关无状态，客户端每轮
//     自带完整 input（与参考实现同口径）。
func ResponsesToChat(req *ResponsesRequest) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("responses: nil request")
	}
	msgs := make([]responsesChatMsg, 0, 8)
	if s := strings.TrimSpace(req.Instructions); s != "" {
		msgs = append(msgs, responsesChatMsg{Role: "system", Content: s})
	}
	if len(req.Input) > 0 {
		var asString string
		if json.Unmarshal(req.Input, &asString) == nil {
			msgs = append(msgs, responsesChatMsg{Role: "user", Content: asString})
		} else {
			var items []responsesInputItem
			if err := json.Unmarshal(req.Input, &items); err != nil {
				return nil, fmt.Errorf("responses: input 既不是字符串也不是数组: %w", err)
			}
			msgs = appendInputItems(msgs, items)
		}
	}

	out := responsesChatRequest{
		Model:       req.Model,
		Messages:    msgs,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		Seed:        req.Seed,
		ToolChoice:  req.ToolChoice,
	}
	if req.MaxOutputTokens > 0 {
		out.MaxTokens = req.MaxOutputTokens
	} else {
		out.MaxTokens = req.MaxTokens
	}
	if t := responsesToolsToChat(req.Tools); t != nil {
		out.Tools = t
	}
	out.ReasoningEffort = responsesReasoningEffort(req.Reasoning)

	return json.Marshal(out)
}

// appendInputItems 把 input 数组投影成 Chat messages。
//
// 关键规则：assistant 正文与紧随其后的 function_call 必须合并进同一条 assistant 消息
// （Chat 要求 tool_calls 挂在 assistant 上），所以这里用 pending 缓冲，遇到非 assistant
// 项时先 flush。顺序错了上游会直接判 400。
func appendInputItems(msgs []responsesChatMsg, items []responsesInputItem) []responsesChatMsg {
	var pendingContent *string
	var pendingCalls []responsesCall
	flush := func() {
		if pendingContent == nil && len(pendingCalls) == 0 {
			return
		}
		content := ""
		if pendingContent != nil {
			content = *pendingContent
		}
		msgs = append(msgs, responsesChatMsg{Role: "assistant", Content: content, ToolCalls: pendingCalls})
		pendingContent = nil
		pendingCalls = nil
	}

	for _, it := range items {
		role := it.Role
		switch {
		case it.Type == "function_call":
			if pendingContent == nil {
				empty := ""
				pendingContent = &empty
			}
			var call responsesCall
			call.ID = it.CallID
			if call.ID == "" {
				call.ID = it.ID
			}
			if call.ID == "" {
				call.ID = randID("call_")
			}
			call.Type = "function"
			call.Function.Name = it.Name
			call.Function.Arguments = it.Arguments
			if call.Function.Arguments == "" {
				call.Function.Arguments = "{}"
			}
			pendingCalls = append(pendingCalls, call)
		case it.Type == "function_call_output":
			flush()
			msgs = append(msgs, responsesChatMsg{
				Role:       "tool",
				ToolCallID: it.CallID,
				Content:    rawToString(it.Output),
			})
		case it.Type == "message" && role == "assistant",
			it.Type == "" && role == "assistant":
			flush()
			text := responsesTextFromContent(it.Content)
			pendingContent = &text
		case role != "":
			flush()
			mapped := role
			if role == "developer" {
				mapped = "system"
			}
			msgs = append(msgs, responsesChatMsg{Role: mapped, Content: responsesTextFromContent(it.Content)})
		default:
			// 带 type 但我们不认识的项（如 reasoning 回传）：忽略，不猜。
		}
	}
	flush()
	return msgs
}

// responsesTextFromContent 从 content（字符串 / part 数组）取纯文本。
func responsesTextFromContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "input_text", "text", "output_text", "":
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// rawToString 把可能为字符串/对象/数组的字段统一成字符串（tool 输出常见是对象）。
func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// responsesToolsToChat 把 Responses 的扁平 tool 行改成 Chat 的 function 嵌套行。
// 已经是 Chat 形态的（带 function 键）原样保留 —— 有些客户端两者混着发。
func responsesToolsToChat(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		var typ string
		_ = json.Unmarshal(it["type"], &typ)
		if typ != "function" {
			continue
		}
		if _, nested := it["function"]; nested {
			m := map[string]any{}
			for k, v := range it {
				m[k] = v
			}
			out = append(out, m)
			continue
		}
		fn := map[string]any{}
		for _, k := range []string{"name", "description", "parameters", "strict"} {
			if v, ok := it[k]; ok {
				fn[k] = v
			}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	if len(out) == 0 {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return b
}

// responsesReasoningEffort 取思考档位：reasoning:{effort} 或裸字符串。
func responsesReasoningEffort(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Effort string `json:"effort"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(obj.Effort)
	}
	return ""
}

// ---------------------------------------------------------------------------
// 响应：Chat -> Responses（流式状态机）
// ---------------------------------------------------------------------------

// ResponsesStreamState 跨分片保存流式翻译的中间态。
type ResponsesStreamState struct {
	Model     string
	respID    string
	msgID     string
	reasonID  string
	createdAt int64

	emittedCreated  bool
	emittedMsgItem  bool
	emittedReason   bool
	emittedComplete bool

	content   strings.Builder
	reasoning strings.Builder

	toolCalls map[int]*responsesToolSlot
	toolOrder []int

	finishReason string
	usage        map[string]any
}

// responsesToolSlot 一个 tool_call 的累计态与它在 output 里的槽位。
type responsesToolSlot struct {
	id        string
	fcID      string
	name      string
	args      string
	outputIdx int
	emitted   bool
}

// NewResponsesStreamState 构造流式状态（model 用于回显）。
func NewResponsesStreamState(model string) *ResponsesStreamState {
	return &ResponsesStreamState{
		Model:     model,
		respID:    randID("resp_"),
		msgID:     randID("msg_"),
		reasonID:  randID("rs_"),
		createdAt: time.Now().Unix(),
		toolCalls: map[int]*responsesToolSlot{},
	}
}

// responsesChatChunk 上游 Chat 流式分片里我们关心的字段。
type responsesChatChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
}

// ChatSSEToResponses 处理一个上游 Chat 分片（"data: {...}\n\n" 形态），把 Responses
// 事件写进 w。语义与参考实现 ResponsesStreamConverter.feed_line 一致。
func ChatSSEToResponses(chunk []byte, w io.Writer, st *ResponsesStreamState) error {
	if st == nil {
		return fmt.Errorf("responses: nil stream state")
	}
	for _, data := range anthropicSSEDataLines(chunk) {
		if data == "" {
			continue
		}
		var c responsesChatChunk
		if json.Unmarshal([]byte(data), &c) != nil {
			continue
		}
		if err := responsesApplyChunk(w, st, &c); err != nil {
			return err
		}
	}
	return nil
}

func responsesApplyChunk(w io.Writer, st *ResponsesStreamState, c *responsesChatChunk) error {
	if c.Model != "" {
		st.Model = c.Model
	}
	if !st.emittedCreated {
		if err := responsesWriteEvent(w, "response.created", map[string]any{
			"response": st.responseObject("in_progress"),
		}); err != nil {
			return err
		}
		st.emittedCreated = true
	}
	if c.Usage != nil {
		st.usage = c.Usage
	}
	for _, ch := range c.Choices {
		if ch.Delta.ReasoningContent != "" {
			if err := responsesAppendReasoning(w, st, ch.Delta.ReasoningContent); err != nil {
				return err
			}
		}
		if ch.Delta.Content != "" {
			if !st.emittedMsgItem {
				if err := responsesWriteEvent(w, "response.output_item.added", map[string]any{
					"output_index": st.bodyIndex(),
					"item":         st.msgItem("in_progress", true),
				}); err != nil {
					return err
				}
				st.emittedMsgItem = true
			}
			st.content.WriteString(ch.Delta.Content)
			// item_id 是必填：缺了它，严格校验的客户端会丢掉整条正文 delta。
			if err := responsesWriteEvent(w, "response.output_text.delta", map[string]any{
				"item_id":       st.msgID,
				"output_index":  st.bodyIndex(),
				"content_index": 0,
				"delta":         ch.Delta.Content,
			}); err != nil {
				return err
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			if err := responsesApplyToolCall(w, st, tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments); err != nil {
				return err
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			st.finishReason = *ch.FinishReason
		}
	}
	return nil
}

func responsesAppendReasoning(w io.Writer, st *ResponsesStreamState, delta string) error {
	if !st.emittedReason {
		if err := responsesWriteEvent(w, "response.output_item.added", map[string]any{
			"output_index": 0,
			"item":         st.reasoningItem("in_progress"),
		}); err != nil {
			return err
		}
		// summary part 的开场事件：客户端靠它初始化 summary 块，缺了 reasoning 无法按 part 收尾。
		if err := responsesWriteEvent(w, "response.reasoning_summary_part.added", map[string]any{
			"item_id":       st.reasonID,
			"summary_index": 0,
		}); err != nil {
			return err
		}
		st.emittedReason = true
	}
	st.reasoning.WriteString(delta)
	return responsesWriteEvent(w, "response.reasoning_summary_text.delta", map[string]any{
		"item_id":       st.reasonID,
		"output_index":  0,
		"summary_index": 0,
		"delta":         delta,
	})
}

func responsesApplyToolCall(w io.Writer, st *ResponsesStreamState, idx int, id, name, args string) error {
	slot, ok := st.toolCalls[idx]
	if !ok {
		base := st.bodyIndex()
		if st.emittedMsgItem || st.content.Len() > 0 {
			base++
		}
		slot = &responsesToolSlot{fcID: randID("fc_"), outputIdx: base + len(st.toolCalls)}
		st.toolCalls[idx] = slot
		st.toolOrder = append(st.toolOrder, idx)
	}
	if id != "" {
		slot.id = id
	}
	if name != "" {
		slot.name = name
	}
	if !slot.emitted {
		if err := responsesWriteEvent(w, "response.output_item.added", map[string]any{
			"output_index": slot.outputIdx,
			"item":         st.fcItem(slot, "in_progress"),
		}); err != nil {
			return err
		}
		slot.emitted = true
	}
	if args != "" {
		slot.args += args
		// item_id 同为必填，需指向 announce 过的 function_call item id（不是 call_id）。
		return responsesWriteEvent(w, "response.function_call_arguments.delta", map[string]any{
			"item_id":      slot.fcID,
			"output_index": slot.outputIdx,
			"delta":        args,
		})
	}
	return nil
}

// ResponsesFinishStream 收尾：关掉未结束的 item，发 response.completed（幂等）。
func ResponsesFinishStream(w io.Writer, st *ResponsesStreamState) error {
	if st == nil || st.emittedComplete {
		return nil
	}
	if st.emittedReason {
		if err := responsesWriteEvent(w, "response.reasoning_summary_part.done", map[string]any{
			"item_id":       st.reasonID,
			"summary_index": 0,
		}); err != nil {
			return err
		}
		if err := responsesWriteEvent(w, "response.output_item.done", map[string]any{
			"output_index": 0,
			"item":         st.reasoningItem("completed"),
		}); err != nil {
			return err
		}
	}
	if st.emittedMsgItem {
		if err := responsesWriteEvent(w, "response.output_item.done", map[string]any{
			"output_index": st.bodyIndex(),
			"item":         st.msgItem("completed", false),
		}); err != nil {
			return err
		}
	}
	for _, idx := range st.toolOrder {
		slot := st.toolCalls[idx]
		if !slot.emitted {
			continue
		}
		if err := responsesWriteEvent(w, "response.output_item.done", map[string]any{
			"output_index": slot.outputIdx,
			"item":         st.fcItem(slot, "completed"),
		}); err != nil {
			return err
		}
	}
	kind, status := "response.completed", "completed"
	if strings.EqualFold(st.finishReason, "length") {
		kind, status = "response.incomplete", "incomplete"
	}
	st.emittedComplete = true
	return responsesWriteEvent(w, kind, map[string]any{"response": st.responseObject(status)})
}

func (st *ResponsesStreamState) bodyIndex() int {
	if st.emittedReason {
		return 1
	}
	return 0
}

func (st *ResponsesStreamState) reasoningItem(status string) map[string]any {
	summary := []any{}
	if st.reasoning.Len() > 0 {
		summary = append(summary, map[string]any{"type": "summary_text", "text": st.reasoning.String()})
	}
	return map[string]any{
		"type":    "reasoning",
		"id":      st.reasonID,
		"summary": summary,
		"status":  status,
	}
}

func (st *ResponsesStreamState) msgItem(status string, empty bool) map[string]any {
	content := []any{}
	if !empty {
		content = append(content, map[string]any{
			"type": "output_text", "text": st.content.String(), "annotations": []any{},
		})
	}
	return map[string]any{
		"type":    "message",
		"id":      st.msgID,
		"status":  status,
		"role":    "assistant",
		"content": content,
	}
}

func (st *ResponsesStreamState) fcItem(slot *responsesToolSlot, status string) map[string]any {
	return map[string]any{
		"type":      "function_call",
		"id":        slot.fcID,
		"call_id":   slot.id,
		"name":      slot.name,
		"arguments": slot.args,
		"status":    status,
	}
}

// responseObject 组装 Response 对象。usage 是必填且 input/output_tokens 必须是数字：
// 上游偶尔不吐 usage，缺了会让 completed 校验失败、客户端拿不到 finishReason，
// 所以缺失时补零而不是留 null。
func (st *ResponsesStreamState) responseObject(status string) map[string]any {
	output := []any{}
	if st.emittedReason || st.reasoning.Len() > 0 {
		output = append(output, st.reasoningItem(status))
	}
	if st.emittedMsgItem || st.content.Len() > 0 {
		output = append(output, st.msgItem(status, false))
	}
	for _, idx := range st.toolOrder {
		if slot := st.toolCalls[idx]; slot.emitted {
			output = append(output, st.fcItem(slot, status))
		}
	}
	usage := map[string]any{
		"input_tokens":          0,
		"input_tokens_details":  map[string]any{"cached_tokens": 0},
		"output_tokens":         0,
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		"total_tokens":          0,
	}
	if st.usage != nil {
		usage = responsesUsageFrom(st.usage)
	}
	return map[string]any{
		"id":                  st.respID,
		"object":              "response",
		"created_at":          st.createdAt,
		"status":              status,
		"model":               st.Model,
		"output":              output,
		"parallel_tool_calls": true,
		"usage":               usage,
	}
}

// responsesUsageFrom 把上游 usage 投影成 Responses 的 usage（含 details 子对象）。
func responsesUsageFrom(u map[string]any) map[string]any {
	num := func(keys ...string) float64 {
		for _, k := range keys {
			if v, ok := u[k].(float64); ok {
				return v
			}
		}
		return 0
	}
	reasoning := num("completion_thinking_tokens")
	if det, ok := u["completion_tokens_details"].(map[string]any); ok {
		if v, ok := det["reasoning_tokens"].(float64); ok {
			reasoning = v
		}
	}
	cached := num("cached_tokens")
	if det, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := det["cached_tokens"].(float64); ok {
			cached = v
		}
	}
	return map[string]any{
		"input_tokens":          num("prompt_tokens", "input_tokens"),
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         num("completion_tokens", "output_tokens"),
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          num("total_tokens"),
	}
}

// responsesWriteEvent 写一个 SSE 事件；不在契约白名单里的事件直接丢弃（见文件头）。
func responsesWriteEvent(w io.Writer, eventType string, data map[string]any) error {
	if !responsesAllowedEvents[eventType] {
		return nil
	}
	payload := map[string]any{"type": eventType}
	for k, v := range data {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("responses: marshal %s: %w", eventType, err)
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
		return fmt.Errorf("responses: write %s: %w", eventType, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 非流式
// ---------------------------------------------------------------------------

// ChatToResponses 把聚合后的 Chat 响应翻成 Responses 对象（非流式出口）。
func ChatToResponses(chatResp []byte, model string) ([]byte, error) {
	var cr struct {
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(chatResp, &cr); err != nil {
		return nil, fmt.Errorf("responses: parse chat response: %w", err)
	}
	st := NewResponsesStreamState(model)
	st.emittedCreated = true
	if len(cr.Choices) > 0 {
		ch := cr.Choices[0]
		text := responsesTextFromContent(ch.Message.Content)
		// 纯工具调用轮次：不要凭空造一个空 text 块（output 里只该有 function_call）。
		if text != "" || len(ch.Message.ToolCalls) == 0 {
			st.emittedMsgItem = true
			st.content.WriteString(text)
		}
		for i, tc := range ch.Message.ToolCalls {
			_ = responsesApplyToolCall(io.Discard, st, i, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
		st.finishReason = ch.FinishReason
	}
	st.usage = cr.Usage
	status := "completed"
	if strings.EqualFold(st.finishReason, "length") {
		status = "incomplete"
	}
	return json.Marshal(st.responseObject(status))
}

// ---------------------------------------------------------------------------
// 响应侧 ResponseWriter 包装（与 anthropicWriter 同构）
// ---------------------------------------------------------------------------

// responsesWriter 把 chatCompletions 写出的 OpenAI 形态响应就地翻成 Responses 形态。
// 分流规则与 anthropicWriter 完全一致：Content-Type 决定走流式逐帧翻还是整包翻。
type responsesWriter struct {
	http.ResponseWriter
	model string

	code   int
	buf    []byte
	state  *ResponsesStreamState
	opened bool
	broken bool
}

func (a *responsesWriter) Header() http.Header { return a.ResponseWriter.Header() }

func (a *responsesWriter) WriteHeader(code int) {
	if a.code == 0 {
		a.code = code
	}
}

func (a *responsesWriter) Write(b []byte) (int, error) {
	if strings.HasPrefix(a.Header().Get("Content-Type"), "text/event-stream") {
		return a.writeStream(b)
	}
	a.buf = append(a.buf, b...)
	return len(b), nil
}

func (a *responsesWriter) Flush() {
	if !a.opened {
		return
	}
	if fl, ok := a.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (a *responsesWriter) open() {
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

func (a *responsesWriter) writeStream(b []byte) (int, error) {
	a.open()
	if a.broken {
		return len(b), nil
	}
	if a.state == nil {
		a.state = NewResponsesStreamState(a.model)
	}
	for _, data := range anthropicSSEDataLines(b) {
		if data == "" {
			continue
		}
		if msg, isErr := anthropicStreamError(data); isErr {
			// 流内错误：用 response.failed 收尾（HTTP 头早已发出，改不了状态码）。
			a.broken = true
			_ = responsesWriteEvent(a.ResponseWriter, "response.failed", map[string]any{
				"response": map[string]any{
					"id":     a.state.respID,
					"object": "response",
					"status": "failed",
					"model":  a.state.Model,
					"error":  map[string]any{"code": "upstream_error", "message": msg},
					"usage":  map[string]any{"input_tokens": 0, "output_tokens": 0},
				},
			})
			a.Flush()
			continue
		}
		if err := ChatSSEToResponses([]byte("data: "+data+"\n\n"), a.ResponseWriter, a.state); err != nil {
			return 0, err
		}
	}
	a.Flush()
	return len(b), nil
}

func (a *responsesWriter) finish() {
	if a.opened {
		if a.broken || a.state == nil {
			return
		}
		_ = ResponsesFinishStream(a.ResponseWriter, a.state)
		return
	}
	code := a.code
	if code == 0 {
		code = http.StatusOK
	}
	if code == http.StatusOK {
		if out, err := ChatToResponses(a.buf, a.model); err == nil {
			a.Header().Set("Content-Type", "application/json")
			a.ResponseWriter.WriteHeader(code)
			_, _ = a.ResponseWriter.Write(out)
			return
		}
	}
	// 非 200 或翻译失败：原样透传上游响应（OpenAI 错误信封与 Responses 通用）。
	a.ResponseWriter.WriteHeader(code)
	_, _ = a.ResponseWriter.Write(a.buf)
}

// ---------------------------------------------------------------------------
// 路由
// ---------------------------------------------------------------------------

// responses POST /v1/responses —— OpenAI Responses 兼容入口。
//
// 与 /v1/messages 同套路：请求投影成 Chat 后交给既有 chatCompletions，响应侧包一层
// responsesWriter。错误体沿用 OpenAI 形状（Responses 与 Chat 共用同一套 error 信封）。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req ResponsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid request body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model: field required")
		return
	}
	chatBody, err := ResponsesToChat(&req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(chatBody)))
	r.ContentLength = int64(len(chatBody))

	rw := &responsesWriter{ResponseWriter: w, model: req.Model}
	if req.Stream {
		rw.state = NewResponsesStreamState(req.Model)
	}
	h.chatCompletions(rw, r)
	rw.finish()
}
