// mcp —— 把 wb-syan 网关暴露成 MCP server（stdio）。
//
// 设计来源：Continuum-AI-Corp/orcarouter-mcp-server 的四工具布局
// （chat / models_list / model_card / providers_list），按我们自己的网关重写：
//   - 不需要 Node 运行时，单二进制，和现有部署方式一致
//   - 工具名用 wb_ 前缀，避免和别的 MCP server 撞名
//   - 多一个 upstreams 工具：这是本网关相对普通 OpenAI 代理的实际差异
//
// 传输：MCP stdio —— 每行一条 JSON-RPC 2.0 消息。只实现 tools 能力，
// 不实现 resources/prompts（网关没有这两类对象，硬造只会误导客户端）。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	serverName    = "wb-syan-mcp"
	serverVersion = "1.0.0"
	protocolVer   = "2024-11-05"
)

// baseURL / apiKey 来自环境变量：与 wb-syan 的部署参数同源，不额外发明配置。
func baseURL() string {
	v := strings.TrimSpace(os.Getenv("WB_SYAN_BASE_URL"))
	if v == "" {
		v = "http://127.0.0.1:7863"
	}
	return strings.TrimRight(v, "/")
}

func apiKey() string { return strings.TrimSpace(os.Getenv("WB_SYAN_API_KEY")) }

// rpcRequest 一条 JSON-RPC 2.0 请求。ID 用 RawMessage：客户端可能发数字或字符串。
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func main() {
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)

	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var req rpcRequest
			if jerr := json.Unmarshal(bytes.TrimSpace(line), &req); jerr != nil {
				_ = enc.Encode(rpcResponse{
					JSONRPC: "2.0",
					Error:   &rpcError{Code: -32700, Message: "parse error: " + jerr.Error()},
				})
				out.Flush()
			} else {
				if resp := handle(req); resp != nil {
					_ = enc.Encode(resp)
					out.Flush()
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// handle 分发一条请求。返回 nil 表示这是通知（无 id），不应回复。
func handle(req rpcRequest) *rpcResponse {
	isNotification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": protocolVer,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
		}}
	case "notifications/initialized":
		return nil
	case "ping":
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	case "tools/list":
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": toolDefs()}}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return &rpcResponse{JSONRPC: "2.0", ID: req.ID,
				Error: &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}}
		}
		result := callTool(p.Name, p.Arguments)
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	default:
		if isNotification {
			return nil
		}
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32601, Message: "method not found: " + req.Method}}
	}
}

// toolDefs 工具清单。描述写清楚"什么时候该用"，客户端才能自己选对工具。
func toolDefs() []map[string]any {
	return []map[string]any{
		{
			"name":        "wb_chat",
			"description": "向 wb-syan 网关发一条单轮对话。model 用网关的模型名（如 smart / cn:glm-5.2 / qoder:auto / groq:llama-3.3-70b）。不传则用 wb_default_model 环境变量或 cn:glm-5.2。",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt":        map[string]any{"type": "string", "description": "用户消息（必填）"},
					"model":         map[string]any{"type": "string", "description": "模型名，可省略"},
					"system_prompt": map[string]any{"type": "string", "description": "可选 system 提示"},
					"temperature":   map[string]any{"type": "number", "description": "采样温度，默认 0.7"},
					"max_tokens":    map[string]any{"type": "integer", "description": "最大生成 token，默认 4096"},
				},
				"required":             []string{"prompt"},
				"additionalProperties": false,
			},
		},
		{
			"name":        "wb_models",
			"description": "列出 wb-syan 网关当前可用的全部模型（含 cn:/global:/qoder:/<provider>: 前缀）。",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{},
				"additionalProperties": false},
		},
		{
			"name":        "wb_upstreams",
			"description": "查看网关连了哪些上游（WorkBuddy 账号池 / Qoder PAT 池 / 通用 OpenAI 兼容 provider），以及路由策略（别名、降级链、时段）。",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{},
				"additionalProperties": false},
		},
	}
}

// callTool 执行工具，统一返回 MCP 的 content 结构。
func callTool(name string, args json.RawMessage) map[string]any {
	switch name {
	case "wb_chat":
		var a struct {
			Prompt       string  `json:"prompt"`
			Model        string  `json:"model"`
			SystemPrompt string  `json:"system_prompt"`
			Temperature  float64 `json:"temperature"`
			MaxTokens    int     `json:"max_tokens"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if strings.TrimSpace(a.Prompt) == "" {
			return errResult("prompt 不能为空")
		}
		model := strings.TrimSpace(a.Model)
		if model == "" {
			model = strings.TrimSpace(os.Getenv("WB_DEFAULT_MODEL"))
		}
		if model == "" {
			model = "cn:glm-5.2"
		}
		temp := a.Temperature
		if temp == 0 {
			temp = 0.7
		}
		maxTok := a.MaxTokens
		if maxTok <= 0 {
			maxTok = 4096
		}
		msgs := []map[string]any{}
		if s := strings.TrimSpace(a.SystemPrompt); s != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": s})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": a.Prompt})
		body := map[string]any{
			"model": model, "messages": msgs, "stream": false,
			"temperature": temp, "max_tokens": maxTok,
		}
		raw, status, err := gatewayPost("/v1/chat/completions", body)
		if err != nil {
			return errResult(err.Error())
		}
		if status < 200 || status >= 300 {
			return errResult(fmt.Sprintf("网关返回 %d: %s", status, truncate(string(raw), 500)))
		}
		var resp struct {
			Choices []struct {
				Message struct {
					Content any `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return errResult("解析响应失败: " + err.Error())
		}
		text := ""
		if len(resp.Choices) > 0 {
			text = stringify(resp.Choices[0].Message.Content)
		}
		return textResult(text)
	case "wb_models":
		raw, status, err := gatewayGet("/v1/models")
		if err != nil {
			return errResult(err.Error())
		}
		if status < 200 || status >= 300 {
			return errResult(fmt.Sprintf("网关返回 %d: %s", status, truncate(string(raw), 500)))
		}
		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return errResult("解析模型列表失败: " + err.Error())
		}
		lines := make([]string, 0, len(resp.Data))
		for _, m := range resp.Data {
			lines = append(lines, m.ID)
		}
		if len(lines) == 0 {
			return textResult("（网关当前没有可用模型：可能是账号池为空或上游未配置）")
		}
		return textResult(strings.Join(lines, "\n"))
	case "wb_upstreams":
		raw, status, err := gatewayGet("/status")
		if err != nil {
			return errResult(err.Error())
		}
		if status < 200 || status >= 300 {
			return errResult(fmt.Sprintf("网关返回 %d: %s", status, truncate(string(raw), 500)))
		}
		var resp struct {
			Upstreams []map[string]any `json:"upstreams"`
			Routing   map[string]any   `json:"routing"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return errResult("解析 /status 失败: " + err.Error())
		}
		pretty, _ := json.MarshalIndent(map[string]any{
			"upstreams": resp.Upstreams,
			"routing":   resp.Routing,
		}, "", "  ")
		return textResult(string(pretty))
	default:
		return errResult("unknown tool: " + name)
	}
}

// gatewayGet / gatewayPost 是这一层唯一的 HTTP 出口：统一带鉴权与超时。
func gatewayGet(path string) ([]byte, int, error) {
	return gatewayDo(http.MethodGet, path, nil)
}

func gatewayPost(path string, body any) ([]byte, int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	return gatewayDo(http.MethodPost, path, raw)
}

func gatewayDo(method, path string, body []byte) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL()+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	if k := apiKey(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("连接网关失败（%s）: %w", baseURL(), err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return raw, resp.StatusCode, nil
}

// textResult / errResult 是 MCP 的标准返回形状。
func textResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": false,
	}
}

func errResult(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(raw)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
