// Package phonelogin 手机号登录（+86 / +852 / +853）：短信验证码换客户端凭证。
//
// 链路（与参考实现的 admin/wb_login.py 逐步对齐，每一步都在下面注释了"为什么"）：
//
//  1. POST {host}/v2/plugin/auth/state          拿设备授权 state
//  2. GET  {host}/console/auth/login            换 Keycloak 地址（同时下发 console 会话 Cookie）
//  3. GET  Keycloak 登录页                      从 HTML 里取 OneID 入口
//  4. GET  broker/oneid/login                   拿 OneID /v1/authorize 地址与 state
//  5. GET  OneID authorize                      建立 OneID 侧会话
//  6. POST {oauth2}/v1/auth/sms/code/send       发短信 -> state_token
//  7. POST {oauth2}/v1/auth/sms/code/verify     校验验证码
//  8. GET  {oauth2}/v1/auth/accounts            拿 OneID 授权 code（会消耗 state_token）
//  9. GET  {host}/auth/realms/copilot/broker/oneid/endpoint   用 code 落 console 会话
//  10. GET  {host}/console/auth/login?state=...  设备 state 绑定 console 会话
//  11. GET  {host}/v2/plugin/auth/token          换取客户端凭证（入池的东西）
//
// 三个必须照做的点（都是实测踩出来的）：
//
//   - **每个会话必须有自己的 cookie jar**。OneID 的登录态横跨 workbuddy.cn 与
//     oauth2.account.tencent.com；共用 client 会把不同用户的会话串在一起，并发登录时
//     表现为"串号"。这里每个 Session 自带一个 http.Client。
//
//   - **重定向只在本域白名单内跳**。上游会依次甩到 keycloak → OneID → console，全程都在
//     workbuddy.cn / tencent.com 上；一旦要跳去站外就停下，防被引走。
//
//   - **第 8 步只能做一次**。它消耗 state_token：先做别的调用再回来，拿到的就是失败。
//     所以第二步（select_account）只是 accounts 没给 code 时的退路，不是常规路径。
//
// 关于图形验证码：上游对**单个手机号**做频率风控，同号短时间内多次请求发码会要求图形验证码
// （实测是按号不是按 IP，换号或等待即恢复）。纯协议过不去 —— 这种情况本包如实返回
// ErrNeedCaptcha，由面板引导用户换号或走官方页面。这是**边界情况**，不是主路径。
package phonelogin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 默认端点。CN 域固定（手机号登录只有 CN 有短信体系；国际版走 OAuth 设备流）。
const (
	DefaultHost   = "https://www.workbuddy.cn"
	DefaultOAuth2 = "https://oauth2.account.tencent.com"
	// defaultUA 官方网页版 UA —— 这条链路走的是**网页**登录，不是桌面端。
	defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	// maxHops 重定向链上限（白名单内）。
	maxHops = 12
	// defaultTTL 会话有效期：够用户收短信 + 输码。
	defaultTTL = 15 * time.Minute
	// defaultMaxSessions 同时在册会话上限。
	defaultMaxSessions = 200
	// defaultMaxUpstream 同时在途的上游登录请求数上限。
	defaultMaxUpstream = 8
)

// ErrNeedCaptcha 上游对该号码要求图形验证码（按号风控，换号或稍后即可恢复）。
var ErrNeedCaptcha = errors.New("phonelogin: need captcha")

// Error 带阶段与原因的登录错误（路由层据此给可执行提示，而不是笼统一句失败）。
type Error struct {
	Step   string // auth_state / console_auth_login / keycloak_page / sms_send / sms_verify / ...
	Reason string // 机器可读原因，如 need_captcha / busy
	Msg    string // 面向用户的人话
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Step == "" {
		return e.Msg
	}
	return "[" + e.Step + "] " + e.Msg
}

func newErr(step, msg string) *Error { return &Error{Step: step, Msg: msg} }

// Reply 路由层可直接回给前端的结构。
func (e *Error) Reply() map[string]any {
	if e == nil {
		return nil
	}
	// error 键是给前端 fetch 封装用的：面板的 api() 只认 d.error，
	// 少了它用户看到的就是"HTTP 400"而不是"请输入所选地区的有效手机号"。
	return map[string]any{"ok": false, "error": e.Msg, "step": e.Step, "reason": e.Reason, "message": e.Msg}
}

// Config 管理器配置（零值 = 默认端点 + 默认上限，可直接用）。
type Config struct {
	Host   string // 默认 DefaultHost
	OAuth2 string // 默认 DefaultOAuth2
	// UA 出站 UA；空 = 官方网页版默认。
	UA string
	// TTL 会话有效期；<=0 = 15 分钟。
	TTL time.Duration
	// MaxSessions 在册会话上限；<=0 = 200。
	MaxSessions int
	// MaxUpstream 在途上游登录请求上限；<=0 = 8。
	MaxUpstream int
	// HTTP 基础 client（可选，只为注入测试用的 Transport）；Cookie jar 由每个会话各自建，
	// 不会共享这里传入的 jar。
	HTTP *http.Client
	// Now 时钟注入（测试用）；nil = time.Now。
	Now func() time.Time
}

// Manager 会话管理器（进程内状态；重启即丢，登录是短时交互）。
type Manager struct {
	cfg Config

	mu       sync.Mutex
	sessions map[string]*Session
	sem      chan struct{}
}

// Session 一次「手机号登录换凭证」的完整状态。
type Session struct {
	sid    string
	client *http.Client
	mu     sync.Mutex

	created    time.Time
	ttl        time.Duration
	consumed   bool
	consumedAt time.Time
	result     *Result

	pluginState   string
	pluginAuthURL string
	kcURL         string
	brokerURL     string
	oneidState    string

	smsStateToken string
	mobile        string
	countryCode   string
	codeSentAt    time.Time
}

// Result 登录成功的结果。
type Result struct {
	// Credential 与桌面端同构的 .info 文本（可直接走 credential.Encrypt 或 auth.Parse）。
	Credential []byte
	UID        string
	Nickname   string
	Phone      string
	Realm      string
	// RawToken 上游原始 token（排障用；不含 refresh 之外的敏感信息由调用方决定是否回显）。
	RawToken map[string]any
}

// New 构造管理器。
func New(cfg Config) *Manager {
	if strings.TrimSpace(cfg.Host) == "" {
		cfg.Host = DefaultHost
	}
	if strings.TrimSpace(cfg.OAuth2) == "" {
		cfg.OAuth2 = DefaultOAuth2
	}
	if strings.TrimSpace(cfg.UA) == "" {
		cfg.UA = defaultUA
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultTTL
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultMaxSessions
	}
	if cfg.MaxUpstream <= 0 {
		cfg.MaxUpstream = defaultMaxUpstream
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{
		cfg:      cfg,
		sessions: map[string]*Session{},
		sem:      make(chan struct{}, cfg.MaxUpstream),
	}
}

func (m *Manager) now() time.Time { return m.cfg.Now() }

// allowedHost 重定向白名单：本域 + 腾讯 OneID。
func (m *Manager) allowedHost(host string) bool {
	h := strings.ToLower(host)
	for _, base := range []string{m.cfg.Host, m.cfg.OAuth2} {
		if u, err := url.Parse(base); err == nil && u.Host != "" && strings.EqualFold(u.Host, host) {
			return true
		}
	}
	for _, suffix := range []string{"workbuddy.cn", "codebuddy.cn", "workbuddy.ai", "tencent.com"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

func randSID() string {
	b := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fmt.Sprintf("sid-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// headers 构造本次请求的头（网页登录形态：Origin/Referer 都是本域首页）。
func (m *Manager) headers(extra map[string]string, referer string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", m.cfg.UA)
	h.Set("Accept-Language", "zh-CN,zh;q=0.9")
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Origin", m.cfg.Host)
	if referer == "" {
		referer = m.cfg.Host + "/"
	}
	h.Set("Referer", referer)
	for k, v := range extra {
		h.Set(k, v)
	}
	return h
}

// newSession 建一个带独立 cookie jar 的会话。
func (m *Manager) newSession() *Session {
	jar, _ := cookiejar.New(nil)
	base := m.cfg.HTTP
	if base == nil {
		base = &http.Client{Timeout: 20 * time.Second}
	}
	c := &http.Client{
		Transport: base.Transport,
		Timeout:   base.Timeout,
		Jar:       jar,
		// 自己跟重定向：白名单之外的跳转要能拦下来，交给 net/http 就跟丢了。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if c.Timeout <= 0 {
		c.Timeout = 20 * time.Second
	}
	return &Session{sid: randSID(), client: c, created: m.now(), ttl: m.cfg.TTL}
}

// mobilePatterns 各地区手机号规则。与参考实现一致：
//
//	+86  十一位且 1[3-9] 开头（注意是**第二位**落在 3-9，只判首位会把 138… 判成非法）
//	+852 八位且 [4-9] 开头
//	+853 八位且 6 开头
var mobilePatterns = map[string]struct {
	Len   int
	Match func(d string) bool
	Label string
}{
	"86": {Len: 11, Label: "中国大陆", Match: func(d string) bool {
		return d[0] == '1' && d[1] >= '3' && d[1] <= '9'
	}},
	"852": {Len: 8, Label: "中国香港", Match: func(d string) bool {
		return d[0] >= '4' && d[0] <= '9'
	}},
	"853": {Len: 8, Label: "中国澳门", Match: func(d string) bool {
		return d[0] == '6'
	}},
}

// CountryCodes 支持的地区代码（供前端下拉展示）。
func CountryCodes() []map[string]string {
	return []map[string]string{
		{"code": "86", "label": "中国大陆", "example": "13800000000"},
		{"code": "852", "label": "中国香港", "example": "51234567"},
		{"code": "853", "label": "中国澳门", "example": "66123456"},
	}
}

// NormalizeMobile 归一化并校验手机号：接受带国家码 / 带分隔符的写法，返回纯数字号码。
//
// 严格校验而不是"长度对就行"：上游对格式错会回一句含糊的失败，用户根本不知道自己哪里写错了；
// 在本地就把「11 位、1[3-9] 开头」这类规则判掉，错误信息才有指向性。
func NormalizeMobile(mobile, countryCode string) (string, error) {
	cc := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(countryCode), "+"))
	pat, ok := mobilePatterns[cc]
	if !ok {
		return "", newErr("mobile", "不支持的地区代码（支持 +86 / +852 / +853）")
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, mobile)
	switch {
	case strings.HasPrefix(digits, cc) && len(digits) == pat.Len+len(cc):
		digits = digits[len(cc):]
	case strings.HasPrefix(digits, "00"+cc) && len(digits) == pat.Len+len(cc)+2:
		digits = digits[len(cc)+2:]
	}
	if len(digits) != pat.Len || !pat.Match(digits) {
		return "", newErr("mobile", "请输入所选地区（"+pat.Label+"）的有效手机号")
	}
	return digits, nil
}

// ---------------------------------------------------------------------------
// HTTP 小工具
// ---------------------------------------------------------------------------

func (s *Session) do(ctx context.Context, method, rawURL string, body []byte, h http.Header) (*http.Response, error) {
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	req.Header = h
	return s.client.Do(req)
}

// follow 手动跟随重定向链，只在本域白名单内跳。
//
// 为什么不用 net/http 的自动跟随：需要拦下站外跳转（防被引走），还要在每一跳更新 Referer。
func (m *Manager) follow(ctx context.Context, s *Session, rawURL, referer string) (*http.Response, error) {
	cur := rawURL
	ref := referer
	var resp *http.Response
	for hop := 0; hop < maxHops; hop++ {
		var err error
		resp, err = s.do(ctx, http.MethodGet, cur, nil,
			m.headers(map[string]string{"Accept": "text/html,application/json,*/*"}, ref))
		if err != nil {
			return nil, err
		}
		loc := resp.Header.Get("Location")
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
			_ = resp.Body.Close()
			return resp, nil
		}
		_ = resp.Body.Close()
		next, err := url.Parse(cur)
		if err != nil {
			return nil, err
		}
		rel, err := url.Parse(loc)
		if err != nil {
			return nil, err
		}
		abs := next.ResolveReference(rel)
		if !m.allowedHost(abs.Host) {
			return &http.Response{StatusCode: resp.StatusCode, Header: resp.Header}, nil
		}
		ref, cur = cur, abs.String()
	}
	return resp, nil
}

// readJSON 读取并解析响应体；非 JSON 一律按"上游/风控拦了"处理（原文截断返回便于排查）。
func readJSON(resp *http.Response) (map[string]any, string, error) {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, "", err
	}
	text := string(raw)
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		head := text
		if len(head) > 200 {
			head = head[:200]
		}
		return nil, head, errors.New("上游返回不是 JSON（可能被风控拦截）")
	}
	return obj, text, nil
}

// envelopeData 解 workbuddy 的 {code,msg,data} 信封；code 非 0 抛错。
func envelopeData(obj map[string]any, step string) (map[string]any, error) {
	if code, ok := obj["code"]; ok && code != nil {
		if n, isNum := code.(float64); isNum && n != 0 {
			return nil, newErr(step, firstNonEmpty(str(obj["msg"]), "上游返回 code="+str(code)))
		}
	}
	if data, ok := obj["data"].(map[string]any); ok {
		return data, nil
	}
	return map[string]any{}, nil
}

// oneidErr OneID 用 errCode/errMessage（与 workbuddy 的信封不同，不能混用）。
func oneidErr(obj map[string]any, step string) error {
	if code, ok := obj["errCode"]; ok && code != nil {
		if n, isNum := code.(float64); isNum && n != 0 {
			return newErr(step, firstNonEmpty(str(obj["errMessage"]), "上游返回 errCode="+str(code)))
		}
		if s, isStr := code.(string); isStr && s != "" && s != "0" {
			return newErr(step, firstNonEmpty(str(obj["errMessage"]), "上游返回 errCode="+s))
		}
	}
	return nil
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 步骤 1-5：建会话并铺授权链
// ---------------------------------------------------------------------------

// Start 建会话并把授权链铺到 OneID 首页（对用户是"瞬时"的，返回后即可发码）。
func (m *Manager) Start(ctx context.Context) (*Session, error) {
	m.gcLocked()

	s := m.newSession()
	host := m.cfg.Host

	// 1) 设备授权 state
	resp, err := s.do(ctx, http.MethodPost, host+"/v2/plugin/auth/state?platform=workbuddy",
		[]byte("{}"), m.headers(map[string]string{
			"Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest"}, ""))
	if err != nil {
		return nil, newErr("auth_state", "网络错误："+err.Error())
	}
	obj, text, err := readJSON(resp)
	if err != nil {
		return nil, newErr("auth_state", err.Error()+" "+text)
	}
	data, err := envelopeData(obj, "auth_state")
	if err != nil {
		return nil, err
	}
	s.pluginState = str(data["state"])
	s.pluginAuthURL = str(data["authUrl"])
	if s.pluginState == "" {
		return nil, newErr("auth_state", "上游未返回设备授权 state")
	}

	// 2) /console/auth/login → Keycloak 地址（同时下发 console 会话 Cookie）
	domain := hostDomain(host)
	u := host + "/console/auth/login?platform=workbuddy&state=" + url.QueryEscape(s.pluginState) +
		"&domain=" + url.QueryEscape(domain)
	// 这里**不能**用 follow：需要读第一跳的 Location（Keycloak 地址），
	// follow 会把 302 链走完，拿到的就是最后一跳的响应头（没有 Location）。
	resp, err = s.do(ctx, http.MethodGet, u, nil,
		m.headers(map[string]string{"X-Domain": domain, "Accept": "text/html,*/*"}, ""))
	if err != nil {
		return nil, newErr("console_auth_login", "网络错误："+err.Error())
	}
	s.kcURL = str(resp.Header.Get("Location"))
	_ = resp.Body.Close()
	if s.kcURL == "" {
		return nil, newErr("console_auth_login", "未能取得 Keycloak 授权地址")
	}
	if !strings.HasPrefix(s.kcURL, "http") {
		s.kcURL = host + s.kcURL
	}

	// 3) Keycloak 登录页：HTML 里内嵌 OneID 入口（服务端渲染，无需 JS）
	resp, err = s.do(ctx, http.MethodGet, s.kcURL, nil,
		m.headers(map[string]string{"Accept": "text/html,*/*"}, ""))
	if err != nil {
		return nil, newErr("keycloak_page", "网络错误："+err.Error())
	}
	page, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	href := oneIDEntryFromHTML(string(page))
	if href == "" {
		return nil, newErr("keycloak_page", "登录页未提供 OneID 入口（上游可能已调整页面）")
	}
	s.brokerURL = resolveURL(host, strings.ReplaceAll(href, "&amp;", "&"))

	// 4) broker/oneid/login → OneID /v1/authorize
	// 同理：要的是 broker 那一跳的 Location（OneID /v1/authorize），不能跟着走完。
	resp, err = s.do(ctx, http.MethodGet, s.brokerURL, nil,
		m.headers(map[string]string{"Accept": "text/html,*/*"}, s.kcURL))
	if err != nil {
		return nil, newErr("broker_login", "网络错误："+err.Error())
	}
	authorize := str(resp.Header.Get("Location"))
	_ = resp.Body.Close()
	if authorize == "" {
		return nil, newErr("broker_login", "OneID 未返回授权地址")
	}
	if q, err := url.Parse(authorize); err == nil {
		s.oneidState = q.Query().Get("state")
	}
	if s.oneidState == "" {
		return nil, newErr("broker_login", "OneID 授权地址缺少 state")
	}

	// 5) 走一趟 OneID 首页建立 OneID 侧会话（失败不致命：有的号已有会话，发码时会再校验）
	resp, err = s.do(ctx, http.MethodGet, authorize, nil,
		m.headers(map[string]string{"Accept": "text/html,*/*"}, ""))
	if err == nil {
		_ = resp.Body.Close()
	}

	if err := m.register(s); err != nil {
		return nil, err
	}
	return s, nil
}

// oneIDEntryFromHTML 从 Keycloak 页面里取 OneID 的 broker 入口。
//
// 用字符串查找而不是正则：上游页面结构会变，正则一旦不匹配报的是"语法"层面的错，
// 字符串查找能精确表达"找这个标记后面那个 href"，且不引入正则引擎的行为差异。
func oneIDEntryFromHTML(page string) string {
	const marker = "data-idp=\"oneid\""
	i := strings.Index(page, marker)
	if i < 0 {
		return ""
	}
	rest := page[i:]
	j := strings.Index(rest, "href=\"")
	if j < 0 {
		return ""
	}
	rest = rest[j+len("href=\""):]
	k := strings.Index(rest, "\"")
	if k < 0 {
		return ""
	}
	return rest[:k]
}

func resolveURL(base, ref string) string {
	b, err1 := url.Parse(base)
	r, err2 := url.Parse(ref)
	if err1 != nil || err2 != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func hostDomain(host string) string {
	if u, err := url.Parse(host); err == nil && u.Host != "" {
		return u.Host
	}
	return host
}

// ---------------------------------------------------------------------------
// 会话表
// ---------------------------------------------------------------------------

// register 把会话登记进表：先回收过期/已消费的，再判上限。
//
// 满了就**立刻给人话错误**而不是排队耗着 —— 参考实现的实测教训：登录链路每个请求都会
// 占住一个线程等腾讯，洪峰时会把无关接口的 p95 从 12ms 拉到 1478ms。这里上限更小
// （默认 200），命中上限说明有异常流量。
func (m *Manager) register(s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) >= m.cfg.MaxSessions {
		for sid, v := range m.sessions {
			if v.expired(m.now()) || v.consumed {
				delete(m.sessions, sid)
			}
		}
	}
	if len(m.sessions) >= m.cfg.MaxSessions {
		return newErr("busy", "当前登录人数较多，请稍后重试")
	}
	m.sessions[s.sid] = s
	return nil
}

func (m *Manager) gcLocked() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for sid, v := range m.sessions {
		if v.expired(now) || (v.consumed && now.Sub(v.consumedAt) > time.Minute) {
			delete(m.sessions, sid)
		}
	}
}

func (s *Session) expired(now time.Time) bool {
	if s == nil {
		return true
	}
	ttl := s.ttl
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return now.Sub(s.created) > ttl
}

// Session 按 sid 取会话。
func (m *Manager) Session(sid string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[sid]
	if !ok {
		return nil, newErr("session", "登录会话不存在或已过期，请重新发起")
	}
	if s.expired(m.now()) {
		delete(m.sessions, sid)
		return nil, newErr("session", "登录会话已超时，请重新发起")
	}
	return s, nil
}

// SID 会话标识（nil 安全：调用方在 Start 失败后可能拿到 nil）。
func (s *Session) SID() string {
	if s == nil {
		return ""
	}
	return s.sid
}

// acquire 占用一个在途上游名额；满了返回 busy 错误（不排队）。
func (m *Manager) acquire() error {
	select {
	case m.sem <- struct{}{}:
		return nil
	default:
		return &Error{Step: "busy", Reason: "busy", Msg: "当前登录请求较多，请稍后重试"}
	}
}

func (m *Manager) release() {
	select {
	case <-m.sem:
	default:
	}
}

// ---------------------------------------------------------------------------
// 步骤 6：发短信验证码
// ---------------------------------------------------------------------------

// SendCode 发短信验证码，返回验证码有效期（秒）与可重发间隔（秒）。
//
// 关于 spi_extra（风控上下文）：官方网页版每次都会带，但实测**不是必需** ——
// 缺失时上游走"未配置"降级路径，短信照常下发（参考实现多次实测：带 spi_extra /
// 带 device_token / 都不带，三者对同一号的结果一致）。所以这里不带，少一个会
// 随时间腐烂的参数。
func (m *Manager) SendCode(ctx context.Context, sid, mobile, countryCode string) (expiresIn, retryAfter int, err error) {
	s, err := m.Session(sid)
	if err != nil {
		return 0, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consumed {
		return 0, 0, newErr("session", "该会话已完成登录，请重新发起")
	}
	num, err := NormalizeMobile(mobile, countryCode)
	if err != nil {
		return 0, 0, err
	}
	cc := strings.TrimSpace(strings.TrimPrefix(countryCode, "+"))
	// 同一会话在等码期间不允许换号：state_token 与号码是绑定的，换了必然校验失败。
	// 与其让用户在"收到码但校验不过"里困惑，不如当场说清。
	if s.smsStateToken != "" && (s.mobile != num || s.countryCode != cc) {
		return 0, 0, newErr("mobile", "当前会话正在等待验证码，请完成验证后再更换号码")
	}
	if err := m.acquire(); err != nil {
		return 0, 0, err
	}
	defer m.release()

	payload, _ := json.Marshal(map[string]any{
		"client_code": "codebuddy",
		"mobile":      "+" + cc + " " + num,
		"scopes":      []string{"openid", "mobile", "profile"},
	})
	resp, err := s.do(ctx, http.MethodPost, m.cfg.OAuth2+"/v1/auth/sms/code/send", payload,
		m.headers(map[string]string{"Content-Type": "application/json", "X-TOA-LANG": "zh-CN"}, ""))
	if err != nil {
		return 0, 0, newErr("sms_send", "网络错误："+err.Error())
	}
	obj, text, err := readJSON(resp)
	if err != nil {
		return 0, 0, newErr("sms_send", err.Error()+" "+text)
	}
	if err := oneidErr(obj, "sms_send"); err != nil {
		return 0, 0, err
	}
	token := str(obj["state_token"])
	if token == "" {
		if obj["captcha"] != nil || str(obj["status"]) == "need_captcha" {
			// 按号风控：换号或等待即可恢复（实测是按号不是按 IP）。
			// 面板据 step + reason 引导用户换号 / 走官方页面，而不是被堵死在报错上。
			return 0, 0, &Error{
				Step: "sms_send", Reason: "need_captcha",
				Msg: "该手机号短时间内请求验证码次数过多，需要完成安全验证。" +
					"可改用其他手机号，或稍后再试。",
			}
		}
		return 0, 0, newErr("sms_send", "上游未返回 state_token")
	}
	s.smsStateToken = token
	s.mobile = num
	s.countryCode = cc
	s.codeSentAt = m.now()

	exp := int(numOf(obj["expires_in"], numOf(obj["expire"], 60)))
	return exp, 60, nil
}

// ---------------------------------------------------------------------------
// 步骤 7-11：校验验证码 → 换 console 会话 → 换取凭据
// ---------------------------------------------------------------------------

// Verify 校验验证码并走完整条登录链，返回可入池的凭证。
//
// 对同一会话**幂等**：整段持会话锁串行执行，成功后缓存结果。用户双击"登录"不会变成
// "成功一次 + 报一次无效 token" —— 这正是参考实现踩过的坑（state_token 是一次性的，
// 两个并发校验会一个成功、一个报无效的 token，用户看到的是莫名其妙的失败）。
func (m *Manager) Verify(ctx context.Context, sid, code string) (*Result, error) {
	s, err := m.Session(sid)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.result != nil {
		return s.result, nil
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, code)
	if len(digits) != 6 {
		return nil, newErr("code", "请输入 6 位数字验证码")
	}
	if s.smsStateToken == "" {
		return nil, newErr("code", "请先获取验证码")
	}
	if err := m.acquire(); err != nil {
		return nil, err
	}
	defer m.release()

	res, err := m.verifyLocked(ctx, s, digits)
	if err != nil {
		return nil, err
	}
	s.result = res
	s.consumed = true
	s.consumedAt = m.now()
	return res, nil
}

func (m *Manager) verifyLocked(ctx context.Context, s *Session, code string) (*Result, error) {
	oauth2 := m.cfg.OAuth2
	host := m.cfg.Host

	// 7) 校验验证码
	payload, _ := json.Marshal(map[string]any{"state_token": s.smsStateToken, "code": code})
	resp, err := s.do(ctx, http.MethodPost, oauth2+"/v1/auth/sms/code/verify", payload,
		m.headers(map[string]string{"Content-Type": "application/json", "X-TOA-LANG": "zh-CN"}, ""))
	if err != nil {
		return nil, newErr("sms_verify", "网络错误："+err.Error())
	}
	obj, text, err := readJSON(resp)
	if err != nil {
		return nil, newErr("sms_verify", err.Error()+" "+text)
	}
	if err := oneidErr(obj, "sms_verify"); err != nil {
		return nil, err
	}
	if tok := str(obj["state_token"]); tok != "" {
		s.smsStateToken = tok
	}

	// 8) 取 OneID 授权 code。**这一步会消耗 state_token，只能做一次**，
	//    所以下面 select_account 只是 accounts 没给 code 时的退路，不是常规路径。
	oneidCode := ""
	var accounts []any
	resp, err = s.do(ctx, http.MethodGet,
		oauth2+"/v1/auth/accounts?state_token="+url.QueryEscape(s.smsStateToken), nil, m.headers(nil, ""))
	if err == nil {
		if o, _, err2 := readJSON(resp); err2 == nil && oneidErr(o, "accounts") == nil {
			if list, ok := o["accounts"].([]any); ok {
				accounts = list
			}
			oneidCode = str(o["code"])
		}
	}
	if oneidCode == "" {
		accID := ""
		for _, a := range accounts {
			if am, ok := a.(map[string]any); ok && str(am["id"]) != "" {
				accID = str(am["id"])
				break
			}
		}
		if accID == "" {
			accID = "codebuddy@"
		}
		body, _ := json.Marshal(map[string]any{"state_token": s.smsStateToken, "account_id": accID})
		resp, err = s.do(ctx, http.MethodPost, oauth2+"/v1/auth/select_account", body,
			m.headers(map[string]string{"Content-Type": "application/json"}, ""))
		if err != nil {
			return nil, newErr("account_select", "网络错误："+err.Error())
		}
		o2, text2, err2 := readJSON(resp)
		if err2 != nil {
			return nil, newErr("account_select", err2.Error()+" "+text2)
		}
		if err := oneidErr(o2, "account_select"); err != nil {
			return nil, err
		}
		oneidCode = str(o2["code"])
	}
	if oneidCode == "" {
		return nil, newErr("account_select", "未能取得 OneID 授权码")
	}

	// 9) 用 code 落 console 会话（判据是 cookie，不是响应体）
	ep := host + "/auth/realms/copilot/broker/oneid/endpoint?code=" + url.QueryEscape(oneidCode) +
		"&state=" + url.QueryEscape(s.oneidState)
	if _, err := m.follow(ctx, s, ep, s.kcURL); err != nil {
		return nil, newErr("broker_endpoint", "网络错误："+err.Error())
	}
	if !s.hasKeycloakSession(host) {
		return nil, newErr("broker_endpoint", "未能建立登录会话（上游可能拒绝了本次授权）")
	}

	// 10) 设备 state 绑定 console 会话（不做这步，第 11 步会一直回 11217 login ing）
	bind := host + "/console/auth/login?platform=workbuddy&state=" + url.QueryEscape(s.pluginState) +
		"&domain=" + url.QueryEscape(hostDomain(host))
	if _, err := m.follow(ctx, s, bind, host+"/"); err != nil {
		return nil, newErr("bind_state", "网络错误："+err.Error())
	}

	// 11) 换取客户端凭证
	resp, err = s.do(ctx, http.MethodGet, host+"/v2/plugin/auth/token?state="+url.QueryEscape(s.pluginState),
		nil, m.headers(map[string]string{"X-No-Authorization": "true"}, ""))
	if err != nil {
		return nil, newErr("auth_token", "网络错误："+err.Error())
	}
	tobj, text3, err := readJSON(resp)
	if err != nil {
		return nil, newErr("auth_token", err.Error()+" "+text3)
	}
	data, err := envelopeData(tobj, "auth_token")
	if err != nil {
		return nil, err
	}
	token := map[string]any{
		"accessToken":      str(data["accessToken"]),
		"refreshToken":     str(data["refreshToken"]),
		"expiresIn":        numOf(data["expiresIn"], 0),
		"refreshExpiresIn": numOf(data["refreshExpiresIn"], 0),
		"tokenType":        firstNonEmpty(str(data["tokenType"]), "Bearer"),
		"sessionState":     str(data["sessionState"]),
		"domain":           firstNonEmpty(str(data["domain"]), hostDomain(host)),
		"scope":            str(data["scope"]),
	}
	if str(token["accessToken"]) == "" {
		return nil, newErr("auth_token", "上游未返回 accessToken")
	}

	account := m.fetchAccount(ctx, s, str(token["accessToken"]), str(token["domain"]))
	cred, uid, nickname, phone := buildCredential(token, account)
	if uid == "" {
		return nil, newErr("selfcheck", "生成的凭证缺少 uid（账号信息获取失败）")
	}
	return &Result{Credential: cred, UID: uid, Nickname: nickname, Phone: phone, Realm: "cn", RawToken: token}, nil
}

// fetchAccount 用刚拿到的 token 拉账号信息（uid / 昵称 / 手机号，用于落库与展示）。
func (m *Manager) fetchAccount(ctx context.Context, s *Session, token, domain string) map[string]any {
	resp, err := s.do(ctx, http.MethodGet, m.cfg.Host+"/v2/plugin/accounts", nil, m.headers(map[string]string{
		"Authorization":        "Bearer " + token,
		"X-Domain":             domain,
		"X-No-User-Id":         "true",
		"X-No-Enterprise-Id":   "true",
		"X-No-Department-Info": "true",
	}, ""))
	if err != nil {
		return nil
	}
	obj, _, err := readJSON(resp)
	if err != nil {
		return nil
	}
	data, err := envelopeData(obj, "accounts")
	if err != nil {
		return nil
	}
	if list, ok := data["accounts"].([]any); ok && len(list) > 0 {
		if first, ok := list[0].(map[string]any); ok {
			return first
		}
	}
	// 有些响应把账号字段摊平在 data 上。
	if str(data["uid"]) != "" {
		return data
	}
	return nil
}

// hasKeycloakSession 会话建立的判据是 cookie（KEYCLOAK_SESSION / KEYCLOAK_IDENTITY）。
func (s *Session) hasKeycloakSession(host string) bool {
	u, err := url.Parse(host)
	if err != nil || s.client.Jar == nil {
		return false
	}
	for _, c := range s.client.Jar.Cookies(u) {
		if c.Name == "KEYCLOAK_SESSION" || c.Name == "KEYCLOAK_IDENTITY" {
			return true
		}
	}
	return false
}

// buildCredential 拼出与桌面端**同构**的 .info 凭证（号池与 converter 直接读这份结构）。
//
// 字段与官方客户端落盘的 .info 对齐：顶层 account / auth，两处时间戳都是**毫秒**，
// expiresIn 是**秒**。结构不对的话读出来是"导入成功但没号"，最难排查的那一类。
func buildCredential(token, account map[string]any) ([]byte, string, string, string) {
	nowMS := time.Now().UnixMilli()
	expiresIn := numOf(token["expiresIn"], 0)
	refreshExpiresIn := numOf(token["refreshExpiresIn"], 0)

	uid := str(account["uid"])
	nickname := str(account["nickname"])
	phone := str(account["phoneNumber"])

	acct := map[string]any{
		"uid":           uid,
		"nickname":      nickname,
		"type":          firstNonEmpty(str(account["type"]), "personal"),
		"lastLogin":     true,
		"pluginEnabled": true,
		"isCreator":     account["isCreator"] == true,
		"isAdmin":       account["isAdmin"] == true,
		"isFirstLogin":  account["isFirstLogin"] == true,
	}
	if phone != "" {
		acct["phoneNumber"] = phone
	}
	if eid := str(account["enterpriseId"]); eid != "" {
		acct["enterpriseId"] = eid
	}
	if en := str(account["enterpriseName"]); en != "" {
		acct["enterpriseName"] = en
	}
	if uin := str(account["uin"]); uin != "" {
		acct["uin"] = uin
	}

	auth := map[string]any{
		"accessToken":      token["accessToken"],
		"refreshToken":     token["refreshToken"],
		"expiresIn":        expiresIn,
		"refreshExpiresIn": refreshExpiresIn,
		"tokenType":        token["tokenType"],
		"sessionState":     token["sessionState"],
		"domain":           token["domain"],
		"lastRefreshTime":  nowMS,
	}
	if expiresIn > 0 {
		auth["expiresAt"] = nowMS + expiresIn*1000
	}
	if refreshExpiresIn > 0 {
		auth["refreshExpiresAt"] = nowMS + refreshExpiresIn*1000
	}
	if sc := str(token["scope"]); sc != "" {
		auth["scope"] = sc
	}
	out, _ := json.MarshalIndent(map[string]any{"account": acct, "auth": auth}, "", "  ")
	return out, uid, nickname, phone
}

// StatusView 会话状态（前端轮询 / 排障）。
type StatusView struct {
	SID         string `json:"sid"`
	Mobile      string `json:"mobile,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	CodeSent    bool   `json:"code_sent"`
	RetryAfter  int    `json:"retry_after"`
	Done        bool   `json:"done"`
}

// Status 返回会话状态；retryAfter 为距可重发验证码的秒数。
func (m *Manager) Status(sid string) (StatusView, error) {
	s, err := m.Session(sid)
	if err != nil {
		return StatusView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := StatusView{SID: s.sid, Mobile: s.mobile, CountryCode: s.countryCode,
		CodeSent: s.smsStateToken != "", Done: s.consumed}
	if s.smsStateToken != "" {
		if remain := 60 - int(m.now().Sub(s.codeSentAt).Seconds()); remain > 0 {
			v.RetryAfter = remain
		}
	}
	return v, nil
}

func numOf(v any, def int64) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case string:
		var n int64
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err == nil {
			return n
		}
	}
	return def
}
