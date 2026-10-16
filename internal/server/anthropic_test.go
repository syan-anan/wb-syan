package server

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 断言小工具（全部加 anthr 前缀，避免与包内既有测试 helper 撞名）
// ---------------------------------------------------------------------------

func anthrObj(t *testing.T, v any, path string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: want object, got %T", path, v)
	}
	return m
}

func anthrArr(t *testing.T, v any, path string) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: want array, got %T", path, v)
	}
	return a
}

func anthrStr(t *testing.T, v any, path string) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s: want string, got %T (%v)", path, v, v)
	}
	return s
}

func anthrNum(t *testing.T, v any, path string) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("%s: want number, got %T (%v)", path, v, v)
	}
	return f
}

func anthrDecode(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode json: %v (raw=%s)", err, raw)
	}
	return m
}

func anthrF(f float64) *float64 { return &f }

// anthrEvents 抽出 SSE 输出里的 event 名序列。
func anthrEvents(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			out = append(out, name)
		}
	}
	return out
}

func anthrWantEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event count = %d, want %d\ngot=%v\nwant=%v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q\ngot=%v", i, got[i], want[i], got)
		}
	}
}

// ---------------------------------------------------------------------------
// 请求翻译
// ---------------------------------------------------------------------------

func TestAnthropicRequestSystemAndMultiTurn(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: 2048,
		System:    json.RawMessage(`"be terse"`),
		Messages: []AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`"hello"`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"again"}]`)},
		},
		Temperature:   anthrF(0.2),
		StopSequences: []string{"</end>"},
	}

	raw, err := AnthropicToChat(req)
	if err != nil {
		t.Fatalf("AnthropicToChat: %v", err)
	}
	got := anthrDecode(t, raw)

	if m := anthrStr(t, got["model"], "model"); m != "claude-sonnet-4" {
		t.Fatalf("model = %q", m)
	}
	if mt := anthrNum(t, got["max_tokens"], "max_tokens"); mt != 2048 {
		t.Fatalf("max_tokens = %v", mt)
	}
	if tp := anthrNum(t, got["temperature"], "temperature"); tp != 0.2 {
		t.Fatalf("temperature = %v", tp)
	}
	stop := anthrArr(t, got["stop"], "stop")
	if len(stop) != 1 || anthrStr(t, stop[0], "stop[0]") != "</end>" {
		t.Fatalf("stop = %v", stop)
	}

	msgs := anthrArr(t, got["messages"], "messages")
	want := []struct{ role, content string }{
		{"system", "be terse"},
		{"user", "hi"},
		{"assistant", "hello"},
		{"user", "again"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("messages len = %d, want %d (got=%v)", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		m := anthrObj(t, msgs[i], "message")
		if r := anthrStr(t, m["role"], "role"); r != w.role {
			t.Fatalf("messages[%d].role = %q, want %q", i, r, w.role)
		}
		if c := anthrStr(t, m["content"], "content"); c != w.content {
			t.Fatalf("messages[%d].content = %q, want %q", i, c, w.content)
		}
	}
}

func TestAnthropicRequestToolUseAndToolResult(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: 512,
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: json.RawMessage(
				`[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}]`)},
			{Role: "user", Content: json.RawMessage(
				`[{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}]`)},
		},
		Tools: []AnthropicTool{{
			Name:        "get_weather",
			Description: "look up weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
		ToolChoice: json.RawMessage(`{"type":"any"}`),
	}

	raw, err := AnthropicToChat(req)
	if err != nil {
		t.Fatalf("AnthropicToChat: %v", err)
	}
	got := anthrDecode(t, raw)

	// tool_choice：Anthropic 的 any -> Chat 的 required
	if tc := anthrStr(t, got["tool_choice"], "tool_choice"); tc != "required" {
		t.Fatalf("tool_choice = %q, want required", tc)
	}

	// tools[].input_schema -> tools[].function.parameters
	tools := anthrArr(t, got["tools"], "tools")
	if len(tools) != 1 {
		t.Fatalf("tools len = %d", len(tools))
	}
	fn := anthrObj(t, anthrObj(t, tools[0], "tools[0]")["function"], "tools[0].function")
	if name := anthrStr(t, fn["name"], "function.name"); name != "get_weather" {
		t.Fatalf("function.name = %q", name)
	}
	params := anthrObj(t, fn["parameters"], "function.parameters")
	if typ := anthrStr(t, params["type"], "parameters.type"); typ != "object" {
		t.Fatalf("parameters.type = %q", typ)
	}

	msgs := anthrArr(t, got["messages"], "messages")
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2 (got=%v)", len(msgs), msgs)
	}

	asst := anthrObj(t, msgs[0], "messages[0]")
	if r := anthrStr(t, asst["role"], "messages[0].role"); r != "assistant" {
		t.Fatalf("messages[0].role = %q", r)
	}
	calls := anthrArr(t, asst["tool_calls"], "messages[0].tool_calls")
	if len(calls) != 1 {
		t.Fatalf("tool_calls len = %d", len(calls))
	}
	call := anthrObj(t, calls[0], "tool_calls[0]")
	if id := anthrStr(t, call["id"], "tool_calls[0].id"); id != "toolu_1" {
		t.Fatalf("tool_calls[0].id = %q", id)
	}
	cfn := anthrObj(t, call["function"], "tool_calls[0].function")
	if n := anthrStr(t, cfn["name"], "function.name"); n != "get_weather" {
		t.Fatalf("tool name = %q", n)
	}
	if a := anthrStr(t, cfn["arguments"], "function.arguments"); a != `{"city":"SF"}` {
		t.Fatalf("arguments = %q", a)
	}

	toolMsg := anthrObj(t, msgs[1], "messages[1]")
	if r := anthrStr(t, toolMsg["role"], "messages[1].role"); r != "tool" {
		t.Fatalf("messages[1].role = %q, want tool", r)
	}
	if id := anthrStr(t, toolMsg["tool_call_id"], "tool_call_id"); id != "toolu_1" {
		t.Fatalf("tool_call_id = %q", id)
	}
	if c := anthrStr(t, toolMsg["content"], "tool content"); c != "sunny" {
		t.Fatalf("tool content = %q", c)
	}
}

func TestAnthropicRequestImageBlock(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: 64,
		Messages: []AnthropicMessage{{
			Role: "user",
			Content: json.RawMessage(
				`[{"type":"text","text":"what is this"},` +
					`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}]`),
		}},
	}

	raw, err := AnthropicToChat(req)
	if err != nil {
		t.Fatalf("AnthropicToChat: %v", err)
	}
	got := anthrDecode(t, raw)

	msgs := anthrArr(t, got["messages"], "messages")
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d", len(msgs))
	}
	msg := anthrObj(t, msgs[0], "messages[0]")
	// 带图片时必须保持数组形态（单文本才能折叠成字符串）
	parts := anthrArr(t, msg["content"], "messages[0].content")
	if len(parts) != 2 {
		t.Fatalf("content parts len = %d, want 2", len(parts))
	}
	if typ := anthrStr(t, anthrObj(t, parts[0], "part[0]")["type"], "part[0].type"); typ != "text" {
		t.Fatalf("part[0].type = %q", typ)
	}
	p1 := anthrObj(t, parts[1], "part[1]")
	if typ := anthrStr(t, p1["type"], "part[1].type"); typ != "image_url" {
		t.Fatalf("part[1].type = %q", typ)
	}
	iu := anthrObj(t, p1["image_url"], "part[1].image_url")
	if url := anthrStr(t, iu["url"], "image_url.url"); url != "data:image/png;base64,aGVsbG8=" {
		t.Fatalf("image_url.url = %q", url)
	}
}

func TestAnthropicToolChoiceToChat(t *testing.T) {
	cases := []struct {
		in   string
		want string // 期望的字符串形态；"obj:" 前缀表示对象
	}{
		{`{"type":"auto"}`, "auto"},
		{`{"type":"any"}`, "required"},
		{`{"type":"none"}`, "none"},
		{`{"type":"tool","name":"get_weather"}`, "obj:get_weather"},
		{`{"type":"tool"}`, ""}, // 缺 name：整个字段省略
	}
	for _, c := range cases {
		req := &AnthropicRequest{
			Model:      "m",
			MaxTokens:  8,
			Messages:   []AnthropicMessage{{Role: "user", Content: json.RawMessage(`"x"`)}},
			ToolChoice: json.RawMessage(c.in),
		}
		raw, err := AnthropicToChat(req)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		got := anthrDecode(t, raw)
		tc, present := got["tool_choice"]
		if c.want == "" {
			if present {
				t.Fatalf("%s: tool_choice should be omitted, got %v", c.in, tc)
			}
			continue
		}
		if strings.HasPrefix(c.want, "obj:") {
			o := anthrObj(t, tc, "tool_choice")
			fn := anthrObj(t, o["function"], "tool_choice.function")
			if n := anthrStr(t, fn["name"], "tool_choice.function.name"); n != strings.TrimPrefix(c.want, "obj:") {
				t.Fatalf("%s: name = %q", c.in, n)
			}
			continue
		}
		if s := anthrStr(t, tc, "tool_choice"); s != c.want {
			t.Fatalf("%s: tool_choice = %q, want %q", c.in, s, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 响应翻译
// ---------------------------------------------------------------------------

func TestAnthropicResponseFromChatWithToolCalls(t *testing.T) {
	chat := `{
		"id":"chatcmpl-abc123",
		"choices":[{
			"index":0,
			"message":{
				"role":"assistant",
				"content":"checking the weather",
				"tool_calls":[{"id":"call_1","type":"function",
					"function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]
			},
			"finish_reason":"tool_calls"
		}],
		"usage":{"prompt_tokens":11,"completion_tokens":7}
	}`

	raw, err := ChatToAnthropic([]byte(chat), "claude-sonnet-4")
	if err != nil {
		t.Fatalf("ChatToAnthropic: %v", err)
	}
	got := anthrDecode(t, raw)

	if typ := anthrStr(t, got["type"], "type"); typ != "message" {
		t.Fatalf("type = %q", typ)
	}
	if r := anthrStr(t, got["role"], "role"); r != "assistant" {
		t.Fatalf("role = %q", r)
	}
	if m := anthrStr(t, got["model"], "model"); m != "claude-sonnet-4" {
		t.Fatalf("model = %q", m)
	}
	if id := anthrStr(t, got["id"], "id"); id != "msg_abc123" {
		t.Fatalf("id = %q, want msg_abc123", id)
	}
	if sr := anthrStr(t, got["stop_reason"], "stop_reason"); sr != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", sr)
	}
	if _, ok := got["stop_sequence"]; !ok {
		t.Fatalf("stop_sequence key must be present (null)")
	}
	if got["stop_sequence"] != nil {
		t.Fatalf("stop_sequence = %v, want null", got["stop_sequence"])
	}

	usage := anthrObj(t, got["usage"], "usage")
	if v := anthrNum(t, usage["input_tokens"], "input_tokens"); v != 11 {
		t.Fatalf("input_tokens = %v", v)
	}
	if v := anthrNum(t, usage["output_tokens"], "output_tokens"); v != 7 {
		t.Fatalf("output_tokens = %v", v)
	}

	content := anthrArr(t, got["content"], "content")
	if len(content) != 2 {
		t.Fatalf("content len = %d, want 2 (got=%v)", len(content), content)
	}
	txt := anthrObj(t, content[0], "content[0]")
	if typ := anthrStr(t, txt["type"], "content[0].type"); typ != "text" {
		t.Fatalf("content[0].type = %q", typ)
	}
	if s := anthrStr(t, txt["text"], "content[0].text"); s != "checking the weather" {
		t.Fatalf("content[0].text = %q", s)
	}
	tu := anthrObj(t, content[1], "content[1]")
	if typ := anthrStr(t, tu["type"], "content[1].type"); typ != "tool_use" {
		t.Fatalf("content[1].type = %q", typ)
	}
	if id := anthrStr(t, tu["id"], "content[1].id"); id != "call_1" {
		t.Fatalf("content[1].id = %q", id)
	}
	if n := anthrStr(t, tu["name"], "content[1].name"); n != "get_weather" {
		t.Fatalf("content[1].name = %q", n)
	}
	in := anthrObj(t, tu["input"], "content[1].input")
	if city := anthrStr(t, in["city"], "input.city"); city != "SF" {
		t.Fatalf("input.city = %q", city)
	}
}

func TestAnthropicResponseStopReasonMapping(t *testing.T) {
	cases := []struct {
		finish string
		want   string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"content_filter", "end_turn"},
		{"", "end_turn"},
	}
	for _, c := range cases {
		chat := `{"id":"chatcmpl-z","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"` + c.finish + `"}],"usage":{}}`
		raw, err := ChatToAnthropic([]byte(chat), "m")
		if err != nil {
			t.Fatalf("finish=%q: %v", c.finish, err)
		}
		got := anthrDecode(t, raw)
		if sr := anthrStr(t, got["stop_reason"], "stop_reason"); sr != c.want {
			t.Fatalf("finish=%q: stop_reason = %q, want %q", c.finish, sr, c.want)
		}
	}
}

func TestAnthropicResponseEmptyContentStillHasTextBlock(t *testing.T) {
	chat := `{"id":"chatcmpl-e","choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"stop"}],"usage":{}}`
	raw, err := ChatToAnthropic([]byte(chat), "m")
	if err != nil {
		t.Fatalf("ChatToAnthropic: %v", err)
	}
	got := anthrDecode(t, raw)
	content := anthrArr(t, got["content"], "content")
	if len(content) != 1 {
		t.Fatalf("content len = %d, want 1", len(content))
	}
	b := anthrObj(t, content[0], "content[0]")
	if typ := anthrStr(t, b["type"], "content[0].type"); typ != "text" {
		t.Fatalf("content[0].type = %q", typ)
	}
	if s := anthrStr(t, b["text"], "content[0].text"); s != "" {
		t.Fatalf("content[0].text = %q, want empty", s)
	}
}

// ---------------------------------------------------------------------------
// 流式翻译
// ---------------------------------------------------------------------------

func TestAnthropicStreamTextSequence(t *testing.T) {
	chunks := []string{
		`data: {"id":"chatcmpl-x","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}` + "\n\n",
		"data: [DONE]\n\n",
	}

	var buf bytes.Buffer
	st := &AnthropicStreamState{Model: "m"}
	for i, c := range chunks {
		if err := AnthropicSSEFromChat([]byte(c), &buf, st); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}

	out := buf.String()
	anthrWantEvents(t, anthrEvents(out),
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	)
	if !strings.Contains(out, `"text":"Hel"`) || !strings.Contains(out, `"text":"lo"`) {
		t.Fatalf("text deltas missing:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Fatalf("stop_reason missing:\n%s", out)
	}
	if !strings.Contains(out, `"output_tokens":2`) {
		t.Fatalf("usage missing:\n%s", out)
	}
}

func TestAnthropicStreamToolUseSequence(t *testing.T) {
	chunks := []string{
		`data: {"id":"chatcmpl-t","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}

	var buf bytes.Buffer
	st := &AnthropicStreamState{Model: "m"}
	for i, c := range chunks {
		if err := AnthropicSSEFromChat([]byte(c), &buf, st); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}

	out := buf.String()
	anthrWantEvents(t, anthrEvents(out),
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	)
	if !strings.Contains(out, `"type":"tool_use"`) {
		t.Fatalf("tool_use block missing:\n%s", out)
	}
	if !strings.Contains(out, `"name":"get_weather"`) {
		t.Fatalf("tool name missing:\n%s", out)
	}
	if !strings.Contains(out, `"partial_json":"{\"city\":"`) {
		t.Fatalf("first input_json_delta missing:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Fatalf("stop_reason must be tool_use:\n%s", out)
	}
}

func TestAnthropicStreamOnlyDoneStillEmitsSkeleton(t *testing.T) {
	var buf bytes.Buffer
	st := &AnthropicStreamState{Model: "m"}
	if err := AnthropicSSEFromChat([]byte("data: [DONE]\n\n"), &buf, st); err != nil {
		t.Fatalf("done-only: %v", err)
	}
	anthrWantEvents(t, anthrEvents(buf.String()),
		"message_start", "message_delta", "message_stop")
}

// 两个工具调用必须各自成块——回归防线：曾经按类型判等复用 block，
// 导致第二个 tool_call 被并进第一个工具的块里。
func TestAnthropicStreamTwoToolCallsStaySeparate(t *testing.T) {
	chunks := []string{
		`data: {"id":"chatcmpl-2","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"alpha","arguments":""}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"beta","arguments":""}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}

	var buf bytes.Buffer
	st := &AnthropicStreamState{Model: "m"}
	for i, c := range chunks {
		if err := AnthropicSSEFromChat([]byte(c), &buf, st); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}

	out := buf.String()
	anthrWantEvents(t, anthrEvents(out),
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta",
		"message_stop",
	)
	if !strings.Contains(out, `"name":"alpha"`) || !strings.Contains(out, `"name":"beta"`) {
		t.Fatalf("both tool blocks expected:\n%s", out)
	}
	if !strings.Contains(out, `"id":"call_a"`) || !strings.Contains(out, `"id":"call_b"`) {
		t.Fatalf("both tool ids expected:\n%s", out)
	}
	// 两个工具块的下标必须是 0 和 1，不能合并成同一个 index。
	if !strings.Contains(out, `"content_block":{"id":"call_a","input":{},"name":"alpha","type":"tool_use"},"index":0`) {
		t.Fatalf("block 0 shape unexpected:\n%s", out)
	}
	if !strings.Contains(out, `"content_block":{"id":"call_b","input":{},"name":"beta","type":"tool_use"},"index":1`) {
		t.Fatalf("block 1 shape unexpected:\n%s", out)
	}
}
