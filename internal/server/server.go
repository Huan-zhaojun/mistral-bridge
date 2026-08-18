// Package server HTTP 路由 / 中间件 / CORS / 静态端点。
// /v1/chat/completions 的实际业务在 chatHandler(见 convert package)中实现,
// 这里只管路由、OPTIONS 预检、body 上限与静态端点。
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"mistral-bridge/internal/config"
)

// MaxRequestBody 入站请求体硬上限(内置护栏,不开放配置)。
// 256MB:合法负载(1M 上下文 tool 历史 + 多张 base64 图)有 10 倍余量,超限 413。
const MaxRequestBody = 256 << 20

// New 构造路由 handler(CORS + body 上限已含);chat 业务处理器由调用方注入,本包不依赖 convert。
func New(cfg *config.Config, logger *slog.Logger, chat http.Handler) http.Handler {
	mux := http.NewServeMux()

	// 纯 liveness:存活语义,不探上游,恒 200
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// 静态模型白名单:仅 glm-5-2(别名 zai-glm-5-2 归一并列,discovery 友好)
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(modelsJSON)
	})

	// 唯一的业务转换端点
	mux.Handle("POST /v1/chat/completions", withBodyLimit(MaxRequestBody, chat))

	// 全部路径过 CORS;浏览器类客户端(含 OPTIONS 预检)一律放开(内网部署前提)
	return withCORS(mux)
}

// modelsJSON /v1/models 的静态响应体(OAI 形状)
var modelsJSON = json.RawMessage(`{
  "object": "list",
  "data": [
    {
      "id": "glm-5-2",
      "object": "model",
      "created": 0,
      "owned_by": "mistral-bridge",
      "aliases": ["zai-glm-5-2"]
    }
  ]
}`)

// withBodyLimit 用 MaxBytesReader 包装,超限时解码前即 413。
func withBodyLimit(limit int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// withCORS 放开全部域(部署在 docker 内网,潜在浏览器调用方).OPTIONS 一律 204 直接应答。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
