// 错误归一化测试(§12.1):上游双 schema 都归一为 OAI 形状。
package convert

import (
	"encoding/json"
	"testing"
)

func TestErrorNormalize(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantCode string // 期望 status(原文为 HTTP 但表述按字符串)
		wantMsg  string // 期望 error.message 包含
	}{
		{"401 detail", 401, `{"detail":"Invalid API Key"}`, "401", "Invalid API Key"},
		{"422 detail array", 422,
			`{"object":"Error","detail":[{"type":"extra_forbidden","msg":"Extra inputs are not permitted","loc":["body","seed"]}]}`,
			"400", "Extra inputs are not permitted"},
		{"business object Error", 400,
			`{"object":"Error","message":"model not in use","type":"invalid_request_error","code":3003}`,
			"400", "model not in use"},
		{"5xx passthrough body sanitized", 502, `Internal Server Error`,
			"502", "upstream error (status 502)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStatus, oe := NormalizeUpstreamError(c.status, []byte(c.body))
			if gotStatus != atoi(c.wantCode) {
				t.Errorf("status: got %d want %s", gotStatus, c.wantCode)
			}
			if !containsStr(oe.Error.Message, c.wantMsg) {
				t.Errorf("message=%q want contains %q", oe.Error.Message, c.wantMsg)
			}
			// 形状必须是 OAI(序列化后有 error 键)
			b, _ := json.Marshal(oe)
			var m map[string]any
			json.Unmarshal(b, &m)
			if m["error"] == nil {
				t.Error("missing error key")
			}
		})
	}
}

// atoi 小助手(测试内)
func atoi(s string) int {
	switch s {
	case "400":
		return 400
	case "401":
		return 401
	case "429":
		return 429
	case "502":
		return 502
	}
	return -1
}

func containsStr(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
