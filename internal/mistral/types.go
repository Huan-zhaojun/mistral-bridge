// Package mistral /v1/conversations 协议类型(请求侧白名单 13 字段、
// 响应侧 conversation.response / SSE 10 事件)。与研究文档 §5 对齐。
package mistral

import "encoding/json"

// ConversationRequest /v1/conversations 请求体(白名单字段)
type ConversationRequest struct {
	Inputs         []json.RawMessage `json:"inputs"` // 各类 entry(message.input/message.output/function.result)
	Stream         bool              `json:"stream"`
	Store          bool              `json:"store"` // 固定 false(无状态策略)
	Model          string            `json:"model"`
	Instructions   string            `json:"instructions,omitempty"`
	Tools          []json.RawMessage `json:"tools,omitempty"`
	CompletionArgs *CompletionArgs   `json:"completion_args,omitempty"`
	Name           string            `json:"name,omitempty"`
}

// CompletionArgs 采样参数白名单(11 字段)
type CompletionArgs struct {
	Temperature      *float64        `json:"temperature,omitempty"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	ReasoningEffort  string          `json:"reasoning_effort,omitempty"` // none|high(服务端收窄为二值)
	Stop             json.RawMessage `json:"stop,omitempty"`
	RandomSeed       *int64          `json:"random_seed,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	ResponseFormat   json.RawMessage `json:"response_format,omitempty"`
	ToolChoice       string          `json:"tool_choice,omitempty"` // auto|none|required
	Prediction       json.RawMessage `json:"prediction,omitempty"`
}

// 输入条目构造助手(三种 entry 形态)
func MessageInput(role string, content json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"object": "entry", "type": "message.input", "role": role,
		"content": content, "prefix": false,
	})
	return b
}

// MessageOutput assistant 历史条目。
// 注:API 直连端点的校验器把 prefix 视为 extra_forbidden(bora/Playground 端点宽容接受)。
// 两态兼容 = 省略 prefix 字段(默认 false,不传即是)——外加含 thinking 块的结构不受影响。
func MessageOutput(role string, content json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"object": "entry", "type": "message.output", "role": role,
		"content": content,
	})
	return b
}
func FunctionCallEntry(toolCallID, name, arguments string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"object": "entry", "type": "function.call",
		"tool_call_id": toolCallID, "name": name, "arguments": arguments,
	})
	return b
}
func FunctionResultEntry(toolCallID, result string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"object": "entry", "type": "function.result",
		"tool_call_id": toolCallID, "result": result,
	})
	return b
}
func FunctionCallWithConfirmation(toolCallID, name, arguments string, confirmation any) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"object": "entry", "type": "function.call",
		"tool_call_id": toolCallID, "name": name, "arguments": arguments,
		"confirmation_status": confirmation,
	})
	return b
}

// ContentBlock assistant message.output 的 content 块
type ContentBlock struct {
	Type     string          `json:"type"` // thinking | text
	Text     string          `json:"text,omitempty"`
	Thinking json.RawMessage `json:"thinking,omitempty"`
}

// ConversationResponse 非流式响应
type ConversationResponse struct {
	Object         string        `json:"object"`
	ConversationID string        `json:"conversation_id"`
	Outputs        []OutputEntry `json:"outputs"`
	Usage          *Usage        `json:"usage"`
	CreatedAt      string        `json:"created_at"`
}

// OutputEntry 响应里的条目(message.output / function.call / tool.execution)
type OutputEntry struct {
	Object     string          `json:"object"`
	Type       string          `json:"type"` // message.output|function.call|tool.execution
	Role       string          `json:"role,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"` // string 或 []ContentBlock
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Arguments  string          `json:"arguments,omitempty"`
	CreatedAt  string          `json:"created_at,omitempty"`
}

// Usage 上游 token 统计(无 reasoning 拆分、无 cached 字段;connectors 单独字典)
type Usage struct {
	PromptTokens     int            `json:"prompt_tokens"`
	CompletionTokens int            `json:"completion_tokens"`
	TotalTokens      int            `json:"total_tokens"`
	Connectors       map[string]int `json:"connectors,omitempty"`
}
