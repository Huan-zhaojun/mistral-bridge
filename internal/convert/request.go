// OAI → Mistral conversations 请求转换(§10)。
// 分四档处理(§10.5):A 客户端错误→400、B 协议差静默修复、C 上游无对应静默忽略、D 直通。
package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"mistral-bridge/internal/mistral"
	"mistral-bridge/internal/oai"
)

// BridgeError 桥自身产生的错误(OAI 形状,code 恒 null)
type BridgeError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param"`
	Code    any    `json:"code"`
}

func (e *BridgeError) Error() string { return e.Message }

func newBridgeError(msg, param string) *BridgeError {
	return &BridgeError{Message: msg, Type: "invalid_request_error", Param: param, Code: nil}
}

// ConvertedRequest 转换产物(上游请求体 + 决策痕迹供日志/修复件使用)
type ConvertedRequest struct {
	Body              []byte   // POST body(JSON)
	Stream            bool     // 下游客户请求是否流式
	ForcedStream      bool     // required 洪水治理:桥内部强制上游流式
	Model             string   // 归一化后的模型 id(供响应回显)
	OriginalModel     string   // 客户端原始入参(若有别名)
	Decisions         []string // 静默修复记录(debug 日志)
	ResponseFormat    string   // response_format.type(判断 JSON 折叠用)
	ReasoningEffort   string   // high|none|""(原始直通,洪水治理路径必流式)
	InputTextForUsage string   // 转换后 inputs 的原文拼接(tokenizer 兜底计数用)
}

// ModelAlias 平台登记别名 → 规范 id
var modelAlias = map[string]string{
	"zai-glm-5-2": "glm-5-2",
	"glm-5-2":     "glm-5-2",
}

// pooledBuiltinTypes 内置工具类型优先级映射(搜索二选一时 premium 优先)
var builtinPriority = map[string]int{
	"web_search_premium": 3,
	"web_search":         2,
	"code_interpreter":   1,
	"image_generation":   1,
}

// builtinToolsDef 内置工具的 OAI 侧定义(直通形态,仅 type 字段)
func builtinToolDef(t string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"type": t})
	return b
}

// ConvertRequest 主转换入口:OAI ChatRequest → upstream ConversationRequest body
func ConvertRequest(req *oai.ChatRequest, cfgListen []string, passReasoning bool) (*ConvertedRequest, error) {
	out := &ConvertedRequest{}

	// ---- 1. 模型白名单(仅 glm-5-2,接受别名) ----
	norm, ok := modelAlias[strings.ToLower(strings.TrimSpace(req.Model))]
	if !ok {
		return nil, newBridgeError(fmt.Sprintf("unsupported model %q (only glm-5-2 available)", req.Model), "model")
	}
	out.Model = norm
	out.OriginalModel = req.Model

	// ---- 2. 参数映射(§10.1) ----
	args := &mistral.CompletionArgs{}

	// max_tokens / max_completion_tokens:取大者,缺省直通(不注入兜底)
	if req.MaxTokens != nil && req.MaxCompletionTok != nil {
		if *req.MaxCompletionTok > *req.MaxTokens {
			args.MaxTokens = req.MaxCompletionTok
		} else {
			args.MaxTokens = req.MaxTokens
		}
	} else if req.MaxTokens != nil {
		args.MaxTokens = req.MaxTokens
	} else if req.MaxCompletionTok != nil {
		args.MaxTokens = req.MaxCompletionTok
	}

	// 直通族
	args.Temperature = req.Temperature // >1 上游 422 → 规范化 400(透传错误,不 clamp)
	args.TopP = req.TopP
	args.Stop = req.Stop
	args.FrequencyPenalty = req.FrequencyPenalty
	args.PresencePenalty = req.PresencePenalty
	args.ResponseFormat = req.ResponseFormat
	args.Prediction = req.Prediction
	if req.Seed != nil {
		rs := *req.Seed
		args.RandomSeed = &rs
		out.Decisions = append(out.Decisions, "seed->random_seed")
	}

	// reasoning_effort:非 none 档一律映射 high(上游二值化)
	if len(req.ReasoningEffort) > 0 && string(req.ReasoningEffort) != `""` {
		var s string
		if err := json.Unmarshal(req.ReasoningEffort, &s); err == nil && s != "" {
			if s == "none" {
				args.ReasoningEffort = "none"
				out.ReasoningEffort = "none"
			} else {
				// minimal/low/medium/high/xhigh → high
				args.ReasoningEffort = "high"
				out.ReasoningEffort = "high"
				if s != "high" {
					out.Decisions = append(out.Decisions, "reasoning_effort "+s+"->high")
				}
			}
		} else if err == nil && s == "" {
			// 空字符串视作未传
		} else {
			return nil, newBridgeError("reasoning_effort must be a string", "reasoning_effort")
		}
	}

	// n>1 报错
	if req.N > 1 {
		return nil, newBridgeError(fmt.Sprintf("n=%d is not supported by this upstream", req.N), "n")
	}
	// 不支持族(JQ 双键探测存在即 400)
	for _, p := range []struct {
		name string
		hit  bool
	}{
		{"logprobs", req.Logprobs != nil},
		{"top_logprobs", req.TopLogprobs != nil},
		{"logit_bias", len(req.LogitBias) > 0},
		{"modalities", len(req.Modalities) > 0},
		{"audio", len(req.Audio) > 0},
	} {
		if p.hit {
			return nil, newBridgeError("parameter "+p.name+" is not supported by glm-5-2 on this upstream", p.name)
		}
	}
	// 静默忽略:parallel_tool_calls / user / prompt_cache_key 等在 Unmarshal 时已解析但不映射——Nothing to do.

	// ---- 3. tool_choice 治理(§10.1/§10.5-B) ----
	toolChoice, err := normalizeToolChoice(req.ToolChoice, req.Tools, out)
	if err != nil {
		return nil, err
	}
	args.ToolChoice = toolChoice

	// ---- 4. tools 处理:客户端 function 直通 + 内置工具合并去重(§10.6) ----
	tools, toolDecisions, effectiveBuiltin := mergeTools(req.Tools, cfgListen)
	out.Decisions = append(out.Decisions, toolDecisions...)

	// required 遇内置工具降级 auto(§10.6 规则 2)
	if args.ToolChoice == "required" && len(effectiveBuiltin) > 0 {
		args.ToolChoice = "auto"
		out.Decisions = append(out.Decisions, "tool_choice required->auto (builtin tools present)")
	}
	// 记录 required 洪水治理触发状态(内部强制上游流式由 handler 层决定)
	if toolChoice == "required" {
		out.ForcedStream = true
	}
	// response_format 量化(JSON folding 决策用)
	if len(req.ResponseFormat) > 0 {
		var rf struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(req.ResponseFormat, &rf); err == nil {
			out.ResponseFormat = rf.Type
		}
	}

	// ---- 5. messages → inputs / instructions(§10.2) ----
	inputs, instructions, sideEffects, err := convertMessages(req.Messages, passReasoning, out)
	if err != nil {
		return nil, err
	}
	out.Decisions = append(out.Decisions, sideEffects...)

	// 估算 prompt 文本(usage 兜底 tokenizer 输入):instructions + inputs 全文拼接
	out.InputTextForUsage = usageTextEstimate(instructions, inputs)

	// ---- 6. 组装上游 body ----
	upstream := &mistral.ConversationRequest{
		Inputs:       inputs,
		Stream:       req.Stream || out.ForcedStream, // 桥强制流式时上游流式
		Store:        false,
		Model:        out.Model,
		Instructions: instructions,
		Tools:        tools,
	}
	if !isZeroCompletionArgs(args) {
		upstream.CompletionArgs = args
	}

	body, err := json.Marshal(upstream)
	if err != nil {
		return nil, fmt.Errorf("marshal upstream request: %w", err)
	}
	out.Body = body
	out.Stream = req.Stream
	return out, nil
}

// normalizeToolChoice 处理 tool_choice 的映射规则
func normalizeToolChoice(raw json.RawMessage, tools []oai.Tool, out *ConvertedRequest) (string, error) {
	if len(raw) == 0 {
		return "", nil // 缺省:auto
	}
	// 字串形态:auto|none|required|any
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return s, nil
		case "any":
			// any → auto(绕开 required 洪水路径;any 非 OAI 参数,实践中几乎无客户端用)
			out.Decisions = append(out.Decisions, "tool_choice any->auto")
			return "auto", nil
		default:
			return "", newBridgeError(fmt.Sprintf("tool_choice %q is not supported by upstream; use auto|none|required", s), "tool_choice")
		}
	}
	// 对象形态:{"type":"function","function":{"name":"X"}}
	var obj oai.ToolChoiceObject
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Type != "function" || obj.Function == nil {
		return "", newBridgeError("invalid tool_choice value", "tool_choice")
	}
	name := obj.Function.Name
	// tools 中仅该函数一个 → required(等价);多函数 → 400
	if len(tools) == 1 {
		var fn struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(tools[0].Function, &fn); err == nil && fn.Name == name {
			out.Decisions = append(out.Decisions, "tool_choice object->required (single function)")
			return "required", nil
		}
	}
	return "", newBridgeError(
		"tool_choice object form (force specific function) is not supported by this upstream; only single-function toolsets can be force-called",
		"tool_choice")
}

// mergeTools function 直通 + 内置工具(客户端携带优先,config 默认注入去重)
// 返回 (最终 tools, 决策痕迹, 生效内置工具集)
func mergeTools(clientTools []oai.Tool, cfgBuiltin []string) ([]json.RawMessage, []string, []string) {
	var decisions []string
	if len(clientTools) == 0 && len(cfgBuiltin) == 0 {
		return nil, nil, nil
	}
	var out []json.RawMessage
	present := map[string]bool{}

	// 客户端 function 直通
	for _, t := range clientTools {
		switch t.Type {
		case "function":
			b, _ := json.Marshal(map[string]any{
				"type":     "function",
				"function": t.Function,
			})
			out = append(out, b)
		case "web_search", "web_search_premium", "code_interpreter", "image_generation":
			if present[t.Type] {
				continue
			}
			out = append(out, builtinToolDef(t.Type))
			present[t.Type] = true
		default:
			// 未知 type 直通(上游 422 时归一化回客户端)
			b, _ := json.Marshal(map[string]string{"type": t.Type})
			out = append(out, b)
		}
	}
	// config 注入:不与客户端携带的重复
	for _, t := range cfgBuiltin {
		if present[t] {
			continue
		}
		out = append(out, builtinToolDef(t))
		present[t] = true
		decisions = append(decisions, "builtin_tool injected: "+t)
	}

	// 搜索互斥:web_search 与 web_search_premium 只留 premium(§10.6 铁则 1)
	if present["web_search"] && present["web_search_premium"] {
		filtered := out[:0]
		for _, raw := range out {
			var probe struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(raw, &probe)
			if probe.Type == "web_search" {
				decisions = append(decisions, "builtin_tool dropped: web_search (premium takes precedence)")
				continue
			}
			filtered = append(filtered, raw)
		}
		out = filtered
		delete(present, "web_search")
	}

	// 生效内置集(按 priority 排序输出,稳定日志)
	var eff []string
	for t := range present {
		eff = append(eff, t)
	}
	for i := 0; i < len(eff); i++ {
		for j := i + 1; j < len(eff); j++ {
			if builtinPriority[eff[i]] < builtinPriority[eff[j]] {
				eff[i], eff[j] = eff[j], eff[i]
			}
		}
	}
	return out, decisions, eff
}

// convertMessages 核心算法(§10.2)
func convertMessages(msgs []oai.Message, passReasoning bool, out *ConvertedRequest) ([]json.RawMessage, string, []string, error) {
	var inputs []json.RawMessage
	var sysParts []string
	var sideEffects []string

	// zip 配对工具结果:tool_call_id → function.result 顺序映射表
	// 注意:OAI 允许松散排列,桥必须按 id 严格 zip
	toolResults := map[string]string{} // tool_call_id → result text(最后一次覆盖,防御)
	pendingCallIDs := []string{}       // 按出现顺序记录 assistant 发起的 tool_call id

	flushResultFor := func(id string) {
		// 不在这里 flush:flush 时机 = 当遇到该 id 对应的 tool message
		_ = id
	}
	_ = flushResultFor

	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {

		case "system", "developer":
			// 拼入 instructions:"↵↵" 分隔;内容清洗(图像块 → [image omitted] 文本)
			text, _, hits := messageTextContent(m, "system")
			if hits > 0 {
				sideEffects = append(sideEffects, fmt.Sprintf("media_cleaned:%d (system)", hits))
			}
			if text != "" {
				sysParts = append(sysParts, text)
			}

		case "user":
			// message.input:user content → string 直通,块数组中 image_url 等富媒体清洗
			content, _, hits := messageContentForEntry(m, "user")
			if hits > 0 {
				sideEffects = append(sideEffects, fmt.Sprintf("media_cleaned:%d (user)", hits))
			}
			inputs = append(inputs, mistral.MessageInput("user", content))

		case "assistant":
			// message.output:thinking 块(若 reasoning_content)+ text 块 + 任何 tool_calls→function.call
			contentRaw, refusalMerged := assistantContent(m, passReasoning)
			if refusalMerged {
				sideEffects = append(sideEffects, "refusal merged into content")
			}
			// 纯 tool_calls 且 content null → 无 thinking/text,只发 function.call
			if len(contentRaw) > 0 {
				inputs = append(inputs, mistral.MessageOutput("assistant", contentRaw))
			}
			// 记录这些 call id,期望后续 tool message 按序 zip
			for _, tc := range m.ToolCalls {
				if tc.Type != "function" {
					continue
				}
				inputs = append(inputs, mistral.FunctionCallEntry(tc.ID, tc.Function.Name, tc.Function.Arguments))
				pendingCallIDs = append(pendingCallIDs, tc.ID)
			}
			// legacy 2023 风格:assistant.function_call 字段 → 单个 function.call
			if m.FunctionCall != nil && len(m.ToolCalls) == 0 {
				var fc struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}
				if err := json.Unmarshal(*m.FunctionCall, &fc); err == nil && fc.Name != "" {
					// legacy 无 id,上游一定会生成 new id;桥的 zip 无法匹配 → 该路径罕见且超长历史
					// 策略:透传;id 置空串,上游宽容接受(§5.3:无硬校验)
					inputs = append(inputs, mistral.FunctionCallEntry("", fc.Name, fc.Arguments))
					sideEffects = append(sideEffects, "legacy function_call mapped")
				}
			}

		case "tool":
			// function.result:必须按 tool_call_id zip(id 上游可能唯一性松散,但桥严格配对)
			id := m.ToolCallID
			resultText, hits := toolResultText(m)
			if hits > 0 {
				sideEffects = append(sideEffects, fmt.Sprintf("media_cleaned:%d (tool)", hits))
			}
			toolResults[id] = resultText
			inputs = append(inputs, mistral.FunctionResultEntry(id, resultText))
			// zip 清理:从 pending 移除
			for i, pid := range pendingCallIDs {
				if pid == id {
					pendingCallIDs = append(pendingCallIDs[:i], pendingCallIDs[i+1:]...)
					break
				}
			}

		case "function":
			// legacy role:function 消息 → function.result
			name := m.Name
			text, _ := json.Marshal(m.Content)
			var s string
			_ = json.Unmarshal(text, &s)
			inputs = append(inputs, mistral.FunctionResultEntry(name, s))
			sideEffects = append(sideEffects, "legacy role:function mapped to function.result")

		default:
			return nil, "", nil, newBridgeError(
				fmt.Sprintf("unsupported message role %q (allowed: system/developer/user/assistant/tool/function)", m.Role), "messages")
		}
	}

	instructions := strings.Join(sysParts, "\n\n")
	return inputs, instructions, sideEffects, nil
}

// messageTextContent system/developer 消息文本化(块数组清洗后拼 text)
// 返回 (text 内容, 是否使用过, 富媒体清洗计数)
func messageTextContent(m oai.Message, ctxTag string) (string, bool, int) {
	_ = ctxTag
	parsed, used, cleans := parseContent(m.Content)
	if parsed.isString {
		var s string
		_ = json.Unmarshal(json.RawMessage(parsed.rawString), &s)
		return s, used, cleans
	}
	return joinStrings(parsed.texts, "\n\n"), used, cleans
}

// messageContentForEntry user 消息 → 上游 content(string 或清洗后 text 块数组)
func messageContentForEntry(m oai.Message, ctxTag string) (json.RawMessage, bool, int) {
	_ = ctxTag
	txt, usedBlocks, cleans := parseContent(m.Content)
	_ = usedBlocks
	if txt.isString {
		return json.RawMessage(txt.rawString), false, cleans
	}
	// 块数组:以 text 块数组形式重组
	var arr []map[string]string
	for _, t := range txt.texts {
		arr = append(arr, map[string]string{"type": "text", "text": t})
	}
	if len(arr) == 0 {
		arr = append(arr, map[string]string{"type": "text", "text": "[image omitted]"})
	}
	b, _ := json.Marshal(arr)
	return b, true, cleans
}

// parsedContent parseContent 的返回结构
type parsedContent struct {
	isString  bool
	rawString string   // JSON 字符串原文(含引号)
	texts     []string // 各 text 段(数组形态)
}

// parseContent content 字段统一解析(§10.2 全矩阵);返回 (parsed, 是否使用数组, 清洗计数)
func parseContent(raw json.RawMessage) (parsedContent, bool, int) {
	var out parsedContent
	if len(raw) == 0 || string(raw) == "null" {
		return out, false, 0
	}
	// 字符串直通
	if raw[0] == '"' {
		out.isString = true
		out.rawString = string(raw)
		return out, false, 0
	}
	// 块数组
	var parts []oai.Part
	if err := json.Unmarshal(raw, &parts); err != nil {
		// 防御:无法解析按字符串处理
		out.isString = true
		out.rawString = `""`
		return out, false, 0
	}
	cleans := 0
	for _, p := range parts {
		switch p.Type {
		case "text", "output_text":
			if p.Text != "" {
				out.texts = append(out.texts, p.Text)
			}
		case "refusal":
			if p.Refusal != "" {
				out.texts = append(out.texts, p.Refusal)
				cleans++
			}
		case "image_url", "input_image":
			out.texts = append(out.texts, "[image omitted]")
			cleans++
		case "input_audio":
			out.texts = append(out.texts, "[audio omitted]")
			cleans++
		case "input_file", "file", "document_url":
			out.texts = append(out.texts, "[file omitted]")
			cleans++
		default:
			// 未知块:保守丢弃(避免上游 3051 截断),计数清洗
			cleans++
		}
	}
	// 数组为空或清洗后无 text → 补占位防止上游 422
	if len(out.texts) == 0 {
		out.texts = append(out.texts, "[image omitted]")
		cleans++ // 占位行为计入清洗(空消息退化)
	}
	return out, true, cleans
}

// assistantContent assistant 消息 → message.output 的 content 块数组
// (含 thinking、refusal 并入、public image 清洗)
func assistantContent(m oai.Message, passReasoning bool) (json.RawMessage, bool) {
	var blocks []map[string]any

	// thinking 块:reasoning_content → thinking(仅当 passReasoning 开)
	if passReasoning && m.ReasoningContent != "" {
		thinkInner, _ := json.Marshal([]map[string]string{{"type": "text", "text": m.ReasoningContent}})
		blocks = append(blocks, map[string]any{
			"type":     "thinking",
			"thinking": json.RawMessage(thinkInner),
		})
	}

	// content:字符串 or 块数组(带 image 清洗)
	if len(m.Content) > 0 && string(m.Content) != "null" {
		parsed, _, _ := parseContent(m.Content)
		if parsed.isString {
			// 字符串 → text 块
			var s string
			_ = json.Unmarshal(json.RawMessage(parsed.rawString), &s)
			if s != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": s})
			}
		} else {
			for _, t := range parsed.texts {
				blocks = append(blocks, map[string]any{"type": "text", "text": t})
			}
		}
	}

	// 纯 tool_calls(content 为 null 且无 thinking) → 空 blocks,不发 message.output
	if len(blocks) == 0 {
		return nil, false
	}
	b, _ := json.Marshal(blocks)
	return b, false
}

// toolResultText tool 消息 content → function.result 的字符串
// (content 是数组时拼接各 text + 图位插占位字符串;清洗计数返回)
func toolResultText(m oai.Message) (string, int) {
	if len(m.Content) == 0 || string(m.Content) == "null" {
		return "", 0
	}
	if m.Content[0] == '"' {
		var s string
		_ = json.Unmarshal(m.Content, &s)
		return s, 0
	}
	parsed, _, cleans := parseContent(m.Content)
	// 数组聚合:各 text 间以换行连接
	return joinStrings(parsed.texts, "\n"), cleans
}

// joinStrings 简单字符串拼接
func joinStrings(parts []string, sep string) string {
	var b strings.Builder
	for i, s := range parts {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(s)
	}
	return b.String()
}

// isZeroCompletionArgs 判断采样参数是否全空(空则不发送 completion_args)
func isZeroCompletionArgs(a *mistral.CompletionArgs) bool {
	return a.Temperature == nil && a.MaxTokens == nil && a.TopP == nil &&
		a.ReasoningEffort == "" && len(a.Stop) == 0 && a.RandomSeed == nil &&
		a.FrequencyPenalty == nil && a.PresencePenalty == nil &&
		len(a.ResponseFormat) == 0 && a.ToolChoice == "" && len(a.Prediction) == 0
}

// usageTextEstimate 将 instructions 与 inputs 的可见文本拼接,供 tokenizer 计数。
// (不是协议重构——粗粒度近似即可,±1 对齐由差分法抵消)
func usageTextEstimate(instructions string, inputs []json.RawMessage) string {
	var b strings.Builder
	b.WriteString(instructions)
	for _, raw := range inputs {
		var entry struct {
			Content   json.RawMessage `json:"content"`
			Result    string          `json:"result"`
			Arguments string          `json:"arguments"`
		}
		_ = json.Unmarshal(raw, &entry)
		if len(entry.Content) > 0 {
			var s string
			if entry.Content[0] == '"' {
				_ = json.Unmarshal(entry.Content, &s)
				b.WriteString(s)
			} else {
				var arr []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				_ = json.Unmarshal(entry.Content, &arr)
				for _, it := range arr {
					b.WriteString(it.Text)
				}
			}
		}
		if entry.Result != "" {
			b.WriteString(entry.Result)
		}
		if entry.Arguments != "" {
			b.WriteString(entry.Arguments)
		}
	}
	return b.String()
}

// ---- 工具函数:JSON 简易探测 ----

// containsJSONKey RawMessage 快捷判断是否在对象中
func containsJSONKey(obj json.RawMessage, key string) bool {
	if len(obj) == 0 || obj[0] != '{' {
		return false
	}
	var m map[string]json.RawMessage
	return json.Unmarshal(obj, &m) == nil && m[key] != nil
}

var _ = containsJSONKey
var _ = bytes.MinRead
