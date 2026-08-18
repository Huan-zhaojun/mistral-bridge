// Mistral → OAI 非流式响应映射(§11.1)+ finish_reason 合成(§11.2)。
// 要点:content 双态(字符串/块数组)、多阶段 outputs 顺序合并、
// usage 无 cached 字段、model 回显入参。
package convert

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mistral-bridge/internal/mistral"
	"mistral-bridge/internal/tokenizer"
)

// OaiChatResponse 非流式响应
type OaiChatResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"` // chat.completion
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []OaiChoice `json:"choices"`
	Usage   *OaiUsage   `json:"usage,omitempty"`
}

type OaiChoice struct {
	Index        int        `json:"index"`
	Message      OaiMessage `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type OaiMessage struct {
	Role             string        `json:"role"` // assistant
	Content          *string       `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []OaiToolCall `json:"tool_calls,omitempty"`
}

type OaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // function
	Index    int    `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type OaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// 无 cached_tokens(上游计费体系无此字段,省略即合法;OAI 规范允许 absence)
}

// aggregated 非流式聚合形态(支撑 finish_reason/usage 修复判断)
type aggregated struct {
	texts     []string
	thinkings []string
	toolCalls []OaiToolCall
}

// RepairInfo 转换过程的修复件命中信息(供 handler access 日志)
type RepairInfo struct {
	UsageRepaired bool // usage 走了 tokenizer 兜底
	EmptyContent  bool // 上游 200 但正文空(guided/搜索+high 偶发,观测用)
}

// ConvertResponse 非流式路径:上游 ConversationResponse body → OAI body。
// usage 全 0 时走 tokenizer 兜底(±1 精确,非猜测);model 回显客户端入参。
// maxTokensArg:客户端 max_tokens(供 finish_reason length 判断;未传 = 0)
func ConvertResponse(upstreamBody []byte, model string, inputText string, maxTokensArg int) ([]byte, *RepairInfo, error) {
	var resp mistral.ConversationResponse
	if err := json.Unmarshal(upstreamBody, &resp); err != nil {
		return nil, nil, fmt.Errorf("decode upstream response: %w", err)
	}

	agg := &aggregated{}
	for _, out := range resp.Outputs {
		switch out.Type {
		case "message.output":
			mergeOutputContent(agg, out.Content)
		case "function.call":
			tc := OaiToolCall{ID: out.ToolCallID, Type: "function", Index: len(agg.toolCalls)}
			tc.Function.Name = out.Name
			tc.Function.Arguments = out.Arguments
			agg.toolCalls = append(agg.toolCalls, tc)
		}
		// tool.execution(内置工具服务端执行)对客户端透明,不进 tool_calls
	}

	finishReason := synthesizeFinishReason(agg, resp.Usage, maxTokensArg)

	// usage 全 0 修复
	usage := resp.Usage
	repaired := false
	if usage == nil || (usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.TotalTokens == 0) {
		if u, ok := repairUsage(inputText, aggregateOutputText(agg)); ok {
			usage = &u
			repaired = true
		}
	}

	res := &OaiChatResponse{
		ID:      normalizeID(resp.ConversationID),
		Object:  "chat.completion",
		Created: parseCreatedAtToUnix(resp.CreatedAt),
		Model:   model,
		Choices: []OaiChoice{{
			Index: 0,
			Message: OaiMessage{
				Role:             "assistant",
				Content:          combinedTextPtr(agg),
				ReasoningContent: combinedThinking(agg),
				ToolCalls:        agg.toolCalls,
			},
			FinishReason: finishReason,
		}},
	}
	if usage != nil {
		res.Usage = &OaiUsage{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
		}
	}
	out, err := json.Marshal(res)
	if err != nil {
		return nil, nil, err
	}
	empty := len(agg.texts) == 0 && len(agg.toolCalls) == 0
	return out, &RepairInfo{UsageRepaired: repaired, EmptyContent: empty}, nil
}

// mergeOutputContent message.output 条目内容(text/thinking 双态 + 多阶段合并)
func mergeOutputContent(agg *aggregated, content json.RawMessage) {
	if len(content) == 0 {
		return
	}
	if content[0] == '"' {
		var s string
		if json.Unmarshal(content, &s) == nil && s != "" {
			agg.texts = append(agg.texts, s)
		}
		return
	}
	var blocks []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking json.RawMessage `json:"thinking"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				agg.texts = append(agg.texts, b.Text)
			}
		case "thinking":
			var thinkArr []struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(b.Thinking, &thinkArr) == nil {
				for _, t := range thinkArr {
					if t.Text != "" {
						agg.thinkings = append(agg.thinkings, t.Text)
					}
				}
			}
		}
	}
}

// synthesizeFinishReason 合成停止原因(§11.2 决策树):
//
//	if outputs 末尾含 function.call                              -> "tool_calls"
//	elif max_tokens 显式传入 且 completion_tokens >= max_tokens -> "length"
//	else                                                          -> "stop"
//
// 已知盲点:max_tokens=N 时模型恰好 N token 自然收尾会误判 length;
// stop 序列命中无法识别 → 归 stop(均近似安全,与下游区分需求一致)。
// 注:usage 归 0 是独立 flaky bug,不再作为截断信号。
func synthesizeFinishReason(agg *aggregated, usage *mistral.Usage, maxTokensArg int) string {
	if len(agg.toolCalls) > 0 {
		return "tool_calls"
	}
	if maxTokensArg > 0 && usage != nil && usage.CompletionTokens >= maxTokensArg {
		return "length"
	}
	return "stop"
}

// aggregateOutputText 聚合输出文本(tokenizer 兜底计数用)
func aggregateOutputText(agg *aggregated) string {
	var b strings.Builder
	for _, s := range agg.thinkings {
		b.WriteString(s)
	}
	for _, s := range agg.texts {
		b.WriteString(s)
	}
	for _, tc := range agg.toolCalls {
		b.WriteString(tc.Function.Name)
		b.WriteString(tc.Function.Arguments)
	}
	return b.String()
}

// repairUsage usage 全 0 时用 tokenizer 精确兜底计数
func repairUsage(inputText, outputText string) (mistral.Usage, bool) {
	pt, err1 := tokenizer.Count(inputText)
	ct, err2 := tokenizer.Count(outputText)
	if err1 != nil || err2 != nil {
		return mistral.Usage{}, false
	}
	return mistral.Usage{PromptTokens: pt, CompletionTokens: ct, TotalTokens: pt + ct}, true
}

// combinedTextPtr 拼接 text 块为 content 字符串。
// nil 仅在「有 tool_calls 且无正文」时(OpenAI 官方形态:纯工具调用 content=null);
// 其余场景(如 high 推理吃满预算导致正文为空)返回空字符串 "",与 DeepSeek 系行为一致。
func combinedTextPtr(agg *aggregated) *string {
	s := strings.Join(agg.texts, "")
	if len(agg.texts) == 0 && len(agg.toolCalls) > 0 {
		return nil
	}
	return &s
}

// combinedThinking 拼接 thinking 块为 reasoning_content(无则空串并省略字段)
func combinedThinking(agg *aggregated) string {
	return strings.Join(agg.thinkings, "")
}

// normalizeID chatcmpl- 前缀规整:conv_xxx → chatcmpl-xxx
func normalizeID(convID string) string {
	if convID == "" {
		return "chatcmpl-unknown"
	}
	if strings.HasPrefix(convID, "conv_") {
		return "chatcmpl-" + convID[5:]
	}
	return "chatcmpl-" + convID
}

// parseCreatedAtToUnix 上游 created_at(ISO8601 / epoch 任意)→ epoch 秒
func parseCreatedAtToUnix(s string) int64 {
	if s == "" {
		return 0
	}
	// 尝试纯数字字符串
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	// ISO8601
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Unix()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	return 0
}
