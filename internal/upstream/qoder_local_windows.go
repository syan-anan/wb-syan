//go:build windows

// qoder_local_windows.go —— 只读导入本机 Qoder 桌面客户端的登录凭证。
//
// 官方存两处（按安装版本不同）：
//
//	%APPDATA%\com.qodercn.app.stable\  → 国内版
//	%APPDATA%\com.qoder.app.stable\    → 国际版
//
// 文件布局（Chromium os_crypt）：
//
//	Local State 里 os_crypt.encrypted_key = base64("DPAPI" + DPAPI(32B key))
//	auth.v1.dat  = "v10" + nonce[12] + AES-256-GCM(ciphertext||tag)
//	明文 JSON    = {token: "dt-…", refreshToken: "drt-…", user: {id, name}}
//
// 全程只读；解不开就返回错误，绝不猜测。
package upstream

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	qoderCrypt32   = syscall.NewLazyDLL("crypt32.dll")
	qoderKernel32  = syscall.NewLazyDLL("kernel32.dll")
	qoderUnprotect = qoderCrypt32.NewProc("CryptUnprotectData")
	qoderLocalFree = qoderKernel32.NewProc("LocalFree")
)

// dpapiUnprotect 调 Windows DPAPI 解开当前用户加密的字节串。
func dpapiUnprotect(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("dpapi: 空输入")
	}
	in := dataBlob{cbData: uint32(len(data)), pbData: &data[0]}
	var out dataBlob
	ret, _, err := qoderUnprotect.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)))
	if ret == 0 {
		return nil, fmt.Errorf("dpapi: CryptUnprotectData 失败: %v", err)
	}
	defer qoderLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	buf := unsafe.Slice(out.pbData, out.cbData)
	res := make([]byte, len(buf))
	copy(res, buf)
	return res, nil
}

// qoderLocalAuth 桌面端 auth.v1.dat 的明文结构（只取需要的字段）。
type qoderLocalAuth struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refreshToken"`
	User         struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"user"`
}

// ImportLocalQoder 扫描两个官方数据目录，返回解出的账号。
func ImportLocalQoder() ([]QoderAccount, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return nil, fmt.Errorf("qoder: 读不到 APPDATA")
	}
	type target struct {
		dir   string
		realm string
	}
	targets := []target{
		{"com.qodercn.app.stable", "cn"},
		{"com.qoder.app.stable", "intl"},
	}
	var out []QoderAccount
	var firstErr error
	for _, t := range targets {
		base := filepath.Join(appData, t.dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		acct, err := readQoderDesktopAuth(base, t.realm)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", t.dir, err)
			}
			continue
		}
		out = append(out, acct)
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, fmt.Errorf("qoder: 没找到本机已登录的客户端凭证")
	}
	return out, nil
}

func readQoderDesktopAuth(base, realm string) (QoderAccount, error) {
	stateRaw, err := os.ReadFile(filepath.Join(base, "Local State"))
	if err != nil {
		return QoderAccount{}, err
	}
	var state struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(stateRaw, &state); err != nil {
		return QoderAccount{}, err
	}
	keyB64 := state.OSCrypt.EncryptedKey
	if keyB64 == "" {
		return QoderAccount{}, fmt.Errorf("Local State 缺少 os_crypt.encrypted_key")
	}
	blob, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return QoderAccount{}, err
	}
	if len(blob) < 5 || string(blob[:5]) != "DPAPI" {
		return QoderAccount{}, fmt.Errorf("encrypted_key 缺少 DPAPI 前缀")
	}
	key, err := dpapiUnprotect(blob[5:])
	if err != nil {
		return QoderAccount{}, err
	}
	raw, err := os.ReadFile(filepath.Join(base, "auth.v1.dat"))
	if err != nil {
		return QoderAccount{}, err
	}
	if len(raw) < 3+12+16 || string(raw[:3]) != "v10" {
		return QoderAccount{}, fmt.Errorf("auth.v1.dat 布局不认识")
	}
	nonce := raw[3:15]
	sealed := raw[15:]
	block, err := aes.NewCipher(key)
	if err != nil {
		return QoderAccount{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return QoderAccount{}, err
	}
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return QoderAccount{}, fmt.Errorf("解密失败: %w", err)
	}
	var a qoderLocalAuth
	if err := json.Unmarshal(plain, &a); err != nil {
		return QoderAccount{}, err
	}
	if !strings.HasPrefix(a.Token, "dt-") {
		return QoderAccount{}, fmt.Errorf("token 不是设备令牌（%s…）", truncateText(a.Token, 6))
	}
	name := a.User.Name
	if name == "" {
		name = a.User.Email
	}
	return QoderAccount{
		Name:        name,
		AccessToken: a.Token,
		Realm:       realm,
		UID:         a.User.ID,
		Nickname:    name,
	}, nil
}
