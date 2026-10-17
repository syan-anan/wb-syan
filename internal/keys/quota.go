// quota.go 子密钥的时间窗请求配额计数（每时 / 每日）。
//
// 为什么单独一个文件：计数是跨请求的可变运行态，不属于"配置"。写回 config.json 会让
// 每次请求都改配置文件（脏、慢，且与面板保存配置互相覆盖）；这里独立落盘
// <state 同目录>/keys_quota.json，脏时原子替换（后台 15s 一把 + 进程退出 Flush）。
//
// 窗口口径：日窗口 = 本地自然日（00:00 重置），时窗口 = 本地自然小时。窗口滚动在读取
// 路径惰性完成（比较窗口键），无需定时任务。
//
// 计数语义：Reserve 在一次判定内完成「检查 + 自增」，避免并发请求同时通过检查。
// 一次 HTTP 请求只 Reserve 一次（轮转重试不重复计数）。
package keys

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// quotaFlushInterval 配额计数落盘节流：高频请求下每次写盘没有意义，
// 计数只用于限流，丢最后 15s 的增量不影响可用性（重启后最多多放行 15s 的量）。
const quotaFlushInterval = 15 * time.Second

// Quota 子密钥配额计数器。零值不可用，必须经 NewQuota 构造。
type Quota struct {
	mu    sync.Mutex
	fp    string
	keys  map[string]*quotaCounter
	dirty bool

	// inFlight 每个密钥当前的并发请求数。纯内存：进程重启后在途请求也随之
	// 消失，落盘只会留下永远归不了零的幽灵计数（把 key 永久卡死）。
	inFlight map[string]int

	stopCh  chan struct{}
	stopped bool
}

type quotaCounter struct {
	Day       string `json:"day"`        // 当前日窗口键 "2006-01-02"
	DayCount  int    `json:"day_count"`  // 日窗口内已放行请求数
	Hour      string `json:"hour"`       // 当前时窗口键 "2006-01-02T15"
	HourCount int    `json:"hour_count"` // 时窗口内已放行请求数
	// Models 当日单模型计数：模型名 → 已放行请求数。与 Day 同窗口滚动。
	Models map[string]int `json:"models,omitempty"`
}

type quotaFile struct {
	Keys map[string]*quotaCounter `json:"keys"`
}

// NewQuota 以落盘路径构建（fp 为空 = 纯内存，不落盘；测试用）。
func NewQuota(fp string) *Quota {
	q := &Quota{fp: fp, keys: map[string]*quotaCounter{}, inFlight: map[string]int{}}
	if fp == "" {
		return q
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		return q
	}
	var f quotaFile
	if json.Unmarshal(raw, &f) != nil || f.Keys == nil {
		return q
	}
	q.keys = f.Keys
	return q
}

// Start 启动后台落盘（幂等）。fp 为空时是空操作。
func (q *Quota) Start() {
	if q == nil || q.fp == "" {
		return
	}
	q.mu.Lock()
	if q.stopped || q.stopCh != nil {
		q.mu.Unlock()
		return
	}
	stopCh := make(chan struct{})
	q.stopCh = stopCh
	q.mu.Unlock()
	go func() {
		t := time.NewTicker(quotaFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				q.Flush()
			case <-stopCh:
				return
			}
		}
	}()
}

// Stop 停止后台落盘并做最后一次 Flush（幂等）。
func (q *Quota) Stop() {
	if q == nil {
		return
	}
	q.mu.Lock()
	stopCh := q.stopCh
	q.stopCh = nil
	q.stopped = true
	q.mu.Unlock()
	if stopCh != nil {
		close(stopCh)
	}
	q.Flush()
}

// Reserve 判定并占用一次配额：在单个临界区内完成窗口滚动、限额检查与自增。
// 返回 ok=false 时 reason 是面向调用方的可读原因；限额均为 0 时恒放行（不计数）。
func (q *Quota) Reserve(keyID string, daily, hourly int, now time.Time) (bool, string) {
	return q.ReserveModel(keyID, daily, hourly, "", 0, now)
}

// ReserveModel 在 Reserve 的基础上增加「单模型每日上限」。所有检查先做完再
// 统一自增：否则模型超限时会留下一次已计入日配额的请求（用户看到「明明没发
// 出去却少了一次」）。model 为空或 modelDaily<=0 时行为与 Reserve 完全一致。
func (q *Quota) ReserveModel(keyID string, daily, hourly int, model string, modelDaily int, now time.Time) (bool, string) {
	if q == nil || keyID == "" || (daily <= 0 && hourly <= 0 && modelDaily <= 0) {
		return true, ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	c := q.counterLocked(keyID, now)
	mkey := normModelKey(model)
	if modelDaily > 0 && mkey == "" {
		modelDaily = 0
	}
	if daily > 0 && c.DayCount >= daily {
		return false, "已超出每日配额（上限 " + strconv.Itoa(daily) + " 次，明日 00:00 重置）"
	}
	if hourly > 0 && c.HourCount >= hourly {
		return false, "已超出每小时配额（上限 " + strconv.Itoa(hourly) + " 次，整点重置）"
	}
	if modelDaily > 0 && c.Models[mkey] >= modelDaily {
		return false, "模型 " + mkey + " 已超出每日配额（上限 " + strconv.Itoa(modelDaily) + " 次，明日 00:00 重置）"
	}
	c.DayCount++
	c.HourCount++
	if modelDaily > 0 {
		if c.Models == nil {
			c.Models = map[string]int{}
		}
		c.Models[mkey]++
	}
	q.dirty = true
	return true, ""
}

// Usage 返回该 key 当前日/时窗口的已用请求数（面板展示用）。
func (q *Quota) Usage(keyID string, now time.Time) (day, hour int) {
	if q == nil || keyID == "" {
		return 0, 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	c := q.counterLocked(keyID, now)
	return c.DayCount, c.HourCount
}

// ModelUsage 返回该 key 指定模型在当前自然日的已用请求数（面板展示用）。
func (q *Quota) ModelUsage(keyID, model string, now time.Time) int {
	if q == nil || keyID == "" {
		return 0
	}
	mkey := normModelKey(model)
	if mkey == "" {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.counterLocked(keyID, now).Models[mkey]
}

// Acquire 占用一个并发名额：当前在途数达到 limit 时不放行。
// 调用方拿到 ok=true 后必须且只能 Release 一次（defer 即可）。
func (q *Quota) Acquire(keyID string, limit int) (bool, string) {
	if q == nil || keyID == "" || limit <= 0 {
		return true, ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.inFlight == nil {
		q.inFlight = map[string]int{}
	}
	if q.inFlight[keyID] >= limit {
		return false, "该密钥并发已满（在途 " + strconv.Itoa(q.inFlight[keyID]) +
			" / 上限 " + strconv.Itoa(limit) + "，稍后重试）"
	}
	q.inFlight[keyID]++
	return true, ""
}

// Release 归还一个并发名额（幂等钳到 0，不会因重复释放把计数打成负数）。
func (q *Quota) Release(keyID string) {
	if q == nil || keyID == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.inFlight[keyID] <= 1 {
		delete(q.inFlight, keyID)
		return
	}
	q.inFlight[keyID]--
}

// InFlight 返回该密钥当前在途请求数（面板展示用）。
func (q *Quota) InFlight(keyID string) int {
	if q == nil || keyID == "" {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.inFlight[keyID]
}

// Reset 清零某个 key 的计数（面板「重置计数」用）。
func (q *Quota) Reset(keyID string) {
	if q == nil || keyID == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.keys, keyID)
	q.dirty = true
}

// Drop 删除某个 key 的计数条目（密钥被删除时调用，避免文件无限增长）。
func (q *Quota) Drop(keyID string) { q.Reset(keyID) }

// counterLocked 取（必要时新建）计数器并滚动过期窗口。调用方必须已持有 q.mu。
func (q *Quota) counterLocked(keyID string, now time.Time) *quotaCounter {
	c := q.keys[keyID]
	if c == nil {
		c = &quotaCounter{}
		q.keys[keyID] = c
	}
	day := now.Format("2006-01-02")
	if c.Day != day {
		c.Day, c.DayCount = day, 0
		c.Models = nil // 模型计数与日窗口同寿命：跨天整表清零
	}
	hour := now.Format("2006-01-02T15")
	if c.Hour != hour {
		c.Hour, c.HourCount = hour, 0
	}
	return c
}

// normModelKey 把模型名归一成计数键：去掉 cn:/global:/qoder: 前缀、去空白、
// 转小写。同一模型用不同前缀调用时共享同一份日计数（否则「glm-5.2」与
// 「cn:glm-5.2」会各算一份，上限被悄悄翻倍）。
func normModelKey(model string) string {
	s := strings.TrimSpace(model)
	if k := strings.IndexByte(s, ':'); k > 0 {
		s = s[k+1:]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// Flush 脏时原子落盘（幂等）。
func (q *Quota) Flush() {
	if q == nil || q.fp == "" {
		return
	}
	q.mu.Lock()
	if !q.dirty {
		q.mu.Unlock()
		return
	}
	q.dirty = false
	raw, err := json.MarshalIndent(quotaFile{Keys: q.keys}, "", "  ")
	q.mu.Unlock()
	if err != nil {
		log.Printf("keys: 配额落盘序列化失败: %v", err)
		return
	}
	if dir := filepath.Dir(q.fp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := q.fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("keys: 配额落盘失败: %v", err)
		return
	}
	if err := os.Rename(tmp, q.fp); err != nil {
		log.Printf("keys: 配额落盘替换失败: %v", err)
	}
}
