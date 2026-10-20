// qoder_cn.go —— Qoder 国内版（qoder.com.cn / gateway.qoder.com.cn）推理链路。
//
// 与 qoder.go 的国际版链路并列，互不影响：
//
//	国际版  openapi.qoder.sh 换 jt → api2-v2.qoder.sh/model/v1/chat/completions（OpenAI 原生）
//	国内版  openapi.qoder.com.cn 换 jt → gateway.qoder.com.cn/algo/.../agent_chat_generation（COSY 签名 + SSE 信封）
//
// 两条链路只在 QoderClient 内部按 Realm 分派，账号池与选号逻辑共用。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	cryptorand "crypto/rand"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// qoderCNBase 国内版统一网关。api1/api2/api3/center 在国内版合并到这一个域名。
const qoderCNBase = "https://gateway.qoder.com.cn"

// qoderCNChatPath 推理入口。Encode=1 是「body 为自定义 base64」的开关，
// 缺了它服务端会按普通 JSON 解析并回 500。签名时 path 不含 query。
const qoderCNChatPath = "/algo/api/v2/service/pro/sse/agent_chat_generation"

// qoderCNChatQuery 必须原样携带。
const qoderCNChatQuery = "FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

// qoderCNModelsPath 模型清单（GET 必须带 {} body 一起签名，否则 403）。
const qoderCNModelsPath = "/algo/api/v2/model/list"

//go:embed qoder_baseprompt.json
var qoderBasePromptJSON []byte

var (
	qoderBasePromptOnce sync.Once
	qoderBasePrompt     map[string]any
	qoderBasePromptErr  error
)

func qoderBasePromptTemplate() (map[string]any, error) {
	qoderBasePromptOnce.Do(func() {
		qoderBasePromptErr = json.Unmarshal(qoderBasePromptJSON, &qoderBasePrompt)
	})
	if qoderBasePromptErr != nil {
		return nil, qoderBasePromptErr
	}
	// 深拷贝：每次请求都要改 id / messages，不能污染模板。
	raw, err := json.Marshal(qoderBasePrompt)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// qoderCNRequest 是从 OpenAI 请求里抽出的最小字段集。
type qoderCNRequest struct {
	Model    string            `json:"model"`
	Messages []json.RawMessage `json:"messages"`
	Stream   bool              `json:"stream"`
}

// buildQoderCNBody 把 OpenAI 请求体改写成 agent_chat_generation 的骨架。
// 模板自带的 system 提示词保留（那是官方 Qoder 人设），只替换会话轮次。
func buildQoderCNBody(openaiBody []byte, modelKey, userType string) ([]byte, error) {
	var req qoderCNRequest
	if err := json.Unmarshal(openaiBody, &req); err != nil {
		return nil, fmt.Errorf("qoder-cn: 解析请求失败: %w", err)
	}
	tpl, err := qoderBasePromptTemplate()
	if err != nil {
		return nil, err
	}
	nid := qoderUUID()
	tpl["request_id"] = nid
	tpl["chat_record_id"] = nid
	tpl["request_set_id"] = qoderUUID()
	tpl["session_id"] = qoderUUID()
	tpl["stream"] = true // 上游只支持流式，非流式由本地聚合
	if userType != "" {
		tpl["aliyun_user_type"] = userType
	}
	tpl["tools"] = []any{}

	if mc, ok := tpl["model_config"].(map[string]any); ok {
		mc["key"] = modelKey
		mc["display_name"] = modelKey
		mc["model"] = ""
		mc["format"] = "openai"
	} else {
		tpl["model_config"] = map[string]any{
			"key": modelKey, "display_name": modelKey, "model": "",
			"format": "openai", "is_vl": false, "is_reasoning": false,
		}
	}

	msgs := make([]any, 0, len(req.Messages))
	for _, raw := range req.Messages {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		msgs = append(msgs, m)
	}
	if len(msgs) > 0 {
		tpl["messages"] = msgs
	}
	return marshalNoHTMLEscapeAny(tpl)
}

func marshalNoHTMLEscapeAny(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// qoderCNChat 发一次国内版推理，返回已解掉 SSE 信封的流。
// 返回的 reader 产出的就是标准 OpenAI SSE（data: {...}\n\n）。
func (c *QoderClient) qoderCNChat(ctx context.Context, openaiBody []byte, modelKey string) (io.ReadCloser, int, []byte, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, 0, nil, err
	}
	sess, err := c.cosyFor(token)
	if err != nil {
		return nil, 0, nil, err
	}
	raw, err := buildQoderCNBody(openaiBody, modelKey, c.UserType)
	if err != nil {
		return nil, 0, nil, err
	}
	encoded := qoderEncode(raw)
	url := c.cnBase() + qoderCNChatPath + "?" + qoderCNChatQuery
	headers, err := sess.headers(encoded, url, modelKey, true)
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(encoded))
	if err != nil {
		return nil, 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, body, fmt.Errorf("qoder-cn: 上游 %d", resp.StatusCode)
	}
	if !qoderWantsStream(openaiBody) {
		// 上游只有流式接口；非流式请求在这里聚合，客户端拿到的仍是单个 JSON。
		agg, err := qoderCNAggregate(resp.Body, modelKey)
		resp.Body.Close()
		if err != nil {
			return nil, 0, nil, err
		}
		return io.NopCloser(bytes.NewReader(agg)), http.StatusOK, nil, nil
	}
	return newQoderCNEnvelopeReader(resp.Body), resp.StatusCode, nil, nil
}

// qoderWantsStream 读客户端请求里的 stream 字段（缺省 false）。
func qoderWantsStream(body []byte) bool {
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Stream
}

// qoderCNAggregate 把整条信封流合并成一个 chat.completion。
// 只拼接 delta.content / delta.reasoning_content / tool_calls 的增量，
// 其余字段（id/created/model）取第一帧。
func qoderCNAggregate(src io.Reader, modelKey string) ([]byte, error) {
	br := bufio.NewReaderSize(src, 64*1024)
	var content, reasoning strings.Builder
	id, model := "", modelKey
	created := time.Now().Unix()
	var usage any
	var toolCalls []map[string]any
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			chunk, ok := qoderCNUnwrap(payload)
			if ok {
				var c struct {
					ID      string `json:"id"`
					Created int64  `json:"created"`
					Model   string `json:"model"`
					Usage   any    `json:"usage"`
					Choices []struct {
						Delta struct {
							Content          string           `json:"content"`
							ReasoningContent string           `json:"reasoning_content"`
							ToolCalls        []map[string]any `json:"tool_calls"`
						} `json:"delta"`
						Message struct {
							Content          string           `json:"content"`
							ReasoningContent string           `json:"reasoning_content"`
							ToolCalls        []map[string]any `json:"tool_calls"`
						} `json:"message"`
					} `json:"choices"`
				}
				if json.Unmarshal(chunk, &c) == nil {
					if id == "" {
						id = c.ID
					}
					if model == "" || model == "auto" {
						if c.Model != "" {
							model = c.Model
						}
					}
					if c.Created > 0 {
						created = c.Created
					}
					if c.Usage != nil {
						usage = c.Usage
					}
					for _, ch := range c.Choices {
						content.WriteString(ch.Delta.Content)
						content.WriteString(ch.Message.Content)
						reasoning.WriteString(ch.Delta.ReasoningContent)
						if len(ch.Delta.ToolCalls) > 0 {
							toolCalls = append(toolCalls, ch.Delta.ToolCalls...)
						}
						if len(ch.Message.ToolCalls) > 0 {
							toolCalls = append(toolCalls, ch.Message.ToolCalls...)
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": "stop"}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return marshalNoHTMLEscapeAny(out)
}

// cosyFor 取（或建）账号的签名会话；access token 变了就重建。
func (c *QoderClient) cosyFor(token string) (*cosySession, error) {
	c.cosyMu.Lock()
	defer c.cosyMu.Unlock()
	if c.cosy != nil && c.cosyToken == token {
		return c.cosy, nil
	}
	uid := c.UID
	if uid == "" {
		uid = c.PAT
	}
	s, err := newCosySession(uid, c.Nickname, token, c.refreshToken, c.UserType, c.OrgID, c.OrgName, "")
	if err != nil {
		return nil, err
	}
	c.cosy = s
	c.cosyToken = token
	return s, nil
}

func (c *QoderClient) cnBase() string {
	if c.CNBase != "" {
		return strings.TrimRight(c.CNBase, "/")
	}
	return qoderCNBase
}

// qoderCNEnvelopeReader 把上游的信封流拆回裸 OpenAI SSE。
//
// 上游每帧形如：
//
//	data:{"headers":{...},"body":"{\"choices\":[...]}","statusCodeValue":200}
//
// 我们只取 body 字段，原样转发；上游结束时不发 [DONE]，这里补一个。
type qoderCNEnvelopeReader struct {
	src      io.ReadCloser
	br       *bufio.Reader
	pending  []byte
	sentDone bool
}

func newQoderCNEnvelopeReader(src io.ReadCloser) *qoderCNEnvelopeReader {
	return &qoderCNEnvelopeReader{src: src, br: bufio.NewReaderSize(src, 64*1024)}
}

func (r *qoderCNEnvelopeReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.sentDone {
			return 0, io.EOF
		}
		line, err := r.br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if line != "" && strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if chunk, ok := qoderCNUnwrap(payload); ok {
				r.pending = append(r.pending, []byte("data: ")...)
				r.pending = append(r.pending, chunk...)
				r.pending = append(r.pending, '\n', '\n')
			}
		}
		if err != nil {
			r.sentDone = true
			if len(r.pending) == 0 {
				r.pending = []byte("data: [DONE]\n\n")
			}
			break
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *qoderCNEnvelopeReader) Close() error { return r.src.Close() }

// qoderCNUnwrap 取信封里的 body；statusCodeValue 非 2xx 时转成一条错误 chunk。
func qoderCNUnwrap(payload string) ([]byte, bool) {
	if payload == "" || payload == "[DONE]" {
		return nil, false
	}
	var env struct {
		Body            string          `json:"body"`
		StatusCodeValue json.RawMessage `json:"statusCodeValue"`
	}
	if err := json.Unmarshal([]byte(payload), &env); err != nil {
		return nil, false
	}
	if env.Body == "" {
		return nil, false
	}
	if len(env.StatusCodeValue) > 0 {
		var code int
		if json.Unmarshal(env.StatusCodeValue, &code) == nil && code >= 400 {
			return qoderCNErrorChunk(fmt.Sprintf("上游状态 %d", code)), true
		}
	}
	return []byte(env.Body), true
}

func qoderCNErrorChunk(msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": "upstream_error"},
	})
	return b
}

// qoderUUID 生成 v4 UUID 字符串。
func qoderUUID() string {
	b := make([]byte, 16)
	if _, err := cryptorand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
