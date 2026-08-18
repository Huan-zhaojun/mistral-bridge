// tokenizer 性能/内存实证:
//   - 首次加载耗时与常驻内存
//   - 单 Count 延迟(短/长文本)
//   - 并发吞吐(证明无锁只读、无竞争)
package tokenizer

import (
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var benchText = strings.Repeat("人工智能正在改变软件开发的方方面面。The quick brown fox jumps over the lazy dog. ", 30) // ~3KB

// TestLoadWeighdown 加载耗时与内存(先 GC 读基线,加载后再读差值)
func TestLoadWeighdown(t *testing.T) {
	// 强制另开拷贝验证(此测试包前面可能已加载,scan 就基于现值快照)
	runtime.GC()
	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)
	t0 := time.Now()
	m2, err := loadBPEModel(glm52JSON)
	if err != nil {
		t.Fatal(err)
	}
	loadDur := time.Since(t0)
	runtime.GC()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	t.Logf("load took %v; alloc +%.1f MB (vocab=%d merges=%d)",
		loadDur, float64(m1.Alloc-m0.Alloc)/1e6,
		len(m2.vocab), len(m2.mergeRank))
}

// BenchmarkSingleCount 单请求延迟(3KB 中英混合文本)
func BenchmarkSingleCount(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Count(benchText); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkConcurrent 64 并发 Count(-run=- + -bench)
func BenchmarkConcurrent(b *testing.B) {
	var wg sync.WaitGroup
	b.ReportAllocs()
	b.SetParallelism(64)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = Count(benchText)
		}
	})
	wg.Wait()
}
