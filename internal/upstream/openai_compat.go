// openai_compat.go —— 通用 OpenAI 兼容上游。
//
// 为什么要有这一层：TraeWork / Grok / Cline / Antigravity 这些上游对外暴露的
// 都是 OpenAI Chat Completions 形状（差别只在 base_url 和鉴权头）。为每一家
// 单独写一个适配器是重复劳动；一份"可配置 base_url + api_key + 模型表"的实现
// 就能覆盖全部兼容方。
//
// 路由协议：model 形如 "<provider>:<model>"，provider 名来自配置。
// 保留前缀 qoder / cn / global 不参与本层（它们有各自语义）。
//
// 设计与 pllm / RouterLLM 的 provider 抽象同构：provider 只描述"连哪里、
// 用什么 key、支持哪些模型"，转发逻辑只有一份。
package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// OpenAIProvider 一个兼容 OpenAI 的上游。
type OpenAIProvider struct {
	// Name 路由前缀（客户端写 "groq:llama-3.3-70b" 里的 groq）。
	Name string
	// BaseURL 到 /v1 为止（例如 https://api.groq.com/openai/v1）。
	// 本层会拼 /chat/completions 与 /models。
	BaseURL string
	// APIKey 出站 Bearer。
	APIKey string
	// Models 该上游可用的模型 ID（面板展示 + 校验用）。
	Models []string
}

// OpenAICompatPool 多 provider 池。只读，并发安全。
type OpenAICompatPool struct {
	providers map[string]*OpenAIProvider
	order     []string
}

// NewOpenAICompatPool 构建池。name 为空/重复/保留字时跳过该条并返回告警列表。
//
// 保留字：qoder / cn / global —— 它们已经被别的路由语义占用，重复使用会让
// "这个前缀到底走哪条链路"变成不可推理的问题。
func NewOpenAICompatPool(list []OpenAIProvider) (*OpenAICompatPool, []string) {
	pool := &OpenAICompatPool{providers: map[string]*OpenAIProvider{}}
	var warnings []string
	for _, p := range list {
		name := strings.TrimSpace(p.Name)
		base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		if name == "" {
			warnings = append(warnings, "openai.providers: 存在空 name，已跳过")
			continue
		}
		switch name {
		case "qoder", "cn", "global":
			warnings = append(warnings, fmt.Sprintf("openai.providers.%s: 与保留前缀冲突，已跳过", name))
			continue
		}
		if base == "" {
			warnings = append(warnings, fmt.Sprintf("openai.providers.%s: base_url 为空，已跳过", name))
			continue
		}
		if _, dup := pool.providers[name]; dup {
			warnings = append(warnings, fmt.Sprintf("openai.providers.%s: 重复定义，保留第一个", name))
			continue
		}
		cp := p
		cp.Name, cp.BaseURL = name, base
		pool.providers[name] = &cp
		pool.order = append(pool.order, name)
	}
	if len(pool.providers) == 0 {
		return nil, warnings
	}
	sort.Strings(pool.order)
	return pool, warnings
}

// Len provider 数量。
func (p *OpenAICompatPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.providers)
}

// Has 报告某前缀是否已注册。
func (p *OpenAICompatPool) Has(name string) bool {
	if p == nil {
		return false
	}
	_, ok := p.providers[name]
	return ok
}

// Split 把 "provider:model" 拆成两段。prefix 未注册时返回 ok=false。
func (p *OpenAICompatPool) Split(model string) (provider, bare string, ok bool) {
	idx := strings.IndexByte(model, ':')
	if idx <= 0 || idx == len(model)-1 {
		return "", "", false
	}
	name := model[:idx]
	if !p.Has(name) {
		return "", "", false
	}
	return name, model[idx+1:], true
}

// ProviderNames 已注册的 provider 名（升序）。
func (p *OpenAICompatPool) ProviderNames() []string {
	if p == nil {
		return nil
	}
	out := make([]string, len(p.order))
	copy(out, p.order)
	return out
}

// Models 展开成面板/模型列表用的条目（带 provider 前缀）。
func (p *OpenAICompatPool) Models() []QoderModel {
	if p == nil {
		return nil
	}
	var out []QoderModel
	for _, name := range p.order {
		prov := p.providers[name]
		if len(prov.Models) == 0 {
			out = append(out, QoderModel{ID: name + ":*", Label: name + "（模型表未配置）"})
			continue
		}
		for _, m := range prov.Models {
			out = append(out, QoderModel{ID: name + ":" + m, Label: name + " / " + m})
		}
	}
	return out
}

// ChatStream 把请求转发给指定 provider。
//
// 返回体与 QoderClient.ChatStream 同构，handler 侧可复用同一套写回逻辑。
func (p *OpenAICompatPool) ChatStream(ctx context.Context, provider string, body []byte) (io.ReadCloser, int, []byte, error) {
	if p == nil {
		return nil, 0, nil, fmt.Errorf("openai-compat: 未配置")
	}
	prov, ok := p.providers[provider]
	if !ok {
		return nil, 0, nil, fmt.Errorf("openai-compat: 未知 provider %q", provider)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		prov.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	if prov.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+prov.APIKey)
	}

	// 流式请求不能设总超时（长生成会被误杀），靠 ctx 取消。
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.Body, resp.StatusCode, nil, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return nil, resp.StatusCode, raw, nil
}

// HealthCheck 探测 provider 的 /models（面板"测试连通性"用）。
func (p *OpenAICompatPool) HealthCheck(ctx context.Context, provider string) error {
	if p == nil {
		return fmt.Errorf("openai-compat: 未配置")
	}
	prov, ok := p.providers[provider]
	if !ok {
		return fmt.Errorf("openai-compat: 未知 provider %q", provider)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, prov.BaseURL+"/models", nil)
	if err != nil {
		return err
	}
	if prov.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+prov.APIKey)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("openai-compat: %s /models status %d", provider, resp.StatusCode)
	}
	return nil
}
