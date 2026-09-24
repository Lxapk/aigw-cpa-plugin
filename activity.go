package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// This file ports the growth-domain activity event & task infrastructure
// from workbuddy2api-panel (internal/upstream/report.go, tasks.go, events.go).

// ---- task API (growth domain) -------------------------------------------

const (
	taskListPath        = "/v2/activity/growth/tasks"
	taskAcceptPath      = "/v2/activity/growth/tasks/accept"
	taskClaimRewardPath = "/v2/activity/growth/tasks/reward/claim"
	reportPath          = "/v2/report"
)

// taskInfo mirrors upstream.Task.
type taskInfo struct {
	TaskCode    string `json:"task_code"`
	Title       string `json:"title,omitempty"`
	Credit      int64  `json:"credit,omitempty"`
	Energy      int64  `json:"energy,omitempty"`
	HasReward   bool   `json:"has_reward,omitempty"`
	TaskType    string `json:"task_type,omitempty"`
	Locked      bool   `json:"locked,omitempty"`
	Accepted    bool   `json:"accepted,omitempty"`
	Progress    int    `json:"progress,omitempty"`
	Goal        int    `json:"goal,omitempty"`
	Claimed     bool   `json:"claimed,omitempty"`
	RewardBuddy bool   `json:"reward_buddy,omitempty"`
}

// taskResult wraps the upstream response.
type taskResult struct {
	TaskCode string `json:"task_code"`
	OK       bool   `json:"ok"`
	Message  string `json:"message,omitempty"`
	Credits  int64  `json:"credits,omitempty"`
}

// listTasks fetches the full task catalogue for one credential.
func listTasks(ctx context.Context, creds *workBuddyCredentials) ([]taskInfo, error) {
	variant := variantForDomain(creds.Domain)
	body, status, errPost := workBuddyUpstream.postBilling(ctx, creds, variant, taskListPath, map[string]any{})
	if errPost != nil {
		return nil, errPost
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("task list HTTP %d", status)
	}

	var doc struct {
		Code int `json:"code"`
		Data *struct {
			Tasks []taskInfo `json:"tasks"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal != nil || doc.Data == nil {
		return nil, fmt.Errorf("task list: bad response")
	}
	return doc.Data.Tasks, nil
}

// acceptTask registers for a task (报名).
func acceptTask(ctx context.Context, creds *workBuddyCredentials, taskCodes []string) error {
	variant := variantForDomain(creds.Domain)
	body, status, errPost := workBuddyUpstream.postBilling(ctx, creds, variant, taskAcceptPath,
		map[string]any{"task_codes": taskCodes})
	if errPost != nil {
		return errPost
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("accept task HTTP %d", status)
	}
	var doc struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(body, &doc)
	if doc.Code != 0 {
		return fmt.Errorf("accept task: %s (code=%d)", doc.Msg, doc.Code)
	}
	return nil
}

// claimTaskReward claims the reward for a completed task.
func claimTaskReward(ctx context.Context, creds *workBuddyCredentials, taskCode string) (*taskResult, error) {
	variant := variantForDomain(creds.Domain)
	body, status, errPost := workBuddyUpstream.postBilling(ctx, creds, variant, taskClaimRewardPath,
		map[string]any{"task_code": taskCode})
	if errPost != nil {
		return nil, errPost
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("claim reward HTTP %d: %s", status, string(body))
	}
	var doc struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(body, &doc)
	if doc.Code != 0 {
		return &taskResult{TaskCode: taskCode, OK: false, Message: fmt.Sprintf("%s (code=%d)", doc.Msg, doc.Code)}, nil
	}
	return &taskResult{TaskCode: taskCode, OK: true, Message: "奖励已领取"}, nil
}

// ---- event reporting ----------------------------------------------------

// chatRequestEvent mimics the upstream chat_request_send event shape.
type chatRequestEvent struct {
	EventCode      string `json:"eventCode"`
	Timestamp      int64  `json:"timestamp"`
	Mode           string `json:"mode"`
	ConversationID string `json:"conversationId"`
	RequestID      string `json:"requestId"`
	InputLength    int    `json:"inputLength"`
	RequestModelID string `json:"requestModelId"`
	UserID         string `json:"userId"`
}

// reportChatActivity sends a chat_request_send event to the growth domain.
func reportChatActivity(ctx context.Context, creds *workBuddyCredentials) error {
	now := time.Now().UnixMilli()
	convID := fmt.Sprintf("wb-%d", now)
	ev := []chatRequestEvent{{
		EventCode:      "chat_request_send",
		Timestamp:      now,
		Mode:           "craft",
		ConversationID: convID,
		RequestID:      convID,
		InputLength:    12,
		RequestModelID: "deepseek-v4-flash",
		UserID:         creds.UID,
	}}
	raw, _ := json.Marshal(ev)
	variant := variantForDomain(creds.Domain)
	body, status, errPost := workBuddyUpstream.postBillingWithBody(ctx, creds, variant, reportPath, raw)
	if errPost != nil {
		return errPost
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("report activity HTTP %d: %s", status, string(body))
	}
	return nil
}

// postBillingWithVariantHeaders sends a billing-domain request with variant-aware headers.
func (c *workBuddyClient) postBillingWithVariantHeaders(
	ctx context.Context,
	creds *workBuddyCredentials,
	variant wbVariant,
	path string,
	body any,
) ([]byte, int, error) {
	if creds == nil || creds.AccessToken == "" {
		return nil, 0, fmt.Errorf("缺少访问令牌")
	}

	var payload []byte
	if body != nil {
		raw, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return nil, 0, errMarshal
		}
		payload = raw
	}

	endpoint := variant.apiBase() + path
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if errRequest != nil {
		return nil, 0, errRequest
	}
	applyWorkBuddyHeadersVariant(req.Header, creds, variant)

	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return nil, 0, fmt.Errorf("请求失败: %w", errDo)
	}
	defer resp.Body.Close()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取响应失败: %w", errRead)
	}
	return raw, resp.StatusCode, nil
}
