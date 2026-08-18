// ParseBuiltinTools 中文输入法宽容解析全形态测试。
// 全部样例用  转义书写,不出现中文字形字面量(防编码事故)。
package config

import (
	"reflect"
	"testing"
)

func TestBuiltinToolsParse(t *testing.T) {
	S := func(s string) string { return s } // 语法糖,展开为 Go 字符串字面量

	cases := []struct {
		name    string
		in      string
		want    []string
		wantLen int // dropped 长度(0=无 WARN)
	}{
		{"empty", S(""), nil, 0},
		{"ascii comma", S("web_search,code_interpreter"), []string{"web_search", "code_interpreter"}, 0},
		{"unicode no-space", S("web_search，code_interpreter"), []string{"web_search", "code_interpreter"}, 0},
		{"unicode space around", S("web_search， code_interpreter"), []string{"web_search", "code_interpreter"}, 0},
		{"donghao separator", S("web_search、code_interpreter"), []string{"web_search", "code_interpreter"}, 0},
		{"full bracket", S("[web_search, code_interpreter]"), []string{"web_search", "code_interpreter"}, 0},
		{"lenticular bracket", S("【web_search,code_interpreter】"), []string{"web_search", "code_interpreter"}, 0},
		{"paren", S("(web_search,code_interpreter)"), []string{"web_search", "code_interpreter"}, 0},
		{"fullwidth paren", S("(web_search,code_interpreter)"), []string{"web_search", "code_interpreter"}, 0},
		{"whole quoted ascii", S("\"web_search,code_interpreter\""), []string{"web_search", "code_interpreter"}, 0},
		{"whole quoted curvy", S("“web_search,code_interpreter”"), []string{"web_search", "code_interpreter"}, 0},
		{"per-item quoted", S("\"web_search\",\"code_interpreter\""), []string{"web_search", "code_interpreter"}, 0},
		{"per-item curvy quoted", S("“web_search”,“code_interpreter”"), []string{"web_search", "code_interpreter"}, 0},
		{"case-insensitive", S("Web_Search,Code_Interpreter"), []string{"web_search", "code_interpreter"}, 0},
		{"unknown dropped + warn", S("web_search,websearch"), []string{"web_search"}, 1},
		{"dup deduped", S("web_search,web_search"), []string{"web_search"}, 0},
		{"search pair conflict premium wins", S("web_search,web_search_premium"),
			[]string{"web_search_premium"}, 1},
		{"search pair premium order independent", S("web_search_premium,web_search"),
			[]string{"web_search_premium"}, 1},
		{"empty segments skipped", S(",,web_search,,"), []string{"web_search"}, 0},
		{"identity u3002 not separator (dot glues → unknown)", S("web_search。"),
			nil, 1}, // 、U+3002 不支持:整个粘连成未知项
		// NOTE:"web_search。"经过 lower 化后仍是未知 → dropped 收录
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, dropped := ParseBuiltinTools(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v want %v", got, c.want)
			}
			if len(dropped) != c.wantLen {
				t.Errorf("dropped=%v (len=%d) want len=%d", dropped, len(dropped), c.wantLen)
			}
		})
	}
}
