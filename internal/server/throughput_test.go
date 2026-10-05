package server

import (
	"math"
	"strings"
	"testing"
	"time"
)

// 吞吐口径 = 输出 token ÷ 解码时长（总耗时 − 首字）。
// 对齐 DeepSeek Harness 的 turn-metrics（completedTime − firstTokenTime）与
// Artificial Analysis 的 Output Speed：首字绝不进分母。
func TestDecodeThroughputExcludesTTFB(t *testing.T) {
	cases := []struct {
		name   string
		tokens int64
		ttfb   time.Duration
		total  time.Duration
		want   float64
	}{
		// 首字 20s / 总 30s / 输出 2000 → 解码 10s → 200 tok/s。
		// 端到端口径会算成 66.7——那正是被首字摊薄的假象。
		{"长首字被剔除", 2000, 20 * time.Second, 30 * time.Second, 200},
		// 首字无观测（非流式）：回落端到端口径，不是 0。
		{"首字缺失回落端到端", 2000, 0, 10 * time.Second, 200},
		// 首字 >= 总耗时（异常观测）：回落，不产生负解码时长。
		{"首字大于总耗时回落", 2000, 12 * time.Second, 10 * time.Second, 200},
		// 首字 == 总耗时（单帧流）：回落，不除零。
		{"首字等于总耗时", 100, 5 * time.Second, 5 * time.Second, 20},
	}
	for _, c := range cases {
		got, ok := decodeThroughput(c.tokens, c.ttfb, c.total)
		if !ok {
			t.Errorf("%s: ok=false want true", c.name)
			continue
		}
		if math.Abs(got-c.want) > 0.05 {
			t.Errorf("%s: got %.2f want %.2f", c.name, got, c.want)
		}
	}
}

// token 数未知（-1 哨兵）或总耗时非正时不产出速率：调用方显示「无观测」，
// 而不是把缺失观测伪造成 0。
func TestDecodeThroughputNoObservation(t *testing.T) {
	if _, ok := decodeThroughput(-1, 0, 10*time.Second); ok {
		t.Error("token 未知: ok=true want false")
	}
	if _, ok := decodeThroughput(100, 0, 0); ok {
		t.Error("总耗时为 0: ok=true want false")
	}
}

// 日志行的吞吐列必须扣掉首字：同一条流只改 TTFB，吞吐就该变。
func TestLogChatRowThroughputExcludesTTFB(t *testing.T) {
	withChatLog(t)
	// 1234 ÷ (27.1 − 0.412) = 46.2；端到端口径是 45.5。
	stream := captureStdout(t, func() {
		logChatRow(412*time.Millisecond, 27100*time.Millisecond, "m", "stream", "u", "", 200, 1234, 0, false)
	})
	if !strings.Contains(stream, "46.2tok/s") {
		t.Errorf("want decode rate 46.2tok/s:\n%s", stream)
	}
	if strings.Contains(stream, "45.5tok/s") {
		t.Errorf("row still uses the end-to-end rate:\n%s", stream)
	}
	// 非流式（TTFB 无观测）：回落端到端口径 1234 ÷ 27.1 = 45.5。
	sync := captureStdout(t, func() {
		logChatRow(0, 27100*time.Millisecond, "m", "sync", "u", "", 200, 1234, 0, false)
	})
	if !strings.Contains(sync, "45.5tok/s") {
		t.Errorf("sync row should fall back to the end-to-end rate:\n%s", sync)
	}
}
