// 真实上游流式 dump 回放测试(gap_stream.jsonl 是研究期抓的真实「thinking→text」全流程流)。
// dump 原始形态为 "event: X\n{data-json}\n"(研究脚本剥离后存储);回放时包装成上游
// 真实 SSE 形态("event: X\ndata: {...}\n\n"),验证状态机全程正确。
package convert

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// gapStreamDump 从 jsonl dump 重构成上游 SSE 形态
func gapStreamDump(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/gap_stream.jsonl")
	if err != nil {
		t.Skip("dump not present:", err)
	}
	var sb strings.Builder
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 256<<10), 256<<10)
	var event string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			event = line[7:]
		} else if strings.HasPrefix(line, "{") && event != "" {
			sb.WriteString("event: " + event + "\n")
			sb.WriteString("data: " + line + "\n\n")
			event = ""
		}
	}
	return sb.String()
}

// TestStreamReplay_ThinkingToText 回放真实「thinking→text」全流程
func TestStreamReplay_ThinkingToText(t *testing.T) {
	sse := gapStreamDump(t)
	if sse == "" {
		t.Fatal("empty reconstructed SSE")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer upstream.Close()

	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	audit := HandleStream(rec, req, resp, StreamConfig{
		OaiModel:     "glm-5-2",
		ClientStream: true,
		Logger:       testLogger(),
		ReqID:        "t1",
	})

	// 断言:产出了 OAI 格式的 SSE,且有思考/正文、finish、[DONE]
	out := rec.Body.String()
	if !strings.Contains(out, "data: [DONE]") {
		t.Error("missing [DONE]")
	}
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Error("missing role chunk")
	}
	if !strings.Contains(out, `"reasoning_content"`) {
		t.Error("thinking should be split to reasoning_content")
	}
	if !strings.Contains(out, `"content"`) {
		t.Error("missing text content chunk")
	}
	if !strings.Contains(out, `"finish_reason"`) {
		t.Error("missing finish chunk")
	}
	if audit.ToolCallCount != 0 {
		t.Errorf("toolCalls=%d want 0", audit.ToolCallCount)
	}
	// 确认没有裸露 event: 行泄漏到出站(OAI 风格只出 data:)
	for line := range strings.Lines(out) {
		if strings.HasPrefix(line, "event:") {
			t.Errorf("outbound leaked upstream event line: %q", line)
		}
	}
}

// TestStreamReplay_UpstreamErrorNoDone 上游异常断无 done 事件(截断 dump 尾部模拟)
func TestStreamReplay_UpstreamErrorNoDone(t *testing.T) {
	sse := gapStreamDump(t)
	// 砍掉 done 事件(留 started + 若干 delta)
	lines := strings.Split(sse, "\n")
	cut := 0
	dot := 0
	for i, l := range lines {
		if l == "" {
			dot++
			if dot == 4 {
				cut = i
				break
			}
		}
	}
	if cut > 0 {
		sse = strings.Join(lines[:cut+1], "\n")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer upstream.Close()
	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	audit := HandleStream(rec, req, resp, StreamConfig{
		OaiModel: "glm-5-2", ClientStream: true, Logger: testLogger(), ReqID: "t2",
	})
	out := rec.Body.String()
	// 即使上游无 done,必须有 finish_reason + [DONE](结束保证)
	if !strings.Contains(out, `[DONE]`) {
		t.Error("missing [DONE] on upstream abort")
	}
	if !strings.Contains(out, `"finish_reason"`) {
		t.Error("missing finish_reason on upstream abort")
	}
	_ = audit
}

// TestStreamReplay_FloodAbort 伪造洪水:同名 function.call 应触发 early-abort
func TestStreamReplay_FloodAbort(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("event: conversation.response.started\ndata: {}\n\n")
	// 5 个同名 function.call(delta) → 第 2 个即触发熔断
	for i := 0; i < 5; i++ {
		j, _ := json.Marshal(map[string]any{
			"type":         "function.call.delta",
			"tool_call_id": "chatcmpl-tool-" + strings.Repeat("x", 5) + itoa(i),
			"name":         "get_weather",
			"arguments":    `{"city":"p"}`,
		})
		sb.WriteString("event: function.call.delta\ndata: " + string(j) + "\n\n")
	}
	// done(event 会被忽略,因为 upstream 在熔�断时被主动断开)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sb.String())
	}))
	defer upstream.Close()
	resp, _ := http.Get(upstream.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	audit := HandleStream(rec, req, resp, StreamConfig{
		OaiModel: "glm-5-2", ClientStream: true, Forced: true, Logger: testLogger(), ReqID: "t3",
	})
	out := rec.Body.String()
	if !audit.FloodAborted {
		t.Error("expected flood abort")
	}
	// 只应保留 1 个 call
	if audit.ToolCallCount != 1 {
		t.Errorf("toolCalls=%d want 1", audit.ToolCallCount)
	}
	// 客户端仍收到正常 [DONE]
	if !strings.Contains(out, "[DONE]") {
		t.Error("missing [DONE] on flood abort path")
	}
}

// TestStreamReplay_UsageRepairBackfill F1 回归:done 带 usage 全 0 → tokenizer 兜底
// 且直发路径兜底值必须回填 audit(否则 access 日志 usage_* 打 0)
func TestStreamReplay_UsageRepairBackfill(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("event: conversation.response.started\ndata: {}\n\n")
	d, _ := json.Marshal(map[string]any{
		"type": "message.output.delta", "content": "hi there",
	})
	sb.WriteString("event: message.output.delta\ndata: " + string(d) + "\n\n")
	sb.WriteString("event: conversation.response.done\ndata: {\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\n")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sb.String())
	}))
	defer upstream.Close()
	resp, _ := http.Get(upstream.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	audit := HandleStream(rec, req, resp, StreamConfig{
		OaiModel: "glm-5-2", ClientStream: true,
		InputText: "a prompt of several real tokens",
		Logger:    testLogger(), ReqID: "t4",
	})
	if !audit.UsageRepaired {
		t.Fatal("expected UsageRepaired=true on zero usage")
	}
	if audit.UsagePromptTok <= 0 || audit.UsageCompletionTok <= 0 {
		t.Errorf("repaired usage not backfilled into audit: prompt=%d completion=%d",
			audit.UsagePromptTok, audit.UsageCompletionTok)
	}
}

// itoa 无 fmt 依赖的小助手
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := "0123456789"
	s := ""
	for i > 0 {
		s = string(digits[i%10]) + s
		i /= 10
	}
	return s
}

// testLogger 测试期丢弃输出的 logger
func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}
