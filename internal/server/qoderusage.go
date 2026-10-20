// qoderusage.go —— Qoder 分支的用量采集。
//
// 为什么单独一个文件：Qoder 走的是另一条链路（handler 里提前 return），
// 进不了 WorkBuddy 的 recordAttempt 汇聚点。这里用一个「尾巴缓冲」把响应流
// 旁路一份，关闭时解析最后一帧 usage，再写进同一个 Recorder —— 两条上游的
// 数字因此仍在同一张表里，只是模型名带 qoder: 前缀。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"time"

	"github.com/syan-anan/wb-syan/internal/usage"
)

// qoderUsageTailSize 只保留响应尾部：usage 总在最后一帧，留 16KB 足够。
const qoderUsageTailSize = 16 * 1024

// qoderUsageTap 透传响应，同时在尾部留一份用于解析 usage。
type qoderUsageTap struct {
	rc    io.ReadCloser
	tail  []byte
	start time.Time
	ttft  time.Duration
	onEnd func(prompt, completion, cached, ttftMs, totalMs int64, ok bool)
}

func newQoderUsageTap(rc io.ReadCloser, start time.Time, onEnd func(int64, int64, int64, int64, int64, bool)) *qoderUsageTap {
	if start.IsZero() {
		start = time.Now()
	}
	return &qoderUsageTap{rc: rc, start: start, onEnd: onEnd}
}

func (t *qoderUsageTap) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		// 第一次读到字节 = 首包时间；流式请求里它就是 TTFT 的近似。
		if t.ttft == 0 {
			t.ttft = time.Since(t.start)
		}
		t.tail = append(t.tail, p[:n]...)
		if len(t.tail) > qoderUsageTailSize {
			t.tail = t.tail[len(t.tail)-qoderUsageTailSize:]
		}
	}
	if err != nil {
		t.finish()
	}
	return n, err
}

func (t *qoderUsageTap) Close() error {
	t.finish()
	return t.rc.Close()
}

func (t *qoderUsageTap) finish() {
	if t.onEnd == nil {
		return
	}
	prompt, completion, cached, ok := parseQoderUsage(t.tail)
	t.onEnd(prompt, completion, cached, t.ttft.Milliseconds(), time.Since(t.start).Milliseconds(), ok)
	t.onEnd = nil
}

// parseQoderUsage 取最后一个 usage 对象。
// 不能用正则：usage 里有嵌套对象（completion_tokens_details），
// 简单字符类会在第一个 } 处截断。这里做括号计数，跳过字符串内的括号。
func parseQoderUsage(tail []byte) (int64, int64, int64, bool) {
	// 信封里的 body 是 JSON 字符串，括号和引号都带反斜杠转义。
	plain := bytes.ReplaceAll(tail, []byte(`\"`), []byte(`"`))
	obj := extractJSONAfterKey(plain, `"usage"`)
	if obj == nil {
		return 0, 0, 0, false
	}
	var u struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		InputTokens      int64 `json:"input_tokens"`
		OutputTokens     int64 `json:"output_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		PromptDetails    struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(obj, &u); err != nil {
		return 0, 0, 0, false
	}
	p, c := u.PromptTokens, u.CompletionTokens
	if p == 0 && u.InputTokens > 0 {
		p = u.InputTokens
	}
	if c == 0 && u.OutputTokens > 0 {
		c = u.OutputTokens
	}
	if p == 0 && c == 0 && u.TotalTokens == 0 {
		return 0, 0, 0, false
	}
	return p, c, u.PromptDetails.CachedTokens, true
}

// extractJSONAfterKey 从最后一次出现的 key 之后取出一个完整的 JSON 对象。
func extractJSONAfterKey(b []byte, key string) []byte {
	i := bytes.LastIndex(b, []byte(key))
	if i < 0 {
		return nil
	}
	j := bytes.IndexByte(b[i:], '{')
	if j < 0 {
		return nil
	}
	start := i + j
	depth := 0
	inStr := false
	esc := false
	for k := start; k < len(b); k++ {
		ch := b[k]
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return b[start : k+1]
			}
		}
	}
	return nil
}

// recordQoderUsage 写一条 Qoder 用量；model 已带 qoder: 前缀。
func (h *Handler) recordQoderUsage(realm, uid, model string, prompt, completion, cached, ttftMs, totalMs int64, ok bool) {
	if h.cfg.Usage == nil || !ok {
		return
	}
	// 吞吐 = 输出 token / 生成时长；生成时长 = 总耗时 - 首字延迟。
	tps := 0.0
	if completion > 0 && totalMs > ttftMs {
		tps = float64(completion) / (float64(totalMs-ttftMs) / 1000.0)
	}
	h.cfg.Usage.AddGrouped(time.Now(), realm, uid, model, "qoder", "",
		usage.Delta{
			PromptTokens:     prompt,
			HasPromptTokens:  true,
			CompletionTokens: completion,
			HasCompletion:    true,
			TotalTokens:      prompt + completion,
			CachedTokens:     cached,
			HasCached:        true,
			LatencyMs:        totalMs,
			HasLatency:       totalMs > 0,
			TokensPerSecond:  tps,
			HasTPS:           tps > 0,
			TTFTMs:           ttftMs,
			HasTTFT:          ttftMs > 0,
		}, true)
}
