// L2 集成测试:httptest 假上游,驱动 chat handler 完整链路四路。
package convert

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestHandler 构造绑向假上游的桥 handler(捕获 header 断言 Authorization 透传)
func newTestHandler(t *testing.T, upstream http.HandlerFunc) (http.Handler, func(url, auth, ct string) bool) {
	t.Helper()
	var seenAuth, seenCT, seenPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenCT = r.Header.Get("Content-Type")
		upstream(w, r)
	}))
	handler := NewChatHandler(ChatConfig{
		UpstreamBase:   up.URL,
		Client:         &http.Client{},
		Logger:         slog.New(slog.NewJSONHandler(io.Discard, nil)),
		SSEIdleTimeout: 5 * time.Second,
	})
	check := func(url, auth, ct string) bool {
		_ = url
		return seenAuth == auth && seenCT == ct && seenPath == "/v1/conversations"
	}
	return handler, check
}

func TestE2E_NonStreamOK(t *testing.T) {
	handler, check := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-kong-request-id", "kong-abc")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"conversation_id":"conv_z","outputs":[{"type":"message.output","content":"OK"}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)
	})
	body := `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-KEY-123")
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	resp := rec.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, rec.Body.String())
	}
	if resp.Header.Get("x-request-id") != "kong-abc" {
		t.Errorf("x-request-id=%q", resp.Header.Get("x-request-id"))
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"OK"`) {
		t.Errorf("body=%s", out)
	}
	if !check("", "Bearer sk-test-KEY-123", "application/json") {
		t.Error("Authorization/Content-Type 未原样透传")
	}
	if strings.Contains(out, "sk-test-KEY-123") {
		t.Error("authorization leaked to response")
	}
	if !strings.Contains(resp.Header.Get("x-request-id"), "kong") {
		t.Error("kong id not surfaced")
	}
}

func TestE2E_MissingAuth401(t *testing.T) {
	handler, _ := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be hit when auth missing")
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5-2","messages":[]}`))
	handler.ServeHTTP(rec, req)
	if rec.Result().StatusCode != 401 {
		t.Fatalf("status=%d", rec.Result().StatusCode)
	}
	if !strings.Contains(rec.Body.String(), "missing Authorization") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

func TestE2E_Upstream422Normalized(t *testing.T) {
	handler, _ := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		_, _ = io.WriteString(w, `{"object":"Error","detail":[{"type":"extra_forbidden","msg":"Extra inputs are not permitted"}]}`)
	})
	body := `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"seedx":1}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	handler.ServeHTTP(rec, req)
	resp := rec.Result()
	if resp.StatusCode != 400 {
		t.Fatalf("status=%d want 400(规范化)", resp.StatusCode)
	}
	if !strings.Contains(rec.Body.String(), "Extra inputs are not permitted") {
		t.Errorf("body=%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_error") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

func TestE2E_StreamOKWithUsage(t *testing.T) {
	// 假上游流式:started → text delta → done(usage 全 0)→ 桥必须 tokenizer 兜底
	handler, _ := newTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: conversation.response.started\ndata: {\"type\":\"conversation.response.started\"}\n\n")
		_, _ = io.WriteString(w, "event: message.output.delta\ndata: {\"type\":\"message.output.delta\",\"content\":\"PONG\"}\n\n")
		_, _ = io.WriteString(w, "event: conversation.response.done\ndata: {\"type\":\"conversation.response.done\",\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0,\"total_tokens\":0}}\n\n")
	})
	body := `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	handler.ServeHTTP(rec, req)
	resp := rec.Result()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("content-type=%v", resp.Header.Get("Content-Type"))
	}
	out := rec.Body.String()
	if !strings.Contains(out, "PONG") || !strings.Contains(out, "[DONE]") {
		t.Errorf("stream body incomplete: %s", out)
	}
	// usage 走了 tokenizer 兜底
	if !strings.Contains(out, `"total_tokens"`) {
		t.Errorf("usage chunk missing")
	}
	// 兜底值应该非零(知道 prompt="hi"、completion="PONG")
	if !strings.Contains(out, `"prompt_tokens":`) || strings.Contains(out, `"total_tokens": 0`) {
		t.Errorf("repaired usage invalid: %s", out)
	}
}
