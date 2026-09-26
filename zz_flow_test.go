package main

import (
	"encoding/json"
	"testing"
	"time"
)

// 走真实的响应拦截路径（而非直接调 pool），确认模型级冷却生效。
func TestInterceptResponseParksModelNotAccount(t *testing.T) {
	resetState()

	// 模拟一次请求的上下文
	requestID := "req-1"
	inflight.put(requestID, requestContext{
		Provider:       "codebuddy",
		Model:          "deepseek-v4.1-flash",
		RequestedModel: "deepseek-v4.1-flash",
		UID:            "u-intercepted",
		Stream:         false,
	})

	body := []byte(`{"error":{"message":"您的使用量已超出频率限制，将在 2030-01-01 12:00:00 UTC+8 重置，您也可以切换其他模型继续使用。","type":"server_error","code":"internal_server_error"}}`)
	payload, _ := json.Marshal(map[string]any{
		"RequestID":       requestID,
		"StatusCode":      502,
		"Body":            body,
		"Model":           "deepseek-v4.1-flash",
		"RequestedModel":  "deepseek-v4.1-flash",
		"RequestHeaders":  map[string][]string{"X-WorkBuddy-Provider": {"codebuddy"}, "X-WorkBuddy-Auth-Id": {"u-intercepted"}, "X-WorkBuddy-Model": {"deepseek-v4.1-flash"}},
		"ResponseHeaders": map[string][]string{},
	})

	if _, errCall := interceptResponse(payload); errCall != nil {
		t.Fatal(errCall)
	}

	lane := state.pool.lanes[laneKey("codebuddy", "u-intercepted")]
	if lane == nil {
		t.Fatal("车道未被创建")
	}
	t.Logf("ModelCooldowns = %v", lane.ModelCooldowns)
	t.Logf("CooldownUntil = %v", lane.CooldownUntil)

	if len(lane.ModelCooldowns) == 0 {
		t.Error("模型级冷却未生效：限流应只停 deepseek-v4.1-flash")
	}
	if !lane.CooldownUntil.IsZero() && time.Now().Before(lane.CooldownUntil) {
		t.Error("账号级冷却不应被触发")
	}
}
