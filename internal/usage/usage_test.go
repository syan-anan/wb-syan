package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 成功/失败尝试计数、total 的 pt+ct 兜底口径、按域/账号聚合。
func TestAddAndTotals(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", "codex", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, LatencyMs: 200, HasLatency: true}, true)
	// 失败尝试：无 usage → 只计请求数与失败数，token 不加。
	r.Add(now, "global", "uid1", "claude-4.6", "codex", Delta{}, false)
	// 上游没给 total 时用 pt+ct 兜底，保证总量口径连续。
	r.Add(now, "cn", "uid1", "glm-5.2", "codex", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 3 || s.Totals.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 3/1", s.Totals.Requests, s.Totals.Errors)
	}
	if s.Totals.PromptTokens != 110 || s.Totals.CompletionTok != 55 {
		t.Fatalf("pt/ct = %d/%d, want 110/55", s.Totals.PromptTokens, s.Totals.CompletionTok)
	}
	if s.Totals.TotalTokens != 165 {
		t.Fatalf("tt = %d, want 165（无 total 时按 pt+ct 兜底）", s.Totals.TotalTokens)
	}
	if s.Totals.AvgLatencyMs != 200 {
		t.Fatalf("avg latency = %v, want 200", s.Totals.AvgLatencyMs)
	}
	if len(s.ByRealm) != 2 {
		t.Fatalf("by_realm = %d 项, want 2", len(s.ByRealm))
	}
	if s.ByAccount[0].Realm == "" {
		t.Fatal("by_account 行缺 realm 标注")
	}
}

// Rollup 把超出 hourlyKeep 的小时桶折叠为日桶，且幂等：重复折叠不重复计数。
// 窗口口径：24h 窗口不含 100 天前的日桶；hours=0（全部历史）才含日点。
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 天前，超出 90 天小时保留
	r.Add(old, "cn", "u", "m", "codex", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", "codex", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", "codex", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Snapshot(24, nil)
	if after.Totals.Requests != 1 || after.Totals.PromptTokens != 1 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/1（窗口外日桶不进聚合）", after.Totals.Requests, after.Totals.PromptTokens)
	}
	if len(after.Series) != 1 || after.Series[0].Scope != "hour" {
		t.Fatalf("series = %+v, want 仅当前小时 1 个点", after.Series)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 3 || all.Totals.PromptTokens != 15 {
		t.Fatalf("全部历史 totals = %d/%d, want 3/15", all.Totals.Requests, all.Totals.PromptTokens)
	}
	if len(all.Series) != 2 || all.Series[0].Scope != "day" || all.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v, want 日点在前 + 小时点在后", all.Series)
	}

	r.Rollup(time.Now())
	again := r.Snapshot(0, nil)
	if again.Totals.Requests != 3 || again.Totals.PromptTokens != 15 {
		t.Fatalf("二次折叠后 totals = %d/%d, want 3/15（幂等被破坏）", again.Totals.Requests, again.Totals.PromptTokens)
	}
}

// 落盘→新实例恢复，数据不丢；落盘结构带版本号。
func TestFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", "codex", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.TotalTokens != 42 {
		t.Fatalf("恢复后 totals = %d/%d, want 1/42", s.Totals.Requests, s.Totals.TotalTokens)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != 1 || len(f.Buckets) != 1 {
		t.Fatalf("落盘文件异常: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// Snapshot 全口径窗口过滤：窗口外的数据不进**任何**聚合（卡片/表格/时序），
// 切窗口数字随之变化；hours=0 全部历史。Buckets 为窗口内命中的桶数。
func TestSnapshotWindowFilter(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", "codex", Delta{PromptTokens: 5, HasPromptTokens: true}, true) // 窗口(24h)外
	r.Add(now, "cn", "u", "m", "codex", Delta{PromptTokens: 3, HasPromptTokens: true}, true)                    // 窗口内
	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.PromptTokens != 3 {
		t.Fatalf("24h 窗口 totals = %d/%d, want 1/3（48h 前的数据应被过滤）", s.Totals.Requests, s.Totals.PromptTokens)
	}
	if len(s.Series) != 1 || s.Series[0].Scope != "hour" || s.Series[0].PromptTokens != 3 {
		t.Fatalf("series = %+v, want 仅窗口内 1 个小时点", s.Series)
	}
	if s.Buckets != 1 {
		t.Fatalf("buckets = %d, want 1（窗口内命中桶数）", s.Buckets)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 2 || all.Totals.PromptTokens != 8 {
		t.Fatalf("全部历史 totals = %d/%d, want 2/8", all.Totals.Requests, all.Totals.PromptTokens)
	}
	// since 是全库数据起点，不受窗口影响。
	if all.Since == "" || s.Since != all.Since {
		t.Fatalf("since 应为全库起点且不随窗口变化: all=%q windowed=%q", all.Since, s.Since)
	}
}

// Stop 触发最终落盘（Start 后未到防抖间隔也要落）。
func TestLifecycleFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", "codex", Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop 后应有落盘文件: %v", err)
	}
}

// 积分口径：只有带 HasCredits 的尝试才进 Cr/CrN；免费模型（观测到 0）与
// 「没有观测」必须分得开——CrN 是判据，不是 Cr。
func TestCreditsAccounting(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u", "m", "codex", Delta{Credits: 12.5, HasCredits: true}, true)
	r.Add(now, "cn", "u", "m", "codex", Delta{Credits: 7.5, HasCredits: true}, true)
	r.Add(now, "cn", "u", "m", "codex", Delta{}, true)                               // 无成本观测
	r.Add(now, "cn", "u2", "m2", "codex", Delta{Credits: 0, HasCredits: true}, true) // 观测到免费
	r.Add(now, "cn", "u", "m", "codex", Delta{}, false)                              // 失败且无观测

	s := r.Snapshot(24, nil)
	if s.Totals.Credits != 20 || s.Totals.CreditsN != 3 {
		t.Fatalf("totals credits = %v/%d, want 20/3（只有带观测的尝试才入账）", s.Totals.Credits, s.Totals.CreditsN)
	}
	var u, u2 bool
	for _, k := range s.ByAccount {
		switch k.Key {
		case "u":
			u = true
			if k.Credits != 20 || k.CreditsN != 2 {
				t.Fatalf("by_account[u] = %v/%d, want 20/2", k.Credits, k.CreditsN)
			}
		case "u2":
			u2 = true
			// 免费模型：有样本（CrN=1）但消耗为 0——前端据此显示 0 而不是「—」。
			if k.Credits != 0 || k.CreditsN != 1 {
				t.Fatalf("by_account[u2] = %v/%d, want 0/1", k.Credits, k.CreditsN)
			}
		}
	}
	if !u || !u2 {
		t.Fatalf("by_account 缺少 u/u2: %+v", s.ByAccount)
	}
	// 窗口过滤同样作用于积分口径。
	if old := r.Snapshot(0, nil); old.Totals.Credits != 20 {
		t.Fatalf("全部历史 credits = %v, want 20", old.Totals.Credits)
	}

	// 「失败不入账」只针对没有观测的情况：流中途断但末帧已回报 credit 时，
	// 消费是真实发生的，照样入账——否则线上看到的消耗会系统性偏低。
	r2 := New("")
	r2.Add(now, "cn", "u", "m", "codex", Delta{Credits: 3, HasCredits: true}, false)
	if s2 := r2.Snapshot(24, nil); s2.Totals.Credits != 3 || s2.Totals.CreditsN != 1 {
		t.Fatalf("失败但有成本观测 credits = %v/%d, want 3/1", s2.Totals.Credits, s2.Totals.CreditsN)
	}
}

// 落盘/折叠后积分不丢：Rollup 合并日桶时 Cr/CrN 必须一起搬。
func TestCreditsSurviveRollupAndFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	old := time.Now().AddDate(0, 0, -100)
	r.Add(old, "cn", "u", "m", "codex", Delta{Credits: 4, HasCredits: true}, true)
	r.Add(time.Now(), "cn", "u", "m", "codex", Delta{Credits: 6, HasCredits: true}, true)
	r.Rollup(time.Now())
	r.Save()
	s := New(path).Snapshot(0, nil)
	if s.Totals.Credits != 10 || s.Totals.CreditsN != 2 {
		t.Fatalf("折叠+落盘后 credits = %v/%d, want 10/2", s.Totals.Credits, s.Totals.CreditsN)
	}
}

// 客户端维度：同 client 合并成一个桶，不同 client 分开；空 client 归 "(unknown)"。
func TestByClientSplit(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u", "glm-5.2", "codex", Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	r.Add(now, "cn", "u", "glm-5.2", "codex", Delta{PromptTokens: 20, HasPromptTokens: true}, true)
	r.Add(now, "cn", "u", "glm-5.2", "claude-code", Delta{PromptTokens: 5, HasPromptTokens: true}, true)
	r.Add(now, "cn", "u", "glm-5.2", "", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	s := r.Snapshot(24, nil)
	if len(s.ByClient) != 3 {
		t.Fatalf("by_client = %d 项, want 3: %+v", len(s.ByClient), s.ByClient)
	}
	// 按 total 降序：codex(30) > claude-code(5) > (unknown)(1)
	if s.ByClient[0].Key != "codex" || s.ByClient[0].Requests != 2 || s.ByClient[0].TotalTokens != 30 {
		t.Fatalf("by_client[0] = %+v, want codex 2 req / 30 tt（同 client 应合并）", s.ByClient[0])
	}
	if s.ByClient[1].Key != "claude-code" || s.ByClient[1].Requests != 1 {
		t.Fatalf("by_client[1] = %+v, want claude-code 1 req", s.ByClient[1])
	}
	if s.ByClient[2].Key != "(unknown)" || s.ByClient[2].Requests != 1 {
		t.Fatalf("by_client[2] = %+v, want (unknown) 1 req（空 client 兜底）", s.ByClient[2])
	}
}

// 老版本落盘数据（桶里没有 cl 字段）加载后归入 "(历史数据)"，且不丢量。
func TestLegacyClientBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	now := time.Now()
	raw := `{"version":1,"saved":"","buckets":[{"s":"h:` + now.Format("2006-01-02T15") + `","r":"cn","u":"u1","m":"glm-5.2","q":3,"e":0,"p":7,"c":0,"t":7,"l":0,"ln":0,"v":0,"vn":0}]}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 3 || s.Totals.PromptTokens != 7 {
		t.Fatalf("恢复后 totals = %d/%d, want 3/7", s.Totals.Requests, s.Totals.PromptTokens)
	}
	if len(s.ByClient) != 1 || s.ByClient[0].Key != legacyClientLabel {
		t.Fatalf("by_client = %+v, want 单行 %q", s.ByClient, legacyClientLabel)
	}
	if s.ByClient[0].Requests != 3 || s.ByClient[0].TotalTokens != 7 {
		t.Fatalf("by_client[0] = %+v, want 3 req / 7 tt", s.ByClient[0])
	}
	// 回填后 key 里应带 client 段，且再次落盘读回仍归同一行。
	r.Save()
	r2 := New(path)
	s2 := r2.Snapshot(24, nil)
	if len(s2.ByClient) != 1 || s2.ByClient[0].Key != legacyClientLabel || s2.ByClient[0].TotalTokens != 7 {
		t.Fatalf("二次加载 by_client = %+v, want 仍为单行 %q / 7 tt", s2.ByClient, legacyClientLabel)
	}
}
