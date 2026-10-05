// keys.go 面板「密钥管理」接口：列出 / 新建 / 修改 / 删除子密钥，并绑定分流约束。
//
// 分流模型：主密钥（config.json 的 api_key）不受任何约束；子密钥可绑定
// 账号白名单 / 模型白名单 / 域白名单 / 每日生效时段 / 有效期 / 每时每日请求配额。
// 用该 key 发起的 /v1/* 请求只在白名单内选号（选号与粘性会话双重限定），并在
// server 侧按模型/域/时段/配额二次判定。子密钥 **不能** 进入管理面板——面板凭据只认
// panel.password / 主密钥，见 panel.go withAuth。
//
// 落盘：经 main 注入的 SaveKeys 闭包写回 config.json 的 api_keys（深合并 + 原子替换），
// 并热更新运行期快照，无需重启即生效。配额计数（运行态）走 keys.Quota 独立文件。
package panel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/syan-anan/wb-syan/internal/keys"
)

// keyView 子密钥的对外视图：只回显掩码，绝不回显明文（明文仅在创建时一次性返回）。
// used_today / used_hour 是配额窗口内的已用请求数（运行态，来自 keys.Quota）。
type keyView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	KeyMasked string   `json:"key_masked"`
	Note      string   `json:"note,omitempty"`
	Accounts  []string `json:"accounts"`
	Models    []string `json:"models"`
	Realms    []string `json:"realms"`
	TimeStart string   `json:"time_start,omitempty"`
	TimeEnd   string   `json:"time_end,omitempty"`
	ExpiresAt int64    `json:"expires_at,omitempty"`
	DailyLimit  int    `json:"daily_limit,omitempty"`
	HourlyLimit int    `json:"hourly_limit,omitempty"`
	Disabled    bool   `json:"disabled"`
	UsedToday   int    `json:"used_today"`
	UsedHour    int    `json:"used_hour"`
}

// accountRef 供前端「绑定账号」多选展示的账号条目（不含任何凭证）。
type accountRef struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Realm    string `json:"realm,omitempty"`
	Disabled bool   `json:"disabled"`
	// AddedAt 加入时间（Unix 秒），前端按「添加顺序」展示绑定账号列表。
	AddedAt int64 `json:"added_at,omitempty"`
}

// maskKey 掩码显示密钥（前 6 后 4）；过短则整体打码。
func maskKey(k string) string {
	if len(k) <= 10 {
		return "****"
	}
	return k[:6] + "…" + k[len(k)-4:]
}

// generateKey 生成子密钥（sk- 前缀 + 32 位十六进制，crypto/rand）。
func generateKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(buf), nil
}

// currentEntries 读取当前生效的子密钥列表（Live 快照；无则空）。
func (p *Panel) currentEntries() []keys.Entry {
	if p.cfg.Live != nil {
		if s := p.cfg.Live.Load(); s.Keys != nil {
			return s.Keys.Entries()
		}
	}
	return nil
}

// accountRefsList 账号池里可用于绑定的账号（无凭证）。
func (p *Panel) accountRefsList() []accountRef {
	out := []accountRef{}
	if p.cfg.Pool == nil {
		return out
	}
	for _, s := range p.cfg.Pool.List() {
		out = append(out, accountRef{
			UID: s.UID, Nickname: s.Nickname, Realm: s.Realm,
			Disabled: s.Disabled, AddedAt: s.AddedAt,
		})
	}
	return out
}

// sanitizeAccounts 规范化绑定账号：去空白、去重、丢弃非法 UID。
func sanitizeAccounts(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, a := range in {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] || !validUID(a) {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// sanitizeModels 规范化模型白名单：去空白、去重、丢弃超长项（保留结尾 * 通配）。
func sanitizeModels(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || len(m) > 120 || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// sanitizeRealms 规范化域白名单：只接受 cn / global，去重、固定顺序。
func sanitizeRealms(in []string) []string {
	hasCN, hasGlobal := false, false
	for _, r := range in {
		switch strings.ToLower(strings.TrimSpace(r)) {
		case "cn":
			hasCN = true
		case "global":
			hasGlobal = true
		}
	}
	out := make([]string, 0, 2)
	if hasCN {
		out = append(out, "cn")
	}
	if hasGlobal {
		out = append(out, "global")
	}
	return out
}

// sanitizeClock 校验 "HH:MM"；空串合法（= 不限制）。非法返回 ok=false。
func sanitizeClock(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	if _, ok := keys.ParseClock(s); !ok {
		return "", false
	}
	return s, true
}

// decodeJSONBody 读取并解析 JSON 请求体（1MB 上限）；空 body 视为无字段。
func decodeJSONBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// listKeys 返回全部子密钥（掩码）、可绑定账号列表与当前配额用量。
func (p *Panel) listKeys(w http.ResponseWriter, r *http.Request) {
	entries := p.currentEntries()
	now := time.Now()
	out := make([]keyView, 0, len(entries))
	for _, e := range entries {
		v := keyView{
			ID: e.ID, Name: e.Name, KeyMasked: maskKey(e.Key),
			Note: e.Note, Disabled: e.Disabled,
			Accounts: orEmpty(e.Accounts), Models: orEmpty(e.Models), Realms: orEmpty(e.Realms),
			TimeStart: e.TimeStart, TimeEnd: e.TimeEnd, ExpiresAt: e.ExpiresAt,
			DailyLimit: e.DailyLimit, HourlyLimit: e.HourlyLimit,
		}
		if p.cfg.KeyQuota != nil {
			v.UsedToday, v.UsedHour = p.cfg.KeyQuota.Usage(e.ID, now)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"keys":     out,
		"accounts": p.accountRefsList(),
		"now":      now.Unix(),
	})
}

// orEmpty 把 nil 切片归一为空数组（前端 .map/.length 不判空）。
func orEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// revealKey 返回指定子密钥的完整明文，供列表里的「复制」按钮使用。
// 仅面板凭据可调用（withAuth 只认面板密码/主密钥，子密钥进不来）。
func (p *Panel) revealKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, e := range p.currentEntries() {
		if e.ID == id {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": e.Key})
			return
		}
	}
	writeErr(w, http.StatusNotFound, "密钥不存在")
}

// accountRefs 单独返回可绑定账号列表（供前端刷新）。
func (p *Panel) accountRefs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": p.accountRefsList()})
}

// keyLimits 新建/修改共用的约束字段。
type keyLimits struct {
	Accounts    []string
	Models      []string
	Realms      []string
	TimeStart   string
	TimeEnd     string
	ExpiresAt   int64
	DailyLimit  int
	HourlyLimit int
}

// sanitizeLimits 规范化并校验约束字段；非法（时段格式/负数配额）返回错误。
func sanitizeLimits(l keyLimits) (keyLimits, error) {
	ts, ok := sanitizeClock(l.TimeStart)
	if !ok {
		return l, errors.New("生效时段格式必须是 HH:MM（例如 09:00）")
	}
	te, ok := sanitizeClock(l.TimeEnd)
	if !ok {
		return l, errors.New("失效时段格式必须是 HH:MM（例如 18:00）")
	}
	if l.ExpiresAt < 0 {
		return l, errors.New("有效期时间戳不能为负")
	}
	if l.DailyLimit < 0 || l.HourlyLimit < 0 {
		return l, errors.New("配额不能为负（0 = 不限）")
	}
	l.Accounts = sanitizeAccounts(l.Accounts)
	l.Models = sanitizeModels(l.Models)
	l.Realms = sanitizeRealms(l.Realms)
	l.TimeStart, l.TimeEnd = ts, te
	return l, nil
}

// createKey 新建子密钥：key 留空则服务端随机生成并一次性返回明文。
func (p *Panel) createKey(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveKeys == nil {
		writeErr(w, http.StatusNotImplemented, "key api not available")
		return
	}
	var body struct {
		Name        string   `json:"name"`
		Key         string   `json:"key"`
		Note        string   `json:"note"`
		Accounts    []string `json:"accounts"`
		Models      []string `json:"models"`
		Realms      []string `json:"realms"`
		TimeStart   string   `json:"time_start"`
		TimeEnd     string   `json:"time_end"`
		ExpiresAt   int64    `json:"expires_at"`
		DailyLimit  int      `json:"daily_limit"`
		HourlyLimit int      `json:"hourly_limit"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求失败: "+err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "名称不能为空")
		return
	}
	lim, err := sanitizeLimits(keyLimits{
		Accounts: body.Accounts, Models: body.Models, Realms: body.Realms,
		TimeStart: body.TimeStart, TimeEnd: body.TimeEnd, ExpiresAt: body.ExpiresAt,
		DailyLimit: body.DailyLimit, HourlyLimit: body.HourlyLimit,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	key := strings.TrimSpace(body.Key)
	if key == "" {
		if key, err = generateKey(); err != nil {
			writeErr(w, http.StatusInternalServerError, "生成密钥失败: "+err.Error())
			return
		}
	} else if len(key) < 8 {
		writeErr(w, http.StatusBadRequest, "自定义密钥至少 8 个字符")
		return
	}
	entries := p.currentEntries()
	for _, e := range entries {
		if e.Key == key {
			writeErr(w, http.StatusConflict, "该密钥已存在")
			return
		}
	}
	entry := keys.Entry{
		ID: keys.NewID(key), Name: name, Key: key, Note: strings.TrimSpace(body.Note),
		Accounts: lim.Accounts, Models: lim.Models, Realms: lim.Realms,
		TimeStart: lim.TimeStart, TimeEnd: lim.TimeEnd, ExpiresAt: lim.ExpiresAt,
		DailyLimit: lim.DailyLimit, HourlyLimit: lim.HourlyLimit,
	}
	if err := p.cfg.SaveKeys(append(entries, entry)); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	log.Printf("panel: 新建子密钥 %s（%s，账号 %d / 模型 %d / 域 %d / 配额 %d日-%d时）",
		entry.ID, entry.Name, len(entry.Accounts), len(entry.Models), len(entry.Realms),
		entry.DailyLimit, entry.HourlyLimit)
	// 明文仅在创建响应里返回一次；此后列表只给掩码。
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": entry.ID, "key": key})
}

// updateKey 修改子密钥（名称 / 备注 / 各类约束 / 启停）；只改提交的字段。
func (p *Panel) updateKey(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveKeys == nil {
		writeErr(w, http.StatusNotImplemented, "key api not available")
		return
	}
	id := r.PathValue("id")
	var body struct {
		Name        *string   `json:"name"`
		Note        *string   `json:"note"`
		Accounts    *[]string `json:"accounts"`
		Models      *[]string `json:"models"`
		Realms      *[]string `json:"realms"`
		TimeStart   *string   `json:"time_start"`
		TimeEnd     *string   `json:"time_end"`
		ExpiresAt   *int64    `json:"expires_at"`
		DailyLimit  *int      `json:"daily_limit"`
		HourlyLimit *int      `json:"hourly_limit"`
		Disabled    *bool     `json:"disabled"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求失败: "+err.Error())
		return
	}
	entries := p.currentEntries()
	idx := -1
	for i := range entries {
		if entries[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "密钥不存在")
		return
	}
	e := entries[idx]
	if body.Name != nil {
		e.Name = strings.TrimSpace(*body.Name)
	}
	if body.Note != nil {
		e.Note = strings.TrimSpace(*body.Note)
	}
	if body.Accounts != nil {
		e.Accounts = sanitizeAccounts(*body.Accounts)
	}
	if body.Models != nil {
		e.Models = sanitizeModels(*body.Models)
	}
	if body.Realms != nil {
		e.Realms = sanitizeRealms(*body.Realms)
	}
	if body.TimeStart != nil {
		ts, ok := sanitizeClock(*body.TimeStart)
		if !ok {
			writeErr(w, http.StatusBadRequest, "生效时段格式必须是 HH:MM（例如 09:00）")
			return
		}
		e.TimeStart = ts
	}
	if body.TimeEnd != nil {
		te, ok := sanitizeClock(*body.TimeEnd)
		if !ok {
			writeErr(w, http.StatusBadRequest, "失效时段格式必须是 HH:MM（例如 18:00）")
			return
		}
		e.TimeEnd = te
	}
	if body.ExpiresAt != nil {
		if *body.ExpiresAt < 0 {
			writeErr(w, http.StatusBadRequest, "有效期时间戳不能为负")
			return
		}
		e.ExpiresAt = *body.ExpiresAt
	}
	if body.DailyLimit != nil {
		if *body.DailyLimit < 0 {
			writeErr(w, http.StatusBadRequest, "每日配额不能为负（0 = 不限）")
			return
		}
		e.DailyLimit = *body.DailyLimit
	}
	if body.HourlyLimit != nil {
		if *body.HourlyLimit < 0 {
			writeErr(w, http.StatusBadRequest, "每小时配额不能为负（0 = 不限）")
			return
		}
		e.HourlyLimit = *body.HourlyLimit
	}
	if body.Disabled != nil {
		e.Disabled = *body.Disabled
	}
	entries[idx] = e
	if err := p.cfg.SaveKeys(entries); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	log.Printf("panel: 更新子密钥 %s（%s）", e.ID, e.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// removeKey 删除子密钥（同时清掉它的配额计数，避免计数文件随删除历史无限增长）。
func (p *Panel) removeKey(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveKeys == nil {
		writeErr(w, http.StatusNotImplemented, "key api not available")
		return
	}
	id := r.PathValue("id")
	entries := p.currentEntries()
	out := make([]keys.Entry, 0, len(entries))
	found := false
	for _, e := range entries {
		if e.ID == id {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		writeErr(w, http.StatusNotFound, "密钥不存在")
		return
	}
	if err := p.cfg.SaveKeys(out); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	if p.cfg.KeyQuota != nil {
		p.cfg.KeyQuota.Drop(id)
	}
	log.Printf("panel: 删除子密钥 %s", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// resetKeyQuota 清零某个子密钥的配额计数（运维在用户撞限额后手动放行一次用）。
func (p *Panel) resetKeyQuota(w http.ResponseWriter, r *http.Request) {
	if p.cfg.KeyQuota == nil {
		writeErr(w, http.StatusNotImplemented, "quota not available")
		return
	}
	id := r.PathValue("id")
	found := false
	for _, e := range p.currentEntries() {
		if e.ID == id {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "密钥不存在")
		return
	}
	p.cfg.KeyQuota.Reset(id)
	log.Printf("panel: 重置子密钥 %s 的配额计数", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
