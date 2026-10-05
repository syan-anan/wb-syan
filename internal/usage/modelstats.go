package usage

import (
	"sort"
	"strings"
	"time"
)

// ModelSlot 模型可用率状态条上的一个时间格（一个整点）。
// Req=0 表示该整点没有观测（前端画灰格），不是「调用 0 次」。
type ModelSlot struct {
	Scope string `json:"scope"` // 恒为 "h"（状态条只吃整点桶）
	T     string `json:"t"`     // "2006-01-02T15"
	Req   int64  `json:"req"`
	Err   int64  `json:"err"`
}

// ModelStat 单个模型在「今日 / 近 N 天」两个窗口的可用率视图（模型与档位页用）。
// 窗口按本地**自然日**切（今日 00:00 起 / 近 N 天含今天），与用量页的「近 N 小时」
// 滚动窗口刻意不同：可用率看的是自然日，不是「此刻往前推」。
type ModelStat struct {
	Model        string      `json:"model"`
	TodayReq     int64       `json:"today_req"`
	TodayErr     int64       `json:"today_err"`
	WindowDays   int         `json:"window_days"`
	WindowReq    int64       `json:"window_req"`
	WindowErr    int64       `json:"window_err"`
	AvgLatencyMs float64     `json:"avg_latency_ms"`
	AvgTPS       float64     `json:"avg_tokens_per_second"`
	Slots        []ModelSlot `json:"slots"`
}

// ModelStats 按模型聚合可用率：今日 + 近 days 天两个窗口，外加一条状态条
// （最近 slots 个整点，每小时一格，缺观测的格子留空）。
//
// 只看桶里已有的数据，不推断：某模型从没被调用过 → 不出现在结果里（前端显示
// 「—」而不是伪造 100%）。日桶（90 天前折叠出来的）只参与窗口统计，不进状态条
// ——画在「最近 24 小时」条上会时间错位。
func (r *Recorder) ModelStats(now time.Time, days, slots int) []ModelStat {
	if r == nil {
		return nil
	}
	if days < 1 {
		days = 1
	}
	if slots < 1 {
		slots = 1
	}

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	todayKey := now.Format(dayLayout)
	dayFrom := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).
		AddDate(0, 0, -(days - 1))
	slotFrom := now.Truncate(time.Hour).Add(-time.Duration(slots-1) * time.Hour)

	type acc struct {
		st   ModelStat
		lat  int64
		latN int64
		tps  float64
		tpsN int64
	}
	agg := map[string]*acc{}
	slotAgg := map[string]map[string]*ModelSlot{}

	for i := range bs {
		b := &bs[i]
		isHour := strings.HasPrefix(b.Scope, "h:")
		var ts time.Time
		var err error
		if isHour {
			ts, err = time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
		} else {
			ts, err = time.ParseInLocation(dayLayout, strings.TrimPrefix(b.Scope, "d:"), time.Local)
		}
		if err != nil {
			continue // 脏桶不进任何口径
		}
		a := agg[b.Model]
		if a == nil {
			a = &acc{st: ModelStat{Model: b.Model, WindowDays: days}}
			agg[b.Model] = a
		}
		if ts.Format(dayLayout) == todayKey {
			a.st.TodayReq += b.Req
			a.st.TodayErr += b.Err
		}
		if !ts.Before(dayFrom) {
			a.st.WindowReq += b.Req
			a.st.WindowErr += b.Err
			a.lat += b.LatMs
			a.latN += b.LatN
			a.tps += b.TPS
			a.tpsN += b.TPSN
		}
		if isHour && !ts.Before(slotFrom) {
			key := ts.Format(hourLayout)
			m := slotAgg[b.Model]
			if m == nil {
				m = map[string]*ModelSlot{}
				slotAgg[b.Model] = m
			}
			sl := m[key]
			if sl == nil {
				sl = &ModelSlot{Scope: "h", T: key}
				m[key] = sl
			}
			sl.Req += b.Req
			sl.Err += b.Err
		}
	}

	out := make([]ModelStat, 0, len(agg))
	for model, a := range agg {
		st := a.st
		if a.latN > 0 {
			st.AvgLatencyMs = float64(a.lat) / float64(a.latN)
		}
		if a.tpsN > 0 {
			st.AvgTPS = a.tps / float64(a.tpsN)
		}
		if m := slotAgg[model]; len(m) > 0 {
			st.Slots = make([]ModelSlot, 0, slots)
			for i := 0; i < slots; i++ {
				k := slotFrom.Add(time.Duration(i) * time.Hour).Format(hourLayout)
				if sl := m[k]; sl != nil {
					st.Slots = append(st.Slots, *sl)
				} else {
					st.Slots = append(st.Slots, ModelSlot{Scope: "h", T: k})
				}
			}
		}
		out = append(out, st)
	}
	// 近 N 天调用降序；同量按今日调用降序，再同按模型名升序（输出稳定）。
	sort.Slice(out, func(i, j int) bool {
		if out[i].WindowReq != out[j].WindowReq {
			return out[i].WindowReq > out[j].WindowReq
		}
		if out[i].TodayReq != out[j].TodayReq {
			return out[i].TodayReq > out[j].TodayReq
		}
		return out[i].Model < out[j].Model
	})
	return out
}
