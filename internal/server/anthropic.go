// anthropic.go —— Anthropic Messages 协议适配层。
//
// 背景：本网关对外只暴露 OpenAI Chat Completions 形态（handler.go），上游也只认
// 这一种 shape（见 payload.go 的 prepareBody）。而 Claude Code / Anthropic SDK
// 只会说 Anthropic Messages。本文件在两个协议之间做**纯数据翻译**，让 Anthropic
// 客户端可以直连本网关，网关侧不必再维护第二套转发链路。
//
// 边界：只做结构变换——不注册路由、不读配置、不发网络请求（唯一例外是
// AnthropicSSEFromChat 往调用方给的 io.Writer 写）。因此所有翻译函数都是纯函数，
// 便于单测与复用；把 /v1/messages 挂到 Handler 上这一步由 handler 侧完成。
//
// 只依赖标准库：协议翻译本质是结构体对翻，引第三方适配库只会增加供应链面与版本耦合。
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ---------------------------------------------------------------------------
// Anthropic Messages 请求侧结构
// ---------------------------------------------------------------------------

// AnthropicRequest 是 Anthropic Messages API 的请求体（只解我们翻译需要的字段）。
//
// system / tool_choice / message.content 在 Anthropic 里都是「多态字段」（string
// 或 block 数组），统一用 json.RawMessage 承接，翻译时再按形状分派——比定义
// 自定义 UnmarshalJSON 更省代码，也不会因为上游新增 block 类型直接解析失败。
type AnthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
}

// AnthropicMessage 单条消息。role 只有 user / assistant 两种。
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// AnthropicContentBlock 覆盖 text / image / tool_use / tool_result 四种形态。
//
// 用一个「字段全可选」的结构体而不是接口联合，是因为这里面对的是外部 JSON：
// 单结构体一次解完，未知字段被静默忽略——转发器不该因为对方多塞了个字段就炸掉。
type AnthropicContentBlock struct {
	Type string `json:"type"`

	// text 块。tool_result 的 content 也可能是这个形状。
	Text string `json:"text,omitempty"`

	// image 块。
	Source *AnthropicImageSource `json:"source,omitempty"`

	// tool_use 块。
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result 块。
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// MarshalJSON 按 Type 只写出该形态应有的字段。
//
// 为什么需要它：text 块里的 text 用了 omitempty（否则 tool_use 块会多出一个
// 无意义的空 text 字段），但 Anthropic 的 text 块**必须**带 text 键——哪怕是空串。
// 两者冲突，只能在序列化时按 Type 分派：text 块强制写出 text，其余块不带。
func (b AnthropicContentBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case "text":
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: b.Type, Text: b.Text})
	case "tool_use":
		input := b.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return json.Marshal(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{Type: b.Type, ID: b.ID, Name: b.Name, Input: input})
	case "tool_result":
		return json.Marshal(struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content,omitempty"`
			IsError   bool            `json:"is_error,omitempty"`
		}{Type: b.Type, ToolUseID: b.ToolUseID, Content: b.Content, IsError: b.IsError})
	case "image":
		return json.Marshal(struct {
			Type   string                `json:"type"`
			Source *AnthropicImageSource `json:"source"`
		}{Type: b.Type, Source: b.Source})
	}
	// 未知形态：按结构体原样写出（定义新类型避免递归调用回本方法）。
	type anthropicContentBlockPlain AnthropicContentBlock
	return json.Marshal(anthropicContentBlockPlain(b))
}

// AnthropicImageSource 图片源。官方只有 base64 形态；url 是部分客户端自造的
// 扩展，这里一并兼容（见 anthropicImageDataURL）。
type AnthropicImageSource struct {
	Type      string `json:"type,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// AnthropicTool 工具声明。Anthropic 用 input_schema，OpenAI 用 function.parameters，
// 两者都是 JSON Schema，直接搬迁。
type AnthropicTool struct {
	// Type 是 Anthropic 新增的 "custom" / "computer_20250124" 等标记。
	// Chat 侧没有等价表达，翻译时丢弃（只认 name/description/input_schema）。
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ---------------------------------------------------------------------------
// Anthropic Messages 响应侧结构
// ---------------------------------------------------------------------------

// AnthropicResponse 是 Anthropic Messages API 的响应体。
type AnthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Model        string                  `json:"model"`
	Content      []AnthropicContentBlock `json:"content"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        AnthropicUsage          `json:"usage"`
}

// AnthropicUsage 用量。input_tokens/output_tokens 直接对应 Chat 的
// prompt_tokens/completion_tokens（同义，只换名字）。
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ---------------------------------------------------------------------------
// Chat Completions 中间结构（翻译产物，不是外部契约）
// ---------------------------------------------------------------------------

// anthropicChatRequest 是本层要写出的 OpenAI Chat Completions 请求体。
// 用结构体而不是 map，是为了让 JSON 字段顺序稳定——同一输入永远得到同一字节流，
// 便于 diff 与测试。
type anthropicChatRequest struct {
	Model       string              `json:"model"`
	Messages    []anthropicChatMsg  `json:"messages"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
	Temperature *float64            `json:"temperature,omitempty"`
	TopP        *float64            `json:"top_p,omitempty"`
	Stop        []string            `json:"stop,omitempty"`
	Stream      bool                `json:"stream,omitempty"`
	Tools       []anthropicChatTool `json:"tools,omitempty"`
	ToolChoice  any                 `json:"tool_choice,omitempty"`
}

// anthropicChatMsg 一条 Chat 消息。Content 用 any：可能是字符串（纯文本）也可能是
// 数组（多模态），取决于该消息里有没有图片 part。
type anthropicChatMsg struct {
	Role       string              `json:"role"`
	Content    any                 `json:"content,omitempty"`
	ToolCalls  []anthropicChatCall `json:"tool_calls,omitempty"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
}

// anthropicChatPart 是消息 content 数组里的一个 part（text / image_url）。
type anthropicChatPart struct {
	Type     string               `json:"type"`
	Text     string               `json:"text,omitempty"`
	ImageURL *anthropicChatImgURL `json:"image_url,omitempty"`
}

// anthropicChatImgURL Chat 侧 image_url 必须是对象形态（字符串形态会被上游 400）。
type anthropicChatImgURL struct {
	URL string `json:"url"`
}

// anthropicChatCall 是 Chat 侧的 tool_call（上游认的字段名就是 tool_calls）。
type anthropicChatCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// anthropicChatTool 是 Chat 侧的工具声明。
type anthropicChatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// ---------------------------------------------------------------------------
// 请求翻译：Anthropic -> Chat
// ---------------------------------------------------------------------------

// AnthropicToChat 把 Anthropic Messages 请求翻成 OpenAI Chat Completions 请求体。
//
// 语义对齐的取舍（下游无人可问，直接在这里定死并写清）：
//   - Anthropic 的 system 是顶层字段，Chat 是 messages 里的第一条 system 消息。
//   - Anthropic 允许一条 user 消息里混排 text 与 tool_result；Chat 要求 tool 结果
//     是独立的 role:"tool" 消息。这里把 tool_result 全部提到该轮的前面，紧跟
//     上一轮 assistant 的 tool_calls（这也正是上游 payload.go 里 repack 期望的顺序）。
//   - Anthropic 的 tool_choice 是 {type:"auto"|"any"|"tool"|"none"} 对象；Chat 用
//     "auto"|"required"|{"type":"function",...}。其 disable_parallel_tool_use 子字段
//     Chat 无等价表达，丢弃。
//   - Anthropic 无 frequency_penalty / presence_penalty / seed / response_format，
//     不补默认值（补了就是替客户端做未经请求的决策）。
func AnthropicToChat(req *AnthropicRequest) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("anthropic: nil request")
	}

	out := anthropicChatRequest{
		Model:       req.Model,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.StopSequences,
		Stream:      req.Stream,
		Tools:       anthropicToolsToChat(req.Tools),
		ToolChoice:  anthropicToolChoiceToChat(req.ToolChoice),
	}

	msgs := make([]anthropicChatMsg, 0, len(req.Messages)+1)
	if sys, ok := anthropicSystemText(req.System); ok {
		msgs = append(msgs, anthropicChatMsg{Role: "system", Content: sys})
	}
	for i := range req.Messages {
		msgs = append(msgs, anthropicMessageToChat(&req.Messages[i])...)
	}
	out.Messages = msgs

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal chat request: %w", err)
	}
	return raw, nil
}

// anthropicSystemText 抽取顶层 system 的纯文本（string 或 text block 数组）。
// 返回 ok=false 表示没有 system（或形状无法识别）——此时不注入空 system 消息，
// 免得给上游多一条无意义消息改变行为。
func anthropicSystemText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, s != ""
	}
	var blocks []AnthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n\n"), true
}

// anthropicMessageToChat 把一条 Anthropic 消息展开成 1..n 条 Chat 消息。
// 一条 Anthropic user 消息可能同时带 text 和 tool_result，因此返回切片。
func anthropicMessageToChat(m *AnthropicMessage) []anthropicChatMsg {
	role := strings.TrimSpace(m.Role)
	if role == "" {
		role = "user"
	}

	// content 是纯字符串：最常见的形态，直接搬。
	if s, ok := anthropicRawString(m.Content); ok {
		return []anthropicChatMsg{{Role: role, Content: s}}
	}

	var blocks []AnthropicContentBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		// content 既不是字符串也不是 block 数组：宁可原样递一个空文本，也不猜测语义。
		return []anthropicChatMsg{{Role: role, Content: ""}}
	}

	parts := make([]anthropicChatPart, 0, len(blocks))
	calls := make([]anthropicChatCall, 0, len(blocks))
	tools := make([]anthropicChatMsg, 0, len(blocks))
	for i := range blocks {
		b := &blocks[i]
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, anthropicChatPart{Type: "text", Text: b.Text})
			}
		case "image":
			if url := anthropicImageDataURL(b.Source); url != "" {
				parts = append(parts, anthropicChatPart{
					Type:     "image_url",
					ImageURL: &anthropicChatImgURL{URL: url},
				})
			}
		case "tool_use":
			if b.Name == "" {
				continue // 没名字的 tool_use 上游必然拒绝，跳过省一次往返
			}
			call := anthropicChatCall{ID: b.ID, Type: "function"}
			call.Function.Name = b.Name
			call.Function.Arguments = anthropicArgumentsFromInput(b.Input)
			calls = append(calls, call)
		case "tool_result":
			tools = append(tools, anthropicChatMsg{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    anthropicToolResultText(b.Content),
			})
		default:
			// 未知 block（如 thinking / server_tool_use）：忽略而不报错。
		}
	}

	out := make([]anthropicChatMsg, 0, len(tools)+1)
	// tool 结果排在本轮文本之前：它们紧跟上一轮 assistant 的 tool_calls，
	// 这才是 Chat 的合法顺序（文本跟在后面不影响语义）。
	out = append(out, tools...)

	content := anthropicContentValue(parts)
	if content != nil || len(calls) > 0 {
		out = append(out, anthropicChatMsg{Role: role, Content: content, ToolCalls: calls})
	}
	if len(out) == 0 {
		out = append(out, anthropicChatMsg{Role: role, Content: ""})
	}
	return out
}

// anthropicContentValue 把 part 列表折叠成 Chat 的 content 值：
// 只有一个文本 part 时退回字符串（省一层数组，也让上游走最简单的解析路径），
// 其余（含图片、或多段）保持数组形态。
func anthropicContentValue(parts []anthropicChatPart) any {
	if len(parts) == 1 && parts[0].Type == "text" {
		return parts[0].Text
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// anthropicImageDataURL 把 Anthropic 的 base64 图片源拼成 data URL。
//
// media_type 缺失时兜底 image/png：Anthropic 官方一定下发该字段，兜底只是防御
// 畸形请求——没有 media_type 的 data URL 浏览器/上游都解不出来。
func anthropicImageDataURL(src *AnthropicImageSource) string {
	if src == nil {
		return ""
	}
	if src.URL != "" { // 非官方 url 形态：原样透传
		return src.URL
	}
	if src.Data == "" {
		return ""
	}
	media := strings.TrimSpace(src.MediaType)
	if media == "" {
		media = "image/png"
	}
	return "data:" + media + ";base64," + anthropicNormalizeBase64(src.Data)
}

// anthropicNormalizeBase64 去掉换行/空格并重新编码，得到规范 base64。
// 有些客户端会把 base64 按 76 列折行塞进 JSON，上游解析器未必容忍；重新编码后
// 只保留合法字符。解码失败说明数据本身有问题：原样返回，让上游报真实错误，
// 网关不做静默修补（修补会掩盖客户端的 bug）。
func anthropicNormalizeBase64(s string) string {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, s)
	raw, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return clean
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// anthropicArgumentsFromInput 把 tool_use.input 压成 Chat 的 arguments 字符串。
// input 缺失按 {} 处理（Chat 侧要求 arguments 是 JSON 文本，空串会让部分上游报错）。
func anthropicArgumentsFromInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

// anthropicToolResultText 把 tool_result.content 拉平成字符串。
// Anthropic 允许 string 或 text block 数组；Chat 的 role:"tool" 只吃字符串。
func anthropicToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s, ok := anthropicRawString(raw); ok {
		return s
	}
	var blocks []AnthropicContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// anthropicRawString 判断 raw 是否是 JSON 字符串并取出其值。
func anthropicRawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// anthropicToolsToChat 翻译工具声明。input_schema（JSON Schema）原样搬到
// function.parameters；Anthropic 的 type 标记（custom / computer_* ）Chat 无等价
// 表达，丢弃——上游只认标准 function 工具。
func anthropicToolsToChat(tools []AnthropicTool) []anthropicChatTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicChatTool, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		ct := anthropicChatTool{Type: "function"}
		ct.Function.Name = t.Name
		ct.Function.Description = t.Description
		ct.Function.Parameters = t.InputSchema
		out = append(out, ct)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// anthropicToolChoiceToChat 翻译 tool_choice。
//   - {type:"auto"}          -> "auto"
//   - {type:"any"}           -> "required"（Anthropic 的「必须调用某个工具」）
//   - {type:"tool",name:"x"} -> {"type":"function","function":{"name":"x"}}
//   - {type:"none"}          -> "none"
//   - 缺失/无法识别          -> nil（省略该字段，交给上游默认）
//
// 注意：下游 payload.go 的 normalizeToolChoice 会把对象形态再收敛成字符串，
// 这里保持标准 OpenAI 形态，不提前替它做那一步。
func anthropicToolChoiceToChat(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(tc.Type)) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if strings.TrimSpace(tc.Name) == "" {
			return nil
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": tc.Name},
		}
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// 响应翻译：Chat -> Anthropic
// ---------------------------------------------------------------------------

// ChatToAnthropic 把上游的 OpenAI Chat Completions 响应翻成 Anthropic Messages 响应。
//
// model 参数用于回填响应里的 model 字段：上游可能回一个带路由前缀的名字，而
// Anthropic 客户端只认自己请求时用的那个名字，所以以调用方传入的为准。
//
// 丢弃项（Chat 有、Anthropic 无等价表达）：logprobs、system_fingerprint、
// service_tier，以及 DeepSeek 系上游的 reasoning_content（思维链没有对应的
// Anthropic block——凭空造一个缺 signature 的 thinking block 会让严格客户端报错）。
func ChatToAnthropic(chatResp []byte, model string) ([]byte, error) {
	var cr anthropicChatResponse
	if err := json.Unmarshal(chatResp, &cr); err != nil {
		return nil, fmt.Errorf("anthropic: parse chat response: %w", err)
	}

	var (
		text   string
		calls  []anthropicChatCall
		finish string
	)
	if len(cr.Choices) > 0 {
		ch := cr.Choices[0]
		text = anthropicTextFromChatContent(ch.Message.Content)
		calls = ch.Message.ToolCalls
		finish = ch.FinishReason
	}

	blocks := make([]AnthropicContentBlock, 0, len(calls)+1)
	if text != "" {
		blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: text})
	}
	for _, c := range calls {
		blocks = append(blocks, AnthropicContentBlock{
			Type:  "tool_use",
			ID:    c.ID,
			Name:  c.Function.Name,
			Input: anthropicInputFromArguments(c.Function.Arguments),
		})
	}
	if len(blocks) == 0 {
		// Anthropic 不允许空 content 数组：纯空回复也要给一个空 text 块。
		blocks = append(blocks, AnthropicContentBlock{Type: "text", Text: ""})
	}

	out := AnthropicResponse{
		ID:           anthropicMessageID(cr.ID, model, chatResp),
		Type:         "message",
		Role:         "assistant",
		Model:        model,
		Content:      blocks,
		StopReason:   anthropicStopReason(finish, len(calls) > 0),
		StopSequence: nil, // Chat 不回具体命中的停止词，只能给 null
		Usage: AnthropicUsage{
			InputTokens:  cr.Usage.PromptTokens,
			OutputTokens: cr.Usage.CompletionTokens,
		},
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal response: %w", err)
	}
	return raw, nil
}

// anthropicChatResponse 是上游 Chat 响应里我们关心字段的投影。
type anthropicChatResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role      string              `json:"role"`
			Content   json.RawMessage     `json:"content"`
			ToolCalls []anthropicChatCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// anthropicTextFromChatContent 从 Chat 的 content 取纯文本。
// content 可能是字符串，也可能是 part 数组（多模态），null 则给空串。
func anthropicTextFromChatContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s, ok := anthropicRawString(raw); ok {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// anthropicInputFromArguments 把 Chat 的 arguments 字符串还原成 Anthropic 的
// input 对象。合法 JSON 对象原样内嵌；空/非法/非对象一律退化成空对象——Anthropic
// 的 input 必须是对象，塞字符串进去会让严格客户端解不出工具调用。
func anthropicInputFromArguments(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return json.RawMessage("{}")
	}
	var probe any
	if json.Unmarshal([]byte(trimmed), &probe) != nil {
		return json.RawMessage("{}")
	}
	if _, isObj := probe.(map[string]any); !isObj {
		return json.RawMessage("{}")
	}
	return json.RawMessage(trimmed)
}

// anthropicStopReason 映射 finish_reason。
// 显式 finish_reason 优先；为空但有 tool_calls 时判 tool_use（上游没给原因，
// 但带了工具调用，回合显然是被工具打断的）。content_filter 等无等价原因收敛到
// end_turn（Anthropic 只有 end_turn / max_tokens / tool_use / stop_sequence 几种）。
func anthropicStopReason(finish string, hasTools bool) string {
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "stop":
		return "end_turn"
	}
	if hasTools {
		return "tool_use"
	}
	return "end_turn"
}

// anthropicMessageID 生成 Anthropic 形态的消息 id（msg_ 前缀）。
// 优先沿用上游 id 的可读后缀；上游没给 id 时用「模型 + 响应体」的哈希，
// 保证同一输入得到同一 id（纯函数要求，也让重放测试可复现）。
func anthropicMessageID(chatID, model string, body []byte) string {
	if s := strings.TrimSpace(chatID); s != "" {
		if rest, ok := strings.CutPrefix(s, "chatcmpl-"); ok {
			return "msg_" + rest
		}
		return "msg_" + s
	}
	sum := sha256.Sum256(append([]byte(model+"\x00"), body...))
	return "msg_" + hex.EncodeToString(sum[:12])
}

// ---------------------------------------------------------------------------
// 流式翻译：Chat SSE -> Anthropic SSE
// ---------------------------------------------------------------------------

// AnthropicStreamState 跨分片保存流式翻译的中间态。调用方在流开始时构造一个，
// 之后每个上游分片都往同一个 state 上喂。
//
// Model / MessageID 由调用方填（MessageID 留空则用首个分片的上游 id 顶上）。
type AnthropicStreamState struct {
	Model     string
	MessageID string

	started  bool        // 是否已发 message_start
	finished bool        // 是否已发 message_stop（之后分片全部忽略）
	hasOpen  bool        // 当前是否有未收尾的 content block
	curIdx   int         // 当前打开的 block 下标
	curType  string      // 当前 block 类型（text / tool_use）
	nextIdx  int         // 下一个 block 下标
	toolIdx  map[int]int // 上游 tool_call 序号 -> Anthropic block 下标
	stop     string      // 已判定的 stop_reason
	usage    AnthropicUsage
}

// anthropicChatChunk 是上游流式分片里我们关心字段的投影。
type anthropicChatChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
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
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// AnthropicSSEFromChat 把一个上游 SSE 分片翻成 Anthropic 的 SSE 事件写进 w。
//
// 事件序列（Anthropic 官方顺序）：
//
//	message_start -> content_block_start -> content_block_delta* ->
//	content_block_stop -> message_delta -> message_stop
//
// 分片边界与事件边界不是一一对应：一个上游分片可能既带 delta 又带 finish_reason，
// 也可能一个事件要跨两个分片（如 [DONE] 才收尾）。state 负责把这条时间线接起来。
//
// 本期为简化实现：文本与工具调用都支持，但**同一时刻只保持一个打开的 block**——
// 开新 block 前先把上一个收尾。并行 tool_calls 交错到达时（罕见）会退化成顺序
// 输出，结构仍合法，只是 block 下标与上游 tool_call 序号不再一一对应。
func AnthropicSSEFromChat(chunk []byte, w io.Writer, state *AnthropicStreamState) error {
	if state == nil {
		return fmt.Errorf("anthropic: nil stream state")
	}
	if state.finished {
		return nil
	}
	if state.toolIdx == nil {
		state.toolIdx = map[int]int{}
	}

	for _, data := range anthropicSSEDataLines(chunk) {
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return anthropicFinishStream(w, state)
		}
		var c anthropicChatChunk
		if json.Unmarshal([]byte(data), &c) != nil {
			continue // 非 JSON 帧（注释/心跳）忽略，不打断主流
		}
		if err := anthropicApplyChunk(w, state, &c); err != nil {
			return err
		}
	}
	return nil
}

// anthropicApplyChunk 处理单个已解析的上游分片。
func anthropicApplyChunk(w io.Writer, state *AnthropicStreamState, c *anthropicChatChunk) error {
	if !state.started {
		if err := anthropicWriteMessageStart(w, state, c); err != nil {
			return err
		}
	}
	if c.Usage != nil {
		state.usage.InputTokens = c.Usage.PromptTokens
		state.usage.OutputTokens = c.Usage.CompletionTokens
	}
	if len(c.Choices) == 0 {
		// 只有 usage 的收尾帧：finish_reason 已在前面出现过就直接收尾。
		if state.stop != "" && c.Usage != nil {
			return anthropicFinishStream(w, state)
		}
		return nil
	}
	ch := c.Choices[0]

	if ch.Delta.Content != "" {
		if err := anthropicOpenBlock(w, state, "text", "", ""); err != nil {
			return err
		}
		if err := anthropicWriteEvent(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": state.curIdx,
			"delta": map[string]any{"type": "text_delta", "text": ch.Delta.Content},
		}); err != nil {
			return err
		}
	}

	for _, tc := range ch.Delta.ToolCalls {
		idx, seen := state.toolIdx[tc.Index]
		if !seen {
			if err := anthropicOpenBlock(w, state, "tool_use", tc.ID, tc.Function.Name); err != nil {
				return err
			}
			idx = state.curIdx
			state.toolIdx[tc.Index] = idx
		}
		if tc.Function.Arguments == "" {
			continue
		}
		if err := anthropicWriteEvent(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": idx,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": tc.Function.Arguments,
			},
		}); err != nil {
			return err
		}
	}

	if ch.FinishReason != nil && *ch.FinishReason != "" {
		if err := anthropicCloseBlock(w, state); err != nil {
			return err
		}
		state.stop = anthropicStopReason(*ch.FinishReason, len(state.toolIdx) > 0)
	}
	// 上游把 finish_reason 与 usage 放同一帧时（部分实现如此）直接收尾，
	// 不必等 [DONE]——否则这种流会永远停在 message_delta 之前。
	if state.stop != "" && c.Usage != nil {
		return anthropicFinishStream(w, state)
	}
	return nil
}

// anthropicWriteMessageStart 发 message_start。MessageID 优先用调用方给的，
// 否则退到上游 id（同样做 chatcmpl- 归一）。
func anthropicWriteMessageStart(w io.Writer, state *AnthropicStreamState, c *anthropicChatChunk) error {
	id := state.MessageID
	if id == "" {
		id = anthropicMessageID(c.ID, state.Model, nil)
	}
	model := state.Model
	if model == "" {
		model = c.Model
	}
	state.started = true
	return anthropicWriteEvent(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// anthropicOpenBlock 打开一个 content block。
//
// 复用规则只对 text 生效：同一段文本的多个 delta 要落进同一个 block。tool_use
// **不复用**——每一个 tool_call 都要独立成块，若在这里复用（按类型判等），
// 第二个工具调用会挤进第一个工具的块里，两个调用被合并成一个。
// 调用方用 state.toolIdx 保证同一个 tool_call 的多个分片只开一次块。
//
// 开新块前先收尾上一个——Anthropic 语义上 block 是串行的。
func anthropicOpenBlock(w io.Writer, state *AnthropicStreamState, typ, id, name string) error {
	if typ == "text" && state.hasOpen && state.curType == "text" {
		return nil
	}
	if err := anthropicCloseBlock(w, state); err != nil {
		return err
	}
	idx := state.nextIdx
	state.nextIdx++
	state.curIdx = idx
	state.curType = typ
	state.hasOpen = true

	block := map[string]any{"type": typ}
	switch typ {
	case "text":
		block["text"] = ""
	case "tool_use":
		block["id"] = id
		block["name"] = name
		block["input"] = map[string]any{}
	}
	return anthropicWriteEvent(w, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": block,
	})
}

// anthropicCloseBlock 收尾当前 block（无打开块则空操作）。
func anthropicCloseBlock(w io.Writer, state *AnthropicStreamState) error {
	if !state.hasOpen {
		return nil
	}
	err := anthropicWriteEvent(w, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": state.curIdx,
	})
	state.hasOpen = false
	return err
}

// anthropicFinishStream 收尾整条流：关掉未结束的 block，发 message_delta 与
// message_stop。幂等——重复调用不会重复发。
func anthropicFinishStream(w io.Writer, state *AnthropicStreamState) error {
	if state.finished {
		return nil
	}
	if err := anthropicCloseBlock(w, state); err != nil {
		return err
	}
	if !state.started {
		// 一个有效帧都没有就 [DONE]：仍要发一套最小骨架，客户端才不会挂等。
		if err := anthropicWriteMessageStart(w, state, &anthropicChatChunk{Model: state.Model}); err != nil {
			return err
		}
	}
	stop := state.stop
	if stop == "" {
		stop = "end_turn"
	}
	if err := anthropicWriteEvent(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": state.usage.OutputTokens},
	}); err != nil {
		return err
	}
	if err := anthropicWriteEvent(w, "message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return err
	}
	state.finished = true
	return nil
}

// anthropicWriteEvent 写一条 Anthropic SSE 事件（event: + data: 两行 + 空行）。
func anthropicWriteEvent(w io.Writer, event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("anthropic: marshal %s: %w", event, err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return fmt.Errorf("anthropic: write %s: %w", event, err)
	}
	return nil
}

// anthropicSSEDataLines 从一个（可能含多行的）SSE 分片里抽出所有 data 行的载荷。
func anthropicSSEDataLines(chunk []byte) []string {
	var out []string
	for _, line := range strings.Split(string(chunk), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	return out
}
