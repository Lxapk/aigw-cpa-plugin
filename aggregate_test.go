package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---- non-stream fallback (discovered in live testing) -------------------

// TestIsNonStreamUnsupported pins the upstream rejection observed live:
//
//	{"code":11101,"msg":"Non-stream chat request is currently not supported"}
func TestIsNonStreamUnsupported(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"code":11101,"msg":"Non-stream chat request is currently not supported"}`, true},
		{200, `{"code":11101}`, true},
		{400, `{"code":40001,"msg":"other"}`, false},
		{400, `not json`, false},
		{400, ``, false},
	}
	for _, c := range cases {
		if got := isNonStreamUnsupported(c.status, []byte(c.body)); got != c.want {
			t.Errorf("isNonStreamUnsupported(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestForceStreamSetsFlagAndUsage(t *testing.T) {
	out, err := forceStream([]byte(`{"model":"m","stream":false,"messages":[]}`))
	if err != nil {
		t.Fatalf("forceStream: %v", err)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatalf("bad json: %v", errUnmarshal)
	}
	if doc["stream"] != true {
		t.Fatalf("stream = %v, want true", doc["stream"])
	}
	if _, ok := doc["stream_options"]; !ok {
		t.Error("stream_options should be added so usage is reported")
	}
	if _, ok := doc["messages"]; !ok {
		t.Error("messages must be preserved")
	}
}

// TestAggregateStreamToCompletion folds frames into one response.
func TestAggregateStreamToCompletion(t *testing.T) {
	frames := [][]byte{
		[]byte(`{"id":"c1","object":"chat.completion.chunk","created":123,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`),
		[]byte(`{"id":"c1","choices":[{"index":0,"delta":{"content":"你"}}]}`),
		[]byte(`{"id":"c1","choices":[{"index":0,"delta":{"content":"好"}}]}`),
		[]byte(`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`),
	}
	out := aggregateStreamToCompletion(frames, "deepseek-v4.1-flash")

	var doc struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatalf("bad json: %v", errUnmarshal)
	}
	if doc.ID != "c1" || doc.Model != "deepseek-v4.1-flash" {
		t.Errorf("envelope = %+v", doc)
	}
	if doc.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", doc.Object)
	}
	if len(doc.Choices) != 1 {
		t.Fatalf("choices = %+v", doc.Choices)
	}
	if doc.Choices[0].Message.Content != "你好" {
		t.Errorf("content = %q, want 你好", doc.Choices[0].Message.Content)
	}
	if doc.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q", doc.Choices[0].Message.Role)
	}
	if doc.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", doc.Choices[0].FinishReason)
	}
	if doc.Usage.TotalTokens != 7 {
		t.Errorf("usage.total_tokens = %d, want 7", doc.Usage.TotalTokens)
	}
}

// TestAggregateStreamPropagatesError surfaces a mid-stream error frame.
func TestAggregateStreamPropagatesError(t *testing.T) {
	out := aggregateStreamToCompletion([][]byte{
		[]byte(`{"error":{"message":"quota exhausted","type":"insufficient_quota","code":"insufficient_quota"}}`),
	}, "m")
	if !strings.Contains(string(out), "quota exhausted") {
		t.Fatalf("error not propagated: %s", out)
	}
	if !strings.Contains(string(out), `"error"`) {
		t.Fatalf("payload is not an error shape: %s", out)
	}
}

// TestAggregateStreamWithoutFramesIsAnError avoids reporting an empty success.
func TestAggregateStreamWithoutFramesIsAnError(t *testing.T) {
	out := aggregateStreamToCompletion(nil, "m")
	if !strings.Contains(string(out), `"error"`) {
		t.Fatalf("empty stream must report an error, got %s", out)
	}
}

// ---- account dedupe -----------------------------------------------------

// TestDedupeAccountsCollapsesSameUid covers the doubled panel row seen live:
// host.auth.list returned the credential both from its file and from the
// runtime index.
func TestDedupeAccountsCollapsesSameUid(t *testing.T) {
	in := []workBuddyAccount{
		{AuthIndex: "codebuddy-a.json", UID: "u-1", Label: "codebuddy"},
		{AuthIndex: "runtime-idx-9", UID: "u-1", Label: "WorkBuddy u-1", Nickname: "Nick"},
	}
	out := dedupeAccounts(in)
	if len(out) != 1 {
		t.Fatalf("dedupeAccounts = %+v, want 1 entry", out)
	}
	// The richer record wins: it carries a nickname, so it scores higher.
	if out[0].Label != "WorkBuddy u-1" {
		t.Fatalf("kept %+v, want the more informative record", out[0])
	}
}

func TestDedupeAccountsKeepsDistinctUids(t *testing.T) {
	in := []workBuddyAccount{
		{AuthIndex: "a.json", UID: "u-1"},
		{AuthIndex: "b.json", UID: "u-2"},
	}
	if out := dedupeAccounts(in); len(out) != 2 {
		t.Fatalf("distinct uids must survive: %+v", out)
	}
}

func TestDedupeAccountsFallsBackToAuthIndex(t *testing.T) {
	// Without a uid, the auth index decides.
	in := []workBuddyAccount{
		{AuthIndex: "same.json"},
		{AuthIndex: "same.json", Label: "With Label"},
	}
	out := dedupeAccounts(in)
	if len(out) != 1 {
		t.Fatalf("out = %+v", out)
	}
	if out[0].Label != "With Label" {
		t.Fatalf("kept %+v", out[0])
	}
}
