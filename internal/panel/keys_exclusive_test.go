// keys_exclusive_test.go 面板侧独占绑定的写入校验、冲突拒绝与回显。
package panel

import (
	"encoding/json"
	"testing"
)

func decodeKeyCreate(t *testing.T, body []byte) (string, bool) {
	t.Helper()
	var out struct {
		OK  bool   `json:"ok"`
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v %s", err, body)
	}
	return out.ID, out.OK
}

// TestKeysAPIExclusiveBinding 独占绑定的四道校验 + 回显 + 停用后释放。
func TestKeysAPIExclusiveBinding(t *testing.T) {
	p, _ := keysAPIHarness(t)

	// 1) 独占总数必须恰好 1：多账号、零账号都要拒。
	if rec := panelReq(p, "POST", "/panel/api/keys",
		`{"name":"bad","accounts":["u1","u2"],"exclusive":true}`); rec.Code != 400 {
		t.Fatalf("多账号独占应 400，实际 %d %s", rec.Code, rec.Body)
	}
	if rec := panelReq(p, "POST", "/panel/api/keys",
		`{"name":"bad","exclusive":true}`); rec.Code != 400 {
		t.Fatalf("零账号独占应 400，实际 %d %s", rec.Code, rec.Body)
	}

	// 2) 合法创建。
	rec := panelReq(p, "POST", "/panel/api/keys",
		`{"name":"专享","accounts":["u1"],"exclusive":true}`)
	if rec.Code != 200 {
		t.Fatalf("创建失败 %d %s", rec.Code, rec.Body)
	}
	id, ok := decodeKeyCreate(t, rec.Body.Bytes())
	if !ok || id == "" {
		t.Fatalf("创建响应缺 id: %s", rec.Body)
	}

	// 3) 同一账号不能被第二条密钥同时独占。
	if rec := panelReq(p, "POST", "/panel/api/keys",
		`{"name":"抢","accounts":["u1"],"exclusive":true}`); rec.Code != 409 {
		t.Fatalf("重复独占应 409，实际 %d %s", rec.Code, rec.Body)
	}

	// 4) 列表回显 exclusive（前端据此显示"独占"标签与勾选态）。
	rec = panelReq(p, "GET", "/panel/api/keys", "")
	var list struct {
		Keys []struct {
			ID        string `json:"id"`
			Exclusive bool   `json:"exclusive"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表解析失败: %v %s", err, rec.Body)
	}
	found := false
	for _, k := range list.Keys {
		if k.ID == id {
			found = true
			if !k.Exclusive {
				t.Errorf("列表未回显 exclusive: %+v", k)
			}
		}
	}
	if !found {
		t.Fatalf("列表里没有新建的密钥: %s", rec.Body)
	}

	// 5) 停用 = 松开：账号回到公共池，可被另一条密钥重新独占。
	if rec := panelReq(p, "POST", "/panel/api/keys/"+id, `{"disabled":true}`); rec.Code != 200 {
		t.Fatalf("停用失败 %d %s", rec.Code, rec.Body)
	}
	if rec := panelReq(p, "POST", "/panel/api/keys",
		`{"name":"接手","accounts":["u1"],"exclusive":true}`); rec.Code != 200 {
		t.Fatalf("停用后应可重新独占，实际 %d %s", rec.Code, rec.Body)
	}

	// 6) 编辑成"独占 + 多账号"同样被拒（改动不会落盘）。
	rec = panelReq(p, "POST", "/panel/api/keys",
		`{"name":"编辑用","accounts":["u9"],"exclusive":true}`)
	id2, _ := decodeKeyCreate(t, rec.Body.Bytes())
	if rec := panelReq(p, "POST", "/panel/api/keys/"+id2,
		`{"accounts":["u9","u8"]}`); rec.Code != 400 {
		t.Fatalf("编辑成多账号独占应 400，实际 %d %s", rec.Code, rec.Body)
	}
}
