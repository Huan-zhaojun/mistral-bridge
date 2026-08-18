// §10 请求映射全表驱动测试(A/B/C/D 分类每行一例 + 边缘)。
package convert

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"mistral-bridge/internal/oai"
)

// helper:解析 OAI 请求 JSON
func mustParse(t *testing.T, body string) *oai.ChatRequest {
	t.Helper()
	req, err := oai.ParseChatRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return req
}

// ---------- A. 客户端调用方式错误 → 400 ----------

// TestA_ClientErrors 应被拒绝的入参,每例返回非 nil BridgeError
func TestA_ClientErrors(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		param string // 期望的 BridgeError.Param
	}{
		{"unsupported model", `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`, "model"},
		{"temperature > 1 goes upstream (no clamp)", `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"temperature":1.5}`, ""}, // 桥不拦截,上游 422 → 规范化
		{"n>1", `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"n":2}`, "n"},
		{"logprobs", `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"logprobs":true}`, "logprobs"},
		{"top_logprobs", `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"top_logprobs":3}`, "top_logprobs"},
		{"logit_bias", `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"logit_bias":{"1":2}}`, "logit_bias"},
		{"modalities", `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"modalities":["text"]}`, "modalities"},
		{"audio", `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"audio":{}}`, "audio"},
		{"tool_choice bad str", `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"tool_choice":"once"}`, "tool_choice"},
		{"tool_choice object multi-func", `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"a"}},{"type":"function","function":{"name":"b"}}],"tool_choice":{"type":"function","function":{"name":"a"}}}`, "tool_choice"},
		{"unknown role", `{"model":"glm-5-2","messages":[{"role":"alien","content":"hi"}]}`, "messages"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := mustParse(t, c.body)
			_, err := ConvertRequest(req, Options{PassReasoning: true})
			if err == nil {
				if c.param == "" {
					return // 桥本不拦截
				}
				t.Fatalf("want error, got nil")
			}
			be, ok := err.(*BridgeError)
			if !ok {
				t.Fatalf("want *BridgeError, got %T", err)
			}
			if c.param != "" && be.Param != c.param {
				t.Errorf("Param: got %q want %q", be.Param, c.param)
			}
		})
	}
}

// ---------- B. 协议差静默修复 ----------

func TestB_SilentRepairs(t *testing.T) {
	t.Run("system+developer -> instructions joined", func(t *testing.T) {
		req := mustParse(t, `{"model":"glm-5-2","messages":[
  {"role":"system","content":"A"},{"role":"developer","content":"B"},
  {"role":"user","content":"hi"}]}`)
		conv, err := ConvertRequest(req, Options{PassReasoning: true})
		if err != nil {
			t.Fatal(err)
		}
		var up map[string]any
		json.Unmarshal(conv.Body, &up)
		if up["instructions"] != "A\n\nB" {
			t.Errorf("instructions=%q", up["instructions"])
		}
	})

	t.Run("seed renamed to random_seed", func(t *testing.T) {
		req := mustParse(t, `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"seed":42}`)
		conv, _ := ConvertRequest(req, Options{PassReasoning: true})
		var up map[string]any
		json.Unmarshal(conv.Body, &up)
		args := up["completion_args"].(map[string]any)
		if args["random_seed"].(float64) != 42 {
			t.Errorf("random_seed=%v", args["random_seed"])
		}
	})

	t.Run("reasoning_effort medium->high", func(t *testing.T) {
		req := mustParse(t, `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`)
		conv, _ := ConvertRequest(req, Options{PassReasoning: true})
		var up map[string]any
		json.Unmarshal(conv.Body, &up)
		args := up["completion_args"].(map[string]any)
		if args["reasoning_effort"] != "high" {
			t.Errorf("reasoning_effort=%v", args["reasoning_effort"])
		}
	})

	t.Run("tool_choice any -> auto", func(t *testing.T) {
		req := mustParse(t, `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"tool_choice":"any"}`)
		conv, _ := ConvertRequest(req, Options{PassReasoning: true})
		var up map[string]any
		json.Unmarshal(conv.Body, &up)
		args := up["completion_args"].(map[string]any)
		if args["tool_choice"] != "auto" {
			t.Errorf("tool_choice=%v", args["tool_choice"])
		}
	})

	t.Run("tool_choice object single-func -> required", func(t *testing.T) {
		req := mustParse(t, `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],
  "tools":[{"type":"function","function":{"name":"only_one","description":"d","parameters":{}}}],
  "tool_choice":{"type":"function","function":{"name":"only_one"}}}`)
		conv, _ := ConvertRequest(req, Options{PassReasoning: true})
		if !conv.ForcedStream {
			t.Errorf("ForcedStream should be true")
		}
		var up map[string]any
		json.Unmarshal(conv.Body, &up)
		args := up["completion_args"].(map[string]any)
		if args["tool_choice"] != "required" {
			t.Errorf("tool_choice=%v", args["tool_choice"])
		}
	})

	t.Run("image blocks cleaned", func(t *testing.T) {
		req := mustParse(t, `{"model":"glm-5-2","messages":[{"role":"user","content":[
  {"type":"text","text":"see:"},
  {"type":"image_url","image_url":{"url":"https://x.com/y.png"}}
]}]}`)
		conv, err := ConvertRequest(req, Options{PassReasoning: true})
		if err != nil {
			t.Fatal(err)
		}
		var up struct {
			Inputs []map[string]any `json:"inputs"`
		}
		json.Unmarshal(conv.Body, &up)
		content := up.Inputs[0]["content"].([]any)
		var texts []string
		for _, c := range content {
			texts = append(texts, c.(map[string]any)["text"].(string))
		}
		joined := strings.Join(texts, "|")
		if !strings.Contains(joined, "see:") || !strings.Contains(joined, "[image omitted]") {
			t.Errorf("content texts=%v", texts)
		}
	})

	t.Run("max_completion_tokens > max_tokens picks larger", func(t *testing.T) {
		req := mustParse(t, `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"max_completion_tokens":50}`)
		conv, _ := ConvertRequest(req, Options{PassReasoning: true})
		var up map[string]any
		json.Unmarshal(conv.Body, &up)
		args := up["completion_args"].(map[string]any)
		if args["max_tokens"].(float64) != 50 {
			t.Errorf("max_tokens=%v", args["max_tokens"])
		}
	})

	t.Run("assistant tool_call history zip to function.result", func(t *testing.T) {
		req := mustParse(t, `{"model":"glm-5-2","messages":[
  {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
  {"role":"tool","tool_call_id":"call_1","content":"result-1"},
  {"role":"user","content":"next"}]}`)
		conv, err := ConvertRequest(req, Options{PassReasoning: true})
		if err != nil {
			t.Fatal(err)
		}
		var up struct {
			Inputs []map[string]any `json:"inputs"`
		}
		json.Unmarshal(conv.Body, &up)
		// 期望 entries: function.call, function.result, message.input
		types := []string{}
		for _, in := range up.Inputs {
			types = append(types, in["type"].(string))
		}
		want := "function.call,function.result,message.input"
		if strings.Join(types, ",") != want {
			t.Errorf("entries=%v want %v", types, want)
		}
	})
}

// ---------- C. OAI 合法但上游无对应 → 静默忽略 ----------

func TestC_IgnoredParams(t *testing.T) {
	req := mustParse(t, `{"model":"glm-5-2","messages":[{"role":"user","content":"hi"}],
 "parallel_tool_calls":false,"user":"u1","prompt_cache_key":"k"}`)
	if _, err := ConvertRequest(req, Options{PassReasoning: true}); err != nil {
		t.Fatalf("should not error, got %v", err)
	}
}

// ---------- D. 直通 ----------

// 模型名标准化:对外官方名 glm-5.2,上游真名 glm-5-2、平台别名 zai-glm-5-2 兼容照收;
// 上游请求一律归一 glm-5-2,响应回显客户端原值(OriginalModel)。
func TestD_ModelAlias(t *testing.T) {
	for _, in := range []string{"glm-5.2", "glm-5-2", "zai-glm-5-2"} {
		req := mustParse(t, fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, in))
		conv, err := ConvertRequest(req, Options{PassReasoning: true})
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if conv.Model != "glm-5-2" {
			t.Errorf("%s normalized to %q", in, conv.Model)
		}
		if conv.OriginalModel != in {
			t.Errorf("%s OriginalModel=%q", in, conv.OriginalModel)
		}
		var up map[string]any
		json.Unmarshal(conv.Body, &up)
		if up["model"] != "glm-5-2" {
			t.Errorf("%s upstream model=%v", in, up["model"])
		}
	}
}

// F5 默认思考(D-35):缺省/auto/任意强度 → high;仅 none 直通。
func TestD_ReasoningDefaultHigh(t *testing.T) {
	upstreamEffort := func(t *testing.T, body string) any {
		conv, err := ConvertRequest(mustParse(t, body), Options{PassReasoning: true})
		if err != nil {
			t.Fatal(err)
		}
		var up map[string]any
		json.Unmarshal(conv.Body, &up)
		args, _ := up["completion_args"].(map[string]any)
		return args["reasoning_effort"]
	}
	if v := upstreamEffort(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`); v != "high" {
		t.Errorf("absent reasoning_effort should inject high, got %v", v)
	}
	if v := upstreamEffort(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"auto"}`); v != "high" {
		t.Errorf("auto should map to high, got %v", v)
	}
	if v := upstreamEffort(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`); v != "high" {
		t.Errorf("xhigh should map to high, got %v", v)
	}
	if v := upstreamEffort(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`); v != "none" {
		t.Errorf("none must passthrough, got %v", v)
	}
}

// F2 CC WebSearch 映射(D-33):function name=WebSearch(不区分大小写)→ 服务端内置搜索。
func TestD_CCWebSearchMapping(t *testing.T) {
	toolset := `,"tools":[{"type":"function","function":{"name":"WebSearch","description":"Search the web","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}}]}`
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	body = body[:len(body)-1] + toolset // toolset 自带根对象收官
	toolTypes := func(t *testing.T, opts Options) []string {
		var up map[string]any
		conv, err := ConvertRequest(mustParse(t, body), opts)
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(conv.Body, &up)
		var types []string
		for _, raw := range up["tools"].([]any) {
			m := raw.(map[string]any)
			s := m["type"].(string)
			if s == "function" {
				s = "function:" + m["function"].(map[string]any)["name"].(string)
			}
			types = append(types, s)
		}
		return types
	}
	contains := func(ts []string, want string) bool {
		for _, x := range ts {
			if x == want {
				return true
			}
		}
		return false
	}

	// ① 默认开:无配置 → premium,function 不直通
	ts := toolTypes(t, Options{MapCCWebSearch: true})
	if !contains(ts, "web_search_premium") || contains(ts, "function:WebSearch") {
		t.Errorf("default mapping should yield builtin premium without function passthrough, got %v", ts)
	}
	// ② config 已配普搜 → 跟随配置(普搜),不再叠 premium
	ts = toolTypes(t, Options{MapCCWebSearch: true, BuiltinTools: []string{"web_search"}})
	if !contains(ts, "web_search") || contains(ts, "web_search_premium") {
		t.Errorf("config plain search should win, got %v", ts)
	}
	// ③ off → function 原样直通,无内置
	ts = toolTypes(t, Options{})
	if !contains(ts, "function:WebSearch") || contains(ts, "web_search_premium") {
		t.Errorf("off should passthrough function untouched, got %v", ts)
	}
}

func TestD_Passthrough(t *testing.T) {
	req := mustParse(t, `{"model":"zai-glm-5-2","messages":[{"role":"user","content":"hi"}],
 "stream":false,"top_p":0.9,"stop":["END"],"frequency_penalty":0.3,"presence_penalty":0.7}`)
	conv, err := ConvertRequest(req, Options{PassReasoning: true})
	if err != nil {
		t.Fatal(err)
	}
	if conv.Model != "glm-5-2" {
		t.Errorf("model alias not normalized: %q", conv.Model)
	}
	if conv.OriginalModel != "zai-glm-5-2" {
		t.Errorf("original model=%q", conv.OriginalModel)
	}
	var up map[string]any
	json.Unmarshal(conv.Body, &up)
	args := up["completion_args"].(map[string]any)
	if args["top_p"].(float64) != 0.9 {
		t.Errorf("top_p=%v", args["top_p"])
	}
	if args["frequency_penalty"].(float64) != 0.3 {
		t.Errorf("fp=%v", args["frequency_penalty"])
	}
	if up["store"] != false {
		t.Errorf("store=%v", up["store"])
	}
}
