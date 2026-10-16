package usage

import (
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 逐模型延迟样本（抖动计算的数据源）
// ---------------------------------------------------------------------------
//
// 为什么需要这个文件：bucket（见 usage.go）只保留 LatMs（延迟累计）与 LatN
// （延迟样本数）两个累计量，**没有逐请求原始延迟**。所以这里定义的「样本」是
// 「一个分桶的均值」——分辨率 = 分桶粒度（时间片 × realm × uid × model × client）。
//
// 这是现有数据能支持的最高分辨率，不是退而求其次的替代方案：它回答的正是抖动
// 要回答的问题——「同一模型在不同时段 / 不同账号下，响应时间稳不稳」。
// 真正的逐请求 p95 需要在 Add 里落原始样本，存储成本会从「每桶约 90 字节」涨到
// 「每请求一条」，不在本次改动范围内（要做得单独评估 usage.json 的体积上限）。
//
// 只读：本文件不产生任何上游调用，也不改变任何写入路径。

// jitterWindowDays 抖动窗口上限（天）。与 hourlyKeep 对齐（90 天）：
// 更早的数据已被折叠成日桶，继续纳入只会让「三个月前的一个慢小时」定义今天的状态。
const jitterWindowDays = 90

// ModelLatencySamples 返回逐模型的延迟样本（单位 ms），每个样本是一个分桶的均值。
//
// days<=0 视为 1；days>jitterWindowDays 收敛到 jitterWindowDays。
// 窗口按**本地自然日**切（今天 00:00 往前推 days-1 天），与 ModelStats 的
// 「今日 / 近 N 天」同口径；与用量页的滚动小时窗口刻意不同（可用率看自然日）。
//
// 只有 LatN>0 的桶产样本：空桶不产 0 值——「没有观测」不是「0 ms」，
// 把没观测的桶当 0 会把抖动算成一个假的天文数字。
// 脏桶（scope 解析失败）与窗口外的桶直接跳过。
// r == nil 返回 nil。
//
// 返回的每个切片已按升序排好，调用方取分位数时无需再排序。
func (r *Recorder) ModelLatencySamples(days int) map[string][]float64 {
	if r == nil {
		return nil
	}
	if days <= 0 {
		days = 1
	}
	if days > jitterWindowDays {
		days = jitterWindowDays
	}

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	now := time.Now()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).
		AddDate(0, 0, -(days - 1))

	out := make(map[string][]float64, len(bs))
	for i := range bs {
		b := &bs[i]
		if b.LatN <= 0 {
			continue // 没有延迟观测的桶不产样本
		}
		var ts time.Time
		var err error
		if strings.HasPrefix(b.Scope, "h:") {
			ts, err = time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
		} else {
			ts, err = time.ParseInLocation(dayLayout, strings.TrimPrefix(b.Scope, "d:"), time.Local)
		}
		if err != nil || ts.Before(from) {
			continue // 脏桶 / 窗口外的桶不进样本
		}
		out[b.Model] = append(out[b.Model], float64(b.LatMs)/float64(b.LatN))
	}
	for _, vals := range out {
		sort.Float64s(vals)
	}
	return out
}
