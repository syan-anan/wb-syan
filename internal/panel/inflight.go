// 面板「账号并发上限」接口：逐号覆盖池级 max_in_flight。
//
//	POST /panel/api/accounts/{uid}/inflight   body: {"limit": null | 0 | 正整数}
//
// 三态（与 internal/pool/inflight.go 一致）：null = 跟随全局、0 = 不限、
// N = 该号最多 N 路在途。上限只影响选号与租约，不影响任何资格判定
// （禁用/冷却/熔断/独占绑定照常），也不改变上游对该号的看法。
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// intLitRe 账号上限的**字面量**判据：只接受可选负号 + 纯数字。
//
// 为什么不用 float64 解码再判整数：JSON 里 true 会被某些解码路径当成 1、
// "3" 与 3.0 也常被混收（上游 1.0.9 用 `type(value) is int` 正是为了挡这个）。
// 这里先按原始字节判形态，再走整数解析，bool/浮点/字符串一律 400。
var intLitRe = regexp.MustCompile(`^-?[0-9]{1,10}$`)

func (p *Panel) accountInFlight(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	// 用 map 收原始字节，而不是结构体里的 *json.RawMessage：后者遇到显式 null
	// 会被解码成 nil 指针，与「根本没带 limit 键」同形——那样「清除覆盖」这个
	// 动作就永远发不出去（被当成缺字段回 400）。键在不在，只能自己看。
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&fields); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	v, present := fields["limit"]
	if !present {
		// 缺字段与显式 null 是两回事：缺字段说明前端拼错了请求，
		// 静默按「跟随全局」处理会把误操作伪装成一次成功的清除。
		writeErr(w, http.StatusBadRequest, "limit 字段必填（null=跟随全局 / 0=不限 / 正整数=上限）")
		return
	}
	raw := strings.TrimSpace(string(v))
	var limit *int
	if raw != "null" {
		if !intLitRe.MatchString(raw) {
			writeErr(w, http.StatusBadRequest, "limit 必须是整数或 null；0 表示不限，布尔/小数/字符串一律不接受")
			return
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "limit 解析失败: "+err.Error())
			return
		}
		if n < 0 {
			writeErr(w, http.StatusBadRequest, "limit 不能为负数；不限请填 0")
			return
		}
		if n > 2147483647 {
			writeErr(w, http.StatusBadRequest, "limit 超出上界 2147483647")
			return
		}
		limit = &n
	}
	if p.cfg.Pool == nil {
		writeErr(w, http.StatusNotImplemented, "pool not available")
		return
	}
	eff, ok := p.cfg.Pool.SetAccountInFlight(uid, limit)
	if !ok {
		writeErr(w, http.StatusNotFound, "账号不存在")
		return
	}
	state := "跟随全局"
	if limit != nil {
		if *limit == 0 {
			state = "不限"
		} else {
			state = strconv.Itoa(*limit)
		}
	}
	log.Printf("panel: 账号并发上限 uid=%s 设置=%s 生效=%d", uid, state, eff)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "limit": limit, "effective": eff})
}
