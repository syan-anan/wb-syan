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

	stopCh  chan struct{}
	stopped bool
}

type quotaCounter struct {
	Day       string `json:"day"`        // 当前日窗口键 "2006-01-02"
	DayCount  int    `json:"day_count"`  // 日窗口内已放行请求数
	Hour      string `json:"hour"`       // 当前时窗口键 "2006-01-02T15"
	HourCount int    `json:"hour_count"` // 时窗口内已放行请求数
}

type quotaFile struct {
	Keys map[string]*quotaCounter `json:"keys"`
}

// NewQuota 以落盘路径构建（fp 为空 = 纯内存，不落盘；测试用）。
func NewQuota(fp string) *Quota {
	q := &Quota{fp: fp, keys: map[string]*quotaCounter{}}
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
	if q == nil || keyID == "" || (daily <= 0 && hourly <= 0) {
		return true, ""
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	c := q.counterLocked(keyID, now)
	if daily > 0 && c.DayCount >= daily {
		return false, "已超出每日配额（上限 " + strconv.Itoa(daily) + " 次，明日 00:00 重置）"
	}
	if hourly > 0 && c.HourCount >= hourly {
		return false, "已超出每小时配额（上限 " + strconv.Itoa(hourly) + " 次，整点重置）"
	}
	c.DayCount++
	c.HourCount++
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
	}
	hour := now.Format("2006-01-02T15")
	if c.Hour != hour {
		c.Hour, c.HourCount = hour, 0
	}
	return c
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
