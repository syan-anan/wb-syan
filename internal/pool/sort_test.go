package pool

import (
	"testing"

	"github.com/syan-anan/wb-syan/internal/auth"
)

// 账号池排序：默认按添加时间升序（历史账号 added_at=0 排最前），
// 「国内 / 国际」把 cn 排在前面、组内仍按添加时间，未知维度退化为默认。
func TestListSortedAddedAndRealm(t *testing.T) {
	p := New("")
	g := &auth.Auth{UID: "g1", Nickname: "global-acc"}
	if _, err := auth.BackfillRealmFor(g, "global"); err != nil {
		t.Fatal(err)
	}
	p.byUID["newest"] = &entry{a: &auth.Auth{UID: "newest"}, addedAt: 300}
	p.byUID["oldest"] = &entry{a: &auth.Auth{UID: "oldest"}, addedAt: 100}
	p.byUID["g1"] = &entry{a: g, addedAt: 200}

	assertOrder := func(name string, got []Status, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: len=%d want %d", name, len(got), len(want))
		}
		for i, s := range got {
			if s.UID != want[i] {
				t.Fatalf("%s: 位置 %d = %s want %s（全部: %+v）", name, i, s.UID, want[i], got)
			}
		}
	}

	assertOrder("added", p.List(), []string{"oldest", "g1", "newest"})
	assertOrder("added-explicit", p.ListSorted(SortAdded), []string{"oldest", "g1", "newest"})
	assertOrder("realm", p.ListSorted(SortRealm), []string{"oldest", "newest", "g1"})
	assertOrder("unknown-falls-back", p.ListSorted("bogus"), []string{"oldest", "g1", "newest"})

	// added_at 缺失（0）排最前：老 state.json 首次启动、尚未回填的账号。
	p.byUID["unknown"] = &entry{a: &auth.Auth{UID: "unknown"}}
	if got := p.List(); got[0].UID != "unknown" {
		t.Fatalf("added_at=0 应排最前，got %s", got[0].UID)
	}

	// AddedAt 透出到 Status（前端排序依赖它）。
	for _, s := range p.List() {
		if s.UID == "g1" && s.AddedAt != 200 {
			t.Fatalf("g1.AddedAt=%d want 200", s.AddedAt)
		}
	}
}

// addedAt 落盘/恢复往返：state.json 里写 added_at，重启后排序不漂移。
func TestAddedAtPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.byUID["u1"] = &entry{a: &auth.Auth{UID: "u1"}, addedAt: 111}
	p.byUID["u2"] = &entry{a: &auth.Auth{UID: "u2"}, addedAt: 222}
	p.mu.Lock()
	p.saveLocked()
	p.mu.Unlock()

	p2 := New(fp)
	if got := p2.List(); got[0].UID != "u1" || got[1].UID != "u2" {
		t.Fatalf("重启后顺序应为 u1,u2，got %+v", got)
	}
	if got := p2.List(); got[0].AddedAt != 111 || got[1].AddedAt != 222 {
		t.Fatalf("重启后 added_at 应保留，got %d,%d", got[0].AddedAt, got[1].AddedAt)
	}
}

// 已存在账号的 added_at 只在为 0 时回填一次：后续 upsert（token 刷新重写 auth 文件）
// 不得改写它，否则排序会随文件 mtime 漂移。
func TestAddedAtBackfillIsOneShot(t *testing.T) {
	p := New("")
	p.byUID["u1"] = &entry{a: &auth.Auth{UID: "u1"}, addedAt: 0}
	p.upsertLocked(&auth.Auth{UID: "u1"})
	first := p.byUID["u1"].addedAt
	if first == 0 {
		t.Fatal("added_at=0 应被回填")
	}
	p.upsertLocked(&auth.Auth{UID: "u1"})
	if got := p.byUID["u1"].addedAt; got != first {
		t.Fatalf("第二次 upsert 不得改写 added_at（%d → %d）", first, got)
	}
}
