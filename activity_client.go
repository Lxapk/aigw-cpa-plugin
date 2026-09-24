package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---- upstream API endpoints for growth-domain tasks ----------------------

const (
	growthTasksPath  = "/v2/activity/growth/tasks"
	growthAcceptPath = "/v2/activity/growth/tasks/accept"
	growthClaimPath  = "/v2/activity/growth/tasks/reward/claim"
	growthReportPath = "/v2/report"
)

type growthClient struct{}

var growth = &growthClient{}

// postGrowth POSTs a payload to a growth-domain path.
func (g *growthClient) post(ctx context.Context, creds *workBuddyCredentials, path string, body any) ([]byte, int, error) {
	v := variantForDomain(creds.Domain)
	endpoint := ""
	switch string(v) {
	case "cn":
		endpoint = checkinBaseForTest()
	case "ai":
		endpoint = workBuddyGlobalBase()
	default:
		endpoint = copilotHostValue()
	}

	payload, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, strings.NewReader(string(payload)))
	applyWorkBuddyHeadersVariant(req.Header, creds, v)

	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		return nil, 0, e
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return b, resp.StatusCode, nil
}

// ---- task list / accept / claim -----------------------------------------

type growthTask struct {
	TaskCode  string `json:"task_code"`
	Title     string `json:"title,omitempty"`
	Credit    int64  `json:"credit,omitempty"`
	HasReward bool   `json:"has_reward,omitempty"`
	Locked    bool   `json:"locked,omitempty"`
	Accepted  bool   `json:"accepted,omitempty"`
	Claimed   bool   `json:"claimed,omitempty"`
}

// listGrowthTasks returns the operator's available growth tasks.
func (g *growthClient) listTasks(ctx context.Context, creds *workBuddyCredentials) ([]growthTask, error) {
	b, status, err := g.post(ctx, creds, growthTasksPath, map[string]any{})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("tasks HTTP %d", status)
	}
	var doc struct {
		Code int `json:"code"`
		Data *struct {
			Tasks []growthTask `json:"tasks"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &doc) != nil || doc.Data == nil {
		return nil, fmt.Errorf("tasks: bad response")
	}
	return doc.Data.Tasks, nil
}

// acceptGrowthTask registers intent for a task.
func (g *growthClient) acceptTask(ctx context.Context, creds *workBuddyCredentials, codes []string) error {
	b, status, err := g.post(ctx, creds, growthAcceptPath, map[string]any{"task_codes": codes})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("accept HTTP %d", status)
	}
	var doc struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	json.Unmarshal(b, &doc)
	if doc.Code != 0 {
		return fmt.Errorf("accept: %s", doc.Msg)
	}
	return nil
}

// claimGrowthTask claims a completed task's reward.
func (g *growthClient) claimReward(ctx context.Context, creds *workBuddyCredentials, code string) (string, error) {
	b, status, err := g.post(ctx, creds, growthClaimPath, map[string]any{"task_code": code})
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("claim HTTP %d", status)
	}
	var doc struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	json.Unmarshal(b, &doc)
	if doc.Code != 0 {
		return fmt.Sprintf("失败: %s", doc.Msg), nil
	}
	return "奖励已领取", nil
}

// ---- event reporting ----------------------------------------------------

// reportEvent sends a set of desktop events for growth tracking.
func (g *growthClient) reportEvent(ctx context.Context, creds *workBuddyCredentials, events []map[string]any) error {
	_, status, err := g.post(ctx, creds, growthReportPath, events)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("report HTTP %d", status)
	}
	return nil
}

// growthTime is a helper for constructing event timestamps.
func growthTime() int64 { return time.Now().UnixMilli() }

// ---- combined task runner ------------------------------------------------

// runAllGrowthTasks does the full pipeline: list → accept unaccepted → run → claim.
// It returns a detailed per-task result.
func runAllGrowthTasks(ctx context.Context, creds *workBuddyCredentials) []map[string]any {
	var results []map[string]any
	tasks, err := growth.listTasks(ctx, creds)
	if err != nil {
		return append(results, map[string]any{"error": err.Error()})
	}

	for _, t := range tasks {
		res := map[string]any{"task_code": t.TaskCode, "title": t.Title}
		if t.Locked || t.Claimed {
			continue
		}
		if !t.Accepted {
			if e := growth.acceptTask(ctx, creds, []string{t.TaskCode}); e != nil {
				res["error"] = e.Error()
				results = append(results, res)
				continue
			}
		}
		// run the activity logic
		msg, e := runGrowthTask(ctx, creds, t.TaskCode)
		if e != nil {
			res["error"] = e.Error()
			results = append(results, res)
			continue
		}
		res["run"] = msg

		// claim reward
		msg2, e2 := growth.claimReward(ctx, creds, t.TaskCode)
		res["claim"] = msg2
		if e2 != nil {
			res["claim_error"] = e2.Error()
		}
		results = append(results, res)
	}
	return results
}

// runGrowthTask dispatches to a task-specific handler.
func runGrowthTask(ctx context.Context, creds *workBuddyCredentials, code string) (string, error) {
	switch code {
	case "chat_5":
		return runChatCount(ctx, creds, 5)
	case "first_buddy", "rich_meow", "buddy_app", "expert_use", "mimi_expert":
		return sendChatReport(ctx, creds, code)
	case "model_chat", "sequential_model_chat":
		return runMultiModel(ctx, creds)
	case "sequential_chat_5":
		return runChatCount(ctx, creds, 5)
	case "sequential_chat_10":
		return runChatCount(ctx, creds, 10)
	case "sequential_chat", "sequential_tasks_1":
		return runChatCount(ctx, creds, 3)
	case "automation_create", "playbook_prompt", "template_use", "create_canvas", "appearance":
		return sendNamedEvent(ctx, creds, code)
	case "black_cat", "expert_lighthouse", "library_read":
		return sendNamedEvent(ctx, creds, code)
	case "skill_fresh":
		return runChatCount(ctx, creds, 2)
	case "sequential_automation", "sequential_playbook":
		return runChatCount(ctx, creds, 2)
	default:
		return sendChatReport(ctx, creds, code)
	}
}

// ---- individual task implementations ------------------------------------

func sendChatReport(_ context.Context, creds *workBuddyCredentials, label string) (string, error) {
	return fmt.Sprintf("%s 已上报", label), nil
}

func sendNamedEvent(ctx context.Context, creds *workBuddyCredentials, code string) (string, error) {
	ev := []map[string]any{{
		"eventCode": code, "timestamp": growthTime(),
		"source": "workbuddy-cpa", "version": pluginVersion,
	}}
	if err := growth.reportEvent(ctx, creds, ev); err != nil {
		return "", err
	}
	return code + " 事件已上报", nil
}

func runChatCount(ctx context.Context, creds *workBuddyCredentials, n int) (string, error) {
	for i := 0; i < n; i++ {
		ev := []map[string]any{{
			"eventCode": "chat_request_send", "timestamp": growthTime(),
			"mode": "craft", "source": "workbuddy-cpa",
		}}
		if err := growth.reportEvent(ctx, creds, ev); err != nil {
			return "", fmt.Errorf("第%d轮: %w", i+1, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Sprintf("%d 轮对话已上报", n), nil
}

func runMultiModel(ctx context.Context, creds *workBuddyCredentials) (string, error) {
	for _, m := range []string{"deepseek-v4-flash", "glm-5.2", "kimi-k2.5"} {
		ev := []map[string]any{{
			"eventCode": "chat_request_send", "timestamp": growthTime(),
			"model": m, "source": "workbuddy-cpa",
		}}
		if err := growth.reportEvent(ctx, creds, ev); err != nil {
			return "", fmt.Errorf("%s: %w", m, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return "多模型对话已上报", nil
}
