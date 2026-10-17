package main

import (
	"context"
	"log"
	"time"

	"github.com/syan-anan/wb-syan/internal/upstream"
)

// runQoderCheckin 按配置时点跑 Qoder 活动领取，并把成功领取记进本地历史。
//
// 为什么复用 schedule.checkin_hours：Qoder 的签到活动窗口和 WorkBuddy 的
// 04:00 重置是两套机制，但用户的心智模型是"一天两次签到"。给 Qoder 单独配一套
// 时点只会让配置表变长，收益为零。
func runQoderCheckin(ctx context.Context, pool *upstream.QoderPool, hours []int, historyPath string) {
	if pool == nil || len(hours) == 0 {
		return
	}
	for {
		wait := time.Until(nextCheckinAt(time.Now(), hours))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		results := pool.CheckinAll(ctx)
		grants := 0
		for _, r := range results {
			switch {
			case r.Status == "ERROR":
				log.Printf("WARN: [qoder] checkin %s: %s", r.CampaignKey, r.Reason)
			case r.Status == "NOTHING_TO_CLAIM":
				log.Printf("[qoder] checkin %s: %s", r.CampaignKey, r.Reason)
			case r.Status == "CLAIMED" && r.Replayed:
				log.Printf("[qoder] checkin %s: 今日已领取（幂等重放）", r.CampaignKey)
			case r.Status == "CLAIMED":
				grants++
				log.Printf("[qoder] checkin %s: 领取成功 grant=%s", r.CampaignKey, r.GrantID)
			}
		}
		if grants > 0 {
			hist := upstream.LoadQoderCheckinHistory(historyPath)
			hist.Mark(time.Now().Format("2006-01-02"), grants)
			if err := hist.Save(historyPath); err != nil {
				log.Printf("WARN: [qoder] 签到历史落盘失败: %v", err)
			}
			cur, total := hist.Streak(time.Now())
			log.Printf("[qoder] 签到完成：今日 %d 次，连续 %d 天 / 累计 %d 天", grants, cur, total)
		}
	}
}

// nextCheckinAt 返回今天/明天最近的一个配置时点（本地时区）。
func nextCheckinAt(now time.Time, hours []int) time.Time {
	best := time.Time{}
	for _, h := range hours {
		if h < 0 || h > 23 {
			continue
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		if best.IsZero() || t.Before(best) {
			best = t
		}
	}
	if best.IsZero() {
		// 没有合法时点：返回一个很远的未来，等价于永不触发（调用方仍可被 ctx 取消）。
		return now.AddDate(10, 0, 0)
	}
	return best
}
