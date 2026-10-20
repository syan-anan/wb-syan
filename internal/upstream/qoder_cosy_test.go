package upstream

import (
	"encoding/hex"
	"strings"
	"testing"
)

// 向量由参考实现 shuishuipingan/qoder2api-hub 的 qoder_sign.py 生成。
func TestQoderEncodeVectors(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", "q$FruHPH"},
		{`{"model":"auto"}`, "JHq*ep$$uoByBOg.mYKtu(CL"},
		{"hi", "$HzP"},
	}
	for _, c := range cases {
		if got := qoderEncode([]byte(c.in)); got != c.want {
			t.Errorf("qoderEncode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestQoderDecodeRoundTrip(t *testing.T) {
	for _, s := range []string{"hello", `{"model":"auto"}`, "hi", "", strings.Repeat("x", 100)} {
		got, err := qoderDecode(qoderEncode([]byte(s)))
		if err != nil {
			t.Fatalf("qoderDecode(%q): %v", s, err)
		}
		if string(got) != s {
			t.Errorf("round trip %q -> %q", s, got)
		}
	}
}

func TestAESCBCPKCS7Vector(t *testing.T) {
	key := []byte("0123456789abcdef")
	out, err := aesCBCEncryptPKCS7([]byte(`{"a":"b"}`), key, key)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(out); got != "e4625bd1f0aee6662446ddbf0fe5004c" {
		t.Fatalf("ciphertext = %s", got)
	}
}

func TestJSONSortedCompact(t *testing.T) {
	m := map[string]string{"b": "2", "a": "1", "c": ""}
	got := string(jsonSortedCompact(m))
	if got != `{"a":"1","b":"2","c":""}` {
		t.Fatalf("got %s", got)
	}
	// 尖括号不能被转义成 \u003c，否则服务端验签字节不一致。
	got = string(jsonSortedCompact(map[string]string{"k": "<&>"}))
	if got != `{"k":"<&>"}` {
		t.Fatalf("html escape leaked: %s", got)
	}
}

func TestQoderFingerprintStable(t *testing.T) {
	uid := "01a1557a-64a8-7e27-b74a-41205a872ddc"
	if got := qoderDeriveID(uid, "machine"); got != "a13313518a085f94fd712198a05d2fea" {
		t.Errorf("machine id = %s", got)
	}
	if got := qoderMachineType(uid); got != "68c2791c9d9c017e77" {
		t.Errorf("machine type = %s", got)
	}
	if got := qoderMachineToken(uid); got != "GBI6PKJ22Cibz1bjJVTBxPB-ugWFsLK_QKzjRSyGjUN" {
		t.Errorf("machine token = %s", got)
	}
	// 幂等：同一 uid 多次派生必须一致。
	if qoderDeriveID(uid, "machine") != qoderDeriveID(uid, "machine") {
		t.Error("derive not stable")
	}
}

func TestCosySessionHeaders(t *testing.T) {
	s, err := newCosySession("01a1557a-64a8-7e27-b74a-41205a872ddc", "syan", "at-1", "rt-1", "", "", "", "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if s.Info == "" || s.CosyKey == "" {
		t.Fatal("session not initialized")
	}
	body := qoderEncode([]byte(`{"model":"auto"}`))
	h, err := s.headers(body, "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation", "auto", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h["authorization"], "Bearer COSY.") {
		t.Errorf("bad auth header: %s", h["authorization"])
	}
	if h["x-model-key"] != "auto" || h["x-model-source"] != "system" {
		t.Error("model key headers missing")
	}
	if h["cosy-machineid"] != "a13313518a085f94fd712198a05d2fea" {
		t.Errorf("machineid = %s", h["cosy-machineid"])
	}
	if h["accept"] != "text/event-stream" {
		t.Errorf("accept = %s", h["accept"])
	}
}

func TestCosyBearerPathStripsAlgo(t *testing.T) {
	s, err := newCosySession("u1", "", "at", "rt", "", "", "", "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	_, _, a, err := s.bearer("body", "https://gateway.qoder.com.cn/algo/api/v2/x")
	if err != nil {
		t.Fatal(err)
	}
	_, _, b, err := s.bearer("body", "https://gateway.qoder.com.cn/api/v2/x")
	if err != nil {
		t.Fatal(err)
	}
	// 两次签名主体不同（requestId/date 不同），但都要能生成且格式合法。
	for _, v := range []string{a, b} {
		parts := strings.Split(strings.TrimPrefix(v, "Bearer COSY."), ".")
		if len(parts) != 2 || len(parts[1]) != 32 {
			t.Errorf("bad bearer shape: %s", v)
		}
	}
}
