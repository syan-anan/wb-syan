// extra.go —— 面板的三个补充接口：Qoder 签到、配置备份、备份恢复。
//
// 单独一个文件是因为它们都不属于账号池的既有语义：签到是另一条上游，
// 备份是运维动作。混进 panel.go 会让那个本来就很长的文件更难读。
package panel

import (
	"encoding/json"
	"net/http"
	"strings"
)

// qoderCheckinStatus 返回 Qoder 签到状态（连续天数 + 最近 30 天）。
func (p *Panel) qoderCheckinStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QoderHistory == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	current, total, recent := p.cfg.QoderHistory()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true,
		"current": current,
		"total":   total,
		"recent":  recent,
	})
}

// qoderCheckinRun 手动跑一次领取。
func (p *Panel) qoderCheckinRun(w http.ResponseWriter, r *http.Request) {
	if p.cfg.QoderCheckin == nil {
		writeErr(w, http.StatusNotImplemented, "未配置 Qoder 上游")
		return
	}
	results, err := p.cfg.QoderCheckin()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

// backupsList 列出配置备份。
func (p *Panel) backupsList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ListBackups == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "backups": []string{}})
		return
	}
	list := p.cfg.ListBackups()
	if list == nil {
		list = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "backups": list})
}

// backupRestore 用指定备份覆盖当前配置。
//
// 恢复后不自动重启：配置里可能改过监听地址/账号目录，热应用只覆盖一部分，
// 悄悄重启会让"我刚点了一下恢复，服务怎么断了"变成一个惊吓。返回提示让用户自己重启。
func (p *Panel) backupRestore(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RestoreBackup == nil {
		writeErr(w, http.StatusNotImplemented, "备份恢复不可用")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}
	if err := p.cfg.RestoreBackup(name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"note": "配置已恢复；部分字段需要重启进程才能生效",
	})
}
