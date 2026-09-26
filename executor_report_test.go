package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 上游限流时，executor 必须自己上报失败并只停该模型。
//
// 这是本轮修复的核心。CPA 在 executor 返回错误时不会调用响应拦截器
// （它记的是 conductor_execution.go "upstream execution failed"），所以：
//
//   - 只在拦截器里分类 → 限流永远不被应用，同一个被限流的账号会被反复重试
//   - 在 executor 里上报 → 冷却真正生效，且只作用于出问题的模型
func TestExecutorStreamReportsThrottleAndParksOnlyThatModel(t *testing.T) {
	resetState()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 上游把限流写成 200 流里的一帧，然后结束。
		_, _ = w.Write([]byte(`data: {"error":{"message":"您的使用量已超出频率限制，将在 2026-09-27 20:03:46 UTC+8 重置，您也可以切换其他模型继续使用。","type":"server_error","code":"internal_server_error"}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	defer pointWorkBuddyAt(server.URL)()

	closed := make(chan string, 1)
	orig := hostCallFunc
	hostCallFunc = func(method string, payload any) (json.RawMessage, error) {
		if method == "host.stream.close" {
			doc, _ := json.Marshal(payload)
			var fields map[string]any
			_ = json.Unmarshal(doc, &fields)
			select {
			case closed <- fmt.Sprint(fields["error"]):
			default:
			}
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	defer func() { hostCallFunc = orig }()

	uid := "u-throttled"
	if _, errCall := executorExecuteStream(buildStreamExecutorRequestForUID(t, "s-throttle", uid)); errCall != nil {
		t.Fatal(errCall)
	}

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("流未关闭")
	}

	lane := waitForLane(t, uid, 3*time.Second)
	if lane == nil {
		t.Fatal("车道未被创建；说明 executor 侧没有上报失败")
	}

	now := time.Now()
	if _, cooled := lane.modelCooled("deepseek-v4.1-flash", now); !cooled {
		t.Errorf("deepseek-v4.1-flash 应被停；ModelCooldowns=%v", lane.ModelCooldowns)
	}
	for _, other := range []string{"deepseek-v4-pro", "kimi-k3"} {
		if _, cooled := lane.modelCooled(other, now); cooled {
			t.Errorf("%s 不应被牵连；限流是模型级的", other)
		}
	}
	if !lane.CooldownUntil.IsZero() && now.Before(lane.CooldownUntil) {
		t.Error("账号级冷却不应被触发")
	}
}

// 非流式路径同样要上报：它返回 errorEnvelope，拦截器看不到。
func TestExecutorNonStreamReportsThrottle(t *testing.T) {
	resetState()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"您的使用量已超出频率限制，将在 2026-09-27 20:03:46 UTC+8 重置，您也可以切换其他模型继续使用。","type":"server_error","code":"internal_server_error"}}`))
	}))
	defer server.Close()
	defer pointWorkBuddyAt(server.URL)()

	uid := "u-nonstream"
	if _, errCall := executorExecute(buildExecutorRequestForUID(t, uid)); errCall != nil {
		t.Fatal(errCall)
	}

	lane := waitForLane(t, uid, 3*time.Second)
	if lane == nil {
		t.Fatal("车道未被创建；说明非流式 executor 没有上报失败")
	}
	if _, cooled := lane.modelCooled("deepseek-v4.1-flash", time.Now()); !cooled {
		t.Errorf("模型应被停；ModelCooldowns=%v", lane.ModelCooldowns)
	}
	if !lane.CooldownUntil.IsZero() && time.Now().Before(lane.CooldownUntil) {
		t.Error("账号级冷却不应被触发")
	}
}

// 配额耗尽是账号级的，应停整个账号。
func TestExecutorReportsQuotaAsAccountLevel(t *testing.T) {
	resetState()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"余额不足，请充值"}}`))
	}))
	defer server.Close()
	defer pointWorkBuddyAt(server.URL)()

	uid := "u-quota"
	if _, errCall := executorExecute(buildExecutorRequestForUID(t, uid)); errCall != nil {
		t.Fatal(errCall)
	}

	lane := waitForLane(t, uid, 3*time.Second)
	if lane == nil {
		t.Fatal("车道未被创建")
	}
	if lane.CooldownUntil.IsZero() || !time.Now().Before(lane.CooldownUntil) {
		t.Error("配额耗尽应触发账号级冷却")
	}
}

// buildExecutorRequestForUID 构造带指定 uid 的非流式 executor 调用体。
func buildExecutorRequestForUID(t *testing.T, uid string) []byte {
	t.Helper()
	return buildExecutorRequestWithStorage(t, uid, false, "")
}

// buildStreamExecutorRequestForUID 构造带指定 uid 的流式 executor 调用体。
func buildStreamExecutorRequestForUID(t *testing.T, streamID, uid string) []byte {
	t.Helper()
	return buildExecutorRequestWithStorage(t, uid, true, streamID)
}

func buildExecutorRequestWithStorage(t *testing.T, uid string, stream bool, streamID string) []byte {
	t.Helper()
	storage, _ := json.Marshal(map[string]any{
		"accessToken": "[REDACTED]",
		"domain":      "cn",
		"uid":         uid,
	})
	fields := map[string]any{
		"OriginalRequest": []byte(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[]}`),
		"StorageJSON":     storage,
		"Stream":          stream,
	}
	if streamID != "" {
		fields["stream_id"] = streamID
	}
	payload, _ := json.Marshal(fields)
	return payload
}

// waitForLane waits for the pool to learn about a credential.
//
// The streaming path reports from a background goroutine (the executor returns
// before the upstream has been read, so the host can start draining), so the lane
// may not exist yet when the call returns.
func waitForLane(t *testing.T, uid string, timeout time.Duration) *credentialLane {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if lane := state.pool.lanes[laneKey("codebuddy", uid)]; lane != nil {
			return lane
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}
