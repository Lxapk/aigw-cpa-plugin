package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// tool 结果必须指向某个真实存在的 tool_call。
//
// 上游的校验：
//
//	tool calls and tool results do not match, please start a new conversation and retry
//
// repackToolResults 只修「相邻性」，id 还得对得上。这是客户端最容易出错的地方。
func TestRepairToolCallIDsLinksMismatchedResult(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"messages":[
			{"role":"system","content":"s"},
			{"role":"user","content":"q"},
			{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"stale_id","content":"r"}
		]
	}`)

	out, errNorm := normaliseUpstreamBody(body)
	if errNorm != nil {
		t.Fatal(errNorm)
	}
	var doc struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if len(doc.Messages) != 4 {
		t.Fatalf("消息数 = %d, want 4", len(doc.Messages))
	}
	if got := doc.Messages[3].ToolCallID; got != "call_a" {
		t.Errorf("tool_call_id = %q, want call_a（应重新指向真实存在的调用）", got)
	}
}

// 完全缺失 tool_call_id 时要补上。
func TestRepairToolCallIDsFillsMissingID(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"messages":[
			{"role":"system","content":"s"},
			{"role":"assistant","tool_calls":[{"id":"call_x","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","content":"r"}
		]
	}`)

	out, errNorm := normaliseUpstreamBody(body)
	if errNorm != nil {
		t.Fatal(errNorm)
	}
	var doc struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	last := doc.Messages[len(doc.Messages)-1]
	if got := toolResultID(last); got != "call_x" {
		t.Errorf("tool_call_id = %q, want call_x", got)
	}
}

// 多个并行调用按顺序回填，不重复使用同一个 id。
func TestRepairToolCallIDsHandlesParallelCalls(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"messages":[
			{"role":"system","content":"s"},
			{"role":"assistant","tool_calls":[
				{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}},
				{"id":"c2","type":"function","function":{"name":"f","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"wrong","content":"r1"},
			{"role":"tool","content":"r2"}
		]
	}`)

	out, errNorm := normaliseUpstreamBody(body)
	if errNorm != nil {
		t.Fatal(errNorm)
	}
	var doc struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	ids := []string{}
	for _, m := range doc.Messages {
		if roleOf(m) == "tool" {
			ids = append(ids, toolResultID(m))
		}
	}
	if len(ids) != 2 {
		t.Fatalf("tool 结果数 = %d, want 2", len(ids))
	}
	if ids[0] != "c1" || ids[1] != "c2" {
		t.Errorf("ids = %v, want [c1 c2]（按顺序各占一个，不重复）", ids)
	}
}

// 已经正确的配对不能被改动。
func TestRepairToolCallIDsLeavesValidConversationAlone(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"messages":[
			{"role":"system","content":"s"},
			{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_a","content":"r"}
		]
	}`)

	out, errNorm := normaliseUpstreamBody(body)
	if errNorm != nil {
		t.Fatal(errNorm)
	}
	var original, after map[string]any
	_ = json.Unmarshal(body, &original)
	_ = json.Unmarshal(out, &after)
	before, _ := json.Marshal(original["messages"])
	now, _ := json.Marshal(after["messages"])
	if string(before) != string(now) {
		t.Errorf("合法对话被改动了\nbefore=%s\nafter =%s", before, now)
	}
}

// 「tool calls 不匹配」是请求本身的问题，不能算作账号的失败。
//
// 否则连续三次就把账号停掉，而换任何账号重试同样的对话都会同样失败——
// 最终表现就是 "no auth available"。
func TestRequestContentFailureIsNotCountedAgainstAccount(t *testing.T) {
	cases := []string{
		`{"error":{"message":"tool calls and tool results do not match, please start a new conversation and retry"}}`,
		`{"error":{"message":"request illegal"}}`,
		`{"error":{"message":"invalid_request_error: bad tool_call_id"}}`,
	}
	for _, body := range cases {
		upErr := classifyUpstream(502, []byte(body))
		if !isRequestContentFailure(upErr.Message) {
			t.Errorf("应识别为请求内容问题: %s", body)
		}
	}

	// 限流与配额不能被误判成请求内容问题。
	for _, body := range []string{
		`{"error":{"message":"您的使用量已超出频率限制，将在 2026-09-27 20:03:46 UTC+8 重置"}}`,
		`{"error":{"message":"余额不足"}}`,
	} {
		upErr := classifyUpstream(502, []byte(body))
		if isRequestContentFailure(upErr.Message) {
			t.Errorf("不应识别为请求内容问题: %s", body)
		}
	}
}

// executor 侧遇到请求内容问题时不改账号状态。
func TestExecutorSkipsAccountParkingForMalformedRequest(t *testing.T) {
	resetState()
	server := newUpstreamReturning(t, 502,
		`{"error":{"message":"tool calls and tool results do not match, please start a new conversation and retry","type":"server_error","code":"internal_server_error"}}`)
	defer server.Close()
	defer pointWorkBuddyAt(server.URL)()

	uid := "u-malformed"
	if _, errCall := executorExecute(buildExecutorRequestForUID(t, uid)); errCall != nil {
		t.Fatal(errCall)
	}

	lane := state.pool.lanes[laneKey("codebuddy", uid)]
	if lane == nil {
		// 完全没有记录也是可接受的：说明根本没算作失败。
		return
	}
	if lane.ConsecutiveErrors != 0 {
		t.Errorf("ConsecutiveErrors = %d，请求内容问题不应累积（三次就会停号）", lane.ConsecutiveErrors)
	}
	if !lane.CooldownUntil.IsZero() && nowBefore(lane.CooldownUntil) {
		t.Errorf("账号被冷却了（until=%v），请求内容问题不该影响账号", lane.CooldownUntil)
	}
	if len(lane.ModelCooldowns) != 0 {
		t.Errorf("模型被冷却了：%v", lane.ModelCooldowns)
	}
}

// newUpstreamReturning 起一个固定返回指定状态与正文的上游。
func newUpstreamReturning(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func nowBefore(ts time.Time) bool { return time.Now().Before(ts) }
