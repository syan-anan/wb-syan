// qoder_oauth.go —— Qoder 设备授权登录（PKCE S256）。
//
// 流程与官方桌面/CLI 客户端一致：
//  1. 本地生成 code_verifier + challenge、nonce、machine_id，拼出授权页 URL；
//  2. 用户在浏览器完成授权（账号密码 / 验证码 / 滑块都在官方页面上）；
//  3. 服务端拿 nonce+verifier 轮询 deviceToken/poll，成功后直接拿到 dt- 令牌。
//
// dt- 令牌实测可直接用于 COSY 签名推理，不需要再换 jobToken——这就是
// 「不用粘贴 PAT 也能加号」的依据。
//
// 双区参数差异（逆向自官方）：CN 带 redirect_uri + client_id + machine_id，
// nonce 用带横线 UUID；Intl 不带 redirect_uri，nonce 用 32 位十六进制。
package upstream

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// qoderRealmCfg 单个区域的授权参数。
type qoderRealmCfg struct {
	Website      string
	OpenAPI      string
	ClientID     string
	RedirectURI  string
	Domain       string
	SendRedirect bool
	NonceDashed  bool
}

var qoderRealmCfgs = map[string]qoderRealmCfg{
	"cn": {
		Website:      "https://qoder.com.cn",
		OpenAPI:      "https://openapi.qoder.com.cn",
		ClientID:     "1c5e33e1-364d-4ce6-b02c-acaa81274a5c",
		RedirectURI:  "qoder-work-cn://",
		Domain:       "qoder.com.cn",
		SendRedirect: true,
		NonceDashed:  true,
	},
	"intl": {
		Website:      "https://qoder.com",
		OpenAPI:      "https://openapi.qoder.sh",
		ClientID:     "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb",
		RedirectURI:  "qoder://aicoding.aicoding-agent/login-success",
		Domain:       "qoder.com",
		SendRedirect: false,
		NonceDashed:  false,
	},
}

// qoderLoginTTL 授权窗口；超时后状态被清理，用户需要重新发起。
const qoderLoginTTL = 10 * time.Minute

type qoderLoginState struct {
	Verifier  string
	Nonce     string
	Realm     string
	MachineID string
	Created   time.Time
}

var qoderLoginStore = struct {
	sync.Mutex
	m map[string]qoderLoginState
}{m: map[string]qoderLoginState{}}

// qoderRealmConfig 取区域配置；未知区域回落到国内版。
func qoderRealmConfig(realm string) qoderRealmCfg {
	if c, ok := qoderRealmCfgs[realm]; ok {
		return c
	}
	return qoderRealmCfgs["cn"]
}

// StartQoderOAuth 生成授权 URL 与状态 ID。
func StartQoderOAuth(realm, machineID string) (stateID, authURL string, err error) {
	cfg := qoderRealmConfig(realm)
	if realm == "" {
		realm = "cn"
	}
	verifierBytes := make([]byte, 32)
	if _, err := cryptorand.Read(verifierBytes); err != nil {
		return "", "", err
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	nonce := qoderUUID()
	if !cfg.NonceDashed {
		nonce = strings.ReplaceAll(nonce, "-", "")
	}
	if machineID == "" {
		machineID = qoderUUID()
	}

	q := url.Values{}
	q.Set("challenge", challenge)
	q.Set("challenge_method", "S256")
	q.Set("nonce", nonce)
	if cfg.SendRedirect {
		q.Set("redirect_uri", cfg.RedirectURI)
	}
	q.Set("client_id", cfg.ClientID)
	q.Set("machine_id", machineID)

	authURL = cfg.Website + "/device/selectAccounts?" + q.Encode()
	stateID = "qd-" + fmt.Sprintf("%d", time.Now().UnixNano())

	qoderLoginStore.Lock()
	// 顺手清掉过期会话：没人 poll 的 state 否则会一直留在内存里。
	now := time.Now()
	for id, st := range qoderLoginStore.m {
		if now.Sub(st.Created) > qoderLoginTTL {
			delete(qoderLoginStore.m, id)
		}
	}
	// 上限兜底：连点「添加账号」也不至于把 map 撑爆。
	if len(qoderLoginStore.m) > 64 {
		for id := range qoderLoginStore.m {
			delete(qoderLoginStore.m, id)
			if len(qoderLoginStore.m) <= 32 {
				break
			}
		}
	}
	qoderLoginStore.m[stateID] = qoderLoginState{
		Verifier: verifier, Nonce: nonce, Realm: realm,
		MachineID: machineID, Created: now,
	}
	qoderLoginStore.Unlock()
	return stateID, authURL, nil
}

// QoderOAuthPoll 轮询一次授权结果。
// status: pending / ok / expired / error。ok 时 account 非空。
func QoderOAuthPoll(stateID string) (string, *QoderAccount, string) {
	qoderLoginStore.Lock()
	st, ok := qoderLoginStore.m[stateID]
	qoderLoginStore.Unlock()
	if !ok {
		return "expired", nil, "授权会话不存在，请重新发起"
	}
	if time.Since(st.Created) > qoderLoginTTL {
		qoderLoginStore.Lock()
		delete(qoderLoginStore.m, stateID)
		qoderLoginStore.Unlock()
		return "expired", nil, "授权窗口已过期，请重新发起"
	}

	cfg := qoderRealmConfig(st.Realm)
	q := url.Values{}
	q.Set("nonce", st.Nonce)
	q.Set("verifier", st.Verifier)
	q.Set("challenge_method", "S256")
	pollURL := cfg.OpenAPI + "/api/v1/deviceToken/poll?" + q.Encode()

	req, err := http.NewRequest(http.MethodGet, pollURL, nil)
	if err != nil {
		return "error", nil, err.Error()
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "QoderWork")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		// 网络抖动按「还没好」处理，不要让一次超时把整个授权作废。
		return "pending", nil, "轮询失败，稍后自动重试"
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return "pending", nil, "等待浏览器完成授权"
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "error", nil, fmt.Sprintf("poll http %d: %s", resp.StatusCode, truncateText(string(raw), 160))
	}
	var data struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		UserID       string `json:"user_id"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "pending", nil, "等待授权结果"
	}
	token := data.Token
	if token == "" {
		token = data.DeviceToken
	}
	if token == "" {
		return "pending", nil, "等待令牌"
	}
	uid := data.UserID
	nickname, userType, orgID, orgName := "", "", "", ""
	if info, err := qoderFetchUserInfo(cfg.OpenAPI, token); err == nil {
		if info.UID != "" {
			uid = info.UID
		}
		nickname = info.Nickname
		userType = info.UserType
	}
	if uid == "" {
		uid = "q-" + strings.ReplaceAll(qoderUUID(), "-", "")[:24]
	}
	if nickname == "" {
		nickname = "qoder-" + st.Realm
	}
	acct := &QoderAccount{
		Name:        nickname,
		AccessToken: token,
		Realm:       st.Realm,
		UID:         uid,
		Nickname:    nickname,
		UserType:    userType,
		OrgID:       orgID,
		OrgName:     orgName,
	}
	qoderLoginStore.Lock()
	delete(qoderLoginStore.m, stateID)
	qoderLoginStore.Unlock()
	return "ok", acct, "授权成功"
}

// qoderFetchUserInfo 读 userinfo（与 PAT 校验共用）。
func qoderFetchUserInfo(openAPI, token string) (QoderIdentity, error) {
	req, err := http.NewRequest(http.MethodGet, openAPI+"/api/v1/userinfo", nil)
	if err != nil {
		return QoderIdentity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
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
