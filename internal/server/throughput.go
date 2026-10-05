// throughput.go 吞吐口径的单一事实来源（日志行与用量桶共用）。
//
// 吞吐 = 输出 token ÷ 解码时长，其中解码时长 = 总耗时 − 首字耗时。
//
// 口径对齐 DeepSeek Harness 的 turn-metrics（completedTime − firstTokenTime）与
// 行业标准（Artificial Analysis「Output Speed：首个 token 之后每秒收到的 token
// 数」）：
//   - 首字（TTFT）绝不进分母——长上下文的首字会把解码速率摊薄成假象；
//   - 首字缺失（非流式 / 未观测到首帧）或 >= 总耗时时回落端到端口径（降级，
//     而不是把缺失观测伪造成 0 或负数）；
//   - 输出 token 数未知（-1 哨兵）或总耗时非正时不产出速率。
package server

import "time"

// decodeThroughput 返回一次请求的吞吐（tok/s）。
// ok=false 表示观测不足以给出速率，调用方应显示「无观测」而不是 0。
func decodeThroughput(tokens int64, ttfb, total time.Duration) (float64, bool) {
	if tokens < 0 || total <= 0 {
		return 0, false
	}
	decode := total
	if ttfb > 0 && ttfb < total {
		decode = total - ttfb
	}
	if decode <= 0 {
		return 0, false
	}
	return float64(tokens) / decode.Seconds(), true
}
