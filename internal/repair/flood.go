// Package repair 转换侧修复件③:tool_choice=required 调用洪水治理(§12.2-③)。
// 机理:required 在上游是文法层约束,模型无法切回纯文本只能不停产 call,
// 单工具场景洪水 = 完全相同的 call 直拷到 max_tokens 顶满。
// 桥策略:流内按函数名去重保留首次;再见同名 call(第二个) → 立即断开上游(early-abort),
// 下游照常收 finish_reason=tool_calls + [DONE]。客户端零感知,上游配额止血。
package repair

// FloodGuard required 洪水治理器(仅 required 路径且上游流式生效)。
// 线程安全由调用方保证(每流一实例)。
type FloodGuard struct {
	seen      map[string]bool // 已见过 name(首个该名 call 会放行)
	aborted   bool            // 是否已触发熔断(上游应已断开)
	firstKept int             // 放行给下游的 call 数
}

// NewFloodGuard 构造
func NewFloodGuard() *FloodGuard {
	return &FloodGuard{seen: map[string]bool{}}
}

// ObserveCallStart 当 function.call 的 name 在流中首次确立时调用。
// 返回 true = 放行该 call;false = 第二个同名 call,触发熔断(调用方应立即断开上游)。
func (g *FloodGuard) ObserveCallStart(name string) bool {
	if g.aborted {
		return false
	}
	if g.seen[name] {
		g.aborted = true
		return false
	}
	g.seen[name] = true
	g.firstKept++
	return true
}

// ObserveStreamDone 流自然结束(未触发熔断)。供日志区分是正常的 required 还是真的洪水。
func (g *FloodGuard) Aborted() bool  { return g.aborted }
func (g *FloodGuard) KeptCount() int { return g.firstKept }
func (g *FloodGuard) SeenCount() int { return len(g.seen) }
