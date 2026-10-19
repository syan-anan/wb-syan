// 积分流水接口：GET /panel/api/journal
//
// 查询参数：uid / source（子串）/ kind（confirmed|observed）/ start / end
//
//	/ page（1 起）/ size（默认 50，上限 500）
//
// 时间参数与用量页共用 usageRange：支持 YYYY-MM-DD、YYYY-MM-DDTHH:MM、RFC3339。
package panel

import (
	"net/http"
	"strconv"
	"strings"
)

// journal 返回筛选后的流水与两套账各自的小计。
func (p *Panel) journal(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Journal == nil {
		writeErr(w, http.StatusNotImplemented, "journal not available")
		return
	}
	q := r.URL.Query()
	from, to, _, err := usageRange(q)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	kind := strings.TrimSpace(q.Get("kind"))
	switch kind {
	case "", "all":
		kind = ""
	case "confirmed", "observed":
	default:
		writeErr(w, http.StatusBadRequest, "kind 只能是 confirmed / observed")
		return
	}
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("size"))
	res := p.cfg.Journal.Query(journalQuery{
		UID:    strings.TrimSpace(q.Get("uid")),
		Source: strings.TrimSpace(q.Get("source")),
		Kind:   kind,
		From:   from,
		To:     to,
		Page:   page,
		Size:   size,
	})
	// 账号昵称在查询时解析：流水本身只存 uid，账号改名/移除后历史行照旧可读。
	nicks := map[string]string{}
	if p.cfg.Pool != nil {
		for _, s := range p.cfg.Pool.List() {
			if s.Nickname != "" {
				nicks[s.UID] = s.Nickname
			}
		}
	}
	for i := range res.Rows {
		if n := nicks[res.Rows[i].UID]; n != "" {
			res.Rows[i].Account = n
		} else if len(res.Rows[i].UID) > 8 {
			res.Rows[i].Account = "已移除/" + res.Rows[i].UID[:8]
		} else {
			res.Rows[i].Account = res.Rows[i].UID
		}
	}
	writeJSON(w, http.StatusOK, res)
}
