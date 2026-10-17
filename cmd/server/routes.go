package main

import (
	"fmt"
	"strings"

	"github.com/syan-anan/wb-syan/internal/server"
)

// buildRoutes 把 models 配置段编译成路由表。
//
// 校验收紧在启动期：别名指向自身/成环、时段格式错误、降级链为空串，
// 这些在运行期只会表现为"某个模型莫名不可用"，排查成本远高于启动时炸掉。
func buildRoutes(cfg *Config) (*server.RouteTable, error) {
	aliases := make(map[string]string, len(cfg.Models.Aliases))
	for k, v := range cfg.Models.Aliases {
		name := strings.TrimSpace(k)
		target := strings.TrimSpace(v)
		if name == "" {
			return nil, fmt.Errorf("models.aliases 存在空别名")
		}
		if target == "" {
			return nil, fmt.Errorf("models.aliases.%s 目标为空", name)
		}
		if name == target {
			return nil, fmt.Errorf("models.aliases.%s 指向自身", name)
		}
		aliases[name] = target
	}

	fallback := make(map[string][]string, len(cfg.Models.Fallback))
	for k, list := range cfg.Models.Fallback {
		model := strings.TrimSpace(k)
		if model == "" {
			return nil, fmt.Errorf("models.fallback 存在空模型名")
		}
		clean := make([]string, 0, len(list))
		for _, item := range list {
			m := strings.TrimSpace(item)
			if m == "" {
				return nil, fmt.Errorf("models.fallback.%s 含空项", model)
			}
			clean = append(clean, m)
		}
		if len(clean) == 0 {
			return nil, fmt.Errorf("models.fallback.%s 降级链为空", model)
		}
		fallback[model] = clean
	}

	windows := make(map[string][]server.HourRange, len(cfg.Models.UpstreamHours))
	for up, specs := range cfg.Models.UpstreamHours {
		name := strings.TrimSpace(up)
		if name == "" {
			return nil, fmt.Errorf("models.upstream_hours 存在空上游名")
		}
		ranges, err := server.ParseHourRanges(specs)
		if err != nil {
			return nil, fmt.Errorf("models.upstream_hours.%s: %w", name, err)
		}
		if len(ranges) > 0 {
			windows[name] = ranges
		}
	}

	return server.NewRouteTable(aliases, fallback, windows), nil
}
