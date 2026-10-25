// handler_responses.go — POST /v1/responses：OpenAI Responses API 原生入口。
//
// Codex CLI 等客户端只讲 Responses 协议；此前需要外部 codex-bridge/codex-relay
// 做一次 Responses→Chat 转换再打进本网关。现在在网关内直接转换：
//
//	Codex (Responses) ──转换──▶ h.chatCompletions（内部 HTTP 复用，保选号/粘性/
//	审计/限流/工具标记还原全链路）──转换──▶ Responses SSE / JSON
//
// 转换实现搬自 EquentR/responses-to-chat-proxy（MIT），见 responses_adapters.go
// 与 responses_stream.go；previous_response_id 仅作会话粘性键使用（Codex 每轮
// 全量重放 input，网关无需自建响应存储——与上游 proxy 的 sticky_key 口径一致）。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// responsesHandler 提供给 mux 的入口（withAuth 在注册处包一层）。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil || req == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}

	model, _ := req["model"].(string)
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing model")
		return
	}

	// previous_response_id → 会话粘性：Codex 的会话连续性靠它表达。
	// 复用 chat 的 sticky 提取口径：把它写进请求体的 conversation_id 位（session.
	// ExtractKey 认得的键族之一），转发给 chatCompletions 内部链路。
	if prev, _ := req["previous_response_id"].(string); prev != "" && req["conversation_id"] == nil {
		req["conversation_id"] = prev
	}

	chatReq := ConvertRequest(req, respProxyConfig{})
	// 模型名必须原样透传（realm 前缀路由是网关协议）。
	chatReq["model"] = model
	// 网关侧始终把 usage 打开，转换层要用它生成 Responses usage 字段。
	if stream, _ := req["stream"].(bool); stream {
		chatReq["stream"] = true
		if so, ok := chatReq["stream_options"].(map[string]any); ok {
			so["include_usage"] = true
		} else {
			chatReq["stream_options"] = map[string]any{"include_usage": true}
		}
	}

	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "marshal chat request: "+err.Error())
		return
	}

	// 内部转发到本进程的 /v1/chat/completions：复用完整 chat 链路（选号、粘性、
	// 审计、限流、DSML 工具标记还原……），避免复制那份 800 行的状态机。
	innerURL := "http://127.0.0.1" + h.cfg.internalListenPath() + "/v1/chat/completions"
	ireq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, innerURL, bytes.NewReader(chatBody))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	ireq.Header = r.Header.Clone()
	ireq.Header.Set("Content-Type", "application/json")
	ireq.ContentLength = int64(len(chatBody))

	resp, err := h.cfg.internalHTTPClient().Do(ireq)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "internal dispatch: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if stream, _ := req["stream"].(bool); stream {
		h.streamResponsesOut(w, req, resp)
		return
	}
	h.jsonResponsesOut(w, req, resp)
}

// jsonResponsesOut 非流式：读 chat JSON → ConvertResponse → 原样回写。
func (h *Handler) jsonResponsesOut(w http.ResponseWriter, req map[string]any, resp *http.Response) {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "read upstream: "+err.Error())
		return
	}
	if resp.StatusCode >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw) // 上游错误体已是 OpenAI error 形态，透传即可
		return
	}
	var chatResp map[string]any
	if err := json.Unmarshal(raw, &chatResp); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "decode upstream: "+err.Error())
		return
	}
	out := ConvertResponse(chatResp, req)
	writeJSON(w, http.StatusOK, out)
}

// streamResponsesOut 流式：chat SSE → StreamingConverter → Responses SSE。
func (h *Handler) streamResponsesOut(w http.ResponseWriter, req map[string]any, resp *http.Response) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode >= 400 {
		// 错误也按 SSE error 事件回，客户端不至于把 JSON 当事件流解析炸掉。
		raw, _ := io.ReadAll(resp.Body)
		msg := strings.TrimSpace(string(raw))
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", sseJSONString(msg))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	isCompaction := false
	if input, _ := req["input"].([]any); len(input) > 0 {
		for _, item := range input {
			dict, _ := item.(map[string]any)
			if s, _ := dict["type"].(string); s == "compaction_trigger" {
				isCompaction = true
				break
			}
		}
	}
	converter := NewStreamingConverter(isCompaction)
	buf := make([]byte, 8192)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			for _, ev := range converter.Feed(buf[:n]) {
				if _, werr := io.WriteString(w, ev); werr != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				for _, ev := range converter.Finish() {
					_, _ = io.WriteString(w, ev)
				}
				if flusher != nil {
					flusher.Flush()
				}
				return
			}
			// 传输错误：发 error 事件收尾，避免客户端挂死。
			_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", sseJSONString(rerr.Error()))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
	}
}

// sseJSONString 把任意文本编码成 JSON 字符串字面量（SSE data 行用）。
func sseJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// internalListenPath 把 cfg.Listen 规整成可拼 URL 的 ":port" 形式；空值回退 :7863。
func (c Config) internalListenPath() string {
	l := c.Listen
	if l == "" {
		l = ":7863"
	}
	if !strings.HasPrefix(l, ":") {
		// "host:port" 形态：取最后一个冒号之后的端口段。
		if i := strings.LastIndex(l, ":"); i >= 0 {
			l = l[i:]
		} else {
			l = ":7863"
		}
	}
	return l
}

// internalHTTPClient 内部回环客户端：本机转发无网络延迟，超时放宽到 10 分钟
// （覆盖最长的流式补全）。
func (c Config) internalHTTPClient() *http.Client {
	return &http.Client{Timeout: 0} // 不设总超时：流式响应由客户端断连控制生命周期
}
