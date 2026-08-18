// BUILTIN_TOOLS 环境变量解析:中文输入法友好的宽容解析。
// 所有匹配一律使用  码点转义书写,源码不出现中文字形字面量,
// 避免编码事故导致行为漂移(解析行为只由码点决定)。
package config

import (
	"strings"
)

// 分隔符归一化:全角逗号(U+FF0C)、顿号(U+3001)→ ASCII 逗号
// 注意:句号(U+3002)刻意不支持——它不是分隔符,粘连后落入未知项 WARN。
var sepNormalize = strings.NewReplacer("，", ",", "、", ",") // U+FF0C/U+3001 -> ASCII comma

// wrapPairs 合法的外层包裹对(成对才剥,只剥一层,不递归)。
var wrapPairs = [][2]rune{
	{0x0022, 0x0022}, // 半角双引号
	{0x0027, 0x0027}, // 半角单引号(.env 单引号值常见)
	{0x201C, 0x201D}, // 中文弯双引号
	{0x005B, 0x005D}, // 半角方括号
	{0x3010, 0x3011}, // 方头括号【】
	{0x0028, 0x0029}, // 半角圆括号
	{0xFF08, 0xFF09}, // 全角圆括号
	// 注:弯单引号 U+2018/U+2019 已按需求删除,不匹配不剥离。
}

// ValidBuiltinTools 上游支持的内置工具白名单(子集,不含 document_library/connector)
var ValidBuiltinTools = []string{
	"web_search",
	"web_search_premium",
	"code_interpreter",
	"image_generation",
}

var validToolSet = func() map[string]bool {
	m := make(map[string]bool, len(ValidBuiltinTools))
	for _, t := range ValidBuiltinTools {
		m[t] = true
	}
	return m
}()

// ParseBuiltinTools 解析 BUILTIN_TOOLS env。
// 返回 (生效集, 被丢弃集):生效集已去重、按白名单过滤、搜索互斥已裁决;
// 被丢弃集供调用方启动时 WARN 列出。
// 解析流水线:trim → 剥一层外层包裹 → 分隔符归一 split → 逐项 trim + 剥引号 → 小写归一 → 过滤/裁决。
func ParseBuiltinTools(s string) (tools []string, dropped []string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	// 整体剥一层外层包裹(仅当成对时)
	s = stripPair(s)
	// 分隔符归一
	s = sepNormalize.Replace(s)

	seen := make(map[string]bool)
	var ordered []string
	for _, seg := range strings.Split(s, ",") {
		item := strings.TrimSpace(seg)
		if item == "" {
			continue // 空段(连续逗号)静默丢弃
		}
		item = stripPair(item) // 逐项剥各自引号
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		if !validToolSet[item] {
			dropped = append(dropped, item)
			continue
		}
		if seen[item] {
			continue // 去重
		}
		seen[item] = true
		ordered = append(ordered, item)
	}

	// 搜索互斥裁决:premium 与 web_search 同现,剔除 web_search(标记 dropped 供 WARN 记录)
	if seen["web_search"] && seen["web_search_premium"] {
		out := ordered[:0]
		for _, t := range ordered {
			if t == "web_search" {
				dropped = append(dropped, "web_search (conflict: web_search_premium takes precedence)")
				continue
			}
			out = append(out, t)
		}
		ordered = out
	}
	return ordered, dropped
}

// stripPair 成对包裹剥壳(剥一层)。
// 边界规则:引号类(first==last)若剥除后内部仍含同引号,说明是「逐项包裹」
// (如 "\"a\",\"b\"" 首尾恰为 " 但中间有残留引号),拒绝整体剥壳 → 逐项再剥。
func stripPair(s string) string {
	if len(s) < 2 {
		return s
	}
	runes := []rune(s)
	if len(runes) < 2 {
		return s
	}
	first, last := runes[0], runes[len(runes)-1]
	for _, p := range wrapPairs {
		if first == p[0] && last == p[1] {
			inner := runes[1 : len(runes)-1]
			if containsRune(inner, first) {
				return s // inner 仍含左包裹符 → 逐项包裹形态,拒绝整体剥
			}
			return string(inner)
		}
	}
	return s
}

// containsRune rune 切片查找
func containsRune(rs []rune, target rune) bool {
	for _, r := range rs {
		if r == target {
			return true
		}
	}
	return false
}
