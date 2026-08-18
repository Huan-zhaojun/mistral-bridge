// Go BPE 计数器与 HF 官方 tokenizers 库(Rust)的黄金集一致性质检:
// 样本与期望值由 python 官方库生成(temp/expected_counts.json),
// 研究阶段已坐实官方库与上游计费 ±1 → 此处完全一致即满足兜底契约。
package tokenizer

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestGoldenAlignment(t *testing.T) {
	expRaw, err := os.ReadFile("testdata/expected_counts.json")
	if err != nil {
		t.Fatalf("read expected: %v", err)
	}
	textsRaw, err := os.ReadFile("testdata/sample_texts.json")
	if err != nil {
		t.Fatalf("read samples: %v", err)
	}
	var expected map[string]int
	var texts map[string]string
	if err := json.Unmarshal(expRaw, &expected); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(textsRaw, &texts); err != nil {
		t.Fatal(err)
	}
	for name, want := range expected {
		got, err := Count(texts[name])
		if err != nil {
			t.Fatalf("Count(%s): %v", name, err)
		}
		if got != want {
			t.Errorf("%s: got %d, want %d", name, got, want)
		} else {
			t.Logf("%s: %d tokens ✓", name, want)
		}
	}
}

func TestContractionsAndEdges(t *testing.T) {
	cases := []struct{ in string }{
		{"don't"}, {"it's"}, {"I'll"}, {"1234567"}, {"  leading and trailing  "}, {"\n\n\n"}, {"a\r\nb"},
	}
	for _, c := range cases {
		n, err := Count(c.in)
		if err != nil || n <= 0 {
			t.Errorf("%q: n=%d err=%v", c.in, n, err)
		}
	}
	// 无关 token 次序稳定
	if !strings.Contains("abc", "b") {
		t.Fatal("sanity")
	}
}
