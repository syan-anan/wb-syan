// qoder_store.go —— Qoder 账号的独立持久化文件。
//
// 为什么不写回 config.json：面板要在运行期加号且不重启，而 config.json 是启动
// 时快照，写它等于让「磁盘状态」和「内存状态」分叉。这里单独一份，启动时优先读，
// 面板改完立即 Replace 到内存池，两边永远一致。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// qoderStoreMu 串行化同一进程内的读写，避免面板连点两次写出半个文件。
var qoderStoreMu sync.Mutex

type qoderAccountFile struct {
	Version  int            `json:"version"`
	Accounts []QoderAccount `json:"accounts"`
}

// LoadQoderAccounts 读取账号文件；文件不存在返回空列表而不是错误。
func LoadQoderAccounts(path string) ([]QoderAccount, error) {
	qoderStoreMu.Lock()
	defer qoderStoreMu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var f qoderAccountFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("qoder: 账号文件解析失败: %w", err)
	}
	return f.Accounts, nil
}

// SaveQoderAccounts 原子写入：先写临时文件再 rename，崩溃不会留下半截 JSON。
func SaveQoderAccounts(path string, accounts []QoderAccount) error {
	qoderStoreMu.Lock()
	defer qoderStoreMu.Unlock()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(qoderAccountFile{Version: 1, Accounts: accounts}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// QoderIdentity 校验 PAT 时顺带取回的身份信息。
type QoderIdentity struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	UserType string `json:"user_type"`
}

// VerifyQoderPAT 用给定 PAT 走一次真实的 jobToken 交换，再调 userinfo 取身份。
// 不落盘、不进池，只回答「这个 PAT 能不能用、它是谁」。
func VerifyQoderPAT(pat, realm, uid, nickname, userType string) (QoderIdentity, error) {
	c := NewQoderClient(pat, "")
	c.Realm = realm
	c.UID = uid
	c.Nickname = nickname
	c.UserType = userType
	if realm == "cn" {
		c.OpenAPI = "https://openapi.qoder.com.cn"
		c.ModelBase = qoderCNBase
	}
	ctx := context.Background()
	tok, err := c.ExchangePAT(ctx)
	if err != nil {
		return QoderIdentity{}, err
	}
	// userinfo 只用来补 UID / 昵称，失败不影响「凭据可用」这个结论。
	id := QoderIdentity{UID: uid, Nickname: nickname, UserType: userType}
	if info, err := c.fetchUserInfo(ctx, tok.Token); err == nil {
		if info.UID != "" {
			id.UID = info.UID
		}
		if info.Nickname != "" {
			id.Nickname = info.Nickname
		}
		if info.UserType != "" {
			id.UserType = info.UserType
		}
	}
	if id.UID == "" {
		return QoderIdentity{}, fmt.Errorf("凭据有效，但读不到 uid，请手动填写")
	}
	return id, nil
}

// fetchUserInfo 读 /api/v1/userinfo（两条链路字段一致：id / name / user_type）。
func (c *QoderClient) fetchUserInfo(ctx context.Context, token string) (QoderIdentity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.OpenAPI+"/api/v1/userinfo", nil)
	if err != nil {
		return QoderIdentity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	for k, v := range qoderHeaders() {
		req.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return QoderIdentity{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return QoderIdentity{}, fmt.Errorf("userinfo %d", resp.StatusCode)
	}
	var body struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		UserType string `json:"user_type"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return QoderIdentity{}, err
	}
	return QoderIdentity{UID: body.ID, Nickname: body.Name, UserType: body.UserType}, nil
}
