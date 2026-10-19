// 积分流水：把「积分事件」与「余额变化」分成两套账分别记录。
//
// 为什么必须分成两套（这不是实现细节，是对账语义）：
//   - confirmed（确认事件）：已知来源的发放——任务奖励、每日权益。金额来自上游
//     领取响应，是「应该到账多少」；
//   - observed（余额观察）：余额从 A 变成 B，但不知道是哪一笔造成的（可能是上游
//     后台补发、退款、过期扣减、或我们还没接的活动）。
//
// 两者**不可相加**：同一笔奖励会先出现在 confirmed，随后余额刷新又在 observed 里
// 出现一次。把两套账加起来等于把同一笔钱数两遍。所以前端分栏展示，合计只给各栏
// 自己的小计。
//
// 并发约束：observed 来自 pool 的余额钩子，钩子在 pool.mu 持有期间调用，因此
// OnBalanceChange 必须是纯内存操作（不读池、不写盘、不发网络）。落盘走 30s
// 后台 ticker + Save()，与 usage.Recorder 同一套节奏。
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// journalCap 内存中保留的最大条数；超出后按时间裁掉最旧的。
// 20k 条 × ~200B ≈ 4MB，对单机面板足够，也不会让 journal.json 无限膨胀。
const journalCap = 20000

// journalFlush 后台落盘周期。
const journalFlush = 30 * time.Second

// CreditEntry 一条积分流水。
type CreditEntry struct {
	Key          string    `json:"key"`                     // 幂等键：同一事件重复写入只留一条
	UID          string    `json:"uid"`                     // 账号 uid
	Kind         string    `json:"kind"`                    // confirmed | observed
	Source       string    `json:"source"`                  // 任务奖励 / 每日权益 / 余额观察（来源未确认）
	Delta        int64     `json:"delta"`                   // 积分变化（正=入账，负=扣减）
	BalanceAfter *int64    `json:"balance_after,omitempty"` // observed 才有：变更后余额
	At           time.Time `json:"at"`
}

type journalFile struct {
	Version int           `json:"version"`
	Saved   string        `json:"saved"`
	Entries []CreditEntry `json:"entries"`
}

// Journal 积分流水存储（并发安全）。
type Journal struct {
	mu      sync.Mutex
	path    string
	entries []CreditEntry
	seen    map[string]bool
	dirty   bool

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// NewJournal 创建流水存储。path 为空 = 纯内存（测试用）。
func NewJournal(path string) *Journal {
	j := &Journal{
		path: path,
		seen: map[string]bool{},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if path != "" {
		if err := j.load(); err != nil {
			log.Printf("[journal] 读取 %s 失败（从零开始）: %v", path, err)
		}
	}
	return j
}

// Start 启动后台防抖落盘。
func (j *Journal) Start() {
	go func() {
		defer close(j.done)
		t := time.NewTicker(journalFlush)
		defer t.Stop()
		for {
			select {
			case <-j.stop:
				j.flush(true)
				return
			case <-t.C:
				j.flush(false)
			}
		}
	}()
}

// Stop 停止后台循环并做最后一次落盘。
func (j *Journal) Stop() {
	j.stopOnce.Do(func() { close(j.stop) })
	<-j.done
}

// Save 立即落盘（退出前 / 备份前调用）。
func (j *Journal) Save() { j.flush(true) }

func (j *Journal) load() error {
	raw, err := os.ReadFile(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f journalFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = f.Entries
	for _, e := range j.entries {
		if e.Key != "" {
			j.seen[e.Key] = true
		}
	}
	j.trimLocked()
	return nil
}

func (j *Journal) flush(force bool) {
	j.mu.Lock()
	if !j.dirty && !force {
		j.mu.Unlock()
		return
	}
	if j.path == "" {
		j.dirty = false
		j.mu.Unlock()
		return
	}
	snap := journalFile{Version: 1, Saved: time.Now().Format(time.RFC3339), Entries: append([]CreditEntry(nil), j.entries...)}
	j.dirty = false
	j.mu.Unlock()
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[journal] 序列化失败: %v", err)
		return
	}
	if dir := dirOf(j.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[journal] 落盘失败: %v", err)
		return
	}
	if err := os.Rename(tmp, j.path); err != nil {
		log.Printf("[journal] 重命名失败: %v", err)
	}
}

// trimLocked 裁剪到 journalCap。调用方必须持锁。
func (j *Journal) trimLocked() {
	if len(j.entries) <= journalCap {
		return
	}
	drop := len(j.entries) - journalCap
	for _, e := range j.entries[:drop] {
		delete(j.seen, e.Key)
	}
	j.entries = append([]CreditEntry(nil), j.entries[drop:]...)
}

// Append 追加一条流水。Key 已存在时返回 false（幂等去重）。
func (j *Journal) Append(e CreditEntry) bool {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if e.Key != "" && j.seen[e.Key] {
		return false
	}
	if e.Key != "" {
		j.seen[e.Key] = true
	}
	j.entries = append(j.entries, e)
	j.trimLocked()
	j.dirty = true
	return true
}

// OnBalanceChange 余额钩子：记录 observed 流水。
//
// 钩子在 pool.mu 持有期间被调用：只做内存追加，任何 IO 都会把池锁住。
// 幂等键里带「秒级时间 + 前后值」：同一个 tick 内同一账号的同一跳变只记一条；
// 不同秒的相同跳变（例如连续两次 +50）仍会各自记账，这是真实发生的两次变动。
func (j *Journal) OnBalanceChange(uid string, before, after int64) {
	delta := after - before
	if delta == 0 {
		return
	}
	now := time.Now()
	bal := after
	j.Append(CreditEntry{
		Key:          fmt.Sprintf("obs|%s|%d|%d>%d", uid, now.Unix(), before, after),
		UID:          uid,
		Kind:         "observed",
		Source:       "余额观察（来源未确认）",
		Delta:        delta,
		BalanceAfter: &bal,
		At:           now,
	})
}

// RecordConfirmed 记录一条已知来源的积分事件。
func (j *Journal) RecordConfirmed(uid, source string, delta int64, key string) {
	if delta <= 0 {
		return
	}
	j.Append(CreditEntry{
		Key:    key,
		UID:    uid,
		Kind:   "confirmed",
		Source: source,
		Delta:  delta,
		At:     time.Now(),
	})
}

// journalQuery 查询条件。空字段 = 不筛选。
type journalQuery struct {
	UID    string
	Source string
	Kind   string
	From   time.Time
	To     time.Time
	Page   int
	Size   int
}

// journalRow 返回给前端的行（账号昵称在查询时解析，不落盘）。
type journalRow struct {
	CreditEntry
	Account string `json:"account"`
}

// journalResult 查询结果。两套账分别给小计（不可相加，见文件头）。
type journalResult struct {
	Rows           []journalRow `json:"rows"`
	Total          int          `json:"total"`
	ConfirmedTotal int64        `json:"confirmed_total"`
	ObservedTotal  int64        `json:"observed_total"`
	Accounts       []string     `json:"accounts"`
	Sources        []string     `json:"sources"`
}

// matchLocked 应用筛选。调用方必须持锁。
func (j *Journal) matchLocked(q journalQuery) []CreditEntry {
	out := make([]CreditEntry, 0, len(j.entries))
	for _, e := range j.entries {
		if q.UID != "" && e.UID != q.UID {
			continue
		}
		if q.Kind != "" && e.Kind != q.Kind {
			continue
		}
		if q.Source != "" && !strings.Contains(e.Source, q.Source) {
			continue
		}
		if !q.From.IsZero() && e.At.Before(q.From) {
			continue
		}
		if !q.To.IsZero() && e.At.After(q.To) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Query 按条件筛选并按时间倒序分页。小计覆盖**筛选后的全部匹配行**，
// 不是当前页——否则翻页时数字会跳。
func (j *Journal) Query(q journalQuery) journalResult {
	if q.Page < 1 {
		q.Page = 1
	}
	if q.Size <= 0 {
		q.Size = 50
	}
	if q.Size > 500 {
		q.Size = 500
	}
	j.mu.Lock()
	matched := j.matchLocked(q)
	acctSet := map[string]bool{}
	srcSet := map[string]bool{}
	for _, e := range j.entries {
		acctSet[e.UID] = true
		srcSet[e.Source] = true
	}
	j.mu.Unlock()
	// 同一时刻的多条（同一批余额刷新会撞进同一个时间戳）按**插入序倒排**：
	// 纯按 At 排时相等元素的顺序是不确定的，翻页会看到同一条在两个位置跳。
	sort.Slice(matched, func(a, b int) bool {
		if !matched[a].At.Equal(matched[b].At) {
			return matched[a].At.After(matched[b].At)
		}
		return a > b
	})
	res := journalResult{Total: len(matched)}
	for _, e := range matched {
		switch e.Kind {
		case "confirmed":
			res.ConfirmedTotal += e.Delta
		case "observed":
			res.ObservedTotal += e.Delta
		}
	}
	start := (q.Page - 1) * q.Size
	if start > res.Total {
		start = res.Total
	}
	end := start + q.Size
	if end > res.Total {
		end = res.Total
	}
	for _, e := range matched[start:end] {
		res.Rows = append(res.Rows, journalRow{CreditEntry: e})
	}
	for u := range acctSet {
		res.Accounts = append(res.Accounts, u)
	}
	sort.Strings(res.Accounts)
	for s := range srcSet {
		res.Sources = append(res.Sources, s)
	}
	sort.Strings(res.Sources)
	return res
}

// dirOf 取文件所在目录；纯文件名返回空串（写当前目录）。
func dirOf(p string) string {
	i := strings.LastIndexAny(p, "/\\")
	if i <= 0 {
		return ""
	}
	return p[:i]
}
