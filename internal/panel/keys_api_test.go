package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/syan-anan/wb-syan/internal/keys"
	"github.com/syan-anan/wb-syan/internal/livecfg"
)

// keysAPIHarness 装配一个只依赖 livecfg + SaveKeys 的面板（不碰 Pool/Upstream），
// 用来端到端验证密钥管理接口：新建（含全部约束）→ 列表回显 → 运行时身份解析 → 修改 → 删除。
func keysAPIHarness(t *testing.T) (*Panel, *livecfg.Holder) {
	t.Helper()
	live := livecfg.New(livecfg.Snapshot{APIKey: "test-key", Keys: keys.New("test-key", nil)})
	p := New(Config{
		Version:  "test",
		APIKey:   "test-key",
		Live:     live,
		KeyQuota: keys.NewQuota(""),
		SaveKeys: func(entries []keys.Entry) error {
			norm, _ := keys.NormalizeEntries(entries)
			snap := live.Load()
			snap.Keys = keys.New(snap.APIKey, norm)
			live.Store(snap)
			return nil
		},
	})
	return p, live
}

func panelReq(p *Panel, method, path, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	return rec
}

func TestKeysAPIRestrictionRoundTrip(t *testing.T) {
	p, live := keysAPIHarness(t)

	createBody := `{"name":"给朋友A","note":"只走白名单","accounts":["u1","u2","bad uid"],` +
		`"models":["glm-5.3","hy3-*"],"realms":["cn","bogus"],"time_start":"09:00","time_end":"18:00",` +
		`"expires_at":1893456000,"daily_limit":100,"hourly_limit":10}`
	rec := panelReq(p, "POST", "/panel/api/keys", createBody)
	if rec.Code != 200 {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		OK  bool   `json:"ok"`
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || !created.OK || created.Key == "" {
		t.Fatalf("create response bad: %v %s", err, rec.Body.String())
	}

	// 列表：只回显掩码 + 全部约束 + 配额用量字段
	rec = panelReq(p, "GET", "/panel/api/keys", "")
	if rec.Code != 200 {
		t.Fatalf("list status=%d", rec.Code)
	}
	var listed struct {
		Keys []keyView `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(listed.Keys) != 1 {
		t.Fatalf("keys=%d want 1", len(listed.Keys))
	}
	k := listed.Keys[0]
	if strings.Contains(k.KeyMasked, created.Key) {
		t.Fatalf("列表不得回显明文：%s", k.KeyMasked)
	}
	if len(k.Accounts) != 2 || len(k.Models) != 2 || len(k.Realms) != 1 || k.Realms[0] != "cn" {
		t.Fatalf("约束回显不对: %+v", k)
	}
	if k.TimeStart != "09:00" || k.TimeEnd != "18:00" || k.ExpiresAt != 1893456000 ||
		k.DailyLimit != 100 || k.HourlyLimit != 10 {
		t.Fatalf("时间/配额回显不对: %+v", k)
	}

	// 运行时身份必须带上全部约束（否则 server 侧闸门拿不到判据）
	id, ok := live.Load().Keys.ResolveToken(created.Key)
	if !ok || id.Master {
		t.Fatalf("子密钥应解析为非主身份: ok=%v id=%+v", ok, id)
	}
	if !id.AllowsModel("hy3-x") || id.AllowsModel("gpt-4o") {
		t.Fatalf("模型白名单未传递: %+v", id.Models)
	}
	if !id.AllowsRealm("cn") || id.AllowsRealm("global") {
		t.Fatalf("域白名单未传递: %+v", id.Realms)
	}
	if id.DailyLimit != 100 || id.HourlyLimit != 10 || id.ExpiresAt != 1893456000 {
		t.Fatalf("配额/有效期未传递: %+v", id)
	}
	if !id.Allows("u1") || id.Allows("u3") {
		t.Fatalf("账号白名单未传递: %+v", id.Accounts)
	}

	// 修改：清空模型白名单 + 停用 + 改配额
	rec = panelReq(p, "POST", "/panel/api/keys/"+created.ID,
		`{"models":[],"disabled":true,"daily_limit":5}`)
	if rec.Code != 200 {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec = panelReq(p, "GET", "/panel/api/keys", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if listed.Keys[0].Disabled != true || listed.Keys[0].DailyLimit != 5 || len(listed.Keys[0].Models) != 0 {
		t.Fatalf("更新未生效: %+v", listed.Keys[0])
	}
	// 停用后 token 不再可解析
	if _, ok := live.Load().Keys.ResolveToken(created.Key); ok {
		t.Fatal("停用后子密钥必须解析失败")
	}

	// 非法时段必须 400（不能静默写脏）
	if rec = panelReq(p, "POST", "/panel/api/keys/"+created.ID, `{"time_start":"25:00"}`); rec.Code != 400 {
		t.Fatalf("非法时段应 400，got %d", rec.Code)
	}
	// 负配额必须 400
	if rec = panelReq(p, "POST", "/panel/api/keys/"+created.ID, `{"daily_limit":-1}`); rec.Code != 400 {
		t.Fatalf("负配额应 400，got %d", rec.Code)
	}

	// 配额计数：Reserve 后列表能看到用量，重置计数后归零
	if ok, _ := p.cfg.KeyQuota.Reserve(created.ID, 100, 10, time.Now()); !ok {
		t.Fatal("reserve 应放行")
	}
	rec = panelReq(p, "GET", "/panel/api/keys", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if listed.Keys[0].UsedToday != 1 || listed.Keys[0].UsedHour != 1 {
		t.Fatalf("用量回显不对: %+v", listed.Keys[0])
	}
	if rec = panelReq(p, "POST", "/panel/api/keys/"+created.ID+"/reset_quota", ""); rec.Code != 200 {
		t.Fatalf("reset_quota status=%d", rec.Code)
	}
	rec = panelReq(p, "GET", "/panel/api/keys", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if listed.Keys[0].UsedToday != 0 || listed.Keys[0].UsedHour != 0 {
		t.Fatalf("重置后用量应为 0: %+v", listed.Keys[0])
	}

	// 删除
	if rec = panelReq(p, "POST", "/panel/api/keys/"+created.ID+"/remove", ""); rec.Code != 200 {
		t.Fatalf("remove status=%d", rec.Code)
	}
	rec = panelReq(p, "GET", "/panel/api/keys", "")
	_ = json.Unmarshal(rec.Body.Bytes(), &listed)
	if len(listed.Keys) != 0 {
		t.Fatalf("删除后应无密钥: %+v", listed.Keys)
	}
}
