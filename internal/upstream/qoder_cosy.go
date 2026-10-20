// qoder_cosy.go —— Qoder 国内版（gateway.qoder.com.cn）COSY 签名层。
//
// 协议移植自 shuishuipingan/qoder2api-hub（MIT）的 qoder_sign.py / qoder_fingerprint.py。
// 三条不变量，改动会直接让上游返回 4xx：
//  1. 请求体必须先经 qoderEncode（自定义 Base64 变体）再参与签名；
//  2. 签名原文按 payload \n cosyKey \n date \n body \n path 连接，
//     path 需剥掉前缀 "/algo"；
//  3. info 明文是 identity 的「键排序 + 无空白」JSON，AES 的 key 与 iv 同为 tempKey。
package upstream

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// qoderCosyVersion 与官方 1.1.x 客户端对齐；服务端按它校验签名版本。
	qoderCosyVersion  = "1.1.64"
	qoderCosyAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	qoderCosyStdTable = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	qoderCosyPad      = '$'

	// qoderCosyClientIP 客户端上报字段；官方桌面端固定写链路本地地址。
	qoderCosyClientIP = "169.254.198.161"

	// qoderCosyPubPEM 官方客户端内嵌的服务端公钥（1024 位 RSA）。
	// 它与服务端私钥配对，换长度在数学上不可行，只能随官方客户端更新。
	qoderCosyPubPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`
)

var (
	qoderStdToCustom [256]byte
	qoderCustomToStd [256]byte

	qoderPubOnce sync.Once
	qoderPubKey  *rsa.PublicKey
	qoderPubErr  error
)

func init() {
	for i := 0; i < 64; i++ {
		qoderStdToCustom[qoderCosyStdTable[i]] = qoderCosyAlphabet[i]
		qoderCustomToStd[qoderCosyAlphabet[i]] = qoderCosyStdTable[i]
	}
	qoderStdToCustom['='] = qoderCosyPad
	qoderCustomToStd[qoderCosyPad] = '='
}

// qoderEncode 把明文编成 Qoder 自定义 Base64：标准编码后按「尾/中/首」三段轮转，
// 再逐字符换字母表，'=' 映射为 '$'。
func qoderEncode(plain []byte) string {
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	out := make([]byte, len(rearranged))
	for i := 0; i < len(rearranged); i++ {
		out[i] = qoderStdToCustom[rearranged[i]]
	}
	return string(out)
}

// qoderDecode 是 qoderEncode 的逆运算，仅用于调试与测试。
func qoderDecode(encoded string) ([]byte, error) {
	std := make([]byte, len(encoded))
	for i := 0; i < len(encoded); i++ {
		b := qoderCustomToStd[encoded[i]]
		if b == 0 {
			return nil, fmt.Errorf("qoder: 非法字符 %q", encoded[i])
		}
		std[i] = b
	}
	n := len(std)
	if n < 3 {
		return base64.StdEncoding.DecodeString(string(std))
	}
	a := n / 3
	// 正向是 R = S[n-a:] + S[a:n-a] + S[:a]，逆向拼回 S = R中 + R尾... 见下。
	// 按正向切分位置还原：R1 = S[n-a:]（长度 a），R2 = S[a:n-a]，R3 = S[:a]。
	r1 := std[:a]
	r2 := std[a : n-a]
	r3 := std[n-a:]
	joined := append(append(append([]byte{}, r3...), r2...), r1...)
	return base64.StdEncoding.DecodeString(string(joined))
}

// jsonSortedCompact 生成「键排序 + 无空白 + 不转义 HTML」的紧凑 JSON。
// 服务端按原始字节校验签名，任何一个空格或转义都会导致验签失败。
func jsonSortedCompact(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(marshalNoHTMLEscape(k))
		buf.WriteByte(':')
		buf.Write(marshalNoHTMLEscape(m[k]))
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

func marshalNoHTMLEscape(v string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// qoderPublicKey 解析内嵌的 SPKI 公钥，只在首次调用时解析。
func qoderPublicKey() (*rsa.PublicKey, error) {
	qoderPubOnce.Do(func() {
		block, _ := pem.Decode([]byte(qoderCosyPubPEM))
		if block == nil {
			qoderPubErr = fmt.Errorf("qoder: 公钥 PEM 解析失败")
			return
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			qoderPubErr = err
			return
		}
		k, ok := pub.(*rsa.PublicKey)
		if !ok {
			qoderPubErr = fmt.Errorf("qoder: 公钥不是 RSA")
			return
		}
		qoderPubKey = k
	})
	return qoderPubKey, qoderPubErr
}

// qoderWrapSessionKey 用服务端公钥 RSA/PKCS#1 v1.5 包裹 16 字节会话密钥。
// 服务端用同一私钥解出后，拿它当 AES key/iv 解 info。
func qoderWrapSessionKey(tempKey string) (string, error) {
	pub, err := qoderPublicKey()
	if err != nil {
		return "", err
	}
	sealed, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(tempKey))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// aesCBCEncryptPKCS7 做 AES-CBC + PKCS#7；Qoder 的 info 用 key == iv。
func aesCBCEncryptPKCS7(plain, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	data := make([]byte, len(plain)+pad)
	copy(data, plain)
	for i := len(plain); i < len(data); i++ {
		data[i] = byte(pad)
	}
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, data)
	return out, nil
}

// qoderDeriveID 由 uid + salt 稳定派生设备/会话标识（32 位十六进制）。
// 同一账号必须长期得到同一值，否则会被上游判定为换机风控。
func qoderDeriveID(uid, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + uid))
	s := hex.EncodeToString(sum[:])
	if len(s) > 36 {
		s = s[:36]
	}
	return s
}

// qoderMachineType 18 位机器类型标识。
func qoderMachineType(uid string) string {
	s := strings.ReplaceAll(qoderDeriveID(uid, "machinetype"), "-", "")
	if len(s) > 18 {
		s = s[:18]
	}
	return s
}

// qoderMachineToken 43 字符 base64url 机器令牌。
func qoderMachineToken(uid string) string {
	sum := sha512.Sum512([]byte("machinetoken:" + uid))
	s := base64.RawURLEncoding.EncodeToString(sum[:])
	if len(s) > 43 {
		s = s[:43]
	}
	return s
}

// qoderRequestID 稳定前缀 + 微秒后缀，防重放且可溯源到账号。
func qoderRequestID(uid string) string {
	prefix := qoderDeriveID(uid, "req")
	suffix := fmt.Sprintf("%06d", time.Now().UnixNano()%1000000)
	return prefix + "-" + suffix
}

// qoderRandomTempKey 生成 16 个十六进制字符作为 AES-128 会话密钥。
// 熵只有 64 bit —— 这是协议现状：服务端按 ASCII 字节解包，换随机字节会失败。
func qoderRandomTempKey() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// cosySession 是单个账号的签名会话。tempKey/cosyKey/info 在 access token
// 轮换后必须整体重建。
type cosySession struct {
	UID          string
	Nickname     string
	AccessToken  string
	RefreshToken string
	UserType     string
	OrgID        string
	OrgName      string

	MachineID    string
	MachineToken string
	MachineType  string

	TempKey string
	CosyKey string
	Info    string
}

// newCosySession 构造会话；tempKey 为空时随机生成。
func newCosySession(uid, nickname, accessToken, refreshToken, userType, orgID, orgName, tempKey string) (*cosySession, error) {
	if tempKey == "" {
		k, err := qoderRandomTempKey()
		if err != nil {
			return nil, err
		}
		tempKey = k
	}
	if userType == "" {
		userType = "personal_professional_trial"
	}
	s := &cosySession{
		UID:          uid,
		Nickname:     nickname,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		UserType:     userType,
		OrgID:        orgID,
		OrgName:      orgName,
		MachineID:    qoderDeriveID(uid, "machine"),
		MachineToken: qoderMachineToken(uid),
		MachineType:  qoderMachineType(uid),
		TempKey:      tempKey,
	}
	cosyKey, err := qoderWrapSessionKey(tempKey)
	if err != nil {
		return nil, err
	}
	s.CosyKey = cosyKey

	identity := map[string]string{
		"name":                 nickname,
		"aid":                  uid,
		"uid":                  uid,
		"yx_uid":               "",
		"organization_id":      orgID,
		"organization_name":    orgName,
		"user_type":            userType,
		"security_oauth_token": accessToken,
		"refresh_token":        refreshToken,
	}
	sealed, err := aesCBCEncryptPKCS7(jsonSortedCompact(identity), []byte(tempKey), []byte(tempKey))
	if err != nil {
		return nil, err
	}
	s.Info = base64.StdEncoding.EncodeToString(sealed)
	return s, nil
}

// bearer 生成 Authorization 头：Bearer COSY.<payload>.<sig>。
// body 是已经 qoderEncode 过的请求体字符串，path 需要剥离 "/algo" 前缀。
func (s *cosySession) bearer(body, rawURL string) (payloadB64, date, auth string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", err
	}
	path := u.Path
	if path == "" {
		path = "/"
	}
	if strings.HasPrefix(path, "/algo") {
		path = strings.TrimPrefix(path, "/algo")
	}
	payload := map[string]string{
		"cosyVersion": qoderCosyVersion,
		"ideVersion":  "",
		"info":        s.Info,
		"requestId":   qoderRequestID(s.UID),
		"version":     "v1",
	}
	payloadB64 = base64.StdEncoding.EncodeToString(jsonSortedCompact(payload))
	date = strconv.FormatInt(time.Now().Unix(), 10)
	raw := strings.Join([]string{payloadB64, s.CosyKey, date, body, path}, "\n")
	sum := md5.Sum([]byte(raw))
	sig := hex.EncodeToString(sum[:])
	return payloadB64, date, "Bearer COSY." + payloadB64 + "." + sig, nil
}

// headers 返回一次推理请求的完整 COSY 头。modelKey 非空时带 x-model-key，
// 上游据此路由到具体模型。
func (s *cosySession) headers(body, rawURL, modelKey string, sse bool) (map[string]string, error) {
	_, date, auth, err := s.bearer(body, rawURL)
	if err != nil {
		return nil, err
	}
	accept := "application/json"
	if sse {
		accept = "text/event-stream"
	}
	h := map[string]string{
		"cosy-data-policy":  "AGREE",
		"content-type":      "application/json",
		"cosy-machinetype":  s.MachineType,
		"cosy-clienttype":   "5",
		"cosy-date":         date,
		"cosy-user":         s.UID,
		"cosy-key":          s.CosyKey,
		"accept":            accept,
		"cosy-clientip":     qoderCosyClientIP,
		"authorization":     auth,
		"accept-encoding":   "identity",
		"cosy-version":      qoderCosyVersion,
		"cosy-machineid":    s.MachineID,
		"cosy-machinetoken": s.MachineToken,
		"login-version":     "v2",
		"user-agent":        "Go-http-client/2.0",
	}
	if sse {
		h["cache-control"] = "no-cache"
	}
	if modelKey != "" {
		h["x-model-key"] = modelKey
		h["x-model-source"] = "system"
	}
	return h, nil
}

// qoderCosySHA256 供后续请求体摘要校验使用。
func qoderCosySHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
