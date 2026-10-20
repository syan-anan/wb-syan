// qoder_quota.go —— Qoder 账号余额（credits）查询。
//
// 接口：GET {openapi}/api/v2/quota/usage（纯 Bearer，不需要 COSY 签名）。
// 面板每次刷新都会问一次太浪费，这里做 60 秒缓存；失败不清缓存，
// 让界面保留上一次的好数据并标一个过时标记。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// QoderQuota 一条账号余额快照。
type QoderQuota struct {
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	ExpiresAt int64   `json:"expires_at,omitempty"`
	Unit      string  `json:"unit,omitempty"`
	OK        bool    `json:"ok"`
	Error     string  `json:"error,omitempty"`
	FetchedAt int64   `json:"fetched_at,omitempty"`
}

type qoderQuotaEntry struct {
	quota QoderQuota
	at    time.Time
}

var qoderQuotaCache = struct {
	sync.Mutex
	m map[string]qoderQuotaEntry
}{m: map[string]qoderQuotaEntry{}}

const qoderQuotaTTL = 60 * time.Second

// Quota 返回账号余额；60 秒内复用缓存。
func (c *QoderClient) Quota(ctx context.Context) QoderQuota {
	key := c.UID
	if key == "" {
		key = c.PAT + c.AccessToken
	}
	qoderQuotaCache.Lock()
	if e, ok := qoderQuotaCache.m[key]; ok && time.Since(e.at) < qoderQuotaTTL {
		qoderQuotaCache.Unlock()
		return e.quota
	}
	qoderQuotaCache.Unlock()

	q, err := c.fetchQuota(ctx)
	if err != nil {
		q.OK = false
		q.Error = err.Error()
	} else {
		q.OK = true
		q.FetchedAt = time.Now().Unix()
	}
	qoderQuotaCache.Lock()
	qoderQuotaCache.m[key] = qoderQuotaEntry{quota: q, at: time.Now()}
	qoderQuotaCache.Unlock()
	return q
}

func (c *QoderClient) fetchQuota(ctx context.Context) (QoderQuota, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return QoderQuota{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.OpenAPI+"/api/v2/quota/usage", nil)
	if err != nil {
		return QoderQuota{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Go-http-client/2.0")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return QoderQuota{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return QoderQuota{}, fmt.Errorf("quota %d", resp.StatusCode)
	}
	var body struct {
		ExpiresAt int64 `json:"expiresAt"`
		UserQuota struct {
			Total     float64 `json:"total"`
			Used      float64 `json:"used"`
			Remaining float64 `json:"remaining"`
			Unit      string  `json:"unit"`
		} `json:"userQuota"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return QoderQuota{}, err
	}
	return QoderQuota{
		Remaining: body.UserQuota.Remaining,
		Total:     body.UserQuota.Total,
		Used:      body.UserQuota.Used,
		Unit:      body.UserQuota.Unit,
		ExpiresAt: body.ExpiresAt,
	}, nil
}
