// 错误归一化(§12.1):上游双 schema 错误 → OAI 标准错误模板。
package convert

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// OaiError 标准错误响应体(OpenAI 形状)
type OaiError struct {
	Error OaiErrorDetail `json:"error"`
}
type OaiErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   any    `json:"param"`
	Code    any    `json:"code"`
}

// WriteOaiError 以 OAI 模板回写错误(param/code 可为 nil)
func WriteOaiError(w http.ResponseWriter, status int, msg, typ string, param, code any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	out := OaiError{Error: OaiErrorDetail{Message: msg, Type: typ, Param: param, Code: code}}
	_ = json.NewEncoder(w).Encode(out)
}

// NormalizeUpstreamError 上游错误 body + 状态码 → OAI 状态码 + OAI 错误体。
// upstreamBody: 上游错误响应原文;status: 上游 HTTP 状态。
func NormalizeUpstreamError(status int, upstreamBody []byte) (int, OaiError) {
	trimmed := strings.TrimSpace(string(upstreamBody))

	// 401 鉴权错误(schema 1:{"detail":"Invalid API Key"})
	if status == http.StatusUnauthorized {
		var v struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(upstreamBody, &v) == nil && v.Detail != "" {
			return http.StatusUnauthorized, OaiError{Error: OaiErrorDetail{
				Message: v.Detail, Type: "invalid_request_error", Param: nil, Code: "invalid_api_key"}}
		}
	}

	// schema 2:{"object":"Error","message":..., "type":..., "code":...}
	var bizErr struct {
		Object  string          `json:"object"`
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
		Detail  json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(upstreamBody, &bizErr) == nil && bizErr.Object == "Error" {
		msg := bizErr.Message
		// pydantic detail 数组(422 等):取 detail[0].msg
		if msg == "" && len(bizErr.Detail) > 0 && bizErr.Detail[0] == '[' {
			var dets []struct {
				Msg  string          `json:"msg"`
				Type string          `json:"type"`
				Loc  json.RawMessage `json:"loc"`
			}
			if json.Unmarshal(bizErr.Detail, &dets) == nil && len(dets) > 0 {
				msg = dets[0].Msg
				if msg == "" {
					msg = dets[0].Type
				}
			}
		}
		if msg == "" {
			msg = trimmed
		}
		// 状态透传:400/404/422 一律回 400(OpenAI 风格)
		outStatus := status
		if status >= 400 && status < 500 && status != http.StatusUnauthorized && status != http.StatusPaymentRequired && status != http.StatusForbidden {
			outStatus = http.StatusBadRequest
		}
		var code any
		if len(bizErr.Code) > 0 {
			var c any
			_ = json.Unmarshal(bizErr.Code, &c)
			code = c
		}
		return outStatus, OaiError{Error: OaiErrorDetail{
			Message: msg, Type: orDefault(bizErr.Type, "invalid_request_error"),
			Param: nil, Code: code}}
	}

	// 默认兜底:不分析原文,脱敏后回传有限前缀
	snippet := trimmed
	if len(snippet) > 500 {
		snippet = snippet[:500] + "…"
	}
	outStatus := status
	if status >= 500 {
		outStatus = http.StatusBadGateway
	}
	return outStatus, OaiError{Error: OaiErrorDetail{
		Message: fmt.Sprintf("upstream error (status %d): %s", status, snippet),
		Type:    "api_error", Param: nil, Code: nil}}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
