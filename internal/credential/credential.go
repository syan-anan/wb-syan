// Package credential 处理 WorkBuddy 客户端导出的**加密凭证**（.info 文件）。
//
// 背景：新版客户端把凭据里的敏感字段（accessToken / refreshToken / 手机号 / 昵称 /
// 部门）以 `$wbEncrypted` 信封形式落盘 —— AES-256-GCM，密钥来自客户端安装目录的
// at-rest secret。网关要能"粘贴导入 .info"，就必须先把这些字段解出来，否则
// auth.Parse 只看到一堆信封，导入必然失败。
//
// 密钥从哪来：客户端那份 secret 只有本机客户端能取（参考实现靠 electron 进程探）；
// 服务端（Linux 容器）没有客户端，所以由运维通过 key 文件提供（路径见
// config.credential_key_file / 环境变量）。key 文件格式与参考实现一致：
//
//	{"keyId": "<sha256(key) 前 16 位十六进制>", "key": "<32 字节 key 的 base64>"}
//
// 只依赖标准库（crypto/aes + cipher.AEAD）：协议翻译/加解密引第三方库只会增加
// 供应链面，AES-GCM 与 base64 标准库已经有。
package credential

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxKeyFileSize key 文件体积上限：正常只有几百字节，超过即视为配错/被塞了别的东西。
const maxKeyFileSize = 4096

// maxDepth 信封递归深度上限：凭证结构正常只有四五层，超过说明输入畸形（防爆栈）。
const maxDepth = 32

// ErrKeyMissing 未配置解密能力时的统一提示（分类错误，调用方据此给运维可执行的指引）。
var ErrKeyMissing = errors.New("未配置凭证解密密钥（credential_key_file）")

// Key 一份对称密钥 + 它的 keyId（keyId = sha256(key) 的前 16 位十六进制）。
type Key struct {
	id  string
	raw [32]byte
}

// ID 返回 keyId（信封里的 keyId 必须与它一致，否则是版本不匹配）。
func (k *Key) ID() string {
	if k == nil {
		return ""
	}
	return k.id
}

// NewKey 用 32 字节原始密钥构造。长度不为 32 返回错误（绝不补齐/截断）。
func NewKey(raw []byte) (*Key, error) {
	if len(raw) != 32 {
		return nil, fmt.Errorf("credential: key must be 32 bytes, got %d", len(raw))
	}
	k := &Key{}
	copy(k.raw[:], raw)
	sum := sha256.Sum256(k.raw[:])
	k.id = hex.EncodeToString(sum[:])[:16]
	return k, nil
}

// keyFile 磁盘上的 key 文件格式（与参考实现互通）。
type keyFile struct {
	KeyID string `json:"keyId"`
	Key   string `json:"key"`
}

// LoadKeyFile 读取并校验 key 文件；路径为空/不存在时返回 ErrKeyMissing。
//
// 三重校验（缺一不可）：base64 必须规范、解出必须 32 字节、自带 keyId 必须等于
// 实际算出来的 keyId。只信算出来的那个 —— keyId 写错的文件会让每个信封都报
// "版本不匹配"，把配置错误伪装成数据错误。
func LoadKeyFile(path string) (*Key, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, ErrKeyMissing
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w（%s）", ErrKeyMissing, path)
	}
	if st.Size() > maxKeyFileSize {
		return nil, errors.New("credential: key 文件过大，疑似配置错误")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("credential: 读取 key 文件失败: %w", err)
	}
	var kf keyFile
	if err := json.Unmarshal(raw, &kf); err != nil {
		return nil, errors.New("credential: key 文件不是合法 JSON")
	}
	key, err := decodeBase64Strict(kf.Key)
	if err != nil {
		return nil, errors.New("credential: key 字段不是合法 base64")
	}
	k, err := NewKey(key)
	if err != nil {
		return nil, err
	}
	if kf.KeyID != "" && kf.KeyID != k.id {
		return nil, errors.New("credential: key 文件的 keyId 与 key 不符（请重新导出）")
	}
	return k, nil
}

// decodeBase64Strict 严格 base64 解码：拒绝非规范编码（防"能解出字节但语义已坏"）。
func decodeBase64Strict(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty")
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, err
	}
	if base64.StdEncoding.EncodeToString(raw) != s {
		return nil, errors.New("non-canonical base64")
	}
	return raw, nil
}

// aad 构造官方 sym-v1 字段绑定串（WBEV1 帧格式，suite 1，无 sequence/final）。
//
// 逐字节对齐是硬要求：AAD 参与 GCM 的认证标签计算，差一个字节解密就会失败。所以
// 这里刻意手写帧格式而不是"看起来等价"的拼接。
func aad(keyID string) []byte {
	var buf bytes.Buffer
	buf.WriteString("WB-AAD\x00\x01")
	text := func(s string) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(s)))
		buf.Write(l[:])
		buf.WriteString(s)
	}
	text("WBEV1")
	text("sym-v1")
	var one [4]byte
	binary.BigEndian.PutUint32(one[:], 1)
	buf.Write(one[:])
	text(keyID)
	buf.Write([]byte{0x02, 0x00, 0x00})
	return buf.Bytes()
}

// envelope 信封本体（base64 后的 JSON）。
type envelope struct {
	Suite      int    `json:"suite"`
	KeyID      string `json:"keyId"`
	Nonce      string `json:"nonce"`
	AuthTag    string `json:"authTag"`
	Ciphertext string `json:"ciphertext"`
}

// wrapper 字段包装：{"$wbEncrypted":1,"envelope":"<base64 envelope>"}。
type wrapper struct {
	Encrypted int             `json:"$wbEncrypted"`
	Envelope  string          `json:"envelope"`
	Scheme    json.RawMessage `json:"scheme"`
}

// openField 解开一个信封，返回明文字段值。
func openField(w wrapper, k *Key) (string, error) {
	if len(w.Scheme) > 0 && string(w.Scheme) != "null" {
		return "", errors.New("credential: 该凭证使用了暂不支持的高级加密格式")
	}
	if w.Encrypted != 1 {
		return "", errors.New("credential: 信封标记不是 $wbEncrypted=1")
	}
	rawEnv, err := decodeBase64Strict(w.Envelope)
	if err != nil {
		return "", errors.New("credential: envelope 不是合法 base64")
	}
	var env envelope
	if err := json.Unmarshal(rawEnv, &env); err != nil {
		return "", errors.New("credential: envelope 不是合法 JSON")
	}
	if env.Suite != 1 {
		return "", fmt.Errorf("credential: 不支持的 suite=%d", env.Suite)
	}
	if env.KeyID != k.id {
		return "", errors.New("credential: 凭证 keyId 与当前密钥不匹配（加密版本不同，请用导出它的客户端密钥或重新导出）")
	}
	nonce, err1 := decodeBase64Strict(env.Nonce)
	tag, err2 := decodeBase64Strict(env.AuthTag)
	ct, err3 := decodeBase64Strict(env.Ciphertext)
	if err1 != nil || err2 != nil || err3 != nil {
		return "", errors.New("credential: envelope 字段不是合法 base64")
	}
	if len(nonce) != 12 || len(tag) != 16 {
		return "", errors.New("credential: envelope nonce/authTag 长度非法")
	}
	aead, err := newAEAD(k)
	if err != nil {
		return "", err
	}
	plain, err := aead.Open(nil, nonce, append(append([]byte{}, ct...), tag...), aad(k.id))
	if err != nil {
		return "", errors.New("credential: 解密失败（凭证损坏或密钥不对）")
	}
	return string(plain), nil
}

func newAEAD(k *Key) (cipher.AEAD, error) {
	block, err := aes.NewCipher(k.raw[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Normalize 把 raw JSON 里所有 $wbEncrypted 信封就地解成明文，返回规范化后的 JSON。
//
// 没有信封时原样返回（老格式零改动）；完全没有启用密钥时也原样返回，让后续的
// 校验器去报它自己的错（不该因为"没配密钥"就把明文凭证也拒了）。
func Normalize(raw []byte, k *Key) ([]byte, error) {
	trimmed := bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var doc any
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return raw, nil // 非 JSON：交给既有校验器报"格式错误"
	}
	if k == nil {
		if containsEncrypted(doc, 0) {
			return nil, ErrKeyMissing
		}
		return trimmed, nil
	}
	out, changed, err := walk(doc, k, 0)
	if err != nil {
		return nil, err
	}
	if !changed {
		return trimmed, nil
	}
	return json.Marshal(out)
}

// containsEncrypted 报告结构里是否出现信封（用于"没配密钥"时的可执行提示）。
func containsEncrypted(v any, depth int) bool {
	if depth > maxDepth {
		return false
	}
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["$wbEncrypted"]; ok {
			return true
		}
		for _, x := range t {
			if containsEncrypted(x, depth+1) {
				return true
			}
		}
	case []any:
		for _, x := range t {
			if containsEncrypted(x, depth+1) {
				return true
			}
		}
	}
	return false
}

// walk 递归解密；返回 (处理后的结构, 是否有改动)。
func walk(v any, k *Key, depth int) (any, bool, error) {
	if depth > maxDepth {
		return nil, false, errors.New("credential: 凭证结构嵌套过深")
	}
	switch t := v.(type) {
	case map[string]any:
		if raw, ok := t["$wbEncrypted"]; ok {
			if _, isNum := raw.(float64); !isNum {
				return nil, false, errors.New("credential: $wbEncrypted 标记不是数字")
			}
			blob, err := json.Marshal(t)
			if err != nil {
				return nil, false, err
			}
			var w wrapper
			if err := json.Unmarshal(blob, &w); err != nil {
				return nil, false, err
			}
			val, err := openField(w, k)
			if err != nil {
				return nil, false, err
			}
			return val, true, nil
		}
		out := make(map[string]any, len(t))
		changed := false
		for key, val := range t {
			nv, ch, err := walk(val, k, depth+1)
			if err != nil {
				return nil, false, err
			}
			out[key] = nv
			changed = changed || ch
		}
		return out, changed, nil
	case []any:
		out := make([]any, len(t))
		changed := false
		for i, val := range t {
			nv, ch, err := walk(val, k, depth+1)
			if err != nil {
				return nil, false, err
			}
			out[i] = nv
			changed = changed || ch
		}
		return out, changed, nil
	}
	return v, false, nil
}

// protectFields 需要加密的字段：auth 里的两个 token，以及账号档案里的三个展示字段
// （与官方导出的字段集合一致 —— 多加密一个字段会让客户端读不出昵称，少加密一个则
// 明文泄露，都算错）。
var (
	authSecretFields = []string{"accessToken", "refreshToken"}
	profileFields    = []string{"phoneNumber", "nickname", "departmentFullName"}
)

// Encrypt 把明文凭证封成官方 `.info` 形态（导出用）。
//
// 每个字段独立 nonce（同一密钥重复用 nonce 会直接毁掉 GCM 的安全性），因此同一份
// 凭证每次导出的密文都不同 —— 这是正确行为，不是不确定性。
func Encrypt(raw []byte, k *Key) ([]byte, error) {
	if k == nil {
		return nil, ErrKeyMissing
	}
	normalized, err := Normalize(raw, k)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(normalized, &doc); err != nil {
		return nil, errors.New("credential: 凭证不是合法 JSON")
	}
	if _, ok := doc["auth"].(map[string]any); !ok {
		return nil, errors.New("credential: 凭证结构无效（缺少 auth 对象）")
	}
	aead, err := newAEAD(k)
	if err != nil {
		return nil, err
	}
	protect := func(area any, names []string) error {
		m, ok := area.(map[string]any)
		if !ok {
			return nil
		}
		for _, name := range names {
			s, ok := m[name].(string)
			if !ok || s == "" {
				continue
			}
			nonce := make([]byte, 12)
			if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
				return err
			}
			sealed := aead.Seal(nil, nonce, []byte(s), aad(k.id))
			env := envelope{
				Suite:      1,
				KeyID:      k.id,
				Nonce:      base64.StdEncoding.EncodeToString(nonce),
				AuthTag:    base64.StdEncoding.EncodeToString(sealed[len(sealed)-16:]),
				Ciphertext: base64.StdEncoding.EncodeToString(sealed[:len(sealed)-16]),
			}
			envRaw, err := json.Marshal(env)
			if err != nil {
				return err
			}
			m[name] = map[string]any{
				"$wbEncrypted": 1,
				"envelope":     base64.StdEncoding.EncodeToString(envRaw),
			}
		}
		return nil
	}
	if err := protect(doc["auth"], authSecretFields); err != nil {
		return nil, err
	}
	if err := protect(doc["account"], profileFields); err != nil {
		return nil, err
	}
	for _, group := range []string{"accounts", "allAccounts"} {
		list, ok := doc[group].([]any)
		if !ok {
			continue
		}
		for _, item := range list {
			if err := protect(item, profileFields); err != nil {
				return nil, err
			}
		}
	}
	return json.Marshal(doc)
}
