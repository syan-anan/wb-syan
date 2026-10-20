package upstream

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestQoderCNLive 只在显式提供凭据时运行：
//
//	QODER_CN_PAT=pt-... QODER_CN_UID=... go test ./internal/upstream/ -run TestQoderCNLive -v
func TestQoderCNLive(t *testing.T) {
	pat := os.Getenv("QODER_CN_PAT")
	if pat == "" {
		t.Skip("QODER_CN_PAT 未设置")
	}
	uid := os.Getenv("QODER_CN_UID")
	if uid == "" {
		t.Skip("QODER_CN_UID 未设置")
	}
	c := NewQoderClient(pat, filepath.Join(t.TempDir(), "tok.json"))
	c.Realm = "cn"
	c.UID = uid
	c.Nickname = "syan"
	c.UserType = "personal_professional_trial"
	c.OpenAPI = "https://openapi.qoder.com.cn"
	c.ModelBase = qoderCNBase

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	body := []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	rc, status, errBody, err := c.ChatStream(ctx, body)
	if err != nil {
		t.Fatalf("status=%d err=%v body=%s", status, err, string(errBody))
	}
	defer rc.Close()
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	buf := make([]byte, 2048)
	n, _ := rc.Read(buf)
	out := string(buf[:n])
	if !strings.Contains(out, "data:") {
		t.Fatalf("no SSE frame: %q", out)
	}
	if !strings.Contains(out, "choices") {
		t.Fatalf("no choices in frame: %q", out)
	}
	t.Logf("first frame: %s", strings.TrimSpace(out[:200]))
	_ = io.Discard
}
