// Package keys 子密钥（分流密钥）的解析与身份判定。
//
// 背景：网关原本只有一个 api_key，无法按调用方分流。本包引入"主 key + 若干子 key"
// 模型：主 key 等价于原 api_key（不限定账号）；子 key 可绑定一个账号白名单，
// 用该 key 发起的请求只会在白名单内选号。
//
// 设计约束：
//   - 常量时间比较（SHA-256 摘要 + subtle.ConstantTimeCompare），不泄露密钥前缀信息；
//   - 遍历全部候选再判定，不在首个不匹配处提前返回（耗时形状与候选数量成正比）；
//   - 子 key 一律不得获得管理面板权限（面板鉴权只认主 key / 面板密码），
//     否则一个受限 key 就能自我提权——这条边界由调用方（panel 包）保证，
//     本包只负责"这个 token 是谁"。
package keys

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// Entry 一条子密钥配置（持久化在 config.json 的 api_keys 数组里）。
type Entry struct {
	ID       string   `json:"id"`                 // 稳定标识（面板增删改按它定位）
	Name     string   `json:"name"`               // 展示名
	Key      string   `json:"key"`                // 密钥本体
	Note     string   `json:"note,omitempty"`     // 备注
	Accounts []string `json:"accounts,omitempty"` // 账号白名单（uid）；空 = 不限账号
	Disabled bool     `json:"disabled,omitempty"` // true = 立即失效（保留配置便于恢复）
}

// Identity 一次请求的调用方身份。
type Identity struct {
	Master   bool     // true = 主 key（不限定账号）
	KeyID    string   // 子 key 的 ID；主 key 为空
	Name     string   // 展示名（子 key 的 Name）
	Accounts []string // 账号白名单；空 = 不限
}

// Restricted 是否限定账号（子 key 且配了白名单）。
func (i Identity) Restricted() bool { return !i.Master && len(i.Accounts) > 0 }

// Allows 该身份是否允许使用指定账号；未限定则恒 true。
func (i Identity) Allows(uid string) bool {
	if !i.Restricted() {
		return true
	}
	for _, a := range i.Accounts {
		if a == uid {
			return true
		}
	}
	return false
}

// AllowSet 返回选号用的白名单集合；未限定返回 nil（调用方据此跳过过滤）。
func (i Identity) AllowSet() map[string]bool {
	if !i.Restricted() {
		return nil
	}
	m := make(map[string]bool, len(i.Accounts))
	for _, a := range i.Accounts {
		m[a] = true
	}
	return m
}

// Store 主 key + 子 key 的不可变快照（配置变更时整体替换，读方无锁）。
type Store struct {
	master  string
	masterD [32]byte
	entries []Entry
	digests [][32]byte
	enabled bool // 是否启用鉴权（主 key 与子 key 全空 = 不鉴权）
}

// New 以主 key 与子 key 列表构建快照。entries 中的空 key 与禁用项会被剔除。
func New(master string, entries []Entry) *Store {
	s := &Store{master: master, masterD: digest(master)}
	for _, e := range entries {
		if strings.TrimSpace(e.Key) == "" {
			continue
		}
		s.entries = append(s.entries, e)
		s.digests = append(s.digests, digest(e.Key))
	}
	s.enabled = master != "" || len(s.entries) > 0
	return s
}

// Enabled 是否启用鉴权（未启用时所有请求视为主身份放行）。
func (s *Store) Enabled() bool { return s != nil && s.enabled }

// Master 返回主 key（面板回显/兼容用）。
func (s *Store) Master() string {
	if s == nil {
		return ""
	}
	return s.master
}

// Entries 返回子 key 列表副本。
func (s *Store) Entries() []Entry {
	if s == nil {
		return nil
	}
	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out
}

// ResolveToken 判定 token 身份。
//
// 返回 ok=false 表示 token 无效（调用方应 401）。未启用鉴权时恒返回主身份。
// 比较过程遍历全部候选、不提前返回，保持耗时形状一致。
func (s *Store) ResolveToken(tok string) (Identity, bool) {
	if s == nil || !s.enabled {
		return Identity{Master: true}, true
	}
	d := digest(tok)
	// 主 key 匹配（master 为空时 digest("") 不会等于真实 token，安全）。
	masterHit := subtle.ConstantTimeCompare(d[:], s.masterD[:]) == 1 && s.master != ""
	matched := -1
	for i := range s.digests {
		eq := subtle.ConstantTimeCompare(d[:], s.digests[i][:])
		if eq == 1 && matched < 0 && !s.entries[i].Disabled {
			matched = i
		}
	}
	if masterHit {
		return Identity{Master: true}, true
	}
	if matched < 0 {
		return Identity{}, false
	}
	e := s.entries[matched]
	return Identity{KeyID: e.ID, Name: e.Name, Accounts: e.Accounts}, true
}

// Resolve 从请求 Authorization 头解析身份。
func (s *Store) Resolve(r *http.Request) (Identity, bool) {
	const prefix = "Bearer "
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, prefix) {
		return s.ResolveToken("")
	}
	return s.ResolveToken(authz[len(prefix):])
}

func digest(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// ---------------------------------------------------------------------------
// context 传递（server 包鉴权后注入，选号处取出）
// ---------------------------------------------------------------------------

type ctxKey struct{}

// WithIdentity 把身份写入 context。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext 取出身份；缺省（未经过鉴权注入）返回主身份，保证裸用/测试场景不误伤。
func FromContext(ctx context.Context) Identity {
	if id, ok := ctx.Value(ctxKey{}).(Identity); ok {
		return id
	}
	return Identity{Master: true}
}

// NormalizeEntries 规范化子 key 列表：去空白 key、按键去重、补齐缺失 ID。
// 返回规范化后的列表与是否有改动（调用方据此决定是否回写 config.json）。
func NormalizeEntries(entries []Entry) ([]Entry, bool) {
	out := make([]Entry, 0, len(entries))
	seen := map[string]bool{}
	changed := false
	for _, e := range entries {
		e.Key = strings.TrimSpace(e.Key)
		e.Name = strings.TrimSpace(e.Name)
		if e.Key == "" {
			changed = true
			continue
		}
		if seen[e.Key] {
			changed = true
			continue
		}
		seen[e.Key] = true
		if strings.TrimSpace(e.ID) == "" {
			e.ID = NewID(e.Key)
			changed = true
		}
		accts := make([]string, 0, len(e.Accounts))
		aseen := map[string]bool{}
		for _, a := range e.Accounts {
			a = strings.TrimSpace(a)
			if a == "" || aseen[a] {
				changed = true
				continue
			}
			aseen[a] = true
			accts = append(accts, a)
		}
		if len(accts) == 0 && len(e.Accounts) > 0 {
			changed = true
		}
		e.Accounts = accts
		out = append(out, e)
	}
	return out, changed
}

// NewID 由密钥派生稳定 ID（同一 key 重复导入得到同一 ID），取摘要前 12 个十六进制字符。
func NewID(key string) string {
	d := digest(key)
	const hex = "0123456789abcdef"
	buf := make([]byte, 0, 12)
	for _, b := range d[:6] {
		buf = append(buf, hex[b>>4], hex[b&0x0f])
	}
	return "k_" + string(buf)
}
