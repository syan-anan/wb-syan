// qoder_accounts.go —— Qoder 账号的运行期管理接口。
//
// 三条约束：
//  1. 加号写 data/qoder_accounts.json，不碰 config.json（那是启动快照）；
//  2. 写完立刻 Replace 到内存池，不重启；
//  3. 面板永远看不到完整 PAT —— 列表只回打码值，只有新增时接受明文。
package panel

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/syan-anan/wb-syan/internal/upstream"
)

// qoderAccountsList 返回当前池里的账号（PAT 打码）。
func (p *Panel) qoderAccountsList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QoderAccounts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "accounts": []any{}})
		return
	}
	accts := p.cfg.QoderAccounts()
	if accts == nil {
		accts = []upstream.QoderAccount{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "accounts": accts})
}

// qoderAccountAdd 校验并加入账号池。
func (p *Panel) qoderAccountAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		PAT      string `json:"personal_access_token"`
		Realm    string `json:"realm"`
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		UserType string `json:"user_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	body.PAT = strings.TrimSpace(body.PAT)
	if body.PAT == "" {
		writeErr(w, http.StatusBadRequest, "personal_access_token 不能为空")
		return
	}
	if !strings.HasPrefix(body.PAT, "pt-") {
		writeErr(w, http.StatusBadRequest, "PAT 应以 pt- 开头")
		return
	}
	if body.Realm == "" {
		body.Realm = "intl"
	}
	if body.Realm != "cn" && body.Realm != "intl" {
		writeErr(w, http.StatusBadRequest, "realm 只能是 cn 或 intl")
		return
	}
	// 国内版必须有 uid：先校验时自动取，取不到再让用户手填。
	if body.Realm == "cn" && strings.TrimSpace(body.UID) == "" && p.cfg.QoderVerify != nil {
		if id, err := p.cfg.QoderVerify(body.PAT, body.Realm, "", body.Nickname, body.UserType); err == nil {
			body.UID = id.UID
			if body.Nickname == "" {
				body.Nickname = id.Nickname
			}
		}
	}
	if body.Realm == "cn" && strings.TrimSpace(body.UID) == "" {
		writeErr(w, http.StatusBadRequest, "国内版必须填 uid（粘贴 PAT 后点「校验」可自动获取）")
		return
	}
	if body.Name == "" {
		body.Name = body.Nickname
		if body.Name == "" {
			body.Name = body.UID
		}
		if body.Name == "" {
			body.Name = "qoder-" + body.Realm
		}
	}
	acct := upstream.QoderAccount{
		Name:     body.Name,
		PAT:      body.PAT,
		Realm:    body.Realm,
		UID:      body.UID,
		Nickname: body.Nickname,
		UserType: body.UserType,
	}
	if p.cfg.QoderVerify != nil {
		id, err := p.cfg.QoderVerify(body.PAT, body.Realm, body.UID, body.Nickname, body.UserType)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "凭据校验失败："+err.Error())
			return
		}
		if body.UID == "" {
			body.UID = id.UID
		}
		if body.Nickname == "" {
			body.Nickname = id.Nickname
		}
	}
	if p.cfg.QoderAdd == nil {
		writeErr(w, http.StatusNotImplemented, "账号管理不可用")
		return
	}
	if err := p.cfg.QoderAdd(acct); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": body.Name, "realm": body.Realm})
}

// qoderAccountDelete 按名称移除账号。
func (p *Panel) qoderAccountDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	if p.cfg.QoderDelete == nil {
		writeErr(w, http.StatusNotImplemented, "账号管理不可用")
		return
	}
	if err := p.cfg.QoderDelete(body.Name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// qoderAccountVerify 只校验不落盘，供「先测一下」用。
func (p *Panel) qoderAccountVerify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PAT      string `json:"personal_access_token"`
		Realm    string `json:"realm"`
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		UserType string `json:"user_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if p.cfg.QoderVerify == nil {
		writeErr(w, http.StatusNotImplemented, "校验不可用")
		return
	}
	id, err := p.cfg.QoderVerify(body.PAT, body.Realm, body.UID, body.Nickname, body.UserType)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": id.UID, "nickname": id.Nickname, "user_type": id.UserType,
	})
}

// qoderUsage 按模型聚合 qoder: 前缀的用量。
// 与主「用量」页共用同一个 Recorder，只是在这里过滤掉 WorkBuddy 的模型。
func (p *Panel) qoderUsage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "models": []any{}})
		return
	}
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
	snap := p.cfg.Usage.Snapshot(hours, nil)
	rows := make([]map[string]any, 0, len(snap.ByModel))
	var total int64
	var sumReq, sumErr, sumIn, sumOut, sumTok, sumCached, sumCachedN int64
	var sumLat, sumTps, sumTtft float64
	for _, m := range snap.ByModel {
		// 三种前缀都算 Qoder：qoder:（旧）/ qodercn: / qoderglobal:
		if !isQoderModelKey(m.Key) {
			continue
		}
		total += m.Requests
		sumReq += m.Requests
		sumErr += m.Errors
		sumIn += m.PromptTokens
		sumOut += m.CompletionTok
		sumTok += m.TotalTokens
		sumCached += m.CachedTokens
		sumCachedN += m.CachedN
		sumLat += m.AvgLatencyMs
		sumTps += m.AvgTPS
		sumTtft += m.AvgTTFTMs
		rows = append(rows, map[string]any{
			"model":    m.Key,
			"requests": m.Requests,
			"errors":   m.Errors,
			"input":    m.PromptTokens,
			"output":   m.CompletionTok,
			"total":    m.TotalTokens,
			"credits":  m.Credits,
			"latency":  m.AvgLatencyMs,
			"tps":      m.AvgTPS,
			"cached":   m.CachedTokens,
			"cached_n": m.CachedN,
			"ttft_ms":  m.AvgTTFTMs,
		})
	}
	// totals 只累加 Qoder 自己：snap.Totals 是全模型合计，混着 WorkBuddy 的数据。
	n := float64(len(rows))
	avg := func(v float64) float64 {
		if n == 0 {
			return 0
		}
		return v / n
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true, "models": rows, "requests": total, "hours": hours,
		"totals": map[string]any{
			"requests":   sumReq,
			"errors":     sumErr,
			"input":      sumIn,
			"output":     sumOut,
			"total":      sumTok,
			"credits":    0,
			"latency_ms": avg(sumLat),
			"tps":        avg(sumTps),
			"cached":     sumCached,
			"cached_n":   sumCachedN,
			"ttft_ms":    avg(sumTtft),
		},
	})
}

// qoderOAuthStart 生成设备授权链接。
func (p *Panel) qoderOAuthStart(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QoderOAuthStart == nil {
		writeErr(w, http.StatusNotImplemented, "OAuth 登录不可用")
		return
	}
	var body struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Realm == "" {
		body.Realm = "cn"
	}
	state, authURL, err := p.cfg.QoderOAuthStart(body.Realm)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": state, "authUrl": authURL, "realm": body.Realm})
}

// qoderOAuthPoll 轮询授权结果；成功即入池。
func (p *Panel) qoderOAuthPoll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QoderOAuthPoll == nil {
		writeErr(w, http.StatusNotImplemented, "OAuth 登录不可用")
		return
	}
	state := r.URL.Query().Get("state")
	status, acct, msg := p.cfg.QoderOAuthPoll(state)
	out := map[string]any{"status": status, "message": msg}
	if status == "ok" && acct != nil {
		if p.cfg.QoderAdd != nil {
			if err := p.cfg.QoderAdd(*acct); err != nil {
				out["status"] = "error"
				out["message"] = err.Error()
				writeJSON(w, http.StatusOK, out)
				return
			}
		}
		out["account"] = acct.Name
		out["uid"] = acct.UID
	}
	writeJSON(w, http.StatusOK, out)
}

// qoderModels 返回双区模型目录；realm 为空时合并。
func (p *Panel) qoderModels(w http.ResponseWriter, r *http.Request) {
	realm := r.URL.Query().Get("realm")
	var models []upstream.QoderCatalogEntry
	if p.cfg.QoderCatalogFn != nil {
		models = p.cfg.QoderCatalogFn(realm)
	} else {
		models = upstream.QoderCatalog(realm)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "realm": realm})
}

// qoderProClaim 逐账号领取 Pro 升级包。
func (p *Panel) qoderProClaim(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QoderPro == nil {
		writeErr(w, http.StatusNotImplemented, "Pro 领取不可用")
		return
	}
	results := p.cfg.QoderPro()
	claimed := 0
	earned := 0
	for _, x := range results {
		if x.State == "claimed_now" {
			claimed++
			earned += x.Earned
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "results": results, "claimed": claimed, "earned": earned,
	})
}

// qoderLocalScan 只读列出本机客户端已登录的账号（不写盘）。
func (p *Panel) qoderLocalScan(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QoderLocalScan == nil {
		writeErr(w, http.StatusNotImplemented, "本机凭证导入不可用")
		return
	}
	accts, err := p.cfg.QoderLocalScan()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 令牌打码后再回给前端，挑选阶段不需要明文。
	safe := make([]upstream.QoderAccount, 0, len(accts))
	for _, a := range accts {
		a.AccessToken = maskToken(a.AccessToken)
		safe = append(safe, a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": safe})
}

func maskToken(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:6] + "***" + s[len(s)-4:]
}

// qoderLocalImport 把扫描到的账号真正加入池（逐个，同名跳过）。
func (p *Panel) qoderLocalImport(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QoderLocalScan == nil || p.cfg.QoderAdd == nil {
		writeErr(w, http.StatusNotImplemented, "本机凭证导入不可用")
		return
	}
	accts, err := p.cfg.QoderLocalScan()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	added := 0
	var msgs []string
	for _, a := range accts {
		if err := p.cfg.QoderAdd(a); err != nil {
			msgs = append(msgs, a.Name+": "+err.Error())
			continue
		}
		added++
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added, "messages": msgs})
}

// qoderCampaigns 活动/签到任务表。
func (p *Panel) qoderCampaigns(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QoderCampaigns == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "campaigns": []any{}})
		return
	}
	rows := p.cfg.QoderCampaigns()
	if rows == nil {
		rows = []upstream.QoderCampaignRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "campaigns": rows})
}

// qoderOverview 账号页顶部的汇总：账号池 / 积分 / 调用量 / 最近请求。
func (p *Panel) qoderOverview(w http.ResponseWriter, r *http.Request) {
	var accts []upstream.QoderAccount
	if p.cfg.QoderAccounts != nil {
		accts = p.cfg.QoderAccounts()
	}
	var total, healthy, cn, intl int
	var credits, cnCr, intlCr float64
	for _, a := range accts {
		total++
		ok := a.Quota != nil && a.Quota.OK
		if ok {
			healthy++
			credits += a.Quota.Remaining
		}
		if a.Realm == "cn" {
			cn++
			if ok {
				cnCr += a.Quota.Remaining
			}
		} else {
			intl++
			if ok {
				intlCr += a.Quota.Remaining
			}
		}
	}

	var reqs, errs, tokens int64
	if p.cfg.Usage != nil {
		snap := p.cfg.Usage.Snapshot(24, nil)
		for _, m := range snap.ByModel {
			if strings.HasPrefix(m.Key, "qoder:") ||
				strings.HasPrefix(m.Key, "qodercn:") ||
				strings.HasPrefix(m.Key, "qoderglobal:") {
				reqs += m.Requests
				errs += m.Errors
				tokens += m.TotalTokens
			}
		}
	}

	recent := make([]map[string]any, 0, 20)
	if ring := p.Logs(); ring != nil {
		rows := ring.Snapshot()
		for i := len(rows) - 1; i >= 0 && len(recent) < 20; i-- {
			e := rows[i]
			if !strings.Contains(strings.ToLower(e.Text), "qoder") {
				continue
			}
			recent = append(recent, map[string]any{
				"ts":   e.TS.Format("01-02 15:04:05"),
				"text": e.Text,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": map[string]any{"total": total, "healthy": healthy, "cn": cn, "intl": intl},
		"credits":  map[string]any{"total": credits, "cn": cnCr, "intl": intlCr},
		"usage":    map[string]any{"requests": reqs, "errors": errs, "tokens": tokens},
		"recent":   recent,
	})
}

// isQoderModelKey 判断一条用量 key 是否属于 Qoder 上游。
func isQoderModelKey(k string) bool {
	return strings.HasPrefix(k, "qoder:") ||
		strings.HasPrefix(k, "qodercn:") ||
		strings.HasPrefix(k, "qoderglobal:")
}

// qoderAccountDisable 停用/启用。
func (p *Panel) qoderAccountDisable(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Disabled bool   `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if p.cfg.QoderSetDisabled == nil {
		writeErr(w, http.StatusNotImplemented, "账号管理不可用")
		return
	}
	if err := p.cfg.QoderSetDisabled(body.Name, body.Disabled); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": body.Name, "disabled": body.Disabled})
}

// qoderAccountTest 对单个账号发一条 ping。
func (p *Panel) qoderAccountTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if p.cfg.QoderTest == nil {
		writeErr(w, http.StatusNotImplemented, "测试不可用")
		return
	}
	if err := p.cfg.QoderTest(body.Name); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": body.Name})
}

// qoderAccountExport 下载完整账号文件（含凭据）。
func (p *Panel) qoderAccountExport(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QoderExport == nil {
		writeErr(w, http.StatusNotImplemented, "导出不可用")
		return
	}
	raw, err := p.cfg.QoderExport()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=qoder_accounts.json")
	_, _ = w.Write(raw)
}
