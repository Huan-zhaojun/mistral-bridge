// 修复件单元测试:① JSON 首块折叠 / ③ required 洪水治理时机。
package repair

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------- JSONFolder(§12.2-①) ----------

func TestJSONFolder(t *testing.T) {
	cases := []struct {
		name     string
		deltas   []string
		wantFold bool
		wantJoin string // 聚合后的最终输出(应为合法 JSON 或不受干扰直通)
	}{
		// —— 修复对象:上游三种重复形态 ——
		{"form A 全等双份 {\\n + {\\n", []string{"{\n", "{\n", "\"a\":1}"}, true, "{\n\"a\":1}"}, // 折叠保留首个 { 与其后的 \n 装饰,仍为合法 JSON
		{"form B 错位双份 {\" + {", []string{"{\"", "{", "\"a\":1}"}, true, "{\"a\":1}"},
		{"form C 交错双份 {\" + { \"...", []string{"{\"", "{ \"x\": 42 }"}, true, "{\"x\": 42 }"},
		// —— 直通保护:合法输入不动 ——
		{"normal JSON", []string{"{\"a\":1}"}, false, "{\"a\":1}"},
		{"normal JSON split delta", []string{"{", "\"a\":1}"}, false, "{\"a\":1}"},
		{"whitespace only prefix kept", []string{"  ", " {", "\"a\":1}"}, false, "   {\"a\":1}"},
		{"whitespace-multiline prefix legal head", []string{"{\n  ", "\"a\":1}"}, false, "{\n  \"a\":1}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := NewJSONFolder()
			var out strings.Builder
			for _, d := range c.deltas {
				out.WriteString(f.Feed(d))
			}
			out.WriteString(f.Flush())
			got := out.String()
			if f.Folded() != c.wantFold {
				t.Errorf("folded=%v want %v", f.Folded(), c.wantFold)
			}
			if got != c.wantJoin {
				t.Errorf("join=%q want %q", got, c.wantJoin)
			}
			// 最后必然仍是合法 JSON(全部用例都提供了决定性字符,聚合后应合法)
			var probe any
			if err := json.Unmarshal([]byte(got), &probe); err != nil {
				t.Errorf("最终聚合非合法 JSON: %q err=%v", got, err)
			}
		})
	}
}

// TestJSONFolder_DoesNotCorruptValid JSON 边界:合法 JSON 永远不以 {{ 开头(零误伤依据)
func TestJSONFolder_DoesNotCorruptValid(t *testing.T) {
	valid := []string{
		`{"a":1}`, `{"nested":{"x":1}}`, `[1,2,3]`, `"str"`, `123`, `true`,
		`{"deep":{"deeper":{"d":[[{"z":1}]]}}}`,
	}
	for _, js := range valid {
		f := NewJSONFolder()
		var out strings.Builder
		for i, ch := range []byte(js) {
			_ = i
			out.WriteString(f.Feed(string(ch)))
		}
		out.WriteString(f.Flush())
		if out.String() != js {
			t.Errorf("valid JSON mutated: in=%q out=%q", js, out.String())
		}
	}
}

// ---------- FloodGuard(§12.2-③) ----------

func TestFloodGuard(t *testing.T) {
	t.Run("first call passes, second same-name aborts", func(t *testing.T) {
		g := NewFloodGuard()
		if !g.ObserveCallStart("get_weather") {
			t.Fatal("first should pass")
		}
		if g.ObserveCallStart("get_weather") {
			t.Fatal("second must abort")
		}
		if !g.Aborted() {
			t.Fatal("aborted flag missing")
		}
		if g.KeptCount() != 1 {
			t.Errorf("kept=%d", g.KeptCount())
		}
	})
	t.Run("different names all pass", func(t *testing.T) {
		g := NewFloodGuard()
		if !g.ObserveCallStart("a") || !g.ObserveCallStart("b") || !g.ObserveCallStart("c") {
			t.Fatal("multi-tool first calls must pass")
		}
		if g.Aborted() {
			t.Fatal("should not abort on distinct names")
		}
	})
	t.Run("after abort still false", func(t *testing.T) {
		g := NewFloodGuard()
		g.ObserveCallStart("f")
		g.ObserveCallStart("f")
		if g.ObserveCallStart("g") {
			t.Fatal("post-abort must still return false")
		}
	})
}
