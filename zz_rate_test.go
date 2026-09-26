package main

import (
	"encoding/json"
	"testing"
)

// 上游以 502/server_error 返回限流时，必须识别为 failureRate。
//
// 这是真实响应：
//
//	HTTP 502
//	{"error":{"message":"您的使用量已超出频率限制，将在 2026-09-27 20:03:46
//	 UTC+8 重置，您也可以切换其他模型继续使用。","type":"server_error",
//	 "code":"internal_server_error"}}
//
// 之前它落入 failureTransient：连续 3 次按 errorCooldownMillis 冷却整个账号，
// 而上游的文案本身就在提示「可以切换其他模型」。
func TestRateLimitDetectedFromChineseMessage(t *testing.T) {
	body := []byte(`{"error":{"message":"您的使用量已超出频率限制，将在 2026-09-27 20:03:46 UTC+8 重置，您也可以切换其他模型继续使用。","type":"server_error","code":"internal_server_error"}}`)

	got := classifyUpstream(502, body)
	if got.Kind != failureRate {
		t.Fatalf("Kind = %v, want failureRate；否则会按硬错误冷却整个账号", got.Kind)
	}
}

// 其他限流写法也要识别。
func TestRateLimitPhraseVariants(t *testing.T) {
	cases := []string{
		`{"error":{"message":"您的使用量已超出频率限制"}}`,
		`{"error":{"message":"请求过于频繁，请稍后再试"}}`,
		`{"error":{"message":"Rate limit exceeded"}}`,
		`{"error":{"message":"Too many requests"}}`,
		`{"error":{"type":"rate_limit_error","message":"slow down"}}`,
	}
	for _, body := range cases {
		if got := classifyUpstream(502, []byte(body)); got.Kind != failureRate {
			t.Errorf("body %s -> Kind %v, want failureRate", body, got.Kind)
		}
	}
}

// 真正的配额耗尽仍应识别为 failureQuota（两者对余额的含义相反）。
func TestQuotaStillDetected(t *testing.T) {
	cases := []string{
		`{"error":{"message":"余额不足，请充值"}}`,
		`{"error":{"message":"Insufficient quota"}}`,
	}
	for _, body := range cases {
		if got := classifyUpstream(402, []byte(body)); got.Kind != failureQuota {
			t.Errorf("body %s -> Kind %v, want failureQuota", body, got.Kind)
		}
	}
}

// 标准 429 与 OpenAI 词汇仍按原样工作。
func TestStandardRateLimitStillWorks(t *testing.T) {
	got := classifyUpstream(429, []byte(`{"error":{"message":"slow down"}}`))
	if got.Kind != failureRate {
		t.Errorf("429 -> %v, want failureRate", got.Kind)
	}
}

// 无意义的 5xx 仍应是 transient，不能被误判成限流。
func TestPlainServerErrorStaysTransient(t *testing.T) {
	cases := []string{
		`{"error":{"message":"internal server error"}}`,
		`{"error":{"message":""}}`,
		``,
	}
	for _, body := range cases {
		if got := classifyUpstream(502, []byte(body)); got.Kind != failureTransient {
			t.Errorf("body %q -> %v, want failureTransient", body, got.Kind)
		}
	}
}

// 认证错误优先级高于文本。
func TestAuthBeatsMessage(t *testing.T) {
	got := classifyUpstream(401, []byte(`{"error":{"message":"频率限制"}}`))
	if got.Kind != failureAuth {
		t.Errorf("401 -> %v, want failureAuth", got.Kind)
	}
}

var _ = json.Marshal
