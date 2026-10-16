package credential

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) *Key {
	t.Helper()
	raw := bytes.Repeat([]byte{0x2a}, 32)
	k, err := NewKey(raw)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return k
}

const samplePlain = `{
  "auth": {"accessToken": "at-abc", "refreshToken": "rt-def", "expiresAt": 1893456000},
  "account": {"uid": "1ac6b920-8b97-4749-85e7-889cf60f6730", "nickname": "syan-anan",
              "phoneNumber": "15516263612", "departmentFullName": "研发"},
  "accounts": [{"uid": "other", "nickname": "x", "phoneNumber": "13800000000"}]
}`

// TestEncryptNormalizeRoundTrip 导出（加密）→ 导入（解密）必须回到完全一致的明文。
func TestEncryptNormalizeRoundTrip(t *testing.T) {
	k := testKey(t)
	enc, err := Encrypt([]byte(samplePlain), k)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !bytes.Contains(enc, []byte("$wbEncrypted")) {
		t.Fatalf("加密后应出现信封: %s", enc)
	}
	if bytes.Contains(enc, []byte("at-abc")) || bytes.Contains(enc, []byte("15516263612")) {
		t.Fatalf("明文 token / 手机号不该留在导出结果里")
	}

	back, err := Normalize(enc, k)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(back, &got); err != nil {
		t.Fatalf("解出的不是 JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(samplePlain), &want); err != nil {
		t.Fatal(err)
	}
	if !equalJSON(got, want) {
		t.Fatalf("往返后不一致:\n got=%v\nwant=%v", got, want)
	}
}

// TestEncryptFreshNonce 同一份凭证两次导出的密文必须不同（每字段独立 nonce；
// 重复 nonce 会直接毁掉 GCM 的安全性）。
func TestEncryptFreshNonce(t *testing.T) {
	k := testKey(t)
	a, err := Encrypt([]byte(samplePlain), k)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Encrypt([]byte(samplePlain), k)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("两次导出的密文相同 —— nonce 复用了")
	}
}

// TestNormalizePassthrough 无信封的明文凭证原样通过（老格式零改动，含 BOM 剥离）。
func TestNormalizePassthrough(t *testing.T) {
	k := testKey(t)
	for _, in := range []string{samplePlain, "\ufeff" + samplePlain, "not json at all"} {
		out, err := Normalize([]byte(in), k)
		if err != nil {
			t.Fatalf("应原样通过，实际报错: %v", err)
		}
		if strings.Contains(string(out), "$wbEncrypted") {
			t.Fatalf("不该凭空造出信封")
		}
	}
}

// TestNormalizeWithoutKey 遇到信封但没配密钥 → 明确的 ErrKeyMissing（给运维可执行提示）。
func TestNormalizeWithoutKey(t *testing.T) {
	k := testKey(t)
	enc, err := Encrypt([]byte(samplePlain), k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Normalize(enc, nil); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("want ErrKeyMissing, got %v", err)
	}
	// 明文输入 + 无密钥：不该被拒（报错留给既有格式校验器）。
	if _, err := Normalize([]byte(samplePlain), nil); err != nil {
		t.Fatalf("明文不该因缺密钥被拒: %v", err)
	}
}

// TestOpenWrongKey keyId 不匹配与密钥不对都要给出可读错误而不是 panic。
func TestOpenWrongKey(t *testing.T) {
	k := testKey(t)
	enc, err := Encrypt([]byte(samplePlain), k)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewKey(bytes.Repeat([]byte{0x11}, 32))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Normalize(enc, other)
	if err == nil || !strings.Contains(err.Error(), "keyId") {
		t.Fatalf("换密钥应报 keyId 不匹配，实际 %v", err)
	}
}

// TestAADFraming AAD 帧格式逐字节锁定：它参与 GCM 认证标签，差一字节就解不开。
func TestAADFraming(t *testing.T) {
	k := testKey(t)
	got := aad(k.ID())
	var want bytes.Buffer
	want.WriteString("WB-AAD\x00\x01")
	writeText := func(s string) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(s)))
		want.Write(l[:])
		want.WriteString(s)
	}
	writeText("WBEV1")
	writeText("sym-v1")
	var one [4]byte
	binary.BigEndian.PutUint32(one[:], 1)
	want.Write(one[:])
	writeText(k.ID())
	want.Write([]byte{0x02, 0x00, 0x00})
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("AAD 不符:\n got=%q\nwant=%q", got, want.Bytes())
	}
	if len(k.ID()) != 16 {
		t.Fatalf("keyId 应为 16 位十六进制，实际 %q", k.ID())
	}
}

// TestKeyIDDerivation keyId = sha256(key) 前 16 位十六进制。
func TestKeyIDDerivation(t *testing.T) {
	raw := bytes.Repeat([]byte{0x2a}, 32)
	k, err := NewKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if k.ID() != hex.EncodeToString(sum[:])[:16] {
		t.Fatalf("keyId 推导不符: %s", k.ID())
	}
	if _, err := NewKey(raw[:31]); err == nil {
		t.Fatal("长度不对的 key 必须被拒（不补齐/不截断）")
	}
}

// TestLoadKeyFile key 文件三重校验 + 缺失时的可执行提示。
func TestLoadKeyFile(t *testing.T) {
	dir := t.TempDir()
	raw := bytes.Repeat([]byte{0x2a}, 32)
	k, _ := NewKey(raw)

	good := filepath.Join(dir, "credential-key.json")
	body, _ := json.Marshal(map[string]string{
		"keyId": k.ID(),
		"key":   base64.StdEncoding.EncodeToString(raw),
	})
	if err := os.WriteFile(good, body, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadKeyFile(good)
	if err != nil || loaded.ID() != k.ID() {
		t.Fatalf("加载失败: %v %v", loaded, err)
	}

	bad := filepath.Join(dir, "bad.json")
	body2, _ := json.Marshal(map[string]string{
		"keyId": "0000000000000000",
		"key":   base64.StdEncoding.EncodeToString(raw),
	})
	_ = os.WriteFile(bad, body2, 0o600)
	if _, err := LoadKeyFile(bad); err == nil {
		t.Fatal("keyId 不符必须被拒")
	}

	if _, err := LoadKeyFile(filepath.Join(dir, "nope.json")); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("缺失文件应报 ErrKeyMissing，实际 %v", err)
	}
	if _, err := LoadKeyFile(""); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("空路径应报 ErrKeyMissing，实际 %v", err)
	}
}

func equalJSON(a, b any) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return bytes.Equal(ra, rb)
}
