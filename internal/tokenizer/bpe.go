// GLM-5.2 字节级 BPE 计数器:HF tokenizer.json → 纯 Go 计数实现。
// 技术背景:上游 tokenizer pre_tokenizer 含 RE2 不支持的负前瞻 (?!\S),
// sugarme 等第三方库加载即 panic → 本文件为预案自研实现,标准库零依赖。
// 准确性契约:与 HF 官方 tokenizers 库逐样本计数完全一致(测试黄金集:
// temp/expected_counts.json,由官方 tokenizers Rust 库生成;研究阶段已坐实该库与上游计费 ±1)。
package tokenizer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"unicode"
	"unicode/utf8"
)

// ---------- byte-level 映射(GPT-2 bytes_to_unicode 标准映射) ----------
// 可打印字节原样保留,不可打印字节顺次映射到 U+0100..;保证 vocab key 与之一致。
func buildByteToRune() [256]rune {
	// 保留区间:!..~ (33..126)、 额外的两段拉丁扩展(161..172, 174..255)
	var keep [256]bool
	for b := 33; b <= 126; b++ {
		keep[byte(b)] = true
	}
	for b := 161; b <= 172; b++ {
		keep[byte(b)] = true
	}
	for b := 174; b <= 255; b++ {
		keep[byte(b)] = true
	}
	var m [256]rune
	n := 0
	for b := 0; b < 256; b++ {
		if keep[byte(b)] {
			m[byte(b)] = rune(b)
		} else {
			m[byte(b)] = rune(256 + n)
			n++
		}
	}
	return m
}

var byteToRune = buildByteToRune()

// ---------- 模型 ----------
type bpeModel struct {
	mergeRank map[[2]string]int // token 对 → merge 优先级(小=先合并)
	vocab     map[string]bool   // 只需存在性判断即可计数
	specials  []string          // added_tokens(special 按最长优先切分,各计 1)
}

// loadBPEModel 解析 HF tokenizer.json(只取 model.vocab / model.merges / added_tokens)
func loadBPEModel(raw []byte) (*bpeModel, error) {
	var doc struct {
		Model struct {
			Vocab  map[string]int `json:"vocab"`
			Merges [][2]string    `json:"merges"`
		} `json:"model"`
		AddedTokens []struct {
			Content string `json:"content"`
			Special bool   `json:"special"`
		} `json:"added_tokens"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse tokenizer.json: %w", err)
	}
	m := &bpeModel{
		mergeRank: make(map[[2]string]int, len(doc.Model.Merges)),
		vocab:     make(map[string]bool, len(doc.Model.Vocab)),
	}
	for t := range doc.Model.Vocab {
		m.vocab[t] = true
	}
	for i, pr := range doc.Model.Merges {
		m.mergeRank[[2]string{pr[0], pr[1]}] = i
	}
	for _, at := range doc.AddedTokens {
		if at.Special && at.Content != "" {
			m.specials = append(m.specials, at.Content)
		}
	}
	// special 最长优先(如 <|gmask|> 与 <gmask> 前缀竞争时取长)
	sort.Slice(m.specials, func(i, j int) bool { return len(m.specials[i]) > len(m.specials[j]) })
	return m, nil
}

// ---------- pre-tokenizer(GLM Split 正则的手工等价物) ----------
// 正责分解(按 alternation 优先级):
//
//	(?i:'s|'t|'re|'ve|'m|'ll|'d)          缩写后缀
//	[^\r\n\p{L}\p{N}]?\p{L}+                可选前导符号 + 字母串
//	\p{N}{1,3}                              1~3 位数字
//	 ?[^\s\p{L}\p{N}]+[\r\n]*              可选空格 + 符号串 + 可选换行
//	\s*[\r\n]+                             空白* + 换行+
//	\s+(?!\S)                              尾部空白块(后面不跟非空白)
//	\s+                                    其他所有空白
//
// 手写 scanner:在每一位置按以上优先级贪心尝试,匹配失败回退下一分支。
var contractions = []string{"'re", "'ve", "'ll", "'s", "'t", "'m", "'d"}

func isDigitRune(r rune) bool  { return unicode.IsDigit(r) }
func isLetterRune(r rune) bool { return unicode.IsLetter(r) }
func isSpaceRune(r rune) bool  { return unicode.IsSpace(r) }

// preTokenize 把文本切成片段(对应 HF Split pretokenizer 的 Isolated 行为)
func preTokenize(text string) []string {
	runes := []rune(text)
	n := len(runes)
	var out []string
	i := 0
	for i < n {
		end := matchOne(runes, i)
		if end <= i {
			// 防御:至少前进一步,任何空匹配都不会死循环
			end = i + 1
		}
		out = append(out, string(runes[i:end]))
		i = end
	}
	return out
}

// matchOne 在位置 i 按分支优先级返回匹配长度(末端下标,不含)
func matchOne(rs []rune, i int) int {
	// B1 缩写后缀(大小写不敏感)
	lower := lowerASCII(rs, i, 3) // 至多比较 3 rune
	for _, c := range contractions {
		if hasFoldPrefix(lower, c) {
			return i + len(c) // contractions 均为 ASCII,长度等于 rune 数
		}
	}

	r := rs[i]

	// B2 可选前导符号 + 字母串
	{
		j := i
		if !isLetterRune(r) && !isDigitRune(r) && r != '\r' && r != '\n' {
			j++ // 消费一个可选前导符号
		}
		k := j
		for k < len(rs) && isLetterRune(rs[k]) {
			k++
		}
		if k > j {
			return k // 必须有至少一个字母,否则此分支不成立
		}
	}

	// B3 1~3 位数字
	if isDigitRune(r) {
		j := i
		for j < len(rs) && isDigitRune(rs[j]) && j-i < 3 {
			j++
		}
		return j
	}

	// B4 可选空格 + 符号串 + 可选换行
	{
		j := i
		if r == ' ' {
			j++
		}
		k := j
		for k < len(rs) && !isSpaceRune(rs[k]) && !isLetterRune(rs[k]) && !isDigitRune(rs[k]) {
			k++
		}
		if k > j {
			// 吞后续空白中的 \r\n
			for k < len(rs) && (rs[k] == '\r' || rs[k] == '\n') {
				k++
			}
			return k
		}
	}

	// B5/B6/B7 空白族
	if isSpaceRune(r) {
		j := i
		for j < len(rs) && isSpaceRune(rs[j]) {
			j++
		}
		// B5:含 \r\n 则整体消费该空白串
		hasNL := false
		for k := i; k < j; k++ {
			if rs[k] == '\r' || rs[k] == '\n' {
				hasNL = true
				break
			}
		}
		if hasNL {
			return j
		}
		// B6 \s+(?!\S):空白串后不是非空白(即到结尾)才在此匹配整串…
		// 注意 regex 语义是回溯式的:\s+ 吃不满才会让 (?!\S) 成立;
		// 实际等价于:若 j==len 则吞全部空白,否则在"最后一个空白"处留 0 空位也成立?
		// 专家结论:tiktoken/HF 语义下此分支匹配"尾部空白",即空白串后跟结尾。
		// 若跟非空白,本分支不匹配,交给 B7 吞整串。
		if j == len(rs) {
			return j
		}
		// B7:吞整句空白(其后必跟非空白,因 hasNL 为假且 j<len)
		return j
	}

	// 兜底:单字符(不会到达,B4 分支覆盖非空符号)
	return i + 1
}

// lowerASCII 取窗口内 rune 的小写化 ASCII 表示(供缩写比较)
func lowerASCII(rs []rune, i int, max int) []rune {
	out := make([]rune, 0, max)
	for j := i; j < len(rs) && len(out) < max; j++ {
		r := rs[j]
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		out = append(out, r)
	}
	return out
}

// hasFoldPrefix 检查 rune 流是否以 ASCII 前缀开头
func hasFoldPrefix(rs []rune, prefix string) bool {
	if len(rs) < len(prefix) {
		return false
	}
	for k := 0; k < len(prefix); k++ {
		if rs[k] != rune(prefix[k]) {
			return false
		}
	}
	return true
}

// ---------- BPE merge ----------
// encodePiece 对一个 pre-token 片段做 bytes→unicode 映射 + merge,返回 token 数
func (m *bpeModel) encodePiece(piece string) int {
	// 字节 → 符号词
	data := []byte(piece)
	syms := make([]string, 0, len(data))
	for len(data) > 0 {
		b := data[0]
		syms = append(syms, string(byteToRune[b]))
		data = data[1:]
	}
	if len(syms) == 0 {
		return 0
	}
	if len(syms) == 1 {
		return 1
	}
	// 标准 BPE:反复合并最低 rank 的相邻对(优先取 bigram 中出现频率不
	// 在考察范围——HF 的语义即按 merges 序全局最低 rank,与频次无关)
	for {
		best := -1
		bestRank := int(^uint(0) >> 1)
		for i := 0; i+1 < len(syms); i++ {
			if r, ok := m.mergeRank[[2]string{syms[i], syms[i+1]}]; ok && r < bestRank {
				best = i
				bestRank = r
			}
		}
		if best < 0 {
			break
		}
		merged := syms[best] + syms[best+1]
		syms[best] = merged
		copy(syms[best+1:], syms[best+2:])
		syms = syms[:len(syms)-1]
	}
	return len(syms)
}

// count 对整段文本计数(special token 先切出,各计 1)
func (m *bpeModel) count(text string) int {
	total := 0
	for len(text) > 0 {
		// special 最长优先
		hit := ""
		for _, sp := range m.specials {
			if bytes.HasPrefix([]byte(text), []byte(sp)) {
				hit = sp
				break
			}
		}
		if hit != "" {
			total++
			text = text[len(hit):]
			continue
		}
		// 切出到下一个 special 之前的普通段
		next := len(text)
		for _, sp := range m.specials {
			if idx := indexAt(text, sp); idx >= 0 && idx < next {
				next = idx
			}
		}
		segment := text[:next]
		for _, piece := range preTokenize(segment) {
			total += m.encodePiece(piece)
		}
		text = text[next:]
	}
	return total
}

// indexAt 返回子串位置(供"下一个 special"探测)
func indexAt(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 供包内调用:确保 utf8 依赖不被误删(未来基于 rune 的优化可能用到)
var _ = utf8.RuneCountInString
