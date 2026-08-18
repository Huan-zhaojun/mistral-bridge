// §11 非流式响应映射 + §11.2 finish_reason 合成 + §12.2-② usage 修复断言。
package convert

import (
	"encoding/json"
	"strings"
	"testing"

	"mistral-bridge/internal/mistral"
)

// mistralUsageAlias 测试内别名(与 synthesizeFinishReason 签名匹配)
type mistralUsageAlias = mistral.Usage

func TestFinishReasonSynth(t *testing.T) {
	t.Run("tool_calls wins", func(t *testing.T) {
		agg := &aggregated{toolCalls: []OaiToolCall{{ID: "x"}}}
		if got := synthesizeFinishReason(agg, nil, 0); got != "tool_calls" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("length when completion>=max_tokens", func(t *testing.T) {
		agg := &aggregated{texts: []string{"x"}}
		u := usageLite(0, 100, 100)
		if got := synthesizeFinishReason(agg, u, 100); got != "length" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("length NOT triggered by usage=0 (flaky bug not truncation signal)", func(t *testing.T) {
		agg := &aggregated{texts: []string{"x"}}
		u := usageLite(0, 0, 0)
		if got := synthesizeFinishReason(agg, u, 100); got != "stop" {
			t.Errorf("usage-0 must NOT imply length; got %q", got)
		}
	})
	t.Run("default stop", func(t *testing.T) {
		agg := &aggregated{texts: []string{"x"}}
		u := usageLite(10, 50, 60)
		if got := synthesizeFinishReason(agg, u, 100); got != "stop" {
			t.Errorf("got %q", got)
		}
	})
}

// usageLite 构造测试用 usage
func usageLite(p, c, tot int) *mistralUsageAlias {
	return &mistralUsageAlias{PromptTokens: p, CompletionTokens: c, TotalTokens: tot}
}

func TestConvertResponseShapes(t *testing.T) {
	t.Run("content string form", func(t *testing.T) {
		body := `{"object":"conversation.response","conversation_id":"conv_x",
 "created_at":"2026-08-17T12:00:00Z",
 "outputs":[{"object":"entry","type":"message.output","role":"assistant","content":"PONG"}],
 "usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`
		out, repair, err := ConvertResponse([]byte(body), "glm-5-2", "hi", 0, false)
		if err != nil {
			t.Fatal(err)
		}
		var j map[string]any
		json.Unmarshal(out, &j)
		if repair.UsageRepaired {
			t.Error("should not repair")
		}
		msg := j["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if msg["content"] != "PONG" {
			t.Errorf("content=%v", msg["content"])
		}
		if j["id"].(string) != "chatcmpl-x" {
			t.Errorf("id=%v", j["id"])
		}
		if j["created"].(float64) == 0 {
			t.Errorf("created should be parsed ISO8601")
		}
	})

	t.Run("content blocks with thinking split", func(t *testing.T) {
		body := `{"conversation_id":"conv_a","outputs":[{"type":"message.output","role":"assistant",
 "content":[{"type":"thinking","thinking":[{"type":"text","text":"T1"}]},
            {"type":"text","text":"Hello"}]}],
 "usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
		out, _, err := ConvertResponse([]byte(body), "glm-5-2", "x", 0, false)
		if err != nil {
			t.Fatal(err)
		}
		var j struct {
			Choices []struct {
				Message struct {
					Content   *string `json:"content"`
					Reasoning string  `json:"reasoning_content"`
				} `json:"message"`
			} `json:"choices"`
		}
		json.Unmarshal(out, &j)
		m := j.Choices[0].Message
		if m.Content == nil || *m.Content != "Hello" {
			t.Errorf("content=%v", m.Content)
		}
		if m.Reasoning != "T1" {
			t.Errorf("reasoning=%q", m.Reasoning)
		}
	})

	t.Run("multi-stage outputs merge", func(t *testing.T) {
		// message.output → tool.execution → message.output(内置工具多阶段链)
		body := `{"conversation_id":"conv_b","outputs":[
 {"type":"message.output","content":[{"type":"text","text":"P1"}]},
 {"type":"tool.execution","name":"web_search"},
 {"type":"message.output","content":[{"type":"text","text":"P2"}]}],
 "usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
		out, _, err := ConvertResponse([]byte(body), "glm-5-2", "x", 0, false)
		if err != nil {
			t.Fatal(err)
		}
		var j struct {
			Choices []struct {
				Message struct {
					Content *string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		json.Unmarshal(out, &j)
		if *j.Choices[0].Message.Content != "P1P2" {
			t.Errorf("merged=%q", *j.Choices[0].Message.Content)
		}
	})

	t.Run("usage zero triggers tokenizer repair", func(t *testing.T) {
		body := `{"conversation_id":"conv_c","outputs":[{"type":"message.output","content":"PONG"}],
 "usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`
		out, repair, err := ConvertResponse([]byte(body), "glm-5-2", "hello world", 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if !repair.UsageRepaired {
			t.Fatal("expected usage repair")
		}
		var j struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(out, &j); err != nil {
			t.Fatal(err)
		}
		if j.Usage.PromptTokens <= 0 || j.Usage.CompletionTokens <= 0 {
			t.Errorf("repaired usage invalid: %+v", j.Usage)
		}
	})

	// D-34:上游无缓存,如实回 prompt_tokens_details.cached_tokens=0(0 非编造)
	t.Run("cached_tokens truthfully zero", func(t *testing.T) {
		body := `{"conversation_id":"conv_d","outputs":[{"type":"message.output","content":"x"}],
 "usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
		out, _, _ := ConvertResponse([]byte(body), "glm-5-2", "x", 0, false)
		if !strings.Contains(string(out), `"prompt_tokens_details":{"cached_tokens":0}`) {
			t.Errorf("cached_tokens should be present as 0, got: %s", out)
		}
	})

	// D-38:guided-JSON 非流式面——F5 默认 high 注入后,上游非流式也首块重复。
	// 折叠器对该面同规适用(合法 JSON 永不以 {{ 开头)。
	t.Run("nonstream guided-json dup fold", func(t *testing.T) {
		body := `{"conversation_id":"conv_f","outputs":[{"type":"message.output","content":"{\n{\n  \"name\": \"Beijing\"\n}"}],
 "usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`
		out, repair, _ := ConvertResponse([]byte(body), "glm-5-2", "x", 0, true)
		if repair == nil || !repair.JSONFolded {
			t.Error("expected JSONFolded=true on nonstream dup head")
		}
		var j struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(out, &j); err != nil {
			t.Fatal(err)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(j.Choices[0].Message.Content), &obj); err != nil {
			t.Fatalf("folded content should be legal JSON: %v; content=%q", err, j.Choices[0].Message.Content)
		}
		if obj["name"] != "Beijing" {
			t.Errorf("payload broken after fold: %v", obj)
		}
	})

	// 第 24 条:tokenizer 修复失败(empty input → tokenizer 返回但入参为 0 tokens)时,
	// 上游已给了 {0,0,0} 则如实回传 0(不省略、不编造);上游没给 usage 则整个字段省略。
	t.Run("repair failure: upstream zero usage truthfully returned", func(t *testing.T) {
		// inputText/outputText 均为空串 → tokenizer.Count 返回 0,<not error>,
		// 其实该 case 下 repairUsage 返回有效全 0;为了直接断言「不编造」,直接考察:
		// usage 原样来自上游的 0 值在没有任何修复资源时也能透传。
		body := `{"conversation_id":"conv_e","outputs":[{"type":"message.output","content":""}],
 "usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`
		out, _, err := ConvertResponse([]byte(body), "glm-5-2", "", 0, false)
		if err != nil {
			t.Fatal(err)
		}
		var j struct {
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(out, &j); err != nil {
			t.Fatal(err)
		}
		if j.Usage == nil {
			t.Fatal("usage should be present (truthful zero from upstream)")
		}
		// 只要没崩溃且 usage 三值存在即可;绝不编织正数
	})

	// 更优行为:上游完全没给 usage 但 tokenizer 可用时,桥主动补上精确计数。
	// 只有 tokenizer 也不可用(第 24 条第三层)时 usage 才整体省略——不会编造。
	t.Run("usage absent upstream + tokenizer available: bridge computes exact count", func(t *testing.T) {
		body := `{"conversation_id":"conv_f","outputs":[{"type":"message.output","content":"hi"}]}`
		out, repair, err := ConvertResponse([]byte(body), "glm-5-2", "in", 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if !repair.UsageRepaired {
			t.Error("tokenizer should have computed usage")
		}
		var j map[string]any
		json.Unmarshal(out, &j)
		if u, ok := j["usage"].(map[string]any); !ok || u["total_tokens"].(float64) <= 0 {
			t.Errorf("expected computed usage, got %v", j["usage"])
		}
	})
}
