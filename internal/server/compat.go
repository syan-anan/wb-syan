// compat.go —— 通用 OpenAI 兼容上游的转发入口。
//
// 与 qoder.go 的分工：Qoder 有自己的一套鉴权（PAT → job token）与指纹头，
// 需要专门实现；这里处理的是"标准 OpenAI 形状 + 可配置 base_url"的上游，
// 转发逻辑只有一份。两者的响应写回共用 pipeUpstream。
package server

import (
	"io"
	"log"
	"net/http"
)

// compatChat 处理 "<provider>:<model>" 请求。
func (h *Handler) compatChat(w http.ResponseWriter, r *http.Request, body []byte, provider, bareModel string, stream bool) {
	if h.cfg.Compat == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "provider_not_configured",
			"没有可用的 OpenAI 兼容上游")
		return
	}
	if bareModel == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"model: "+provider+": 后缺少模型名")
		return
	}
	out, err := rewriteQoderModel(body, bareModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "rewrite model: "+err.Error())
		return
	}
	rc, status, respBody, err := h.cfg.Compat.ChatStream(r.Context(), provider, out)
	if err != nil {
		log.Printf("ERR: [compat] %s:%s: %v", provider, bareModel, err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	if rc == nil {
		code := status
		if code < 400 {
			code = http.StatusBadGateway
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(respBody)
		return
	}
	defer rc.Close()
	pipeUpstream(w, rc, stream)
}

// pipeUpstream 把上游响应原样搬回客户端，流式时逐块 flush。
//
// 抽出来是因为 Qoder / 通用兼容上游 / 未来的其他上游，写回逻辑完全一致：
// 都已经是 OpenAI 形状，网关不该在这一步再做任何翻译。
func pipeUpstream(w http.ResponseWriter, rc io.Reader, stream bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	buf := make([]byte, 32*1024)
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 客户端断了
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				log.Printf("WARN: [compat] stream read: %v", rerr)
			}
			return
		}
	}
}

// compatModelEntries 供 /v1/models 列出通用上游模型。
func (h *Handler) compatModelEntries() []map[string]any {
	if h.cfg.Compat == nil {
		return nil
	}
	models := h.cfg.Compat.Models()
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{
			"id":       m.ID,
			"object":   "model",
			"owned_by": "openai-compat",
			"label":    m.Label,
		})
	}
	return out
}

// compatSummary 面板"上游与路由"卡片的数据。
func (h *Handler) compatSummary() []map[string]any {
	if h.cfg.Compat == nil {
		return nil
	}
	out := make([]map[string]any, 0, h.cfg.Compat.Len())
	for _, name := range h.cfg.Compat.ProviderNames() {
		out = append(out, map[string]any{
			"name":    name,
			"kind":    "openai-compat",
			"label":   name + "（OpenAI 兼容）",
			"enabled": true,
		})
	}
	return out
}

