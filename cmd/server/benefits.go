// benefits.go 每日权益自动领取（对齐 workbuddy2api 1.0.4）。
//
// 上游把「限时补偿 / 每日权益」放在官网 web 域，且领取非幂等。这里的策略与
// 手动领取完全一致（先查后领、失败回查），额外只做三件事：
//   - 每天只跑一次（按本地日期去重，进程重启也不会同一天重复跑）；
//   - 只查不领也算一次运行——没权益的号不会每小时被扫一遍；
//   - 全部失败不重试，等下一次排程（重试写请求才是重复发放的来源）。
package main

import (
	"context"
	"log"
	"time"

	"github.com/syan-anan/wb-syan/internal/panel"
)

// benefitTick 检查间隔：小时级排程不需要秒级精度，10 分钟足够落在任意整点内。
const benefitTick = 10 * time.Minute

func runDailyBenefits(ctx context.Context, p *panel.Panel, hours []int) {
	if p == nil || len(hours) == 0 {
		return
	}
	want := make(map[int]bool, len(hours))
	for _, h := range hours {
		if h >= 0 && h <= 23 {
			want[h] = true
		}
	}
	if len(want) == 0 {
		return
	}
	last := ""
	check := func() {
		now := time.Now()
		day := now.Format("2006-01-02")
		if last == day || !want[now.Hour()] {
			return
		}
		last = day
		claimed, skipped, failed, _ := p.ClaimAllBenefits()
		log.Printf("benefits: 每日权益领取完成 claimed=%d skipped=%d failed=%d", claimed, skipped, failed)
	}
	check()
	t := time.NewTicker(benefitTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
