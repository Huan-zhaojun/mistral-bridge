//go:build !windows

package proxydial

// registryProxy 非 Windows 平台无注册表概念,恒为空(容器场景依赖环境变量)
func registryProxy() string {
	return ""
}
