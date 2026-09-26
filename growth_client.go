package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// This file implements the domestic 成长任务 (growth task) engine.
//
// Ported from the reference implementation's wb_tasks.py, which reverse
// engineered the desktop client's growth-centre traffic. The endpoints live on
// the chat host and only exist for the domestic realm:
//
//	GET  /v2/activity/growth/tasks              task list and progress
//	POST /v2/activity/growth/tasks/accept       batch accept
//	POST /activity/growth/tasks/{code}/claim    collect the reward
//	POST /v2/report                             event report (progress)
//
// The lifecycle is accept -> report -> claim, and each stage has a way to fail
// silently if the previous one did not land:
//
//   - Reporting against a task that was never accepted accumulates nothing, so
//     accept failures must be visible and retried.
//   - Claiming a task whose progress has not been booked yet answers
//     "task not completed", so the progress must be re-read before claiming.
//   - Expert/team events are de-duplicated upstream by (eventCode, id), so
//     repeating one id never advances progress past the first report.

const (
	// workBuddyGrowthTasksPath lists the growth tasks.
	workBuddyGrowthTasksPath = "/v2/activity/growth/tasks"
	// workBuddyGrowthAcceptPath accepts tasks in batches.
	workBuddyGrowthAcceptPath = "/v2/activity/growth/tasks/accept"
	// workBuddyReportPath receives event batches.
	workBuddyReportPath = "/v2/report"
	// workBuddyEnergyPath reports the energy balance.
	workBuddyEnergyPath = "/v2/activity/growth/energy"
	// workBuddyStreakPath reports the consecutive check-in streak.
	workBuddyStreakPath = "/activity/growth/streak"
	// workBuddyTravelStatusPath reports the cat travel state.
	workBuddyTravelStatusPath = "/activity/growth/buddy/travel/status"
	// workBuddyTravelClaimPath collects a finished trip.
	workBuddyTravelClaimPath = "/activity/growth/buddy/travel/claim"
	// workBuddyTravelConfigPath lists travel destinations.
	workBuddyTravelConfigPath = "/activity/growth/buddy/travel/config"
	// workBuddyTravelDepartPath sends the cat travelling.
	workBuddyTravelDepartPath = "/activity/growth/buddy/travel/depart"

	// workBuddyGrowthGap is the minimum spacing between upstream calls.
	//
	// The reference implementation enforces >= 1.0s between reports. Bursts of
	// synthetic events are the most obvious automation signal we send, so the
	// spacing is part of the design rather than a nicety.
	workBuddyGrowthGap = time.Second

	// workBuddyAcceptChunk bounds one accept request.
	workBuddyAcceptChunk = 20
)

// growthTaskSpec describes one task the engine knows how to light up.
type growthTaskSpec struct {
	// Kind selects the event shape built by buildGrowthEvent.
	Kind string
	// Target is the progress needed, used when the upstream omits it.
	Target int
	// Reward is the expected credit, used for display when upstream omits it.
	Reward int
	// Name is the display name, used when upstream omits a title.
	Name string
	// Unforgeable marks tasks that require a real action (a donation) and must
	// never be reported synthetically.
	Unforgeable bool
}

// growthTaskSpecs is the ported TASK_SPECS table (wb_tasks.py:52).
var growthTaskSpecs = map[string]growthTaskSpec{
	"create_canvas":       {Kind: "canvas", Target: 1, Reward: 300, Name: "创建设计任务"},
	"template_5":          {Kind: "template", Target: 5, Reward: 200, Name: "模板创建任务"},
	"expert_5":            {Kind: "expert", Target: 5, Reward: 200, Name: "使用专家助手"},
	"Expert_team_use_3":   {Kind: "team", Target: 3, Reward: 150, Name: "使用专家团队"},
	"skill_1":             {Kind: "skill", Target: 1, Reward: 100, Name: "体验技能"},
	"automation_1":        {Kind: "automation", Target: 1, Reward: 100, Name: "创建自动化任务"},
	"playbook_prompt":     {Kind: "playbook", Target: 1, Reward: 100, Name: "灵感案例使用"},
	"Expert_lighthouse":   {Kind: "lighthouse", Target: 1, Reward: 100, Name: "轻量云专家使用"},
	"Buddy_App":           {Kind: "buddy5", Target: 1, Reward: 100, Name: "进入 Buddy 应用"},
	"Buddy_App_QQ":        {Kind: "buddy5", Target: 1, Reward: 100, Name: "企鹅教师助手"},
	"Hp_Appearance":       {Kind: "skin", Target: 1, Reward: 100, Name: "应用主题外观"},
	"chat_5":              {Kind: "chat", Target: 5, Reward: 100, Name: "发起 5 次对话"},
	"Model_chat_GLM5.2":   {Kind: "glmchat", Target: 1, Reward: 100, Name: "体验 GLM-5.2"},
	"black_cat":           {Kind: "cat", Target: 3, Reward: 100, Name: "夜猫子任务 (23:00-08:00)"},
	"RichMeow_Chat":       {Kind: "richmeow", Target: 1, Reward: 100, Name: "桌面对话事件链"},
	"Library_read":        {Kind: "library", Target: 1, Reward: 100, Name: "浏览资料库"},
	"first_buddy":         {Kind: "buddy_first", Target: 1, Reward: 0, Name: "领养首只猫猫"},
	"Expert_Philanthropy": {Kind: "", Target: 1, Reward: 0, Name: "公益爱心捐赠", Unforgeable: true},
}

// growthDesktopOnlyTasks lists tasks the upstream only credits for genuine
// desktop-client behaviour.
//
// Their jump_url values are workbuddy:// deep links that require a real click in
// the desktop app. A synthetically reported event is ignored (or lands on
// heartbeat), so progress stays 0/target and the claim is rejected with
// "task not completed". The reference implementation skips these honestly and
// tells the operator what to click; doing otherwise only burns requests and
// makes the run look broken.
var growthDesktopOnlyTasks = map[string]string{
	"RichMeow_Chat": "在桌面端发起 1 次对话",
	"Library_read":  "在桌面端打开「资料库」并读完介绍文档",
	"Buddy_App":     "在桌面端左上角「发现应用」进入任意一个 Buddy 应用",
	"Buddy_App_QQ":  "在桌面端「发现应用」进入「企鹅教师助手」",
}

// growthNightTaskCodes lists tasks that only count between 23:00 and 08:00.
var growthNightTaskCodes = map[string]bool{"black_cat": true}

// growthExpertIDs and growthTeamIDs are the id pools for expert/team events.
//
// Upstream de-duplicates by (eventCode, id) and only accumulates progress for
// ids it has not seen, so a fixed id advances the task exactly once. Cycling
// through the pool is what makes a 5-target task reach 5.
var growthExpertIDs = []struct{ ID, Name string }{
	{"ex_PZw8Gu81HfN4", "运维工程师"},
	{"ex_ROsDtJbzADFV", "产品经理"},
	{"ex_SMUnl0nJbPix", "UI设计师"},
	{"ex_ZTR062oVBOCW", "数据分析师"},
	{"ex_a3sSSFBy8qaC", "后端架构师"},
	{"ex_aG1kvKbq8lPx", "文案策划"},
	{"ex_al1vxtUOYQ10", "测试专家"},
	{"ex_cZfiyuET9UQP", "安全顾问"},
	{"ex_eggOvQuVP0hq", "算法工程师"},
	{"ex_hSwsQjkSKnkX", "前端工程师"},
	{"ex_mMbwwmFA9n9P", "项目管理专家"},
	{"ex_uAQE5POfk7Zh", "增长运营专家"},
	{"ex_uZzSAScSy7FZ", "行业研究员"},
	{"ex_LHywGrZOtG7G", "数据分析师"},
	{"ex_NX5C8GBciVed", "测试架构师"},
	{"ex_DdCsaoq4AtcO", "云端运维专家"},
	{"ex_KzqKQguubrNQ", "内容创作专家"},
	{"ex_2cvvUZQhDyeJ", "腾讯轻量云专家"},
}

var growthTeamIDs = []struct{ ID, Name string }{
	{"CloudOpsTeam", "运维专家团队"},
	{"CloudContentTeam", "内容专家团队"},
	{"CloudDevTeam", "研发专家团队"},
	{"ProductStrategyTeam", "产品战略团队"},
	{"MarketingCampaignTeam", "营销活动团队"},
	{"SalesBattleTeam", "销售作战团队"},
	{"DesignEngineTeam", "设计引擎团队"},
	{"HrOperationsTeam", "人力运营团队"},
}

// growthTask is one entry of the growth task list.
type growthTask struct {
	Code         string `json:"task_code"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	JumpURL      string `json:"jump_url,omitempty"`
	Status       string `json:"status"`
	Current      int    `json:"current"`
	Target       int    `json:"target"`
	Reward       int    `json:"reward_credit"`
	RewardEnergy int    `json:"reward_energy,omitempty"`
	// Unforgeable and DesktopOnly explain why a task is skipped, so the panel
	// can say why rather than looking like a silent failure.
	Unforgeable bool   `json:"unforgeable,omitempty"`
	DesktopOnly bool   `json:"desktop_only,omitempty"`
	SkipReason  string `json:"skip_reason,omitempty"`
}

// growthSummary is the daily-welfare snapshot.
type growthSummary struct {
	Energy     int              `json:"energy"`
	StreakDays int              `json:"streak_days"`
	Travel     growthTravelInfo `json:"travel"`
}

// growthTravelInfo mirrors the travel status payload.
type growthTravelInfo struct {
	State             string `json:"state"`
	DailyLimitReached bool   `json:"daily_limit_reached,omitempty"`
	Location          string `json:"location,omitempty"`
}

// growthRunLog is one line of the human-readable run log.
type growthRunLog struct {
	Level   string `json:"level"` // info | ok | warn | skip | error
	Message string `json:"message"`
}

// growthRunResult is the outcome of one account's growth pass.
type growthRunResult struct {
	AuthID     string         `json:"auth_id"`
	UID        string         `json:"uid"`
	Label      string         `json:"label"`
	OK         bool           `json:"ok"`
	Earned     int            `json:"earned_credit"`
	Credits    int64          `json:"credits"`
	TaskCount  int            `json:"task_count"`
	Claimed    int            `json:"claimed"`
	Lit        int            `json:"lit"`
	Skipped    int            `json:"skipped"`
	Failed     int            `json:"failed"`
	Logs       []growthRunLog `json:"logs"`
	Error      string         `json:"error,omitempty"`
	Trigger    string         `json:"trigger,omitempty"`
	FinishedAt time.Time      `json:"finished_at"`
}

// ---- upstream calls -------------------------------------------------------

// growthBase returns the chat base for a credential's realm.
//
// Growth endpoints live on the chat host (copilot.tencent.com for cn), which
// differs from the check-in host (www.codebuddy.cn) — the reference
// implementation keeps the same split.
func growthBase(creds *workBuddyCredentials) string {
	if variantForCredentials(creds) == variantAi {
		return workBuddyGlobalBase()
	}
	return chatBaseForTest()
}

var (
	workBuddyChatMu     sync.RWMutex
	workBuddyChatBaseCN = "https://copilot.tencent.com"
)

// chatBaseForTest exposes the domestic chat base, redirectable for tests.
func chatBaseForTest() string {
	workBuddyChatMu.RLock()
	defer workBuddyChatMu.RUnlock()
	return workBuddyChatBaseCN
}

func setChatBase(v string) {
	workBuddyChatMu.Lock()
	workBuddyChatBaseCN = v
	workBuddyChatMu.Unlock()
}

// workBuddyWebBaseCN is the web fallback used when a claim is rejected with 400.
const workBuddyWebBaseDefaultCN = "https://www.workbuddy.cn"

var (
	workBuddyWebMu     sync.RWMutex
	workBuddyWebBaseCN = workBuddyWebBaseDefaultCN
)

func webBaseForTest() string {
	workBuddyWebMu.RLock()
	defer workBuddyWebMu.RUnlock()
	return workBuddyWebBaseCN
}

func setWebBase(v string) {
	workBuddyWebMu.Lock()
	workBuddyWebBaseCN = v
	workBuddyWebMu.Unlock()
}

// growthRequest performs one JSON request against the growth API.
//
// The body handling is shared because every endpoint answers with the same
// envelope ({code,msg,data}) and the same success rule (code == 0).
func (c *workBuddyClient) growthRequest(
	ctx context.Context,
	method, base, path string,
	creds *workBuddyCredentials,
	body any,
	headers map[string]string,
) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		payload, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return 0, nil, fmt.Errorf("编码请求失败: %w", errMarshal)
		}
		reader = bytes.NewReader(payload)
	}
	req, errRequest := http.NewRequestWithContext(ctx, method, base+path, reader)
	if errRequest != nil {
		return 0, nil, errRequest
	}
	applyWorkBuddyHeaders(req.Header, creds)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return 0, nil, errDo
	}
	defer resp.Body.Close()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return resp.StatusCode, nil, errRead
	}
	return resp.StatusCode, raw, nil
}

// growthEnvelope is the shared response shape.
type growthEnvelope struct {
	Code *json.Number    `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// decodeGrowthEnvelope parses the envelope and reports whether code == 0.
func decodeGrowthEnvelope(raw []byte) (growthEnvelope, bool) {
	var env growthEnvelope
	if len(raw) == 0 {
		return env, false
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return env, false
	}
	if env.Code == nil {
		return env, false
	}
	n, errInt := env.Code.Int64()
	if errInt != nil {
		return env, false
	}
	return env, n == 0
}

// growthMessage extracts a human-readable reason from an envelope.
func growthMessage(env growthEnvelope, status int) string {
	if msg := strings.TrimSpace(env.Msg); msg != "" {
		return msg
	}
	if env.Code != nil {
		return "code=" + env.Code.String()
	}
	return fmt.Sprintf("HTTP %d", status)
}

// fetchGrowthTasks lists the growth tasks for one credential.
func (c *workBuddyClient) fetchGrowthTasks(ctx context.Context, creds *workBuddyCredentials) ([]growthTask, error) {
	if creds == nil || creds.AccessToken == "" {
		return nil, errors.New("缺少访问令牌")
	}
	status, raw, errRequest := c.growthRequest(ctx, http.MethodGet, growthBase(creds), workBuddyGrowthTasksPath, creds, nil, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("查询成长任务失败: %w", errRequest)
	}
	env, ok := decodeGrowthEnvelope(raw)
	if !ok {
		return nil, fmt.Errorf("查询成长任务失败: %s", growthMessage(env, status))
	}

	var payload struct {
		Tasks []struct {
			TaskCode    string `json:"task_code"`
			Title       string `json:"title"`
			Description string `json:"description"`
			TaskDesc    string `json:"task_desc"`
			JumpURL     string `json:"jump_url"`
			AcceptState string `json:"accept_status"`
			Progress    struct {
				Current int `json:"current"`
				Target  int `json:"target"`
			} `json:"progress"`
			RewardCredit int `json:"reward_credit"`
			RewardEnergy int `json:"reward_energy"`
		} `json:"tasks"`
	}
	if len(env.Data) > 0 {
		if errUnmarshal := json.Unmarshal(env.Data, &payload); errUnmarshal != nil {
			return nil, fmt.Errorf("解析成长任务失败: %w", errUnmarshal)
		}
	}

	out := make([]growthTask, 0, len(payload.Tasks))
	for _, t := range payload.Tasks {
		code := strings.TrimSpace(t.TaskCode)
		if code == "" {
			continue
		}
		spec := growthTaskSpecs[code]
		task := growthTask{
			Code: code,
			Name: firstNonEmpty(strings.TrimSpace(t.Title), spec.Name, code),
			// description and task_desc are both names used by the upstream for
			// the same field.
			Description:  firstNonEmpty(strings.TrimSpace(t.Description), strings.TrimSpace(t.TaskDesc)),
			JumpURL:      strings.TrimSpace(t.JumpURL),
			Status:       firstNonEmpty(strings.TrimSpace(t.AcceptState), "not_accepted"),
			Current:      t.Progress.Current,
			Target:       t.Progress.Target,
			Reward:       t.RewardCredit,
			RewardEnergy: t.RewardEnergy,
			Unforgeable:  spec.Unforgeable,
		}
		if task.Target <= 0 {
			task.Target = spec.Target
		}
		if task.Target <= 0 {
			task.Target = 1
		}
		if task.Reward <= 0 {
			task.Reward = spec.Reward
		}
		// Annotate why a task will be skipped, so the panel explains itself.
		if _, desktop := growthDesktopOnlyTasks[code]; desktop {
			task.DesktopOnly = true
			task.SkipReason = growthDesktopOnlyTasks[code]
		}
		if task.Unforgeable {
			task.SkipReason = "需真实捐款动作，无法代做"
		}
		out = append(out, task)
	}
	return out, nil
}

// acceptGrowthTasks accepts the given task codes in batches.
//
// The reference implementation returns the per-task statuses rather than a bare
// boolean because a rejected accept is otherwise invisible: every later report
// would accumulate nothing and the run would look like "accepted a batch, lit
// none of them".
func (c *workBuddyClient) acceptGrowthTasks(ctx context.Context, creds *workBuddyCredentials, codes []string) (accepted, failed []string, msg string, err error) {
	if len(codes) == 0 {
		return nil, nil, "", nil
	}
	base := growthBase(creds)
	for start := 0; start < len(codes); start += workBuddyAcceptChunk {
		end := start + workBuddyAcceptChunk
		if end > len(codes) {
			end = len(codes)
		}
		part := codes[start:end]
		status, raw, errRequest := c.growthRequest(ctx, http.MethodPost, base, workBuddyGrowthAcceptPath, creds,
			map[string]any{"task_codes": part}, nil)
		if errRequest != nil {
			failed = append(failed, part...)
			msg = errRequest.Error()
			continue
		}
		env, ok := decodeGrowthEnvelope(raw)
		if !ok {
			failed = append(failed, part...)
			msg = growthMessage(env, status)
			continue
		}
		var payload struct {
			Results []struct {
				TaskCode string `json:"task_code"`
				Status   string `json:"status"`
			} `json:"results"`
		}
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &payload)
		}
		seen := make(map[string]bool, len(payload.Results))
		for _, item := range payload.Results {
			seen[item.TaskCode] = true
			switch item.Status {
			case "accepted", "already_accepted":
				accepted = append(accepted, item.TaskCode)
			default:
				failed = append(failed, item.TaskCode)
			}
		}
		// A code the upstream did not mention was not accepted.
		for _, code := range part {
			if !seen[code] {
				failed = append(failed, code)
			}
		}
	}
	return accepted, failed, msg, nil
}

// claimGrowthTask collects one task reward.
//
// A 400 answer is retried against the web host, which is what the official web
// growth centre calls and which accepts some claims the app host rejects
// (wb_tasks.py:285).
func (c *workBuddyClient) claimGrowthTask(ctx context.Context, creds *workBuddyCredentials, code string) (credit, energy int, msg string, ok bool) {
	path := "/activity/growth/tasks/" + code + "/claim"
	status, raw, errRequest := c.growthRequest(ctx, http.MethodPost, growthBase(creds), path, creds, map[string]any{}, nil)
	if errRequest == nil {
		if env, good := decodeGrowthEnvelope(raw); good {
			var data struct {
				Credit int `json:"credit"`
				Energy int `json:"energy"`
			}
			if len(env.Data) > 0 {
				_ = json.Unmarshal(env.Data, &data)
			}
			return data.Credit, data.Energy, "", true
		} else if status != http.StatusBadRequest {
			return 0, 0, growthMessage(env, status), false
		} else {
			// Fall through to the web host on 400.
			msg = growthMessage(env, status)
		}
	} else {
		msg = errRequest.Error()
	}

	webHeaders := map[string]string{
		"Accept":            "application/json, text/plain, */*",
		"Origin":            webBaseForTest(),
		"Referer":           webBaseForTest() + "/profile/growth-center",
		"x-client-platform": "web",
		"User-Agent":        workBuddyWebUserAgent,
		"X-Domain":          webBaseForTest(),
	}
	if creds.UID != "" {
		webHeaders["X-User-Id"] = creds.UID
	}
	status, raw, errRequest = c.growthRequest(ctx, http.MethodPost, webBaseForTest(), path, creds, map[string]any{}, webHeaders)
	if errRequest != nil {
		return 0, 0, firstNonEmpty(msg, errRequest.Error()), false
	}
	env, good := decodeGrowthEnvelope(raw)
	if !good {
		return 0, 0, firstNonEmpty(msg, growthMessage(env, status)), false
	}
	var data struct {
		Credit int `json:"credit"`
		Energy int `json:"energy"`
	}
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &data)
	}
	return data.Credit, data.Energy, "", true
}

// workBuddyWebUserAgent identifies the browser used by the web fallback.
const workBuddyWebUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// reportGrowthEvents posts an event batch.
func (c *workBuddyClient) reportGrowthEvents(ctx context.Context, creds *workBuddyCredentials, events []map[string]any) bool {
	if len(events) == 0 {
		return true
	}
	status, raw, errRequest := c.growthRequest(ctx, http.MethodPost, growthBase(creds), workBuddyReportPath, creds, events, nil)
	if errRequest != nil {
		return false
	}
	env, ok := decodeGrowthEnvelope(raw)
	if ok {
		return true
	}
	return status >= 200 && status < 300 && env.Code == nil
}
