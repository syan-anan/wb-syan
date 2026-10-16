// responses_test.go —— /v1/responses 接线端到端测试。
//
// 重点锁两件事：
//  1. 请求投影（instructions / input 项 / tools / reasoning）真的翻成了 Chat 形态；
//  2. 流式事件的字段齐全 —— response.output_text.delta 缺 item_id、response.completed
//     缺 usage 都是"静默丢数据"那类故障（客户端逐条丢弃，不报错），必须有回归测试钉住。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/upstream"
)

// TestResponsesToChatProjects 请求投影：instructions / input 项 / tools / reasoning。
func TestResponsesToChatProjects(t *testing.T) {
	body := `{
	  "model": "glm-5.2",
	  "instructions": "你是助手",
	  "input": [
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"在的"}]},
	    {"type":"function_call","call_id":"call_1","name":"shell","arguments":"ls"},
	    {"type":"function_call_output","call_id":"call_1","output":"file.txt"},
	    {"type":"message","role":"user","content":"继续"}
	  ],
	  "tools": [{"type":"function","name":"shell","description":"跑命令","parameters":{"type":"object"}}],
	  "reasoning": {"effort":"high","summary":"auto"},
	  "max_output_tokens": 128,
	  "stream": true
	}`
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := ResponsesToChat(&req)
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	var got struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		MaxToken int    `json:"max_tokens"`
		Effort   string `json:"reasoning_effort"`
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是 JSON: %v %s", err, out)
	}
	if got.Model != "glm-5.2" || !got.Stream || got.MaxToken != 128 || got.Effort != "high" {
		t.Errorf("标量字段映射不对: %+v", got)
	}
	wantRoles := []string{"system", "user", "assistant", "tool", "user"}
	if len(got.Messages) != len(wantRoles) {
		t.Fatalf("消息数=%d want %d: %+v", len(got.Messages), len(wantRoles), got.Messages)
	}
	for i, r := range wantRoles {
		if got.Messages[i].Role != r {
			t.Errorf("#%d role=%q want %q", i, got.Messages[i].Role, r)
		}
	}
	// assistant 正文与紧随的 function_call 必须合并进同一条消息（顺序错了上游 400）。
	if got.Messages[2].Content != "在的" || len(got.Messages[2].ToolCalls) != 1 {
		t.Errorf("assistant 项未正确合并: %+v", got.Messages[2])
	}
	if got.Messages[2].ToolCalls[0].Function.Name != "shell" {
		t.Errorf("tool_call 名字丢了: %+v", got.Messages[2].ToolCalls[0])
	}
	if got.Messages[2].ToolCalls[0].Function.Arguments != "ls" {
		t.Errorf("tool_call 参数丢了: %+v", got.Messages[2].ToolCalls[0])
	}
	if got.Messages[3].ToolCallID != "call_1" || got.Messages[3].Content != "file.txt" {
		t.Errorf("tool 结果未落位: %+v", got.Messages[3])
	}
	if len(got.Tools) != 1 || got.Tools[0].Type != "function" || got.Tools[0].Function.Name != "shell" {
		t.Errorf("tools 未改成 Chat 嵌套格式: %+v", got.Tools)
	}
}

// TestResponsesToChatSimpleString input 直接给字符串时退化成单条 user 消息。
func TestResponsesToChatSimpleString(t *testing.T) {
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(`{"model":"m","input":"hi"}`), &req); err != nil {
		t.Fatal(err)
	}
	out, err := ResponsesToChat(&req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(out, &got)
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" || got.Messages[0].Content != "hi" {
		t.Fatalf("messages=%+v", got.Messages)
	}
}

// responsesHarness 单账号池 + fake upstream 的 handler。
func responsesHarness(t *testing.T, up *upstream.Client) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
}

// TestResponsesNonStreamTranslates 非流式：Responses 进、Responses 对象出。
func TestResponsesNonStreamTranslates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := responsesHarness(t, up)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"glm-5.2","input":"hi","max_output_tokens":32}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Model  string `json:"model"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v %s", err, rec.Body)
	}
	if resp.Object != "response" || resp.Status != "completed" || resp.Model != "glm-5.2" {
		t.Errorf("信封字段=%+v", resp)
	}
	if len(resp.Output) == 0 || resp.Output[0].Type != "message" ||
		len(resp.Output[0].Content) == 0 || resp.Output[0].Content[0].Text != "你好" {
		t.Errorf("output=%+v", resp.Output)
	}
	if _, ok := resp.Usage["input_tokens"].(float64); !ok {
		t.Errorf("usage.input_tokens 必须是数字: %+v", resp.Usage)
	}
}

// TestResponsesStreamTranslates 流式：事件序列 + 必填字段（item_id / usage）。
func TestResponsesStreamTranslates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := responsesHarness(t, up)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(
		`{"model":"glm-5.2","input":"hi","stream":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"response.created", "response.output_item.added", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(body, want) {
			t.Errorf("流里缺 %s | %s", want, body)
		}
	}
	// item_id 是 output_text.delta 的必填字段：缺了客户端会丢掉整条正文。
	for _, line := range strings.Split(body, string(rune(10))) {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type   string `json:"type"`
			ItemID string `json:"item_id"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			t.Fatalf("事件不是 JSON: %s", line)
		}
		if ev.Type == "response.output_text.delta" && ev.ItemID == "" {
			t.Errorf("output_text.delta 缺 item_id: %s", line)
		}
		if ev.Type == "response.completed" {
			var full struct {
				Response struct {
					Status string         `json:"status"`
					Usage  map[string]any `json:"usage"`
				} `json:"response"`
			}
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &full)
			if _, ok := full.Response.Usage["output_tokens"].(float64); !ok {
				t.Errorf("completed.response.usage.output_tokens 必须是数字: %s", line)
			}
		}
	}
	if strings.Contains(body, "response.content_part") {
		t.Errorf("不该发可选扩展事件（严格客户端会判非法）: %s", body)
	}
}
