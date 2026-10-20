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

	// Realm 决定走哪条链路："cn" = gateway.qoder.com.cn 的 COSY 签名，
	// 其它值 = api2-v2.qoder.sh 的 OpenAI 原生接口。
	Realm string
	// AccessToken 非空时直接用它（OAuth 设备令牌），跳过 PAT 交换。
	AccessToken string
	// Disabled 手工停用，选号时跳过。
	Disabled bool
	// CNBase 覆盖国内版网关（测试用）；空 = 官方地址。
	CNBase string
	// UID/Nickname/UserType/OrgID/OrgName 参与 COSY 身份体与设备指纹派生，
	// 国内版必须与账号真实信息一致，否则上游拒绝。
	UID      string
	Nickname string
	UserType string
	OrgID    string
	OrgName  string

	mu           sync.Mutex
	tok          *QoderToken
	refreshToken string

	cosyMu    sync.Mutex
	cosy      *cosySession
	cosyToken string
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
	// OAuth 设备令牌本身就是推理凭证，没有交换环节。
	if c.AccessToken != "" {
		return c.AccessToken, nil
	}
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
	// 国内版走 COSY 签名 + SSE 信封，与国际版的 OpenAI 原生接口完全分离。
	// 账号池只看到这一个入口，不需要知道 Realm。
	if c.Realm == "cn" {
		return c.qoderCNChat(ctx, body, qoderModelKeyFromBody(body))
	}
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

// qoderModelKeyFromBody 取请求里的裸模型名；国内版用它填 x-model-key 与
// model_config.key 两处，缺一处上游都会回落到默认模型。
func qoderModelKeyFromBody(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) == nil && req.Model != "" {
		return req.Model
	}
	return "auto"
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
	Name string `json:"name"`
	PAT  string `json:"personal_access_token"`
	// TokenFile 可选：显式指定 token 缓存路径（多账号必须互不相同）。
	TokenFile string `json:"token_file,omitempty"`

	// AccessToken 是 OAuth 设备授权拿到的 dt- 令牌，与 PAT 二选一。
	// 实测它可以直接用于推理（COSY 签名），不需要再换 jobToken。
	AccessToken string `json:"access_token,omitempty"`
	// Realm 为空或 "intl" 走国际版；"cn" 走国内版 COSY 链路。
	Realm string `json:"realm,omitempty"`
	// UID/Nickname/UserType/OrgID/OrgName 只有国内版需要，参与身份体与设备指纹。
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	UserType string `json:"user_type,omitempty"`
	OrgID    string `json:"org_id,omitempty"`
	OrgName  string `json:"org_name,omitempty"`
	// Disabled 手工停用：仍在池里但不参与选号（区别于冷却）。
	Disabled bool `json:"disabled,omitempty"`
	// Quota 只在对账/展示时填，落盘时为空。
	Quota *QoderQuota `json:"quota,omitempty"`
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
		// OAuth 账号只有设备令牌（dt-），没有 PAT —— 两者任一即可入池。
		// 这里只挡「两个都空」的脏数据，否则授权登录加进来的号会被静默丢掉。
		if pat == "" && strings.TrimSpace(a.AccessToken) == "" {
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
		cl := NewQoderClient(pat, path)
		cl.Realm = a.Realm
		cl.AccessToken = a.AccessToken
		cl.Disabled = a.Disabled
		if a.Realm == "cn" {
			// 国内版的 openapi 与推理网关都在独立域名上，不能复用国际版默认值。
			cl.OpenAPI = "https://openapi.qoder.com.cn"
			cl.ModelBase = qoderCNBase
		}
		cl.UID = a.UID
		cl.Nickname = a.Nickname
		cl.UserType = a.UserType
		cl.OrgID = a.OrgID
		cl.OrgName = a.OrgName
		clients = append(clients, cl)
		name := strings.TrimSpace(a.Name)
		if name == "" {
			name = fmt.Sprintf("qoder-%d", len(clients))
		}
		names = append(names, name)
	}
	if len(clients) == 0 {
		// 返回空池而不是 nil：面板可以在运行期往空池里加账号，
		// 调用方的指针因此永远不需要替换。
		return &QoderPool{
			sticky:    map[string]int{},
			cooling:   map[int]time.Time{},
			stickyTTL: qoderStickyTTL,
		}
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
// realmLabel 把内部区域名翻成给人看的说法。
func realmLabel(realm string) string {
	if realm == "cn" {
		return "国内"
	}
	if realm == "intl" {
		return "国际"
	}
	return "全部"
}

// qoderRealmMatch 账号是否属于指定区；realm 为空表示全池。
// 旧账号 Realm 为空按国际版处理（历史默认）。
func (p *QoderPool) qoderRealmMatch(idx int, realm string) bool {
	if realm == "" {
		return true
	}
	r := p.clients[idx].Realm
	if realm == "cn" {
		return r == "cn"
	}
	return r != "cn"
}

func (p *QoderPool) pickLocked(stickyKey string, now time.Time, realm string) int {
	n := len(p.clients)
	if stickyKey != "" {
		if idx, ok := p.sticky[stickyKey]; ok && idx < n {
			if until, cooling := p.cooling[idx]; !cooling || !now.Before(until) {
				if !p.clients[idx].Disabled && p.qoderRealmMatch(idx, realm) {
					return idx
				}
			}
		}
	}
	start := p.rr % n
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if p.clients[idx].Disabled {
			continue
		}
		if !p.qoderRealmMatch(idx, realm) {
			continue
		}
		if until, cooling := p.cooling[idx]; cooling && now.Before(until) {
			continue
		}
		p.rr = idx + 1
		return idx
	}
	// 全部冷却：只在同区内轮询兜底。跨区借用会让 qodercn: 跑到国际站，
	// 那比直接失败更糟——请求会被上游拒绝，还会污染另一个区的冷却状态。
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if !p.clients[idx].Disabled && p.qoderRealmMatch(idx, realm) {
			p.rr = idx + 1
			return idx
		}
	}
	return -1
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
func (p *QoderPool) ChatStream(ctx context.Context, body []byte, stickyKey, realm string) (io.ReadCloser, int, []byte, error) {
	if p == nil || len(p.clients) == 0 {
		return nil, 0, nil, fmt.Errorf("qoder: 未配置账号")
	}
	now := time.Now()
	p.mu.Lock()
	idx := p.pickLocked(stickyKey, now, realm)
	p.mu.Unlock()
	if idx < 0 {
		return nil, http.StatusServiceUnavailable, nil,
			fmt.Errorf("qoder: %s 区没有可用账号", realmLabel(realm))
	}

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

// Replace 用新的账号集合原地重建客户端列表。
// 调用方（handler）持有同一个 *QoderPool，不需要换指针；粘性与冷却全部重置，
// 因为账号下标已经变了，旧绑定指向的可能是另一个号。
func (p *QoderPool) Replace(accounts []QoderAccount, tokenDir string) {
	if p == nil {
		return
	}
	np := NewQoderPool(accounts, tokenDir)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clients = np.clients
	p.names = np.names
	p.rr = 0
	p.sticky = map[string]int{}
	p.cooling = map[int]time.Time{}
}

// Accounts 返回当前账号的副本（面板展示用，PAT 已打码）。
func (p *QoderPool) Accounts() []QoderAccount {
	if p == nil {
		return nil
	}
	// 先拷一份再解锁：Quota 会发网络请求，持锁期间做这件事会把选号一起卡住。
	p.mu.Lock()
	clients := append([]*QoderClient(nil), p.clients...)
	names := append([]string(nil), p.names...)
	p.mu.Unlock()

	out := make([]QoderAccount, 0, len(clients))
	for i, c := range clients {
		name := ""
		if i < len(names) {
			name = names[i]
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		q := c.Quota(ctx)
		cancel()
		out = append(out, QoderAccount{
			Name:        name,
			PAT:         maskPAT(c.PAT),
			AccessToken: maskPAT(c.AccessToken),
			Realm:       c.Realm,
			UID:         c.UID,
			Nickname:    c.Nickname,
			UserType:    c.UserType,
			Disabled:    c.Disabled,
			Quota:       &q,
		})
	}
	return out
}

func maskPAT(p string) string {
	if len(p) <= 12 {
		if p == "" {
			return ""
		}
		return p[:min(4, len(p))] + "***"
	}
	return p[:8] + "***" + p[len(p)-6:]
}

// TestAccount 对指定账号发一条最小请求，验证凭据与链路。
// 只读语义：发一条 "ping"，读掉响应，不写任何状态。
func (p *QoderPool) TestAccount(ctx context.Context, name string) error {
	if p == nil {
		return fmt.Errorf("qoder: 未配置账号")
	}
	p.mu.Lock()
	var c *QoderClient
	for i, cl := range p.clients {
		if i < len(p.names) && p.names[i] == name {
			c = cl
			break
		}
	}
	p.mu.Unlock()
	if c == nil {
		return fmt.Errorf("没有叫 %q 的账号", name)
	}
	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"ping"}],"stream":false}`)
	rc, status, _, err := c.ChatStream(ctx, body)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 8192))
	if status != http.StatusOK {
		return fmt.Errorf("上游返回 %d", status)
	}
	return nil
}
