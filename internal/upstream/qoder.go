// qoder.go —— Qoder（qoder.sh / qoder.ai）上游适配层。
//
// 事实来源（全部为 2026-10-17 对官方客户端 0.4.8 + qoder-agent-sdk 1.1.69 的实测）：
//
//	PAT 换 job token：POST https://openapi.qoder.sh/api/v1/jobToken/exchange
//	job token 续期：  POST https://openapi.qoder.sh/api/v1/jobToken/refresh
//	对话：            POST https://api2-v2.qoder.sh/model/v1/chat/completions
//
// 三条链路都用同一套 cosy-* 头；对话端点返回的就是 OpenAI Chat Completions 形状
// （choices / delta / reasoning_content / usage），所以这一层只做搬运，不做协议翻译。
//
// 与 WorkBuddy 的关系：完全独立的一条上游。它不进 pool（没有多账号概念），
// 走 model 前缀 "qoder:" 显式路由，WorkBuddy 的选号/冷却/粘性一条都不受影响。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// QoderOpenAPIBase PAT 交换 / 续期所在域。
	QoderOpenAPIBase = "https://openapi.qoder.sh"
	// QoderModelBase 对话推理所在域（global 构建的 model server）。
	QoderModelBase = "https://api2-v2.qoder.sh"
	// QoderCLIVersion 客户端版本号，进 cosy-version 头。
	QoderCLIVersion = "1.1.69"
	// qoderTokenRefreshSkew job token 剩余不足该时长就先续期，避免请求卡在边界上。
	qoderTokenRefreshSkew = 10 * time.Minute
)

// QoderToken job token 与续期凭证（字段名与上游响应一一对应，便于落盘/回读）。
type QoderToken struct {
	Token            string `json:"token"`
	CreatedAt        string `json:"created_at,omitempty"`
	ExpiresAt        string `json:"expires_at,omitempty"`
	ExpiresIn        int64  `json:"expires_in,omitempty"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	RefreshExpiresAt string `json:"refresh_token_expires_at,omitempty"`
	RefreshExpiresIn int64  `json:"refresh_token_expires_in,omitempty"`
}

// QoderModel 面板展示用的模型条目。ID 是出站真实模型名，Label 是给人看的。
type QoderModel struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// QoderClient 单账号 Qoder 上游。并发安全：所有 token 读写都在 mu 下。
type QoderClient struct {
	HTTP      *http.Client
	PAT       string
	OpenAPI   string
	ModelBase string
	TokenPath string

	mu  sync.Mutex
	tok *QoderToken
}

// NewQoderClient 构造客户端。tokenPath 非空时 token 落盘，重启不必重新交换。
func NewQoderClient(pat, tokenPath string) *QoderClient {
	return &QoderClient{
		HTTP:      &http.Client{}, // 流式请求不能设总超时，靠 ctx 取消
		PAT:       strings.TrimSpace(pat),
		OpenAPI:   QoderOpenAPIBase,
		ModelBase: QoderModelBase,
		TokenPath: tokenPath,
	}
}

// QoderModels 已知可用模型。
//
// 为什么是硬编码而不是拉接口：model list 端点（/api/v2/model/list）要求 WASM 签名，
// 本层拿不到签名上下文；而对话端点本身不校验签名。这里只列实测确认可用的 ID。
// auto 由服务端路由（实测落到 qwen3-coder-plus），是唯一不需要用户记模型名的入口。
func QoderModels() []QoderModel {
	return []QoderModel{
		{ID: "auto", Label: "Auto（服务端自动选模型）"},
		{ID: "gpt-5", Label: "GPT-5"},
	}
}

// qoderHeaders 出站公共头。cosy-* 是官方客户端的指纹头，缺了会被 WAF 当非官方流量。
func qoderHeaders() map[string]string {
	return map[string]string{
		"User-Agent":      "qoder-cli/" + QoderCLIVersion,
		"cosy-version":    QoderCLIVersion,
		"cosy-clienttype": "10",
		"cosy-machineos":  "win32_x64",
	}
}

// qoderPostJSON 发一个 JSON POST 并解出目标结构。
func (c *QoderClient) qoderPostJSON(ctx context.Context, url string, payload, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	for k, v := range qoderHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("qoder: %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("qoder: read %s: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("qoder: %s status %d: %s", url, resp.StatusCode, truncateText(string(body), 200))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("qoder: parse %s: %w (body: %s)", url, err, truncateText(string(body), 160))
	}
	return nil
}

// ExchangePAT 用 Personal Access Token 换第一组 job token。
func (c *QoderClient) ExchangePAT(ctx context.Context) (*QoderToken, error) {
	if c.PAT == "" {
		return nil, fmt.Errorf("qoder: personal access token 未配置")
	}
	var tok QoderToken
	if err := c.qoderPostJSON(ctx, c.OpenAPI+"/api/v1/jobToken/exchange",
		map[string]string{"personal_token": c.PAT}, &tok); err != nil {
		return nil, err
	}
	if tok.Token == "" {
		return nil, fmt.Errorf("qoder: exchange 未返回 token")
	}
	return &tok, nil
}

// RefreshJobToken 用 refresh token 续期。
func (c *QoderClient) RefreshJobToken(ctx context.Context, refresh string) (*QoderToken, error) {
	if refresh == "" {
		return nil, fmt.Errorf("qoder: refresh token 为空")
	}
	var tok QoderToken
	if err := c.qoderPostJSON(ctx, c.OpenAPI+"/api/v1/jobToken/refresh",
		map[string]string{"refresh_token": refresh}, &tok); err != nil {
		return nil, err
	}
	if tok.Token == "" {
		return nil, fmt.Errorf("qoder: refresh 未返回 token")
	}
	return &tok, nil
}

// Token 返回当前可用 job token：内存 → 磁盘 → 续期 → 重新交换，逐级兜底。
//
// 为什么按这个顺序：PAT 是最贵的一步（要人工生成），refresh token 能用就别动它；
// 只有 refresh 也过期才回去用 PAT。磁盘缓存只是省一次网络往返。
func (c *QoderClient) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.tok == nil {
		c.tok = c.loadToken()
	}
	now := time.Now()
	if c.tok != nil && c.tok.Token != "" {
		if exp, ok := parseQoderTime(c.tok.ExpiresAt); !ok || exp.After(now.Add(qoderTokenRefreshSkew)) {
			return c.tok.Token, nil
		}
	}
	if c.tok != nil && c.tok.RefreshToken != "" {
		if exp, ok := parseQoderTime(c.tok.RefreshExpiresAt); !ok || exp.After(now) {
			if tok, err := c.RefreshJobToken(ctx, c.tok.RefreshToken); err == nil {
				c.tok = tok
				c.saveToken(tok)
				return tok.Token, nil
			}
		}
	}
	tok, err := c.ExchangePAT(ctx)
	if err != nil {
		return "", err
	}
	c.tok = tok
	c.saveToken(tok)
	return tok.Token, nil
}

// parseQoderTime 解析上游 RFC3339 时间；空/坏值返回 ok=false（调用方按"未知"处理）。
func parseQoderTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// loadToken 从磁盘恢复 token。读不到/坏文件返回 nil（不是错误：首次运行就该没有）。
func (c *QoderClient) loadToken() *QoderToken {
	if c.TokenPath == "" {
		return nil
	}
	raw, err := os.ReadFile(c.TokenPath)
	if err != nil {
		return nil
	}
	var tok QoderToken
	if json.Unmarshal(raw, &tok) != nil {
		return nil
	}
	return &tok
}

// saveToken 原子落盘。失败只影响重启后是否要重新交换，不影响本次请求，故不返回错误。
func (c *QoderClient) saveToken(tok *QoderToken) {
	if c.TokenPath == "" || tok == nil {
		return
	}
	raw, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(c.TokenPath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := c.TokenPath + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, c.TokenPath)
}

// ChatStream 把 OpenAI 形状的请求转发到 Qoder 对话端点。
//
// 返回体与 workbuddy 的 ChatStreamContext 同构（rc/status/body/err），
// 这样 handler 侧可以走同一套写回逻辑（流式逐帧、非流式整包）。
func (c *QoderClient) ChatStream(ctx context.Context, body []byte) (io.ReadCloser, int, []byte, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.ModelBase+"/model/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	for k, v := range qoderHeaders() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")

	resp, err := c.HTTP.Do(req)
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

// truncateText 截断长文本用于日志/错误（包内已有 truncate，这里换名避免重复定义）。
func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------------------------------------------------------------------------
// 多账号池
// ---------------------------------------------------------------------------

// QoderAccount 一个 Qoder 账号。TokenFile 为空时按序号自动放到 dir 下。
type QoderAccount struct {
	Name string
	PAT  string
	// TokenFile 可选：显式指定 token 缓存路径（多账号必须互不相同）。
	TokenFile string
}

// QoderPool 多账号 Qoder 上游。
//
// 亲和策略：同一会话键（conversation / session）优先复用上次成功的账号——
// 上游的 prompt cache 是按账号侧缓存命中的，换号等于从零重建上下文。
// 账号失败时短冷却，粘性绑定随之失效，下一次请求自然漂到健康账号。
type QoderPool struct {
	mu      sync.Mutex
	clients []*QoderClient
	names   []string
	rr      int
	// sticky 会话键 → 账号下标；cooling 账号下标 → 冷却截止。
	sticky  map[string]int
	cooling map[int]time.Time
	// stickyTTL 粘性绑定存活时长：长时间不用的绑定没有价值，留着只会把
	// 新会话钉在历史账号上。
	stickyTTL time.Duration
}

// qoderPoolDefaults 亲和时间窗与失败冷却基数。
const (
	qoderStickyTTL     = 30 * time.Minute
	qoderAccountCool   = 60 * time.Second
	qoderStickyMaxSize = 4096
)

// NewQoderPool 构建多账号池。accounts 为空返回 nil（= 未启用）。
// tokenDir 用于给未显式指定 TokenFile 的账号生成互不冲突的缓存路径。
func NewQoderPool(accounts []QoderAccount, tokenDir string) *QoderPool {
	clients := make([]*QoderClient, 0, len(accounts))
	names := make([]string, 0, len(accounts))
	for i, a := range accounts {
		pat := strings.TrimSpace(a.PAT)
		if pat == "" {
			continue
		}
		path := a.TokenFile
		if path == "" && tokenDir != "" {
			name := strings.TrimSpace(a.Name)
			if name == "" {
				name = fmt.Sprintf("acct%d", i+1)
			}
			path = filepath.Join(tokenDir, "qoder_token_"+sanitizeFilePart(name)+".json")
		}
		clients = append(clients, NewQoderClient(pat, path))
		name := strings.TrimSpace(a.Name)
		if name == "" {
			name = fmt.Sprintf("qoder-%d", len(clients))
		}
		names = append(names, name)
	}
	if len(clients) == 0 {
		return nil
	}
	return &QoderPool{
		clients:   clients,
		names:     names,
		sticky:    map[string]int{},
		cooling:   map[int]time.Time{},
		stickyTTL: qoderStickyTTL,
	}
}

// sanitizeFilePart 把账号名压成可安全用于文件名的片段（保留字母数字与 _-）。
func sanitizeFilePart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "acct"
	}
	return out
}

// Len 账号数。
func (p *QoderPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.clients)
}

// Names 账号展示名（面板用）。
func (p *QoderPool) Names() []string {
	if p == nil {
		return nil
	}
	out := make([]string, len(p.names))
	copy(out, p.names)
	return out
}

// pickLocked 选一个可用账号下标：粘性命中优先，否则轮询跳过冷却中的账号。
// 全部冷却时退化为轮询（宁可试一次也不直接失败）。调用方需持 p.mu。
func (p *QoderPool) pickLocked(stickyKey string, now time.Time) int {
	n := len(p.clients)
	if stickyKey != "" {
		if idx, ok := p.sticky[stickyKey]; ok && idx < n {
			if until, cooling := p.cooling[idx]; !cooling || !now.Before(until) {
				return idx
			}
		}
	}
	start := p.rr % n
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if until, cooling := p.cooling[idx]; cooling && now.Before(until) {
			continue
		}
		p.rr = idx + 1
		return idx
	}
	// 全部冷却：轮询兜底（账号可能已经恢复，试一次比直接失败便宜）。
	idx := p.rr % n
	p.rr = idx + 1
	return idx
}

// bindLocked 记录粘性绑定，并做一次懒惰清理防 map 无限增长。
func (p *QoderPool) bindLocked(stickyKey string, idx int, now time.Time) {
	if stickyKey == "" {
		return
	}
	if len(p.sticky) >= qoderStickyMaxSize {
		// 满了就整体丢弃：粘性是性能优化，不是正确性依赖，重建成本可接受。
		p.sticky = map[string]int{}
	}
	p.sticky[stickyKey] = idx
	_ = now
}

// ChatStream 转发一条请求，stickyKey 为空时不启用会话亲和。
//
// 失败标记：只有"账号级"失败才冷却（401/403/429/5xx）。400 类参数错误换号
// 也不会成功，冷却它只会白白缩小可用池。
func (p *QoderPool) ChatStream(ctx context.Context, body []byte, stickyKey string) (io.ReadCloser, int, []byte, error) {
	if p == nil || len(p.clients) == 0 {
		return nil, 0, nil, fmt.Errorf("qoder: 未配置账号")
	}
	now := time.Now()
	p.mu.Lock()
	idx := p.pickLocked(stickyKey, now)
	p.mu.Unlock()

	rc, status, respBody, err := p.clients[idx].ChatStream(ctx, body)
	if rc != nil {
		p.mu.Lock()
		p.bindLocked(stickyKey, idx, now)
		delete(p.cooling, idx)
		p.mu.Unlock()
		return rc, status, respBody, nil
	}
	if err == nil && status >= 400 && status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		p.mu.Lock()
		p.cooling[idx] = time.Now().Add(qoderAccountCool)
		for k, v := range p.sticky {
			if v == idx {
				delete(p.sticky, k)
			}
		}
		p.mu.Unlock()
	}
	return nil, status, respBody, err
}
