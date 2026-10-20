// qoder.go —— "qoder:" 前缀模型的上游转发。
//
// 设计取向：Qoder 的对话端点本身就返回 OpenAI Chat Completions 形状
// （choices / delta / reasoning_content / usage 都在），所以这里**不做任何协议翻译**，
// 只做三件事：剥前缀、换 Authorization、把上游响应原样搬回客户端。
//
// 这条链路刻意不进 pool：Qoder 是单账号上游，没有多号轮转/冷却/粘性的语义。
// 混进 pool 会让 WorkBuddy 的选号逻辑凭空多出"没有 uid 的账号"这种脏状态。
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/syan-anan/wb-syan/internal/session"
	"github.com/syan-anan/wb-syan/internal/upstream"
)

// qoderModelPrefix 模型名前缀。用 "qoder:" 而不是 "qoder/" 是为了和既有的
// "cn:" / "global:" realm 前缀保持同一种语法，客户端不用记两套规则。
const (
	qoderModelPrefix = "qoder:"
	// 双区显式前缀：同一把 key、同一个地址，用前缀决定走哪一区。
	qoderCNPrefix     = "qodercn:"
	qoderGlobalPrefix = "qoderglobal:"
)

// qoderChat 处理一条 qoder:* 请求。
//
// body 是**原始请求体**（已由 chatCompletions 读入）；这里只改 model 字段，
// 其余字段（messages / tools / temperature / stream …）一字不动地透传给上游——
// 上游要什么形状就给他什么形状，网关不在这里二次发明。
// splitQoderModel 解析 Qoder 模型名前缀，返回 (realm, 裸模型名, 是否匹配)。
// realm 为空表示「全池」（兼容旧的 qoder: 前缀）。
func splitQoderModel(model string) (string, string, bool) {
	switch {
	case strings.HasPrefix(model, qoderCNPrefix):
		return "cn", strings.TrimPrefix(model, qoderCNPrefix), true
	case strings.HasPrefix(model, qoderGlobalPrefix):
		return "intl", strings.TrimPrefix(model, qoderGlobalPrefix), true
	case strings.HasPrefix(model, qoderModelPrefix):
		return "", strings.TrimPrefix(model, qoderModelPrefix), true
	}
	return "", "", false
}

func (h *Handler) qoderChat(w http.ResponseWriter, r *http.Request, body []byte, modelSlug, realm string, stream bool) {
	t0 := time.Now()
	if h.cfg.Qoder == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "qoder_not_configured",
			"Qoder 上游未配置：在 config 里填 qoder.personal_access_token 后重启")
		return
	}
	if strings.TrimSpace(modelSlug) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model: qoder: 后缺少模型名")
		return
	}
	// 客户端传的是 slug（deepseek-flash），上游要 key（dfmodel）。
	modelKey := upstream.QoderResolveKey(realm, modelSlug)

	out, err := rewriteQoderModel(body, modelKey)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "rewrite model: "+err.Error())
		return
	}

	// 缓存亲和：把会话键注入 prompt_cache_key。
	// Qoder 侧实测接受该字段（200，未知字段被忽略也不报错），而它是上游
	// prompt 缓存命中的唯一凭据——同一会话复用同一个 key 才能吃到缓存折扣。
	// 客户端已自带 key 时不覆盖（那是它自己的缓存策略，比我们懂）。
	stickyKey := qoderStickyKey(body)
	if stickyKey != "" {
		out = injectQoderCacheKey(out, stickyKey)
	}

	rc, status, respBody, err := h.cfg.Qoder.ChatStream(r.Context(), out, stickyKey, realm)
	if err != nil {
		log.Printf("ERR: [qoder] %s: %v", modelSlug, err)
		code := http.StatusBadGateway
		if status == http.StatusServiceUnavailable {
			// 「该区没有可用账号」是容量问题，不是上游故障：503 才能让客户端重试。
			code = http.StatusServiceUnavailable
		}
		writeOpenAIError(w, code, "upstream_error", err.Error())
		return
	}
	if rc == nil {
		// 非 2xx：上游错误体已经是 OpenAI 信封，原样透出（不二次包装，免得丢掉
		// request_id 之类的排查线索）。
		code := status
		if code < 400 {
			code = http.StatusBadGateway
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(respBody)
		return
	}
	usagePrefix := qoderModelPrefix
	switch realm {
	case "cn":
		usagePrefix = qoderCNPrefix
	case "intl":
		usagePrefix = qoderGlobalPrefix
	}
	// 用量旁路：Qoder 分支在 handler 里提前 return，进不了 WorkBuddy 的
	// recordAttempt，所以在这里单独接一次。
	tap := newQoderUsageTap(rc, t0, func(p, c, cached, ttft, total int64, ok bool) {
		// 记录 slug 而不是 key：用量页要跟 /v1/models 显示的名字一致。
		h.recordQoderUsage(realm, "", usagePrefix+modelSlug, p, c, cached, ttft, total, ok)
	})
	pipeUpstream(w, tap, stream)
}

// rewriteModel 把请求体的 model 换成裸名，其余字段原样保留。
//
// 用 map 而不是 typed struct：客户端可能带网关不认识的扩展字段（reasoning、
// prediction、audio…），typed struct 会在 unmarshal/marshal 之间把它们丢掉。
func rewriteQoderModel(body []byte, bareModel string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(bareModel)
	if err != nil {
		return nil, err
	}
	obj["model"] = raw
	return json.Marshal(obj)
}

// qoderModelEntries 供 /v1/models 列出的 Qoder 模型（带 qoder: 前缀）。
func (h *Handler) qoderModelEntries() []map[string]any {
	out := make([]map[string]any, 0, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	for _, r := range []struct {
		prefix string
		realm  string
	}{{qoderCNPrefix, "cn"}, {qoderGlobalPrefix, "intl"}} {
		entries := upstream.QoderCatalog(r.realm)
		if h.cfg.Qoder != nil {
			// 动态目录（官方此刻），拉不到时 Catalog 内部已回落快照。
			if dyn := h.cfg.Qoder.Catalog(ctx, r.realm); len(dyn) > 0 {
				entries = dyn
			}
		}
		for _, m := range entries {
			out = append(out, map[string]any{
				"id":       r.prefix + m.Slug,
				"object":   "model",
				"owned_by": "qoder-" + r.realm,
				"label":    m.Name,
			})
		}
	}
	return out
}

// qoderStickyKey 从请求体派生会话亲和键。
//
// 优先用会话键（客户端显式声明的 conversation / session），没有则退到轮级键
// （最后一条 user 消息的哈希）——后者让"同一轮重试"落在同一账号上，但不同轮
// 之间不绑定，避免把独立请求错误地钉在一起。
func qoderStickyKey(body []byte) string {
	if k := session.ExtractKey(body); k != "" {
		return "sess:" + k
	}
	if k := session.TurnKey(body); k != "" {
		return "turn:" + k
	}
	return ""
}

// injectQoderCacheKey 在请求体里补 prompt_cache_key（已存在则不动）。
func injectQoderCacheKey(body []byte, stickyKey string) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	if _, has := obj["prompt_cache_key"]; has {
		return body
	}
	raw, err := json.Marshal(qoderCacheKey(stickyKey))
	if err != nil {
		return body
	}
	obj["prompt_cache_key"] = raw
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// qoderCacheKey 生成稳定的缓存键（同一 stickyKey 恒等）。
// 加前缀是为了在日志里一眼看出这是网关注入的，而不是客户端自带。
func qoderCacheKey(stickyKey string) string {
	sum := sha256.Sum256([]byte(stickyKey))
	return "wb2a-q-" + hex.EncodeToString(sum[:8])
}
