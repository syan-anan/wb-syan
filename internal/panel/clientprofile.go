// clientprofile.go 面板「客户端参数」接口：展示 / 保存 / 重置 / 重新探测。
//
//	GET  /panel/api/client_profile          → 生效档案 + 每键来源 + 已保存 + 现场探测 + 兜底
//	POST /panel/api/client_profile          → 保存档案（可选 merge / 切换 auto|saved）
//	POST /panel/api/client_profile/reset    → 清空保存值，回到「纯兜底 + 环境变量」
//	POST /panel/api/client_profile/detect   → 重新现场探测（服务器上恒为空，见下）
//
// 为什么要「来源视图」：这层是三层合成（兜底 / 保存 / 环境变量），运维改了一个值却发现
// 不生效时，最需要知道的就是"这个键当前到底取自哪一层" —— 没有这个视图只能靠猜。
package panel

import (
	"net/http"
	"strings"

	"github.com/syan-anan/wb-syan/internal/clientprofile"
)

// profileView 生效档案的对外视图：类型化字段 + 原始 map + 来源。
type profileView struct {
	OK        bool              `json:"ok"`
	Source    string            `json:"source"`
	Defaults  map[string]any    `json:"defaults"`
	Saved     map[string]any    `json:"saved"`
	Detected  map[string]any    `json:"detected"`
	Effective map[string]any    `json:"effective"`
	Sources   map[string]string `json:"sources"`
	Derived   map[string]string `json:"derived"`
}

func (p *Panel) profileHolder() *clientprofile.Holder { return p.cfg.ClientProfile }

// profileViewOf 组装对外视图（未注入持有者时回落到纯兜底，页面不空白）。
func profileViewOf(h *clientprofile.Holder) profileView {
	if h == nil {
		return profileView{
			OK:        true,
			Source:    clientprofile.SourceAuto,
			Defaults:  clientprofile.Defaults(),
			Saved:     map[string]any{},
			Detected:  map[string]any{},
			Effective: clientprofile.Compose(nil, clientprofile.SourceAuto, nil),
			Sources:   clientprofile.Sources(nil, clientprofile.SourceAuto, nil, nil),
			Derived:   map[string]string{},
		}
	}
	eff := h.Effective()
	return profileView{
		OK:        true,
		Source:    h.Source(),
		Defaults:  clientprofile.Defaults(),
		Saved:     h.Saved(),
		Detected:  h.Detected(),
		Effective: h.Snapshot(),
		Sources:   h.SourcesView(),
		// derived 是"不是配置出来的、而是算出来的"那两个值：UA 与四个归属头。
		// 单独列出来是为了让人一眼看出「改了版本号之后 UA 会跟着变」。
		Derived: map[string]string{
			"user_agent_cn":     eff.UAByRealm(false),
			"user_agent_global": eff.UAByRealm(true),
			"x_ide_version":     eff.RiskHeaders()["X-IDE-Version"],
			"x_product":         eff.RiskHeaders()["X-Product"],
		},
	}
}

// getClientProfile 列出当前档案（含来源视图）。
func (p *Panel) getClientProfile(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, profileViewOf(p.profileHolder()))
}

// saveClientProfile 保存档案。
//
// body: {"profile": {...}, "source": "auto|saved", "merge": true}
// 也接受直接传档案本体（没有 profile 键时按整体当档案处理），方便 curl 手写。
// merge=true（默认）与已保存值合并 —— 面板上「只改一个字段」是最常见的用法。
func (p *Panel) saveClientProfile(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveClientProfile == nil {
		writeErr(w, http.StatusNotImplemented, "client profile api not available")
		return
	}
	var body map[string]any
	if err := decodeJSONBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "解析请求失败: "+err.Error())
		return
	}
	rawProfile, ok := body["profile"].(map[string]any)
	if !ok {
		// 允许直接传档案本体（去掉控制键）。
		rawProfile = map[string]any{}
		for k, v := range body {
			if k == "source" || k == "merge" {
				continue
			}
			rawProfile[k] = v
		}
	}
	clean, errs := clientprofile.Normalize(rawProfile)
	if len(clean) == 0 && len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "errors": errs})
		return
	}
	merge := true
	if mv, ok := body["merge"].(bool); ok {
		merge = mv
	}
	base := map[string]any{}
	if merge && p.profileHolder() != nil {
		base = p.profileHolder().Saved()
	}
	for k, v := range clean {
		if k == "headers" {
			// headers 也是合并语义：只改一个头不该把其余已保存的头抹掉。
			merged := map[string]any{}
			if old, ok := base["headers"].(map[string]any); ok {
				for hk, hv := range old {
					merged[hk] = hv
				}
			}
			if hm, ok := v.(map[string]any); ok {
				for hk, hv := range hm {
					merged[hk] = hv
				}
			}
			base["headers"] = merged
			continue
		}
		base[k] = v
	}
	source := ""
	if sv, ok := body["source"].(string); ok {
		source = strings.TrimSpace(sv)
	}
	if source == "" && p.profileHolder() != nil {
		source = p.profileHolder().Source()
	}
	if err := p.cfg.SaveClientProfile(base, source); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	view := profileViewOf(p.profileHolder())
	view.Sources = nil
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "errors": errs, "saved": view.Saved, "source": view.Source,
		"derived": view.Derived,
	})
}

// resetClientProfile 清空保存值（回到纯兜底 + 环境变量）。
func (p *Panel) resetClientProfile(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveClientProfile == nil {
		writeErr(w, http.StatusNotImplemented, "client profile api not available")
		return
	}
	source := clientprofile.SourceAuto
	if p.profileHolder() != nil {
		source = p.profileHolder().Source()
	}
	if err := p.cfg.SaveClientProfile(map[string]any{}, source); err != nil {
		writeErr(w, http.StatusInternalServerError, "重置失败: "+err.Error())
		return
	}
	view := profileViewOf(p.profileHolder())
	view.Sources = nil
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": view.Saved, "effective": view.Effective})
}

// detectClientProfile 重新现场探测。
//
// 服务器/容器里没有客户端安装目录可读，所以恒为空 —— 这不是失败，面板要如实说明，
// 否则运维会一直点「探测」等一个永远不会有的结果。
func (p *Panel) detectClientProfile(w http.ResponseWriter, r *http.Request) {
	if p.profileHolder() != nil {
		p.profileHolder().Set(p.profileHolder().Saved(), p.profileHolder().Source())
	}
	detected := clientprofile.Detect()
	note := ""
	if len(detected) == 0 {
		note = "本机没有可读的 WorkBuddy 客户端安装目录（网关通常跑在服务器/容器里）——" +
			"现场探测为空是预期结果，档案改由「保存值 + 环境变量」提供。" +
			"在装了客户端的机器上跑网关时，探测才会返回真实参数。"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detected": detected, "note": note})
}
