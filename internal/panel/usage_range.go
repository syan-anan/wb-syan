// 使用记录的显式起止日期（面板日期选择器的服务端）：对齐上游 workbuddy2api
// 1.0.9 的 stats._time_filters 语义——**显式区间覆盖滚动窗口**，两者共用同一份
// 聚合实现（usage.Recorder.SnapshotWindow），卡片 / 分表 / 时序 / 分组一次改齐。
//
// 为什么强调同一份：口径漂移的经典现场就是「卡片是全部历史、图是近 3 天」，
// 用户读成「筛选没生效」。这里从结构上不给第二份实现留位置。
package panel

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// 前端可发来的三种时间形态：纯日期（日期选择器给的粒度）、本地日期时间
// （datetime-local，到分钟）、以及带时区的 RFC3339（脚本/curl 手拼）。
const (
	rangeDateLayout     = "2006-01-02"
	rangeDateTimeLayout = "2006-01-02T15:04"
)

// parseRangeBound 解析一个时间边界；空串返回零值（= 不设这一侧的界）。
//
// isEnd 时纯日期取**当天最后一纳秒**：语义是「包含结束当天」，与页面上的提示
// 一字对应。若取次日 00:00 做闭区间上界，10-19 选到 10-19 却查不出 10-19 的数据
// （当天的桶会被 ts.After(to) 判出去）——这类差一个 tick 的边界是最难发现的 bug。
func parseRangeBound(v string, isEnd bool) (time.Time, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		// 带时区的入参按它自己的时区，再折算到本地墙钟（与桶 scope 的解析口径对齐）。
		return t.Local(), nil
	}
	if t, err := time.ParseInLocation(rangeDateTimeLayout, s, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation(rangeDateLayout, s, time.Local); err == nil {
		if isEnd {
			return t.Add(24*time.Hour - time.Nanosecond), nil
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("无效的时间格式 %q（可用 YYYY-MM-DD / YYYY-MM-DDTHH:MM / RFC3339）", s)
}

// usageRange 读 start / end 查询参数。返回 ranged=false 表示没给显式区间
// （调用方回落到 hours 滚动窗口）。
//
// 两条校验：
//   - 格式非法 → 报错，不静默忽略（忽略等于把用户筛到另一个区间还不吭声）
//   - start > end → 报错。上游同样直接 400：空结果集不该伪装成「这段时间没数据」
func usageRange(q url.Values) (from, to time.Time, ranged bool, err error) {
	startArg, endArg := strings.TrimSpace(q.Get("start")), strings.TrimSpace(q.Get("end"))
	if startArg == "" && endArg == "" {
		return time.Time{}, time.Time{}, false, nil
	}
	if from, err = parseRangeBound(startArg, false); err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("start: %w", err)
	}
	if to, err = parseRangeBound(endArg, true); err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("end: %w", err)
	}
	// 只给 end 不给 start：不设下界（上游同口径）。只给 start：上界到今天为止自然
	// 由数据本身封顶，不额外补一个 now——补了会让「未来日期」变成隐式过滤。
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return time.Time{}, time.Time{}, false, fmt.Errorf("开始日期不能晚于结束日期")
	}
	return from, to, true, nil
}

// formatRangeBound 把边界回显给前端（空 = 未设界）。RFC3339 带本地时区偏移，
// 前端可以直接喂回 Date() 做展示，不必猜是哪个时区。
func formatRangeBound(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// windowLabel 告诉前端本次是哪种口径（不猜、不让前端根据参数反推）。
func windowLabel(ranged bool, hours int) string {
	if ranged {
		return "custom"
	}
	if hours <= 0 {
		return "all"
	}
	return "rolling"
}
