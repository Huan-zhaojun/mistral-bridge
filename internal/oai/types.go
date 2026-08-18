// Package oai OpenAI Chat Completions 协议类型(请求侧解析需要完整字段面,
// 响应侧仅需要桥会产出的子集)。解析策略:宽容 decode + 显式分档处理
// (不支持→400、协议差静默修复、合法但无对应→忽略、直通)。
package oai

import (
	"encoding/json"
	"fmt"
)

// ChatRequest OAI /v1/chat/completions 请求体(只声明我们关心的字段;
// 未知字段不应静默吞——用 DisallowUnknownFields 前的宽解析先取 RawMsg)
type ChatRequest struct {
	Model            string          `json:"model"`
	Messages         []Message       `json:"messages"`
	Stream           bool            `json:"stream"`
	MaxTokens        *int            `json:"max_tokens"`
	MaxCompletionTok *int            `json:"max_completion_tokens"`
	Temperature      *float64        `json:"temperature"`
	TopP             *float64        `json:"top_p"`
	Stop             json.RawMessage `json:"stop"` // string 或 []string,直通
	Seed             *int64          `json:"seed"`
	FrequencyPenalty *float64        `json:"frequency_penalty"`
	PresencePenalty  *float64        `json:"presence_penalty"`
	ResponseFormat   json.RawMessage `json:"response_format"` // text|json_object|json_schema 对象直通
	Tools            []Tool          `json:"tools"`
	ToolChoice       json.RawMessage `json:"tool_choice"` // "auto"|"none"|"required"|"any"|{"type":"function",...}
	Prediction       json.RawMessage `json:"prediction"`  // {"type":"content","content":...} 直通
	ReasoningEffort  json.RawMessage `json:"reasoning_effort"`
	StreamOptions    *StreamOptions  `json:"stream_options"`

	// 不支持参数(检测到即报错):类型无关紧要,存在即 400
	N              int             `json:"n"`
	Logprobs       *bool           `json:"logprobs"`
	TopLogprobs    *int            `json:"top_logprobs"`
	LogitBias      json.RawMessage `json:"logit_bias"`
	Modalities     json.RawMessage `json:"modalities"`
	Audio          json.RawMessage `json:"audio"`
	UnusedPresence map[string]bool `json:"-"` // 在自定义解析里填充已见到的 key
	rawKeys        map[string]bool
}

// StreamOptions 流式选项(桥按 include_usage=true 语义恒定处理)
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// 已知顶层字段白名单(用于区分"客户端错误 400"vs"静默忽略")
var knownKeys = []string{
	"model", "messages", "stream", "max_tokens", "max_completion_tokens",
	"temperature", "top_p", "stop", "seed", "frequency_penalty", "presence_penalty",
	"response_format", "tools", "tool_choice", "prediction", "reasoning_effort",
	"stream_options",
	// 检测到即 400 的
	"n", "logprobs", "top_logprobs", "logit_bias", "modalities", "audio",
	// 静默忽略的(OAI 合法但上游无对应)
	"parallel_tool_calls", "user", "prompt_cache_key", "store", "service_tier",
	"metadata", "safety_identifier", "verbosity", "max_iterations",
}

// ParseChatRequest 宽容解析:先收 keys,再 decode 已知字段
func ParseChatRequest(body []byte) (*ChatRequest, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	req := &ChatRequest{rawKeys: map[string]bool{}}
	for k := range raw {
		req.rawKeys[k] = true
	}
	if err := json.Unmarshal(body, req); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	return req, nil
}

// UnknownKeys 返回 body 里出现、但白名单之外的字段名(供严格校验场景)
func (r *ChatRequest) UnknownKeys() []string {
	known := map[string]bool{}
	for _, k := range knownKeys {
		known[k] = true
	}
	var out []string
	for k := range r.rawKeys {
		if !known[k] {
			out = append(out, k)
		}
	}
	return out
}

// ---------- 消息 ----------

// Message OAI 消息(content 可为字符串或块数组)
type Message struct {
	Role             string           `json:"role"`
	Content          json.RawMessage  `json:"content"` // string 或 []Part,null 也合法(纯 tool_calls 的 assistant)
	Name             string           `json:"name,omitempty"`
	ToolCalls        []ToolCall       `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"` // 客户端回传的思考(回传时用)
	FunctionCall     *json.RawMessage `json:"function_call,omitempty"`     // legacy 2023 风格
}

// Part 消息内容块
type Part struct {
	Type       string          `json:"type"` // text|image_url|input_audio|file|input_image|input_file|output_text|refusal
	Text       string          `json:"text,omitempty"`
	ImageURL   json.RawMessage `json:"image_url,omitempty"`
	InputAudio json.RawMessage `json:"input_audio,omitempty"`
	InputFile  json.RawMessage `json:"input_file,omitempty"`
	File       json.RawMessage `json:"file,omitempty"`
	Refusal    string          `json:"refusal,omitempty"`
}

// ToolCall assistant 侧 tool 调用
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // function
	Index    int    `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Tool tools 数组条目(function 或内置)
type Tool struct {
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function,omitempty"` // {"name","description","parameters"}
}

// ToolChoiceObject tool_choice 对象形态
type ToolChoiceObject struct {
	Type     string `json:"type"`
	Function *struct {
		Name string `json:"name"`
	} `json:"function,omitempty"`
}
