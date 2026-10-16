// phone.go 面板「手机号登录」接口：发短信验证码 → 校验 → 落盘入池。
//
//	POST /panel/api/login/phone/start                → 建会话，返回 sid + 支持的地区
//	POST /panel/api/login/phone/send   {sid,mobile,country_code} → 发验证码
//	POST /panel/api/login/phone/verify {sid,code}   → 校验并完成登录（落盘 + 热加载入池）
//	GET  /panel/api/login/phone/status?sid=         → 会话状态（前端轮询/排障）
//
// 与既有的「浏览器登录」（login.go 的设备授权流）并列：那条走 OAuth 授权页，
// 这条走短信。两条最终都落到同一个地方 —— auths/ 目录 + 池。
//
// 图形验证码：上游对**单个手机号**做频率风控，同号短时间内多次请求发码会要求图形验证码。
// 这一步纯协议过不去，接口如实返回 step=sms_send / reason=need_captcha 并提示换号，
// 由前端引导用户，而不是把失败伪装成"网络错误"。
package panel

import (
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/syan-anan/wb-syan/internal/auth"
	"github.com/syan-anan/wb-syan/internal/phonelogin"
)

// phoneErr 把 phonelogin 的错误翻成可回给前端的结构（带阶段与原因）。
func phoneErr(w http.ResponseWriter, err error) {
	var pe *phonelogin.Error
	if errors.As(err, &pe) {
		status := http.StatusBadRequest
		switch pe.Step {
		case "busy":
			status = http.StatusServiceUnavailable
		case "session":
			status = http.StatusNotFound
		}
		writeJSON(w, status, pe.Reply())
		return
	}
	writeErr(w, http.StatusBadGateway, err.Error())
}

func (p *Panel) phoneManager() *phonelogin.Manager { return p.cfg.PhoneLogin }

// phoneStart 建会话并返回 sid + 支持的地区代码。
//
// 这一步会真的打上游（5 个请求，不涉短信），所以对用户是"瞬时"的 —— 返回后即可发码。
func (p *Panel) phoneStart(w http.ResponseWriter, r *http.Request) {
	m := p.phoneManager()
	if m == nil {
		writeErr(w, http.StatusNotImplemented, "phone login not available")
		return
	}
	sess, err := m.Start(r.Context())
	if err != nil {
		phoneErr(w, err)
		return
	}
	log.Printf("panel: 手机号登录会话已建立 sid=%s", sess.SID())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "sid": sess.SID(), "countries": phonelogin.CountryCodes(),
	})
}

// phoneSend 给指定手机号发验证码。
func (p *Panel) phoneSend(w http.ResponseWriter, r *http.Request) {
	m := p.phoneManager()
	if m == nil {
		writeErr(w, http.StatusNotImplemented, "phone login not available")
		return
	}
	var body struct {
		SID         string `json:"sid"`
		Mobile      string `json:"mobile"`
		CountryCode string `json:"country_code"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求失败: "+err.Error())
		return
	}
	if strings.TrimSpace(body.SID) == "" {
		writeErr(w, http.StatusBadRequest, "缺少 sid（请先调用 start）")
		return
	}
	cc := strings.TrimSpace(body.CountryCode)
	if cc == "" {
		cc = "86" // 缺省中国大陆（与登录页默认一致）
	}
	expires, retry, err := m.SendCode(r.Context(), body.SID, body.Mobile, cc)
	if err != nil {
		phoneErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "expires_in": expires, "retry_after": retry,
	})
}

// phoneVerify 校验验证码并完成登录：凭证落盘 + 热加载入池 + 顺带签到/余额。
//
// 落盘与入池的步骤与浏览器登录（loginPoll）完全一致 —— 两条登录路径最终产物必须一模一样，
// 否则会出现"这条登录的号在池里表现不一样"这类难查的差异。
func (p *Panel) phoneVerify(w http.ResponseWriter, r *http.Request) {
	m := p.phoneManager()
	if m == nil {
		writeErr(w, http.StatusNotImplemented, "phone login not available")
		return
	}
	var body struct {
		SID  string `json:"sid"`
		Code string `json:"code"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求失败: "+err.Error())
		return
	}
	res, err := m.Verify(r.Context(), body.SID, body.Code)
	if err != nil {
		phoneErr(w, err)
		return
	}
	// 自产自检：解不出来就绝不入池（写了坏数据比不写更糟）。
	a, err := auth.Parse(res.Credential)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "生成的凭证未通过自检: "+err.Error())
		return
	}
	if !validUID(a.UID) {
		writeErr(w, http.StatusBadGateway, "上游返回的 uid 含非法字符，拒绝落盘（防路径穿越）")
		return
	}
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}
	a.FilePath = filepath.Join(p.cfg.AuthDir, "workbuddy-"+a.UID+".json")
	_, _ = a.BackfillRealm() // 手机号登录只有 CN（短信体系仅国内有）
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存凭证失败: "+err.Error())
		return
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(a.UID)

	// 顺带签到 + 余额刷新（幂等；失败只体现在返回字段里，不影响登录结果）。
	checkinMsg := ""
	if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error()
	}
	remain := int64(-1)
	total := int64(0)
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		remain, total = rm, tt
		p.cfg.Pool.ReenableIfCredits(a.UID, rm, tt)
	}
	log.Printf("panel: 手机号登录成功 uid=%s nickname=%q（免重启热加载）", a.UID, a.Nickname)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": a.UID, "nickname": a.Nickname,
		"phone": res.Phone, "realm": res.Realm,
		"credits": remain, "credits_total": total, "checkin_message": checkinMsg,
	})
}

// phoneStatus 会话状态（前端轮询 / 排障）。
func (p *Panel) phoneStatus(w http.ResponseWriter, r *http.Request) {
	m := p.phoneManager()
	if m == nil {
		writeErr(w, http.StatusNotImplemented, "phone login not available")
		return
	}
	sid := r.URL.Query().Get("sid")
	v, err := m.Status(sid)
	if err != nil {
		phoneErr(w, err)
		return
	}
	now := time.Now()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "sid": v.SID, "mobile": v.Mobile,
		"country_code": v.CountryCode, "code_sent": v.CodeSent,
		"retry_after": v.RetryAfter, "done": v.Done, "now": now.Unix(),
	})
}
