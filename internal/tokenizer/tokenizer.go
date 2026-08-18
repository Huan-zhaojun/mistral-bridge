// Package tokenizer GLM-5.2 BPE 计数器:go:embed 资产 + 懒加载,
// 用于上游 usage 归 0 时的精确兜底(研究阶段与上游计费 ±1 对齐)。
// 加载失败仅降级(Count 返回 error),不影响主流程。
package tokenizer

import (
	_ "embed"
	"sync"
)

//go:embed glm52.json
var glm52JSON []byte

var (
	once    sync.Once
	model   *bpeModel
	loadErr error
)

// ensureLoaded 懒加载(幂等)
func ensureLoaded() (*bpeModel, error) {
	once.Do(func() {
		m, err := loadBPEModel(glm52JSON)
		if err != nil {
			loadErr = err
			return
		}
		model = m
	})
	return model, loadErr
}

// Count 统计文本 token 数;不可用时返回 (0, error)
func Count(text string) (int, error) {
	m, err := ensureLoaded()
	if err != nil {
		return 0, err
	}
	return m.count(text), nil
}

// Enabled 报告 tokenizer 是否可用
func Enabled() bool {
	_, err := ensureLoaded()
	return err == nil
}

// LoadError 返回加载错误(用于启动日志状态输出)
func LoadError() error {
	_, err := ensureLoaded()
	return err
}
