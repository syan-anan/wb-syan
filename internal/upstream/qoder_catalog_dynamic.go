// qoder_catalog_dynamic.go —— 从官方接口拉「此刻」的模型目录。
//
// 端点：GET {gateway}/algo/api/v2/model/list?Encode=1
// 关键点：GET 也必须带一个 qoderEncode("{}") 的 body，并且签名覆盖这个 body，
// 否则服务端按空体验签直接 403（参考实现里踩过的坑）。
//
// 优先级：动态 > 内嵌快照。动态失败（无账号/网络/风控）时静默回落，
// 面板永远有东西可显示。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// qoderCatalogTTL 动态目录缓存时长。模型上下架不频繁，5 分钟够用。
const qoderCatalogTTL = 5 * time.Minute

type qoderCatalogCacheEntry struct {
	entries []QoderCatalogEntry
	at      time.Time
}

var qoderCatalogCache = struct {
	sync.Mutex
	m map[string]qoderCatalogCacheEntry
}{m: map[string]qoderCatalogCacheEntry{}}

// FetchCatalog 拉取该账号所属区域的模型目录。
// realm != cn 目前回落静态快照（国际版网关的模型端点尚未验证）。
func (c *QoderClient) FetchCatalog(ctx context.Context) ([]QoderCatalogEntry, error) {
	if c.Realm != "cn" {
		return nil, fmt.Errorf("qoder: 国际版动态目录未启用")
	}
	key := c.UID
	qoderCatalogCache.Lock()
	if e, ok := qoderCatalogCache.m[key]; ok && time.Since(e.at) < qoderCatalogTTL {
		qoderCatalogCache.Unlock()
		return e.entries, nil
	}
	qoderCatalogCache.Unlock()

	entries, err := c.fetchCatalogCN(ctx)
	if err != nil {
		return nil, err
	}
	qoderCatalogCache.Lock()
	qoderCatalogCache.m[key] = qoderCatalogCacheEntry{entries: entries, at: time.Now()}
	qoderCatalogCache.Unlock()
	return entries, nil
}

func (c *QoderClient) fetchCatalogCN(ctx context.Context) ([]QoderCatalogEntry, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := c.cosyFor(token)
	if err != nil {
		return nil, err
	}
	// GET 也要带 body：签名与体必须一致，否则 403。
	body := qoderEncode([]byte("{}"))
	url := c.cnBase() + "/algo/api/v2/model/list?Encode=1"
	headers, err := sess.headers(body, url, "", false)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qoder: 模型目录 %d: %s", resp.StatusCode, truncateText(string(raw), 160))
	}
	return parseQoderCatalogResponse(raw, "cn")
}

// parseQoderCatalogResponse 解析动态接口响应。
// 两种形态都见过：{"chat":[...]} 与裸数组。按「哪个能解出非空列表」判断。
func parseQoderCatalogResponse(raw []byte, realm string) ([]QoderCatalogEntry, error) {
	var wrapped struct {
		Chat []qoderCatalogRaw `json:"chat"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Chat) > 0 {
		return buildQoderCatalogEntries(wrapped.Chat, realm), nil
	}
	var plain []qoderCatalogRaw
	if err := json.Unmarshal(raw, &plain); err == nil && len(plain) > 0 {
		return buildQoderCatalogEntries(plain, realm), nil
	}
	return nil, fmt.Errorf("qoder: 模型目录解析失败")
}

// Catalog 取某区域的模型目录：优先用该区第一个账号动态拉，
// 失败（无账号/无网络/风控）回落内嵌快照。
func (p *QoderPool) Catalog(ctx context.Context, realm string) []QoderCatalogEntry {
	if p != nil {
		p.mu.Lock()
		var c *QoderClient
		for _, cl := range p.clients {
			if cl.Disabled {
				continue
			}
			if realm == "cn" && cl.Realm == "cn" {
				c = cl
				break
			}
			if realm != "cn" && cl.Realm != "cn" {
				c = cl
				break
			}
		}
		p.mu.Unlock()
		if c != nil {
			if entries, err := c.FetchCatalog(ctx); err == nil && len(entries) > 0 {
				return entries
			}
		}
	}
	return QoderCatalog(realm)
}
