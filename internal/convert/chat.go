// Package convert 桥业务链路(handler):POST /v1/chat/completions。
// 无鉴权(内网信任);Authorization 原样透传到上游(key 生命周期归下游/newapi 渠道管理)。
// store=false 固定;下游断连上线程随上游 ctx 级联取消(省上游配额)。
package convert

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"mistral-bridge/internal/oai"
)

// ChatConfig 桥运行配置(注入到 handler)
type ChatConfig struct {
	UpstreamBase   string
	Client         *http.Client
	BuiltinTools   []string
	PassReasoning  bool
	MapCCWebSearch bool
	SSEIdleTimeout time.Duration
	Logger         *slog.Logger
}

// MaxNonStreamBody 上游非流式响应读全上限(防异常巨大 body;超限 502)
const MaxNonStreamBody = 64 << 20

type chatHandler struct {
	cfg ChatConfig
}

// NewChatHandler 构造 chat 处理器
func NewChatHandler(cfg ChatConfig) http.Handler { return &chatHandler{cfg: cfg} }

// ServeHTTP 完整业务链路
func (h *chatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	logger := h.cfg.Logger

	// ---- 1) Authorization 透传检查 ----
	auth := r.Header.Get("Authorization")
	if strings.TrimSpace(auth) == "" {
		WriteOaiError(w, http.StatusUnauthorized,
			"missing Authorization header", "invalid_request_error", nil, "invalid_api_key")
		h.accessLog(reqID, r, start, 0, http.StatusUnauthorized, "missing_auth", nil)
		return
	}

	// ---- 2) 读下游 body(body 上限已由 server 包裹 MaxBytesReader) ----
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// 超 413 或截断
		WriteOaiError(w, http.StatusRequestEntityTooLarge,
			"request body too large or malformed", "invalid_request_error", nil, nil)
		h.accessLog(reqID, r, start, 0, http.StatusRequestEntityTooLarge, "body_read_error", nil)
		return
	}

	// ---- 3) 解析 OAI 请求 ----
	req, err := oai.ParseChatRequest(body)
	if err != nil {
		WriteOaiError(w, http.StatusBadRequest,
			"parse request: "+err.Error(), "invalid_request_error", nil, nil)
		h.accessLog(reqID, r, start, 0, http.StatusBadRequest, "parse_error", nil)
		return
	}
	if len(req.Messages) == 0 {
		WriteOaiError(w, http.StatusBadRequest,
			"messages must not be empty", "invalid_request_error", "messages", nil)
		h.accessLog(reqID, r, start, 0, http.StatusBadRequest, "empty_messages", nil)
		return
	}

	// ---- 4) 协议转换(§10) ----
	conv, err := ConvertRequest(req, Options{
		BuiltinTools:   h.cfg.BuiltinTools,
		PassReasoning:  h.cfg.PassReasoning,
		MapCCWebSearch: h.cfg.MapCCWebSearch,
	})
	if err != nil {
		if be, ok := err.(*BridgeError); ok {
			WriteOaiError(w, http.StatusBadRequest, be.Message, be.Type,
				nullableParam(be.Param), be.Code)
			h.accessLog(reqID, r, start, 0, http.StatusBadRequest, "bridge_validation:"+be.Message[:minInt(60, len(be.Message))], nil)
			return
		}
		WriteOaiError(w, http.StatusInternalServerError,
			"convert error: "+err.Error(), "api_error", nil, nil)
		h.accessLog(reqID, r, start, 0, http.StatusInternalServerError, "convert_error", nil)
		return
	}
	// 决策 debug 日志
	for _, d := range conv.Decisions {
		logger.Debug("convert decision", "req_id", reqID, "decision", d)
	}

	// ---- 5) 构造上游请求(Authorization 透传 + Content-Type + 桥 UA,极简头集) ----
	upstreamURL := strings.TrimSuffix(h.cfg.UpstreamBase, "/") + "/v1/conversations"
	// 用 r.Context() 作为上游 ctx:下游断开即级联取消
	ureq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(conv.Body))
	if err != nil {
		WriteOaiError(w, http.StatusInternalServerError, "build upstream request failed", "api_error", nil, nil)
		h.accessLog(reqID, r, start, 0, http.StatusInternalServerError, "upstream_build_err", nil)
		return
	}
	ureq.Header.Set("Authorization", auth)
	ureq.Header.Set("Content-Type", "application/json")
	ureq.Header.Set("User-Agent", "mistral-bridge/1.0")

	// ---- 6) 发上游 ----
	resp, err := h.cfg.Client.Do(ureq)
	if err != nil {
		// 下游断连属于常态收尾,INFO 降噪
		if isDownstreamCancel(err) {
			logger.Info("upstream request aborted by downstream", "req_id", reqID)
			h.accessLog(reqID, r, start, 0, 499, "downstream_canceled", nil)
			return
		}
		logger.Warn("upstream roundtrip failed", "req_id", reqID, "err", err.Error())
		WriteOaiError(w, http.StatusBadGateway, "upstream request failed", "api_error", nil, nil)
		h.accessLog(reqID, r, start, 0, http.StatusBadGateway, "upstream_failed", nil)
		return
	}
	// 响应头 x-request-id:优先 x-kong-request-id(排查关联),否则自生成
	outReqID := resp.Header.Get("x-kong-request-id")
	if outReqID == "" {
		outReqID = reqID
	}

	// ---- 7) 状态分档 ----
	if resp.StatusCode >= 400 {
		// 4xx/5xx/429:错误归一化
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		outStatus, oe := NormalizeUpstreamError(resp.StatusCode, errBody)
		// 429:状态/body/限流头**原样回传**
		if resp.StatusCode == http.StatusTooManyRequests {
			h.passthroughHeaders(w, resp, outReqID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write(errBody)
			h.accessLog(reqID, r, start, resp.StatusCode, http.StatusTooManyRequests, "rate_limit_passthrough", nil)
			return
		}
		h.passthroughHeaders(w, resp, outReqID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(outStatus)
		_ = writeJSON(w, oe)
		h.accessLog(reqID, r, start, resp.StatusCode, outStatus, "error_normalized", nil)
		return
	}

	// ---- 8) 2xx:非流式 / 流式 分流 ----
	isJSON := conv.ResponseFormat == "json_object" || conv.ResponseFormat == "json_schema"
	if !conv.Stream && !conv.ForcedStream {
		// 非流式(客户端请求流式=false):读全并转换
		defer resp.Body.Close()
		upBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxNonStreamBody))
		if err != nil {
			h.passthroughHeaders(w, resp, outReqID)
			WriteOaiError(w, http.StatusBadGateway, "upstream body read failed", "api_error", nil, nil)
			h.accessLog(reqID, r, start, resp.StatusCode, http.StatusBadGateway, "upstream_body_err", nil)
			return
		}
		maxTok := maxOf(req.MaxTokens, req.MaxCompletionTok)
		oaiBody, repair, err := ConvertResponse(upBody, conv.OriginalModel, conv.InputTextForUsage, maxTok, isJSON)
		if err != nil {
			WriteOaiError(w, http.StatusBadGateway, "upstream response decode failed", "api_error", nil, nil)
			h.accessLog(reqID, r, start, resp.StatusCode, http.StatusBadGateway, "decode_err", nil)
			return
		}
		h.passthroughHeaders(w, resp, outReqID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(oaiBody)
		h.accessLog(reqID, r, start, resp.StatusCode, http.StatusOK,
			"nonstream", repair)
		return
	}

	// ---- 9) 流式(或桥强制流式) ----
	h.passthroughHeaders(w, resp, outReqID)
	audit := HandleStream(w, r, resp, StreamConfig{
		OaiModel:     conv.OriginalModel,
		MaxTokens:    maxOf(req.MaxTokens, req.MaxCompletionTok),
		InputText:    conv.InputTextForUsage,
		IsJSON:       isJSON,
		Forced:       conv.ForcedStream,
		ClientStream: conv.Stream,
		IdleTimeout:  h.cfg.SSEIdleTimeout,
		Logger:       logger,
		ReqID:        reqID,
	})
	h.accessLogStream(reqID, r, start, audit)
}

// passthroughHeaders 透传上游响应头(限流/缓存/追踪类)+ 自生成 x-request-id
func (p *chatHandler) passthroughHeaders(w http.ResponseWriter, resp *http.Response, reqID string) {
	for k, vs := range resp.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-ratelimit-") || lk == "retry-after" ||
			strings.HasPrefix(lk, "x-cache") || strings.HasPrefix(lk, "x-kong-") {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
	}
	w.Header().Set("x-request-id", reqID)
}

// newRequestID 自生成请求 ID(crypto/rand 16 字节 hex)
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// maxOf 两指针返回较大值(nil 视为 0)
func maxOf(a, b *int) int {
	x, y := 0, 0
	if a != nil {
		x = *a
	}
	if b != nil {
		y = *b
	}
	if x > y {
		return x
	}
	return y
}

// nullableParam 空转 nil(OAI 模板 param 可为 null)
func nullableParam(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// minInt 下限小助手
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// isDownstreamCancel 判断上游错误是否因下游断连引起
func isDownstreamCancel(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "client disconnected") ||
		strings.Contains(msg, "Client.Timeout")
}

// writeJSON 编码响应 JSON
func writeJSON(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// accessLog access 日志(非流式/错误路径共享)
func (h *chatHandler) accessLog(reqID string, r *http.Request, start time.Time, upstreamStatus, outStatus int, outcome string, repair *RepairInfo) {
	attrs := []any{
		"req_id", reqID,
		"method", r.Method,
		"path", r.URL.Path,
		"outcome", outcome,
		"latency_ms", time.Since(start).Milliseconds(),
	}
	if upstreamStatus > 0 {
		attrs = append(attrs, "upstream_status", upstreamStatus)
	}
	if outStatus > 0 {
		attrs = append(attrs, "status", outStatus)
	}
	if repair != nil && repair.UsageRepaired {
		attrs = append(attrs, "usage_repaired", true)
	}
	if repair != nil && repair.JSONFolded {
		attrs = append(attrs, "json_dup_folded", true)
	}
	if repair != nil && repair.EmptyContent {
		// 非流式空回:上游 200 但无 text 无 tool_calls(guaranteed/搜索+high 偶发;观测,_jsorry缺陷,
		// 不改造不回改内容,仅打上观测标记;重试与否归客户端)
		h.cfg.Logger.Warn("upstream returned empty content (guided/search+high flaky)", "req_id", reqID)
		attrs = append(attrs, "empty_content", true)
	}
	h.cfg.Logger.Info("request done", attrs...)
}

// accessLogStream 流式 access 日志
func (h *chatHandler) accessLogStream(reqID string, r *http.Request, start time.Time, a *StreamAudit) {
	attrs := []any{
		"req_id", reqID,
		"method", r.Method,
		"path", r.URL.Path,
		"stream", a.Stream,
		"model", a.Model,
		"finish_reason", a.FinishReason,
		"tool_calls", a.ToolCallCount,
		"latency_ms", time.Since(start).Milliseconds(),
	}
	if a.UsagePromptTok >= 0 {
		attrs = append(attrs, "usage_prompt", a.UsagePromptTok, "usage_completion", a.UsageCompletionTok, "usage_total", a.UsageTotalTok)
	}
	if a.UsageRepaired {
		attrs = append(attrs, "usage_repaired", true)
	}
	if a.JSONFolded {
		attrs = append(attrs, "json_dup_folded", true)
	}
	if a.FloodAborted {
		attrs = append(attrs, "flood_aborted", true)
	}
	if a.DownstreamAbort {
		attrs = append(attrs, "downstream_abort", true)
	}
	if a.IdleCut {
		attrs = append(attrs, "idle_cut", true)
	}
	if a.UpstreamErr != "" {
		attrs = append(attrs, "upstream_err", a.UpstreamErr)
	}
	if a.EmptyContent {
		// 流式空回:同样是观测标记(guided/搜索+high 装填偶发)
		h.cfg.Logger.Warn("upstream returned empty content on stream (flaky)", "req_id", reqID)
		attrs = append(attrs, "empty_content", true)
	}
	h.cfg.Logger.Info("request done", attrs...)
}
