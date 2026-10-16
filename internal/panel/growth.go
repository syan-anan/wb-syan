// growth.go 面板「成长中心」接口：一屏看清一个账号的能量 / 活跃地图 / 热力图 /
// 宠物图鉴 / 盲盒额度 / 旅行 / 抽奖 / 兑换 / 成长等级。
//
//	GET  /panel/api/growth?uid=<uid>[&refresh=1]  → 该账号的快照（带缓存）
//	GET  /panel/api/growth/overview                → 已缓存的所有账号（不触发上游）
//
// 两条边界（对齐参考实现的 admin/growth_center.py）：
//
//  1. **只读**：这一层只发 GET，不触发对话、不领奖、不兑换。打开页面不该改变账号状态。
//
//  2. **缓存**：一期快照 = 10 个上游请求。不做缓存的话每次刷新页面都是 10 次请求，
//     运维来回点几下就把账号打进风控。默认 TTL 10 分钟，refresh=1 强制重拉。
package panel

import (
	"net/http"
	"sync"
	"time"

	"github.com/syan-anan/wb-syan/internal/upstream"
)

// growthTTL 快照缓存时长：上游这些接口是"天级变化"的观测量，10 分钟足够新鲜。
const growthTTL = 10 * time.Minute

// growthEntry 一份带时间戳的快照。
type growthEntry struct {
	snap      *upstream.GrowthSnapshot
	fetchedAt time.Time
}

// growthGet 取账号快照：命中缓存直接回，过期或 refresh 才打上游。
func (p *Panel) growthGet(uid string, refresh bool) (*upstream.GrowthSnapshot, bool, bool) {
	if p.cfg.Pool == nil || p.cfg.Upstream == nil {
		return nil, false, false
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return nil, false, false
	}
	p.growthMu.Lock()
	ent := p.growthCache[uid]
	fresh := ent != nil && time.Since(ent.fetchedAt) < growthTTL
	p.growthMu.Unlock()
	if fresh && !refresh {
		return ent.snap, true, true
	}
	// 同一个账号的并发刷新只跑一次：两个请求同时进来时后到的等前一个的结果。
	// （面板上"连点几下"很常见，重复拉 10 个上游请求毫无意义。）
	mu := p.taskLockFor(uid + ":growth")
	mu.Lock()
	defer mu.Unlock()
	p.growthMu.Lock()
	ent = p.growthCache[uid]
	fresh = ent != nil && time.Since(ent.fetchedAt) < growthTTL
	p.growthMu.Unlock()
	if fresh && !refresh {
		return ent.snap, true, true
	}
	snap := p.cfg.Upstream.GrowthSnapshot(a)
	if snap == nil {
		return nil, false, false
	}
	p.growthMu.Lock()
	p.growthCache[uid] = &growthEntry{snap: snap, fetchedAt: time.Now()}
	p.growthMu.Unlock()
	return snap, false, true
}

// taskLockFor 取（必要时创建）按 key 复用的互斥锁。
func (p *Panel) taskLockFor(key string) *sync.Mutex {
	p.taskMu.Lock()
	defer p.taskMu.Unlock()
	if p.taskLocks == nil {
		p.taskLocks = map[string]*sync.Mutex{}
	}
	mu := p.taskLocks[key]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[key] = mu
	}
	return mu
}

// growth 返回单个账号的成长中心快照。
func (p *Panel) growth(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	if uid == "" {
		writeErr(w, http.StatusBadRequest, "缺少 uid")
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	snap, cached, ok := p.growthGet(uid, refresh)
	if !ok {
		writeErr(w, http.StatusNotFound, "账号不存在或成长中心不可用")
		return
	}
	// 分段清单一起回：前端据此渲染顺序与展示名，不必在 JS 里再抄一份。
	defs := make([]map[string]string, 0, len(upstream.GrowthSectionDefs))
	for _, d := range upstream.GrowthSectionDefs {
		defs = append(defs, map[string]string{"name": d.Name, "label": d.Label})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "cached": cached, "snapshot": snap, "sections": defs,
		"ttl_sec": int(growthTTL.Seconds()),
	})
}

// growthOverview 返回已缓存的所有账号快照（**不触发上游**）。
//
// 为什么要有这个"只看缓存"的入口：一屏 9 个账号 = 90 个上游请求，页面一打开就跑
// 这么多必然触发风控。所以页面先只看缓存，缺哪个账号由用户点单个账号去拉。
func (p *Panel) growthOverview(w http.ResponseWriter, r *http.Request) {
	p.growthMu.Lock()
	out := make([]map[string]any, 0, len(p.growthCache))
	for uid, ent := range p.growthCache {
		out = append(out, map[string]any{
			"uid": uid, "snapshot": ent.snap,
			"age_sec": int(time.Since(ent.fetchedAt).Seconds()),
		})
	}
	p.growthMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cached": out, "ttl_sec": int(growthTTL.Seconds())})
}
