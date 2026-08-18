// Mistral conversations SSE → OAI Chat Completions SSE 流式状态机(§11.3)。
// 要点:
//   - 入站双行 event+data,出站单行 data+即时 flush
//   - tool_calls.index 按"新 tool_call_id 出现次序"分配(不能用上游 output_index)
//   - 结束保证:无 done(异常断连/idle-cut/熔断)也必须补 finish_reason + [DONE]
//   - conversation.response.error:正常收流(不写非标准错误 JSON),错误进入桥观测日志
//   - 下游断连:立即 cancel 上游(上游 request 以 r.Context() 构建,天然级联)
//   - 内置工具 tool.execution.* 与 agent.handoff.* 对客户端透明,不进工具列
//   - 修复件只在各自路径触发:① JSON 首块折叠 ② usage=0 tokenizer 兜底 ③ required 洪水熔断
package convert

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"mistral-bridge/internal/repair"
	"mistral-bridge/internal/tokenizer"
)

// StreamConfig 流式会话配置
type StreamConfig struct {
	OaiModel     string        // model 回显值
	MaxTokens    int           // 客户端显式 max_tokens(0=未传;判定 length)
	InputText    string        // tokenizer 兜底的 prompt 计数文本
	IsJSON       bool          // response_format 为 json_*(是否启用①)
	Forced       bool          // required 洪水治理(强制内部流式 + 折叠 + early-abort)
	ClientStream bool          // 下游客户端是否真请求了流式(决定 chunk 是否下发)
	IdleTimeout  time.Duration // SSE idle watchdog
	Logger       *slog.Logger
	ReqID        string
}

// streamSession 流式会话状态(累加器)
type streamSession struct {
	cfg StreamConfig

	roleSent bool
	finished bool // 已发过末尾 finish chunk

	// 工具调用索引:tool_call_id → oai index
	toolCallIndex map[string]int
	nextToolIndex int
	// 每个 index 的 name 与累加 arguments(非流式聚合/估算用)
	toolCalls []*OaiToolCall

	aggText     strings.Builder // 聚合正文 text
	aggThinking strings.Builder // 聚合 reasoning

	jsonFolder *repair.JSONFolder // ① JSON 折叠器(仅 JSON 流)
	floodGuard *repair.FloodGuard // ③ required 洪水
	floodAbort bool               // 已触发熔断

	usage        *usageFromStream
	doneReceived bool
	convErr      string              // conversation.response.error 的 message(观测日志)
	resetIdle    func()              // SSE idle watchdog 重置回调(由 HandleStream 装配)
	rawWriter    http.ResponseWriter // 原始 ResponseWriter(非流式回包用)
}

// usageFromStream done 事件内 usage 字段
type usageFromStream struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// HandleStream 执行一次流式会话转换,返回(access 日志必要字段)。
// upResp: 上游 200 的 SSE response;body 归本函数负责关闭。
func HandleStream(
	w http.ResponseWriter,
	r *http.Request,
	upResp *http.Response,
	cfg StreamConfig,
) *StreamAudit {
	audit := &StreamAudit{Model: cfg.OaiModel, Stream: cfg.ClientStream}
	defer upResp.Body.Close()

	s := &streamSession{
		cfg:           cfg,
		toolCallIndex: map[string]int{},
	}
	if cfg.IsJSON {
		s.jsonFolder = repair.NewJSONFolder()
	}
	if cfg.Forced {
		s.floodGuard = repair.NewFloodGuard()
	}

	flusher, isFlusher := w.(http.Flusher)
	// 客户端流式:设置 SSE 头(非流式 = 桥强制流式,客户端拿到整包)
	writeDown := cfg.ClientStream && isFlusher
	var wDown io.Writer = io.Discard
	if writeDown {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // 禁代理缓冲
		wDown = w
	}
	s.rawWriter = w // 非流式聚合路径用的原始 ResponseWriter

	// SSE idle watchdog:超时 cancel 上游 ctx
	ctxCancel := func() {}
	if cfg.IdleTimeout > 0 {
		ctx, cancel := context.WithCancel(upResp.Request.Context())
		_ = ctx
		ctxCancel = cancel
		defer cancel()
		timer := time.AfterFunc(cfg.IdleTimeout, func() {
			cancel()
			if cfg.Logger != nil {
				cfg.Logger.Warn("sse stream idle timeout, cutting upstream", "req_id", cfg.ReqID)
			}
			audit.IdleCut = true
		})
		defer timer.Stop()
		s.resetIdle = func() { timer.Reset(cfg.IdleTimeout) }
	}

	br := bufio.NewReaderSize(upResp.Body, 64<<10)
	eventType := ""
	var dataBuf []byte

	for {
		select {
		case <-r.Context().Done():
			// 下游断连:不补结尾,上游 ctx 已随 request cancel
			audit.DownstreamAbort = true
			s.finishAudit(audit)
			return audit
		default:
		}

		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event:"):
				eventType = strings.TrimSpace(line[6:])
			case strings.HasPrefix(line, "data:"):
				payload := strings.TrimSpace(line[5:])
				if payload != "" {
					dataBuf = append(dataBuf, payload...)
				}
			case line == "":
				// 事件边界:dispatch
				if len(dataBuf) > 0 {
					s.dispatch(eventType, dataBuf, wDown, flusher, audit)
					dataBuf = dataBuf[:0]
					eventType = ""
					if s.resetIdle != nil {
						s.resetIdle()
					}
					if s.floodAbort {
						s.finalize(wDown, flusher, audit)
						s.finishAudit(audit)
						ctxCancel()
						return audit
					}
				}
			default:
				// comment /: ping 等原样忽略(出站 OAI 只发 data 行)
			}
		}
		if err != nil {
			// EOF 或异常:无 done 也必须补收尾(§11.3 结束保证)
			s.finalize(wDown, flusher, audit)
			s.finishAudit(audit)
			if err != io.EOF {
				audit.UpstreamErr = err.Error()
			}
			return audit
		}
		if s.finished {
			s.finishAudit(audit)
			return audit
		}
	}
}

// dispatch 事件分发(状态机核心)
func (s *streamSession) dispatch(eventType string, data []byte, w io.Writer, flusher http.Flusher, audit *StreamAudit) {
	switch eventType {

	case "conversation.response.started":
		// 首发 role chunk
		if !s.roleSent {
			s.roleSent = true
			s.sendChunk(w, flusher, &chunkFirst{role: true})
		}

	case "message.output.delta":
		var ev struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(data, &ev) != nil || len(ev.Content) == 0 {
			return
		}
		s.handleOutputDelta(ev.Content, w, flusher, audit)

	case "function.call.delta":
		s.handleFunctionCallDelta(data, w, flusher, audit)

	case "conversation.response.done":
		var ev struct {
			Usage *usageFromStream `json:"usage"`
		}
		if json.Unmarshal(data, &ev) == nil {
			s.usage = ev.Usage
		}
		s.doneReceived = true
		s.finalize(w, flusher, audit)

	case "conversation.response.error":
		// 上游流内错误:invisible 收流(观测日志,客户端拿正常 [DONE])
		var ev struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &ev)
		s.convErr = ev.Message
		if s.cfg.Logger != nil {
			s.cfg.Logger.Warn("upstream conversation.response.error event",
				"req_id", s.cfg.ReqID, "msg", ev.Message)
		}
		s.finalize(w, flusher, audit)

	default:
		// tool.execution.started/.delta/.done(内置工具)、agent.handoff.*:对客户端透明
	}
}

// handleOutputDelta message.output.delta:text 是裸字符串 / thinking 是 dict
func (s *streamSession) handleOutputDelta(content json.RawMessage, w io.Writer, f http.Flusher, audit *StreamAudit) {
	if content[0] == '"' {
		var text string
		if json.Unmarshal(content, &text) != nil || text == "" {
			return
		}
		// ① JSON 首块折叠
		emit := text
		if s.jsonFolder != nil {
			emit = s.jsonFolder.Feed(text)
			if emit == "" {
				return // 仍在缓冲
			}
			if s.jsonFolder.Folded() {
				audit.JSONFolded = true
			}
		}
		s.aggText.WriteString(emit)
		s.sendChunk(w, f, &chunkDelta{content: emit})
		return
	}
	// thinking 块:{"type":"thinking","thinking":[{"type":"text","text":"..."}]}
	var think struct {
		Type     string `json:"type"`
		Thinking []struct {
			Text string `json:"text"`
		} `json:"thinking"`
	}
	if json.Unmarshal(content, &think) != nil {
		return
	}
	for _, t := range think.Thinking {
		if t.Text == "" {
			continue
		}
		s.aggThinking.WriteString(t.Text)
		s.sendChunk(w, f, &chunkDelta{reasoning: t.Text})
	}
}

// handleFunctionCallDelta function.call.delta:每个 delta 带全量 name+tool_call_id
func (s *streamSession) handleFunctionCallDelta(data []byte, w io.Writer, f http.Flusher, audit *StreamAudit) {
	var ev struct {
		ToolCallID string `json:"tool_call_id"`
		Name       string `json:"name"`
		Arguments  string `json:"arguments"`
	}
	if json.Unmarshal(data, &ev) != nil || ev.ToolCallID == "" {
		return
	}

	idx, seen := s.toolCallIndex[ev.ToolCallID]
	if !seen {
		// ③ 洪水治理:首个 call 放行;再见同名 → 熔断断开上游
		if s.floodGuard != nil {
			if !s.floodGuard.ObserveCallStart(ev.Name) {
				s.floodAbort = true
				audit.FloodAborted = true
				if s.cfg.Logger != nil {
					s.cfg.Logger.Info("required flood aborted, upstream cut",
						"req_id", s.cfg.ReqID, "kept_calls", s.floodGuard.KeptCount())
				}
				return
			}
		}
		idx = s.nextToolIndex
		s.nextToolIndex++
		s.toolCallIndex[ev.ToolCallID] = idx
		tc := OaiToolCall{ID: ev.ToolCallID, Type: "function", Index: idx}
		tc.Function.Name = ev.Name
		s.toolCalls = append(s.toolCalls, &tc)
		// 首发 chunk 带 id+type+name
		s.sendChunk(w, f, &chunkDelta{toolCall: &chunkToolCall{
			Index: idx, ID: ev.ToolCallID, Type: "function",
			Name: ev.Name, ArgumentsDelta: "",
		}})
		if ev.Arguments == "" {
			return
		}
	}
	if ev.Arguments != "" {
		// 累加本仪表的聚合(usage 估算/非流式 assembly)
		if idx < len(s.toolCalls) {
			s.toolCalls[idx].Function.Arguments += ev.Arguments
		}
		s.sendChunk(w, f, &chunkDelta{toolCall: &chunkToolCall{
			Index: idx, ArgumentsDelta: ev.Arguments,
		}})
	}
}

// finalize 终止路径:补 finish chunk + usage + [DONE](只发一次)
func (s *streamSession) finalize(w io.Writer, f http.Flusher, audit *StreamAudit) {
	if s.finished {
		return
	}
	s.finished = true

	// 客户端非流式(桥强制流式路径):聚合 chunk 全部留在内存,此处组装标准整包回写
	if !s.cfg.ClientStream {
		s.writeNonStreamPackage(s.rawWriter, audit)
		return
	}

	// ① JSON 折叠器冲刷(若整个流都在缓冲内)
	if s.jsonFolder != nil && !audit.JSONFolded {
		if rest := s.jsonFolder.Flush(); rest != "" {
			s.aggText.WriteString(rest)
			s.sendChunk(w, f, &chunkDelta{content: rest})
			if s.jsonFolder.Folded() {
				audit.JSONFolded = true
			}
		}
	}

	finishReason := "stop"
	if len(s.toolCalls) > 0 {
		finishReason = "tool_calls"
	} else if s.cfg.MaxTokens > 0 && s.usage != nil && s.usage.CompletionTokens >= s.cfg.MaxTokens {
		finishReason = "length"
	}
	audit.FinishReason = finishReason

	// ② usage 兜底:done 缺失或 usage 全 0 → tokenizer
	usage := s.usage
	if usage == nil || (usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0) {
		outputText := s.aggText.String() + s.aggThinking.String()
		for _, tc := range s.toolCalls {
			outputText += tc.Function.Name + tc.Function.Arguments
		}
		if u, ok := repairUsage(s.cfg.InputText, outputText); ok {
			usage = &usageFromStream{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
			audit.UsageRepaired = true
			s.usage = usage // 回填:finishAudit 从 s.usage 拷贝 access 日志字段(否则修复值打出全 0)
		}
	}

	// finish chunk(不携带 usage于此;OAI 惯例 usage 在独立 chunk)
	finalChunk := map[string]any{
		"id":     "chatcmpl-stream",
		"object": "chat.completion.chunk",
		"model":  s.cfg.OaiModel,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finishReason,
		}},
	}
	s.sendRaw(w, f, finalChunk)

	// usage chunk(choices: [], usage 语义恒定开,§10.1 stream_options 兼容)
	if usage != nil {
		usageChunk := map[string]any{
			"id":      "chatcmpl-stream",
			"object":  "chat.completion.chunk",
			"model":   s.cfg.OaiModel,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens":     usage.PromptTokens,
				"completion_tokens": usage.CompletionTokens,
				"total_tokens":      usage.TotalTokens,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": 0, // 上游无缓存,如实回 0(D-34)
				},
			},
		}
		s.sendRaw(w, f, usageChunk)
	}
	// [DONE]
	if w != io.Discard {
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}
}

// finishAudit 记录 access 日志的最终字段
func (s *streamSession) finishAudit(a *StreamAudit) {
	a.ToolCallCount = len(s.toolCalls)
	a.UsagePromptTok, a.UsageCompletionTok, a.UsageTotalTok = 0, 0, 0
	if s.usage != nil {
		a.UsagePromptTok = s.usage.PromptTokens
		a.UsageCompletionTok = s.usage.CompletionTokens
		a.UsageTotalTok = s.usage.TotalTokens
	}
	// 空回判定:聚合文本与思考均为空且无工具调用 → 上游给了一全程空白响应
	if s.aggText.Len() == 0 && s.aggThinking.Len() == 0 && len(s.toolCalls) == 0 {
		a.EmptyContent = true
	}
	if s.convErr != "" {
		a.UpstreamErr = s.convErr
	}
}

// writeNonStreamPackage 非流式客户端的整包组装(forced-stream 折叠路径的出站点)。
// 聚合 texts/thinkings/toolCalls + usage(含②兜底),输出标准 OaiChatResponse。
func (s *streamSession) writeNonStreamPackage(w http.ResponseWriter, audit *StreamAudit) {
	// JSON 折叠器冲刷(若整个流还在缓冲内)
	if s.jsonFolder != nil {
		if rest := s.jsonFolder.Flush(); rest != "" {
			s.aggText.WriteString(rest)
			if s.jsonFolder.Folded() {
				audit.JSONFolded = true
			}
		}
	}

	finishReason := "stop"
	if len(s.toolCalls) > 0 {
		finishReason = "tool_calls"
	} else if s.cfg.MaxTokens > 0 && s.usage != nil && s.usage.CompletionTokens >= s.cfg.MaxTokens {
		finishReason = "length"
	}
	audit.FinishReason = finishReason

	// usage(含② tokenizer 兜底)
	usage := s.usage
	if usage == nil || (usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0) {
		outputText := s.aggText.String() + s.aggThinking.String()
		for _, tc := range s.toolCalls {
			outputText += tc.Function.Name + tc.Function.Arguments
		}
		if u, ok := repairUsage(s.cfg.InputText, outputText); ok {
			usage = &usageFromStream{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
			audit.UsageRepaired = true
		}
	}

	msg := OaiMessage{
		Role:             "assistant",
		ReasoningContent: s.aggThinking.String(),
		ToolCalls:        nil,
	}
	agg := &aggregated{
		texts:     []string{s.aggText.String()},
		toolCalls: nil,
	}
	if s.aggText.Len() > 0 {
		agg.texts = []string{s.aggText.String()}
	} else {
		agg.texts = nil
	}
	for _, tc := range s.toolCalls {
		if tc != nil {
			agg.toolCalls = append(agg.toolCalls, *tc)
		}
	}
	msg.Content = combinedTextPtr(agg)
	msg.ToolCalls = agg.toolCalls

	resp := &OaiChatResponse{
		ID:      "chatcmpl-forced-stream",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   s.cfg.OaiModel,
		Choices: []OaiChoice{{Index: 0, Message: msg, FinishReason: finishReason}},
	}
	if usage != nil {
		resp.Usage = &OaiUsage{
			PromptTokens:        usage.PromptTokens,
			CompletionTokens:    usage.CompletionTokens,
			TotalTokens:         usage.TotalTokens,
			PromptTokensDetails: &OaiPromptTokensDetails{CachedTokens: 0}, // 上游无缓存,如实回 0(D-34)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
	if audit.UsageRepaired {
		// 保持 UsagePromptTok 等字段在 finishAudit 里正确反映(usage 已修)
		s.usage = usage
	}
}

// ---------- chunk 构造 ----------

// chunkFirst 首发 role chunk
type chunkFirst struct{ role bool }

// chunkDelta 增量 chunk
type chunkDelta struct {
	content   string
	reasoning string
	toolCall  *chunkToolCall
}

type chunkToolCall struct {
	Index          int
	ID             string
	Type           string
	Name           string
	ArgumentsDelta string
}

// sendChunk 发送一条 OAI chunk(若客户端非流式= io.Discard,只聚合)
func (s *streamSession) sendChunk(w io.Writer, fl http.Flusher, c any) {
	var delta map[string]any
	switch v := c.(type) {
	case *chunkFirst:
		delta = map[string]any{"role": "assistant"}
	case *chunkDelta:
		delta = map[string]any{}
		if v.content != "" {
			delta["content"] = v.content
		}
		if v.reasoning != "" {
			delta["reasoning_content"] = v.reasoning
		}
		if v.toolCall != nil {
			tc := v.toolCall
			entry := map[string]any{"index": tc.Index}
			if tc.ID != "" {
				entry["id"] = tc.ID
				entry["type"] = tc.Type
			}
			fn := map[string]string{}
			if tc.Name != "" {
				fn["name"] = tc.Name
			}
			fn["arguments"] = tc.ArgumentsDelta
			entry["function"] = fn
			delta["tool_calls"] = []any{entry}
		}
		if len(delta) == 0 {
			return
		}
	default:
		return
	}
	chunk := map[string]any{
		"id":      "chatcmpl-stream",
		"object":  "chat.completion.chunk",
		"model":   s.cfg.OaiModel,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	}
	s.sendRaw(w, fl, chunk)
}

// sendRaw 原序列化发送(format: data: <json>\n\n + flush)
func (s *streamSession) sendRaw(w io.Writer, fl http.Flusher, v any) {
	if w == io.Discard {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	fl.Flush()
}

// StreamAudit access 日志流水字段
type StreamAudit struct {
	Model              string
	Stream             bool   // 客户端是否流式(桥内部强制不算)
	DownstreamAbort    bool   // 下游断连主动断开
	IdleCut            bool   // idle watchdog 切断
	UpstreamErr        string // 上游异常(非nil为空串)
	JSONFolded         bool   // 修复件①触发
	UsageRepaired      bool   // 修复件②触发
	FloodAborted       bool   // 修复件③触发(熔断)
	EmptyContent       bool   // 上游 200 但正文空(非 JSON 也应该 tracking;观测标记)
	ToolCallCount      int
	FinishReason       string
	UsagePromptTok     int
	UsageCompletionTok int
	UsageTotalTok      int
}

// 让 tokenizer import 在编译期被使用(usage 修复实际经由 repairUsage 调 Count)
var _ = tokenizer.Enabled
