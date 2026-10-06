package panel

// 在线更新检查：比对本地 appVersion 与 GitHub 上的最新发布，面板有新版本时
// 在侧栏挂一个提示。
//
// 数据源是 GitHub 的公开只读 API（无需 token）：
//   GET {base}/repos/{repo}/releases/latest  → tag_name
//   404（仓库还没发过 Release）时回落到 {base}/repos/{repo}/tags，取第一个 tag。
//
// 三条设计约束：
//   - 永不阻塞请求线程：/panel/api/update 只读内存里的缓存快照；缓存过期就在
//     后台起一个刷新，本次请求照样立刻返回旧值。
//   - 永不打扰：外网不通 / 私有部署 / 被墙，只是拿不到结果，面板不显示任何
//     东西，也不会写 WARN 刷日志。想彻底关掉就配 panel.update_check=false。
//   - 不猜版本：tag 解析不出来就当"没有新版本"，绝不在格式变了以后瞎报。
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// updateAPIBase GitHub 公开 API 根；测试指向 httptest。
	updateAPIBase = "https://api.github.com"
	// updateRepoDefault 检查的仓库（owner/name）。
	updateRepoDefault = "syan-anan/wb-syan"
	// updateInterval 两次检查之间的间隔。
	updateInterval = 6 * time.Hour
	// updateFirstDelay 进程启动后首次检查的延迟：不跟启动期的账号加载抢时间，
	// 也避免每次重启都立刻打一次 GitHub（重启频繁时这就是限流）。
	updateFirstDelay = 20 * time.Second
	// updateTimeout 单次 HTTP 超时。
	updateTimeout = 10 * time.Second
	// updateBodyLimit 响应体上限，防上游返回巨大 JSON。
	updateBodyLimit = 1 << 20
)

// updateInfo /panel/api/update 的响应体，也是面板侧栏提示的唯一数据来源。
type updateInfo struct {
	Current   string `json:"current"`              // 本进程版本（appVersion）
	Latest    string `json:"latest,omitempty"`     // GitHub 上最新 tag（未取到为空）
	HasUpdate bool   `json:"has_update"`           // latest 比 current 新
	URL       string `json:"url,omitempty"`        // 发布页/标签页
	CheckedAt string `json:"checked_at,omitempty"` // 上次检查完成时间（RFC3339）
	Error     string `json:"error,omitempty"`      // 最近一次失败原因（面板不展示，排障用）
	Enabled   bool   `json:"enabled"`              // false = 关闭了更新检查
}

// updater 更新检查器：缓存 + 后台刷新。
type updater struct {
	enabled bool
	repo    string
	base    string
	current string
	client  *http.Client

	mu       sync.Mutex
	info     updateInfo
	checked  time.Time
	inflight bool
}

// newUpdater 构造检查器。enabled=false 时不发任何请求（离线/内网部署）。
func newUpdater(enabled bool, current, repo string) *updater {
	if repo == "" {
		repo = updateRepoDefault
	}
	u := &updater{
		enabled: enabled,
		repo:    repo,
		base:    updateAPIBase,
		current: current,
		client:  &http.Client{Timeout: updateTimeout},
	}
	u.info = updateInfo{Current: current, Enabled: enabled}
	return u
}

// snapshot 返回缓存快照；缓存过期（或从未取到）就在后台补一次刷新。
// 无论如何都立刻返回，绝不在这里等网络。
func (u *updater) snapshot() updateInfo {
	if u == nil {
		return updateInfo{}
	}
	u.mu.Lock()
	info := u.info
	stale := u.checked.IsZero() || time.Since(u.checked) >= updateInterval
	u.mu.Unlock()
	if u.enabled && stale {
		go u.refresh()
	}
	return info
}

// refresh 拉一次最新版本并写入缓存。已在刷新中时直接返回（避免并发打 GitHub）。
func (u *updater) refresh() {
	if u == nil || !u.enabled {
		return
	}
	u.mu.Lock()
	if u.inflight {
		u.mu.Unlock()
		return
	}
	u.inflight = true
	u.mu.Unlock()

	latest, url, err := u.fetchLatest()

	u.mu.Lock()
	u.inflight = false
	u.checked = time.Now()
	u.info = updateInfo{
		Current:   u.current,
		Latest:    latest,
		HasUpdate: versionNewer(latest, u.current),
		URL:       url,
		CheckedAt: u.checked.Format(time.RFC3339),
		Enabled:   true,
	}
	if err != nil {
		u.info.Error = err.Error()
	}
	u.mu.Unlock()
}

// run 后台定时刷新：启动后先等 updateFirstDelay，之后每 updateInterval 一次。
func (u *updater) run(ctx context.Context) {
	if u == nil || !u.enabled {
		return
	}
	t := time.NewTicker(updateInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-time.After(updateFirstDelay):
	}
	u.refresh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			u.refresh()
		}
	}
}

// fetchLatest 取最新 tag：先 releases/latest，404 再回落 tags 列表第一条。
func (u *updater) fetchLatest() (tag, url string, err error) {
	api := u.base + "/repos/" + u.repo

	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	relErr := u.getJSON(api+"/releases/latest", &rel)
	if relErr == nil && rel.TagName != "" {
		return rel.TagName, rel.HTMLURL, nil
	}

	var tags []struct {
		Name string `json:"name"`
	}
	if err := u.getJSON(api+"/tags", &tags); err != nil {
		// 两个源都失败：优先报 releases 的错误（更可能是限流/网络），
		// 否则报 tags 的。错误只进 info.Error，面板不显示。
		if relErr != nil {
			return "", "", relErr
		}
		return "", "", err
	}
	if len(tags) == 0 {
		return "", "", nil
	}
	return tags[0].Name, api + "/releases/tag/" + tags[0].Name, nil
}

// getJSON 取一个 JSON 端点；非 200 视为错误（含 403 限流、404 无 Release）。
func (u *updater) getJSON(url string, dst any) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "wb-syan-update-check")
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, updateBodyLimit)).Decode(dst)
}

// parseSemver 解析 "v1.17.0" / "1.18.0-panel" / "1.2" 这类版本串。
// 容忍 v 前缀、预发布后缀（-panel / -rc1）、位数不齐（1.2 → 1.2.0）；
// 解析不出来返回 ok=false —— 宁可判"不更新"，也不瞎报。
func parseSemver(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return out, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// versionNewer 报告 latest 是否比 current 新；任一侧解析失败一律返回 false。
func versionNewer(latest, current string) bool {
	l, okL := parseSemver(latest)
	c, okC := parseSemver(current)
	if !okL || !okC {
		return false
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}
