package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file exercises the growth-task engine end to end against a fake upstream,
// so the ported lifecycle (accept -> report -> claim) is verified as a whole
// rather than by inspecting individual helpers.
//
// The fake server reproduces the behaviours that broke the naive
// implementations:
//
//   - progress only accumulates for accepted tasks;
//   - expert/team events de-duplicate by (eventCode, id);
//   - a claim before progress is booked answers "task not completed";
//   - the task list only reflects progress after a report lands.

// growthFake is a scripted stand-in for the growth API.
type growthFake struct {
	mu sync.Mutex

	// accepted tracks task codes the fake considers accepted.
	accepted map[string]bool
	// progress is per task code.
	progress map[string]int
	// targets is the per code goal.
	targets map[string]int
	// statuses overrides accept_status per code.
	statuses map[string]string
	// claimed records claimed codes.
	claimed map[string]bool
	// seenExpertIDs records (code, id) pairs used for expert events.
	seenExpertIDs map[string]bool
	// reports counts report calls.
	reports int
	// acceptFailures makes accept reject the first N calls.
	acceptFailures int
	// claimBeforeProgress rejects claims when progress is short (the upstream
	// rule).
	claimBeforeProgress bool
	// base is the server URL, filled in by start.
	base string
}

func newGrowthFake() *growthFake {
	return &growthFake{
		accepted:            make(map[string]bool),
		progress:            make(map[string]int),
		targets:             make(map[string]int),
		statuses:            make(map[string]string),
		claimed:             make(map[string]bool),
		seenExpertIDs:       make(map[string]bool),
		claimBeforeProgress: true,
	}
}

// start launches the fake and points the client bases at it.
func (f *growthFake) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	f.base = srv.URL

	// Redirect both hosts the engine can use.
	previousChat, previousWeb := workBuddyChatBase(), workBuddyWebBase()
	setChatBase(srv.URL)
	setWebBase(srv.URL)
	t.Cleanup(func() {
		setChatBase(previousChat)
		setWebBase(previousWeb)
	})
}

func (f *growthFake) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := r.URL.Path
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	switch {
	case path == workBuddyGrowthTasksPath && r.Method == http.MethodGet:
		tasks := make([]map[string]any, 0, len(f.targets))
		for code, target := range f.targets {
			status := f.statuses[code]
			if status == "" {
				if f.claimed[code] {
					status = "claimed"
				} else if f.accepted[code] {
					status = "accepted"
				} else {
					status = "not_accepted"
				}
			}
			if f.progress[code] >= target && status != "claimed" {
				status = "completed"
			}
			tasks = append(tasks, map[string]any{
				"task_code":     code,
				"title":         growthTaskSpecs[code].Name,
				"jump_url":      "workbuddy://chat",
				"accept_status": status,
				"progress":      map[string]any{"current": f.progress[code], "target": target},
				"reward_credit": growthTaskSpecs[code].Reward,
			})
		}
		writeJSON(map[string]any{"code": 0, "data": map[string]any{"tasks": tasks}})

	case path == workBuddyGrowthAcceptPath && r.Method == http.MethodPost:
		if f.acceptFailures > 0 {
			f.acceptFailures--
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(map[string]any{"code": 500, "msg": "busy"})
			return
		}
		var body struct {
			TaskCodes []string `json:"task_codes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		results := make([]map[string]any, 0, len(body.TaskCodes))
		for _, code := range body.TaskCodes {
			f.accepted[code] = true
			results = append(results, map[string]any{"task_code": code, "status": "accepted"})
		}
		writeJSON(map[string]any{"code": 0, "data": map[string]any{"results": results}})

	case strings.HasPrefix(path, "/activity/growth/tasks/") && strings.HasSuffix(path, "/claim"):
		code := strings.TrimSuffix(strings.TrimPrefix(path, "/activity/growth/tasks/"), "/claim")
		if !f.accepted[code] {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(map[string]any{"code": 400, "msg": "task not accepted"})
			return
		}
		if f.claimBeforeProgress && f.progress[code] < f.targets[code] {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(map[string]any{"code": 400, "msg": "task not completed"})
			return
		}
		if f.claimed[code] {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(map[string]any{"code": 400, "msg": "already claimed"})
			return
		}
		f.claimed[code] = true
		writeJSON(map[string]any{"code": 0, "data": map[string]any{
			"credit": growthTaskSpecs[code].Reward,
		}})

	case path == workBuddyReportPath && r.Method == http.MethodPost:
		f.reports++
		var events []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&events)
		for _, event := range events {
			eventCode, _ := event["eventCode"].(string)
			if eventCode == "heartbeat" {
				continue
			}
			// Map the event back to a task and advance it, honouring the
			// (eventCode, id) de-duplication the upstream applies.
			for code, spec := range growthTaskSpecs {
				if growthEventMatchesTask(eventCode, spec.Kind) == false {
					continue
				}
				if !f.accepted[code] {
					continue
				}
				if id, ok := event["id"].(string); ok && id != "" {
					key := code + "|" + id
					if f.seenExpertIDs[key] {
						continue
					}
					f.seenExpertIDs[key] = true
				}
				if f.progress[code] < f.targets[code] {
					f.progress[code]++
				}
			}
		}
		writeJSON(map[string]any{"code": 0})

	case path == workBuddyEnergyPath:
		writeJSON(map[string]any{"code": 0, "data": map[string]any{"balance": 42}})

	case path == workBuddyStreakPath:
		writeJSON(map[string]any{"code": 0, "data": map[string]any{"streak": map[string]any{"days": 7}}})

	case path == workBuddyTravelStatusPath:
		writeJSON(map[string]any{"code": 0, "data": map[string]any{"state": "idle"}})

	case path == workBuddyTravelConfigPath:
		writeJSON(map[string]any{"code": 0, "data": map[string]any{
			"locations": []map[string]any{{"id": 3, "name": "杭州"}},
		}})

	case path == workBuddyTravelDepartPath:
		var body struct {
			LocationID int `json:"location_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.LocationID == 0 {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(map[string]any{"code": 400, "msg": "invalid request"})
			return
		}
		writeJSON(map[string]any{"code": 0, "data": map[string]any{
			"location": map[string]any{"name": "杭州"},
		}})

	default:
		// Unknown endpoints (quota, models) answer empty so the pass can finish.
		writeJSON(map[string]any{"code": 0, "data": map[string]any{}})
	}
}

// growthEventMatchesTask maps an eventCode back to the task kind that emits it.
func growthEventMatchesTask(eventCode, kind string) bool {
	switch kind {
	case "canvas":
		return eventCode == "wbx_design_canvas_task_create"
	case "template":
		return eventCode == "agent_task_created_with_template"
	case "expert", "team", "lighthouse":
		return eventCode == "expert_actual_use"
	case "skill":
		return eventCode == "skill_info"
	case "automation":
		return eventCode == "automated_task_create_suc"
	case "playbook":
		return eventCode == "playbook_prompt_send"
	case "skin":
		return eventCode == "appearance_skin_apply"
	case "chat", "glmchat", "cat":
		return eventCode == "chat_request_send"
	}
	return false
}

// newGrowthCreds builds a domestic credential pointed at the fake.
func newGrowthCreds() *workBuddyCredentials {
	return &workBuddyCredentials{
		AccessToken: "[REDACTED]",
		UID:         "u-growth",
		Domain:      "copilot.tencent.com",
	}
}

func newGrowthTestRunner() *growthRunner {
	return &growthRunner{
		client:  workBuddyUpstream,
		limiter: newGrowthLimiter(),
		// No spacing in tests; the spacing itself is covered separately.
		gap: 0,
	}
}

// ---- lifecycle ------------------------------------------------------------

// TestGrowthPassCompletesLifecycle walks accept -> report -> claim for a
// multi-target expert task, which is the case that needs distinct event ids.
func TestGrowthPassCompletesLifecycle(t *testing.T) {
	resetState()
	fake := newGrowthFake()
	fake.targets["expert_5"] = 5
	fake.start(t)

	result := newGrowthTestRunner().run(context.Background(), newGrowthCreds(), "测试账号")

	if !result.OK {
		t.Fatalf("pass failed: %+v", result)
	}
	if !fake.claimed["expert_5"] {
		t.Fatal("expert_5 was never claimed")
	}
	if fake.progress["expert_5"] != 5 {
		t.Fatalf("progress = %d, want 5 (distinct expert ids must each count)", fake.progress["expert_5"])
	}
	if result.Earned != growthTaskSpecs["expert_5"].Reward {
		t.Fatalf("earned = %d, want %d", result.Earned, growthTaskSpecs["expert_5"].Reward)
	}
	if result.Claimed != 1 {
		t.Fatalf("claimed = %d, want 1", result.Claimed)
	}
}

// TestGrowthPassReusesExpertIDDoesNotProgress is the negative control for the
// id-pool rule: reporting the same id twice must advance progress only once.
func TestGrowthPassReusesExpertIDDoesNotProgress(t *testing.T) {
	resetState()
	fake := newGrowthFake()
	fake.targets["expert_5"] = 5
	fake.accepted["expert_5"] = true
	fake.progress["expert_5"] = 3 // three more needed
	fake.start(t)

	// Report the same expert three times.
	for i := 0; i < 3; i++ {
		same := growthExpertIDs[0]
		event := buildGrowthEvent(newGrowthCreds(), "expert", i, &struct{ ID, Name string }{same.ID, same.Name})
		if !workBuddyUpstream.reportGrowthEvents(context.Background(), newGrowthCreds(), []growthEvent{event}) {
			t.Fatalf("report %d failed", i)
		}
	}

	fake.mu.Lock()
	progress := fake.progress["expert_5"]
	fake.mu.Unlock()
	if progress != 4 {
		t.Fatalf("progress = %d, want 4 (a repeated id must not count twice)", progress)
	}
}

// TestGrowthPassRetriesAccept covers the "accepted a batch, lit none of them"
// failure: a transient accept rejection must be retried before reporting.
func TestGrowthPassRetriesAccept(t *testing.T) {
	resetState()
	fake := newGrowthFake()
	fake.targets["create_canvas"] = 1
	fake.acceptFailures = 1 // first accept call fails
	fake.start(t)

	result := newGrowthTestRunner().run(context.Background(), newGrowthCreds(), "测试账号")

	fake.mu.Lock()
	accepted, progress, claimed := fake.accepted["create_canvas"], fake.progress["create_canvas"], fake.claimed["create_canvas"]
	fake.mu.Unlock()

	if !accepted {
		t.Fatal("task was never accepted, so the retry did not happen")
	}
	if progress == 0 && !claimed {
		t.Fatal("progress stayed 0: the retry did not feed the report stage")
	}
	if !result.OK {
		t.Fatalf("pass reported failure: %+v", result)
	}
}

// TestGrowthPassSkipsUnacceptedTasks checks the guard that prevents wasted
// reports: a task still unaccepted after the retry must not be reported against.
func TestGrowthPassSkipsUnacceptedTasks(t *testing.T) {
	resetState()
	fake := newGrowthFake()
	fake.targets["chat_5"] = 5
	// The fake rejects every accept by keeping acceptFailures high.
	fake.acceptFailures = 99
	fake.start(t)

	result := newGrowthTestRunner().run(context.Background(), newGrowthCreds(), "测试账号")

	fake.mu.Lock()
	reports, progress := fake.reports, fake.progress["chat_5"]
	fake.mu.Unlock()

	if progress != 0 {
		t.Fatalf("progress = %d, want 0 (reports against an unaccepted task must not be sent)", progress)
	}
	if reports != 0 {
		t.Fatalf("reports = %d, want 0", reports)
	}
	if result.Skipped == 0 {
		t.Fatal("the skipped task was not counted")
	}
}

// TestGrowthPassSkipsDesktopOnlyTasks pins the honest skip: these tasks cannot
// be lit synthetically, so the engine must skip them and surface the deep link
// rather than burn requests.
func TestGrowthPassSkipsDesktopOnlyTasks(t *testing.T) {
	resetState()
	fake := newGrowthFake()
	for code := range growthDesktopOnlyTasks {
		fake.targets[code] = 1
		fake.accepted[code] = true
	}
	fake.start(t)

	result := newGrowthTestRunner().run(context.Background(), newGrowthCreds(), "测试账号")

	for _, code := range []string{"Library_read", "RichMeow_Chat"} {
		if fake.progress[code] != 0 {
			t.Errorf("%s progress = %d, want 0", code, fake.progress[code])
		}
	}
	if result.Skipped < len(growthDesktopOnlyTasks) {
		t.Fatalf("skipped = %d, want at least %d", result.Skipped, len(growthDesktopOnlyTasks))
	}
	// The reason must reach the log so the operator knows what to click.
	var found bool
	for _, line := range result.Logs {
		if strings.Contains(line.Message, "需真实操作完成") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no log line explains the desktop-only skip")
	}
}

// TestGrowthPassClaimsAlreadyCompletedTasks ensures a task completed elsewhere
// (the operator's desktop, or the night pass) is still collected.
func TestGrowthPassClaimsAlreadyCompletedTasks(t *testing.T) {
	resetState()
	fake := newGrowthFake()
	fake.targets["skill_1"] = 1
	fake.accepted["skill_1"] = true
	fake.progress["skill_1"] = 1 // already complete
	fake.start(t)

	result := newGrowthTestRunner().run(context.Background(), newGrowthCreds(), "测试账号")

	fake.mu.Lock()
	claimed := fake.claimed["skill_1"]
	fake.mu.Unlock()
	if !claimed {
		t.Fatal("an already-completed task was not claimed")
	}
	if result.Earned != growthTaskSpecs["skill_1"].Reward {
		t.Fatalf("earned = %d, want %d", result.Earned, growthTaskSpecs["skill_1"].Reward)
	}
}

// TestGrowthPassSkipsInternationalAccounts pins the realm gate.
func TestGrowthPassSkipsInternationalAccounts(t *testing.T) {
	resetState()
	fake := newGrowthFake()
	fake.targets["chat_5"] = 5
	fake.start(t)

	creds := &workBuddyCredentials{
		AccessToken: "[REDACTED]",
		UID:         "u-intl",
		Domain:      "www.workbuddy.ai",
	}
	result := newGrowthTestRunner().run(context.Background(), creds, "国际账号")

	if !result.OK {
		t.Fatalf("an international account should be a clean skip, got %+v", result)
	}
	fake.mu.Lock()
	accepted := len(fake.accepted)
	fake.mu.Unlock()
	if accepted != 0 {
		t.Fatal("the engine contacted the growth API for an international account")
	}
}

// TestGrowthPassHonoursNightWindow checks the 23:00-08:00 gate.
func TestGrowthPassHonoursNightWindow(t *testing.T) {
	resetState()
	fake := newGrowthFake()
	fake.targets["black_cat"] = 3
	fake.accepted["black_cat"] = true
	fake.start(t)

	daytime := time.Date(2026, 9, 26, 14, 0, 0, 0, time.Local)
	runner := newGrowthTestRunner()
	runner.now = func() time.Time { return daytime }

	result := runner.run(context.Background(), newGrowthCreds(), "测试账号")
	if fake.progress["black_cat"] != 0 {
		t.Fatalf("black_cat progress = %d, want 0 outside the night window", fake.progress["black_cat"])
	}
	var found bool
	for _, line := range result.Logs {
		if strings.Contains(line.Message, "23:00-08:00") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("the night-window skip was not logged")
	}

	// Inside the window it must report.
	nighttime := time.Date(2026, 9, 26, 23, 30, 0, 0, time.Local)
	runner.now = func() time.Time { return nighttime }
	runner.limiter = newGrowthLimiter()
	runner.run(context.Background(), newGrowthCreds(), "测试账号")
	if fake.progress["black_cat"] == 0 {
		t.Fatal("black_cat was not reported inside the night window")
	}
}

// TestGrowthPassRunsTravel covers the daily welfare stage.
func TestGrowthPassRunsTravel(t *testing.T) {
	resetState()
	fake := newGrowthFake()
	fake.targets["skill_1"] = 1
	fake.start(t)

	result := newGrowthTestRunner().run(context.Background(), newGrowthCreds(), "测试账号")

	var departed bool
	for _, line := range result.Logs {
		if strings.Contains(line.Message, "出发") || strings.Contains(line.Message, "旅行") {
			departed = true
			break
		}
	}
	if !departed {
		t.Fatalf("no travel log line: %+v", result.Logs)
	}
}

// ---- event construction ---------------------------------------------------

func TestBuildGrowthEventShapes(t *testing.T) {
	creds := newGrowthCreds()
	cases := map[string]string{
		"canvas":     "wbx_design_canvas_task_create",
		"template":   "agent_task_created_with_template",
		"expert":     "expert_actual_use",
		"team":       "expert_actual_use",
		"lighthouse": "expert_actual_use",
		"skill":      "skill_info",
		"automation": "automated_task_create_suc",
		"playbook":   "playbook_prompt_send",
		"skin":       "appearance_skin_apply",
		"chat":       "chat_request_send",
		"glmchat":    "chat_request_send",
		"cat":        "chat_request_send",
	}
	for kind, want := range cases {
		event := buildGrowthEvent(creds, kind, 0, nil)
		if got, _ := event["eventCode"].(string); got != want {
			t.Errorf("kind %s: eventCode = %q, want %q", kind, got, want)
		}
		for _, field := range []string{"timestamp", "userId"} {
			if _, ok := event[field]; !ok {
				t.Errorf("kind %s: missing %s", kind, field)
			}
		}
	}

	// An unknown kind degrades to a heartbeat rather than fabricating an event.
	unknown := buildGrowthEvent(creds, "no-such-kind", 0, nil)
	if got, _ := unknown["eventCode"].(string); got != "heartbeat" {
		t.Fatalf("unknown kind eventCode = %q, want heartbeat", got)
	}
}

func TestBuildGrowthEventNightCatUsesNightMode(t *testing.T) {
	cat := buildGrowthEvent(newGrowthCreds(), "cat", 0, nil)
	if mode, _ := cat["mode"].(string); mode != "night" {
		t.Fatalf("cat mode = %q, want night", mode)
	}
	if model, _ := cat["requestModelId"].(string); model != "glm-5.2" {
		t.Fatalf("cat model = %q, want glm-5.2", model)
	}
}

func TestBuildGrowthEventDistinctIdentityPerIndex(t *testing.T) {
	creds := newGrowthCreds()
	first := buildGrowthEvent(creds, "expert", 0, nil)
	second := buildGrowthEvent(creds, "expert", 1, nil)
	if first["conversationId"] == second["conversationId"] {
		t.Fatal("conversationId must differ per index or the upstream treats it as a duplicate")
	}
}

func TestGrowthExpertForCyclesPools(t *testing.T) {
	// Team events use the team pool, expert events the expert pool.
	team := growthExpertFor("team", 0)
	if team == nil || team.ID != growthTeamIDs[0].ID {
		t.Fatalf("team id = %+v, want the first team pool entry", team)
	}
	expert := growthExpertFor("expert", 0)
	if expert == nil || expert.ID != growthExpertIDs[0].ID {
		t.Fatalf("expert id = %+v, want the first expert pool entry", expert)
	}
	// It must wrap around rather than run out.
	wrapped := growthExpertFor("expert", len(growthExpertIDs))
	if wrapped == nil || wrapped.ID != growthExpertIDs[0].ID {
		t.Fatalf("wrapped id = %+v, want the pool to cycle", wrapped)
	}
	// Non-pooled kinds get no id override.
	if growthExpertFor("chat", 0) != nil {
		t.Fatal("chat events must not carry an expert id")
	}
}

func TestInNightWindow(t *testing.T) {
	cases := []struct {
		hour int
		want bool
	}{
		{0, true}, {7, true}, {8, false}, {12, false},
		{22, false}, {23, true},
	}
	for _, c := range cases {
		at := time.Date(2026, 9, 26, c.hour, 0, 0, 0, time.Local)
		if got := inNightWindow(at); got != c.want {
			t.Errorf("hour %d: inNightWindow = %v, want %v", c.hour, got, c.want)
		}
	}
}

// ---- limiter --------------------------------------------------------------

func TestGrowthLimiterSpacesCalls(t *testing.T) {
	limiter := newGrowthLimiter()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if errWait := limiter.wait(context.Background(), "u", 40*time.Millisecond); errWait != nil {
			t.Fatalf("wait %d: %v", i, errWait)
		}
	}
	elapsed := time.Since(start)
	// Two gaps between three calls.
	if elapsed < 70*time.Millisecond {
		t.Fatalf("elapsed = %v, want at least ~80ms of spacing", elapsed)
	}
}

func TestGrowthLimiterHonoursContext(t *testing.T) {
	limiter := newGrowthLimiter()
	if errWait := limiter.wait(context.Background(), "u", time.Second); errWait != nil {
		t.Fatalf("first wait: %v", errWait)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if errWait := limiter.wait(ctx, "u", time.Second); errWait == nil {
		t.Fatal("a cancelled context must interrupt the spacing wait")
	}
}

// ---- store ----------------------------------------------------------------

func TestGrowthStoreKeepsLatestAndHistory(t *testing.T) {
	store := newGrowthStore()
	store.record("u1", growthRunResult{Label: "一", OK: true, Earned: 10})
	store.record("u1", growthRunResult{Label: "一", OK: true, Earned: 20})
	store.record("u2", growthRunResult{Label: "二", OK: false, Error: "boom"})

	last, ok := store.last("u1")
	if !ok || last.Earned != 20 {
		t.Fatalf("last(u1) = %+v, want the newest (20)", last)
	}
	if history := store.recentHistory(); len(history) != 3 {
		t.Fatalf("history len = %d, want 3", len(history))
	}
	if snapshot := store.snapshot(); len(snapshot) != 2 {
		t.Fatalf("snapshot len = %d, want 2 accounts", len(snapshot))
	}
}

// ---- UI contract ----------------------------------------------------------

func TestTaskPageExposesGrowthControls(t *testing.T) {
	resetState()
	page := renderMainPage()
	for _, needle := range []string{
		`id="btnRunGrowth"`,
		`id="btnTravel"`,
		`id="growthMsg"`,
		`id="growthDetail"`,
	} {
		if !strings.Contains(page, needle) {
			t.Errorf("task page is missing %s", needle)
		}
	}
	for _, handler := range []string{"runGrowthTasks", "runTravel", "loadGrowthTasks", "escapeHTML"} {
		if !strings.Contains(page, "window."+handler+" = function") && !strings.Contains(page, "function "+handler) {
			t.Errorf("handler %s is referenced but never defined", handler)
		}
	}
}

func TestGrowthSectionExplainsLimits(t *testing.T) {
	resetState()
	page := renderMainPage()
	if !strings.Contains(page, "需要真实桌面操作的任务") {
		t.Fatal("the task tab must explain that some tasks cannot be automated")
	}
	if !strings.Contains(page, "国际版账号不在成长任务中心范围内") {
		t.Fatal("the task tab must explain the international exclusion")
	}
}

// ---- action wiring --------------------------------------------------------

// TestActivityTaskKindIsWired guards the regression that made taskKindActivity a
// dead constant: it must appear in the default catalogue, have run state and
// reach the growth engine.
func TestActivityTaskKindIsWired(t *testing.T) {
	var inCatalogue bool
	for _, spec := range defaultTasks {
		if spec.Kind == taskKindActivity {
			inCatalogue = true
		}
	}
	if !inCatalogue {
		t.Fatal("taskKindActivity is not in defaultTasks, so it never gets scheduled")
	}

	account := &accountTasks{}
	account.setRunState(taskKindActivity, &taskRunState{LastRun: time.Now(), LastOK: true})
	if account.runState(taskKindActivity) == nil {
		t.Fatal("run state for the activity task is not stored")
	}
	if account.runState(taskKindActivity).LastOK != true {
		t.Fatal("run state did not round-trip")
	}
}

func TestRunActivityRecordsResult(t *testing.T) {
	resetState()
	fake := newGrowthFake()
	fake.targets["skill_1"] = 1
	fake.start(t)

	// Register the credential so resolveAccount can find it.
	restore := stubHostCallForGrowth(t, "u-wired")
	defer restore()

	engine := state.taskEngine
	engine.initFromAccounts(listWorkBuddyAccounts())
	engine.runActivity(context.Background(), "u-wired")

	status := taskStatusJSON()
	accounts, _ := status["accounts"].([]map[string]any)
	if len(accounts) == 0 {
		t.Fatal("no account in the task status after an activity run")
	}
	found := false
	for _, raw := range accounts {
		tasks, _ := raw["tasks"].([]map[string]any)
		for _, task := range tasks {
			if label, _ := task["label"].(string); label == "成长任务" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("the 成长任务 label is not reported: %+v", accounts)
	}
}

// stubHostCallForGrowth registers one domestic credential for the task engine.
func stubHostCallForGrowth(t *testing.T, uid string) func() {
	t.Helper()
	storage := fmt.Sprintf(`{"accessToken":"tok","uid":%q,"domain":"copilot.tencent.com"}`, uid)
	return stubHostCall(func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return mustMarshal(t, map[string]any{
				"files": []map[string]any{{
					"auth_index":   "codebuddy-" + uid + ".json",
					"provider":     workBuddyProviderKey,
					"storage_json": json.RawMessage(storage),
				}},
			}), nil
		case "host.auth.get":
			return json.RawMessage(storage), nil
		}
		return json.RawMessage(`{}`), nil
	})
}
