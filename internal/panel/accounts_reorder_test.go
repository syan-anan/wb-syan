package panel

import (
	"encoding/json"
	"testing"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/pool"
)

// TestAccountsReorderAPI 整表拖动排序接口（POST /panel/api/accounts/reorder）。
//
// 前端 drop 之后一次提交全部 uid；这里锁死三件事：
//   - 正常顺序写成连续优先级（第 1 个 N、最后 1），返回 applied 数；
//   - 空数组 / 坏 JSON 是 400（不能悄悄当成"清空排序"）；
//   - 全部 uid 都不存在是 404（前端据此提示"账号已不存在"）。
func TestAccountsReorderAPI(t *testing.T) {
	p := pool.New("")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid})
	}
	pn := New(Config{Pool: p, Version: "test"})

	rec := panelReq(pn, "POST", "/panel/api/accounts/reorder", `{"uids":["u3","u1","u2"]}`)
	if rec.Code != 200 {
		t.Fatalf("reorder status=%d body=%s", rec.Code, rec.Body.String())
	}
	var ok struct {
		OK      bool           `json:"ok"`
		Applied int            `json:"applied"`
		Pri     map[string]int `json:"priority"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("decode: %v %s", err, rec.Body.String())
	}
	if !ok.OK || ok.Applied != 3 {
		t.Fatalf("applied=%d ok=%v want 3 true", ok.Applied, ok.OK)
	}
	if ok.Pri["u3"] != 3 || ok.Pri["u1"] != 2 || ok.Pri["u2"] != 1 {
		t.Fatalf("priority=%v want u3:3 u1:2 u2:1", ok.Pri)
	}

	// 空数组：400，且池内优先级不得被清空。
	rec = panelReq(pn, "POST", "/panel/api/accounts/reorder", `{"uids":[]}`)
	if rec.Code != 400 {
		t.Fatalf("empty uids status=%d want 400", rec.Code)
	}
	for _, s := range p.List() {
		if s.UID == "u3" && s.Priority != 3 {
			t.Fatalf("空数组不应改动优先级，u3=%d want 3", s.Priority)
		}
	}

	// 坏 JSON：400
	rec = panelReq(pn, "POST", "/panel/api/accounts/reorder", `{"uids":`)
	if rec.Code != 400 {
		t.Fatalf("bad json status=%d want 400", rec.Code)
	}

	// 全部不存在：404
	rec = panelReq(pn, "POST", "/panel/api/accounts/reorder", `{"uids":["nope1","nope2"]}`)
	if rec.Code != 404 {
		t.Fatalf("unknown uids status=%d want 404", rec.Code)
	}
}
