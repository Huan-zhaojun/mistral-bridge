// Package repair 转换侧修复件②:guided-JSON 流式开头重复 bug 的静默自动修复(§12.2-①)。
//
// 背景:json_object/json_schema + stream + reasoning_effort=high 时,上游偶发把正文
// 开头的 `{` 重复发两次。实测重复形态(研究 checkpoint + E2E 实测):
//
//	形态 A: "{\n" + "{\n" + "\"a\":1}"   → bug 聚合 "{ {\n{"a":1}" → 合法目标 "{\"a\":1}"
//	形态 B: "{\"" + "{" + "\"a\":1}"     → bug 聚合 "{\"{\"a\":1}"    → 合法目标 "{\"a\":1}"
//	形态 C: "{\"" + "{ \"x\": 42 }"      → bug 聚合 "{\"{ \"x\":42}"   → 合法目标 "{\"x\":42}"
//
// 规律:不合法聚合与合法目标的差,恰是 buffer 头部「第 2 个 `{`」及其后所有装饰符
// (空白+多余引号)。所以折叠算法:判定触发后,输出 = head[:第2个'{'的 index] + 决定性字符起的 rest。
//
// 触发条件(在首个「非 `{`/非空白/非 `"`」决定性字符处停下判定):
//
//	buffer 含 ≥2 个 `{` 且 `"` 数 ≤ 2  → fold
//
// 已知边界(记录的取舍):合法 JSON 以 key="{" 开头(`{"{":1}`)与 bug 形态 B 的 buffer
// 同形(brace=2,quote=2),会被同规折叠 → 该条合法 JSON 变非法。该 key 形态现实中趋零,
// 接收此理论误伤换取对上游 bug 的确修;如需消除,后续加缓冲跟踪成对引号。
// 其余任意合法 JSON(俗称 `"{{"` 的另述):`{{` 开头本身不是合法 JSON,永不触发。
//
// 成本:首块最多推迟 1 个 delta。
package repair

import "strings"

// JSONFolder guided-JSON 流式首块折叠器(常开,无开关)。每 stream 请求一个(一条 text 流只缓冲头部)。
type JSONFolder struct {
	buf    []byte
	done   bool
	folded bool
}

// NewJSONFolder 构造
func NewJSONFolder() *JSONFolder { return &JSONFolder{} }

// headCap 缓冲上限(防御:头部形态装饰不会超过此量级,超限强制直通判定)
const headCap = 64

// Feed 送入一段 text delta,返回应下发的文本(可为空=继续缓冲)
func (f *JSONFolder) Feed(delta string) string {
	if f.done {
		return delta
	}
	f.buf = append(f.buf, delta...)
	for i := 0; i < len(f.buf); i++ {
		c := f.buf[i]
		if c == '{' || c == '"' || isJSONSpace(c) {
			continue
		}
		// 决定性字符:判定并收尾
		f.done = true
		head := f.foldedHeadLocked(f.buf[:i])
		rest := string(f.buf[i:])
		f.buf = nil
		return head + rest
	}
	if len(f.buf) > headCap {
		f.done = true
		out := f.foldedHeadLocked(f.buf)
		f.buf = nil
		return out
	}
	return ""
}

// Folded 是否触发了折叠
func (f *JSONFolder) Folded() bool { return f.folded }

// Flush 流提前结束时冲刷残存缓冲
func (f *JSONFolder) Flush() string {
	if f.done {
		return ""
	}
	f.done = true
	out := f.foldedHeadLocked(f.buf)
	f.buf = nil
	return out
}

// foldedHeadLocked 判定并折叠头部;返回应下发的头部文本
func (f *JSONFolder) foldedHeadLocked(head []byte) string {
	if len(head) == 0 {
		return ""
	}
	second := secondBraceIndex(head)
	if second < 0 {
		return string(head) // <2 个 `{` → 直通
	}
	quotes := countByte(head, '"')
	if quotes > 2 {
		return string(head) // 引号>2 视为合法成对 key 内容开头 → 直通
	}
	f.folded = true
	// 删除第 2 个 `{` 及其紧随空白;
	// 若第 1 个 `{` 的装饰组已带结尾 quote(形态 B/C),则第 2 组开头剥出的首个
	// 多余 quote 也要一并丢弃——三形态统一折叠为合法 JSON 正文头。
	keep := head[:second]
	tail := head[second+1:]
	i := 0
	for i < len(tail) && isJSONSpace(tail[i]) {
		i++
	}
	tail = tail[i:]
	// keep 末尾是 quote 且 tail 开头也是 quote → 剥 tail 的首个 quote(多余装饰)
	if len(keep) > 0 && keep[len(keep)-1] == '"' && len(tail) > 0 && tail[0] == '"' {
		tail = tail[1:]
	}
	var b strings.Builder
	b.Write(keep)
	b.Write(tail)
	return b.String()
}

// secondBraceIndex 第 2 个 `{` 的索引(无则 -1)
func secondBraceIndex(head []byte) int {
	seen := 0
	for i, c := range head {
		if c == '{' {
			seen++
			if seen == 2 {
				return i
			}
		}
	}
	return -1
}

// countByte 统计字节出现次数
func countByte(b []byte, c byte) int {
	n := 0
	for _, x := range b {
		if x == c {
			n++
		}
	}
	return n
}

// isJSONSpace JSON 空白定义
func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
