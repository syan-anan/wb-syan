// keys.go 面板「密钥管理」接口：列出 / 新建 / 修改 / 删除子密钥，并绑定账号白名单。
//
// 分流模型：主密钥（config.json 的 api_key）不限定账号；子密钥可绑定一个账号白名单，
// 用该 key 发起的 /v1/* 请求只会在白名单内选号（选号与粘性会话双重限定）。子密钥
// **不能**进入管理面板——面板凭据只认 panel.password / 主密钥，见 panel.go withAuth。
//
// 落盘：经 main 注入的 SaveKeys 闭包写回 config.json 的 api_keys（深合并 + 原子替换），
// 并热更新运行期快照，无需重启即生效。
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

	"github.com/linguo2625469/workbuddy2api-panel/internal/keys"
)

// keyView 子密钥的对外视图：只回显掩码，绝不回显明文（明文仅在创建时一次性返回）。
type keyView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	KeyMasked string   `json:"key_masked"`
	Note      string   `json:"note,omitempty"`
	Accounts  []string `json:"accounts"`
	Disabled  bool     `json:"disabled"`
}

// accountRef 供前端「绑定账号」多选展示的账号条目（不含任何凭证）。
type accountRef struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Realm    string `json:"realm,omitempty"`
	Disabled bool   `json:"disabled"`
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
		out = append(out, accountRef{UID: s.UID, Nickname: s.Nickname, Realm: s.Realm, Disabled: s.Disabled})
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

// listKeys 返回全部子密钥（掩码）与可绑定账号列表。
func (p *Panel) listKeys(w http.ResponseWriter, r *http.Request) {
	entries := p.currentEntries()
	out := make([]keyView, 0, len(entries))
	for _, e := range entries {
		accts := e.Accounts
		if accts == nil {
			accts = []string{}
		}
		out = append(out, keyView{
			ID: e.ID, Name: e.Name, KeyMasked: maskKey(e.Key),
			Note: e.Note, Accounts: accts, Disabled: e.Disabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"keys":     out,
		"accounts": p.accountRefsList(),
	})
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

// createKey 新建子密钥：key 留空则服务端随机生成并一次性返回明文。
func (p *Panel) createKey(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveKeys == nil {
		writeErr(w, http.StatusNotImplemented, "key api not available")
		return
	}
	var body struct {
		Name     string   `json:"name"`
		Key      string   `json:"key"`
		Note     string   `json:"note"`
		Accounts []string `json:"accounts"`
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
	key := strings.TrimSpace(body.Key)
	if key == "" {
		var err error
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
		ID: keys.NewID(key), Name: name, Key: key,
		Note: strings.TrimSpace(body.Note), Accounts: sanitizeAccounts(body.Accounts),
	}
	if err := p.cfg.SaveKeys(append(entries, entry)); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	log.Printf("panel: 新建子密钥 %s（%s，绑定账号 %d 个）", entry.ID, entry.Name, len(entry.Accounts))
	// 明文仅在创建响应里返回一次；此后列表只给掩码。
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": entry.ID, "key": key})
}

// updateKey 修改子密钥（名称 / 备注 / 绑定账号 / 启停）；只改提交的字段。
func (p *Panel) updateKey(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveKeys == nil {
		writeErr(w, http.StatusNotImplemented, "key api not available")
		return
	}
	id := r.PathValue("id")
	var body struct {
		Name     *string   `json:"name"`
		Note     *string   `json:"note"`
		Accounts *[]string `json:"accounts"`
		Disabled *bool     `json:"disabled"`
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
	if body.Name != nil {
		entries[idx].Name = strings.TrimSpace(*body.Name)
	}
	if body.Note != nil {
		entries[idx].Note = strings.TrimSpace(*body.Note)
	}
	if body.Accounts != nil {
		entries[idx].Accounts = sanitizeAccounts(*body.Accounts)
	}
	if body.Disabled != nil {
		entries[idx].Disabled = *body.Disabled
	}
	if err := p.cfg.SaveKeys(entries); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	log.Printf("panel: 更新子密钥 %s（%s）", entries[idx].ID, entries[idx].Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// removeKey 删除子密钥。
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
	log.Printf("panel: 删除子密钥 %s", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
