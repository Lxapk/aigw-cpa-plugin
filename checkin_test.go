package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ---- base URL selection (smali a2/b.smali:2660) ------------------------

func TestWorkBuddyCheckinBaseMatchesSource(t *testing.T) {
	// The check-in base differs from the chat base for the cn domain:
	// chat -> copilot.tencent.com, check-in -> codebuddy.cn.
	if got := workBuddyCheckinBase("cn"); got != "https://www.codebuddy.cn" {
		t.Errorf("cn check-in base = %q, want https://www.codebuddy.cn", got)
	}
	// global uses the redirectable workbuddy base.
	orig := workBuddyGlobalBase()
	setWorkBuddyGlobalBase("https://www.workbuddy.ai")
	defer setWorkBuddyGlobalBase(orig)
	if got := workBuddyCheckinBase("www.workbuddy.ai"); got != "https://www.workbuddy.ai" {
		t.Errorf("global check-in base = %q", got)
	}
}

// ---- response decision table (smali 2676-2919) -------------------------

func TestInterpretCheckinResponse(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		success   bool
		already   bool
		msgSubstr string
	}{
		{
			name:   "code 0 is a fresh success",
			status: 200, body: `{"code":0}`,
			success: true, msgSubstr: "签到成功",
		},
		{
			name:   "already checked in (chinese marker)",
			status: 200, body: `{"code":40001,"msg":"今日已签到"}`,
			success: true, already: true, msgSubstr: "今日已签到",
		},
		{
			name:   "already checked in (english marker)",
			status: 200, body: `{"code":40001,"message":"Already checked in"}`,
			success: true, already: true, msgSubstr: "今日已签到",
		},
		{
			name:   "non-zero code without markers is a failure",
			status: 200, body: `{"code":50000,"msg":"服务器开小差"}`,
			success: false, msgSubstr: "服务器开小差",
		},
		{
			name:   "missing code falls back to the code=-1 message",
			status: 200, body: `{}`,
			success: false, msgSubstr: "签到失败（code=-1）",
		},
		{
			name:   "http error carries the status",
			status: 502, body: `bad gateway`,
			success: false, msgSubstr: "签到失败（HTTP 502）",
		},
		{
			name:   "http 401 is a failure",
			status: 401, body: `{"code":401}`,
			success: false, msgSubstr: "签到失败（HTTP 401）",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := interpretCheckinResponse(c.status, []byte(c.body))
			if out.Success != c.success {
				t.Fatalf("success = %v, want %v (%+v)", out.Success, c.success, out)
			}
			if out.AlreadyCheckedIn != c.already {
				t.Errorf("already = %v, want %v", out.AlreadyCheckedIn, c.already)
			}
			if !strings.Contains(out.Message, c.msgSubstr) {
				t.Errorf("message = %q, want it to contain %q", out.Message, c.msgSubstr)
			}
			if out.HTTPStatus != c.status {
				t.Errorf("httpStatus = %d, want %d", out.HTTPStatus, c.status)
			}
		})
	}
}

func TestInterpretCheckinResponseParsesNumericCode(t *testing.T) {
	out := interpretCheckinResponse(200, []byte(`{"code":0}`))
	if out.Code != 0 {
		t.Fatalf("code = %d, want 0", out.Code)
	}
	out = interpretCheckinResponse(200, []byte(`{"code":9074,"msg":"device"}`))
	if out.Code != 9074 {
		t.Fatalf("code = %d, want 9074", out.Code)
	}
	// Unparsable code falls back to -1, matching the source's sentinel.
	out = interpretCheckinResponse(200, []byte(`not json`))
	if out.Code != -1 {
		t.Fatalf("code = %d, want -1", out.Code)
	}
}

// ---- HTTP call ----------------------------------------------------------

func TestCheckinSendsExpectedRequest(t *testing.T) {
	var gotPath, gotMethod, gotAuth, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	// Point the cn check-in base at the test server.
	restore := stubCheckinBase(server.URL)
	defer restore()

	outcome, errCheckin := workBuddyUpstream.checkin(testContext(), &workBuddyCredentials{
		AccessToken: "tok", Domain: "cn", UID: "u-1",
	})
	if errCheckin != nil {
		t.Fatalf("unexpected error: %v", errCheckin)
	}
	if !outcome.Success {
		t.Fatalf("outcome = %+v", outcome)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if !strings.HasSuffix(gotPath, workBuddyCheckinPath) {
		t.Errorf("path = %q, want suffix %q", gotPath, workBuddyCheckinPath)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
	if strings.TrimSpace(gotBody) != "{}" {
		t.Errorf("body = %q, want {}", gotBody)
	}
}

func TestCheckinRequiresToken(t *testing.T) {
	if _, errCheckin := workBuddyUpstream.checkin(testContext(), &workBuddyCredentials{}); errCheckin == nil {
		t.Fatal("expected an error without an access token")
	}
}

func TestCheckinIdempotentResponseIsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":40001,"msg":"今天已签到"}`))
	}))
	defer server.Close()
	restore := stubCheckinBase(server.URL)
	defer restore()

	outcome, errCheckin := workBuddyUpstream.checkin(testContext(), &workBuddyCredentials{AccessToken: "t", Domain: "cn"})
	if errCheckin != nil {
		t.Fatalf("unexpected error: %v", errCheckin)
	}
	if !outcome.Success || !outcome.AlreadyCheckedIn {
		t.Fatalf("outcome = %+v, want success+already", outcome)
	}
}

// ---- account collection -------------------------------------------------

func TestCollectCheckinAccountsFiltersProvider(t *testing.T) {
	resetState()

	storage, _ := json.Marshal(map[string]any{
		"type": workBuddyProviderKey, "accessToken": "at-1", "uid": "u-1", "domain": "cn",
	})
	otherStorage, _ := json.Marshal(map[string]any{"accessToken": "at-2", "uid": "u-2"})

	restore := stubHostCall(func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			t.Fatalf("unexpected host method %q", method)
		}
		return mustMarshal(t, map[string]any{
			"auths": []map[string]any{
				{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey, "label": "Acct 1", "storage_json": json.RawMessage(storage)},
				{"auth_index": "anthropic-x.json", "provider": "anthropic", "label": "Other", "storage_json": json.RawMessage(otherStorage)},
			},
		}), nil
	})
	defer restore()

	accounts, errCollect := collectCheckinAccounts()
	if errCollect != nil {
		t.Fatalf("unexpected error: %v", errCollect)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1: %+v", len(accounts), accounts)
	}
	if accounts[0].AuthID != "codebuddy-u-1.json" || accounts[0].Label != "Acct 1" {
		t.Fatalf("account = %+v", accounts[0])
	}
	if accounts[0].Creds.AccessToken != "at-1" {
		t.Fatalf("creds = %+v", accounts[0].Creds)
	}
}

func TestCollectCheckinAccountsSkipsUnparsable(t *testing.T) {
	resetState()
	restore := stubHostCall(func(_ string, _ any) (json.RawMessage, error) {
		return mustMarshal(t, map[string]any{
			"auths": []map[string]any{
				{"auth_index": "bad.json", "provider": workBuddyProviderKey, "storage_json": json.RawMessage(`{}`)},
			},
		}), nil
	})
	defer restore()

	accounts, errCollect := collectCheckinAccounts()
	if errCollect != nil {
		t.Fatalf("unexpected error: %v", errCollect)
	}
	if len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want none", accounts)
	}
}

func TestCollectCheckinAccountsAcceptsDisplayName(t *testing.T) {
	resetState()
	storage, _ := json.Marshal(map[string]any{"accessToken": "at", "uid": "u-1"})
	restore := stubHostCall(func(_ string, _ any) (json.RawMessage, error) {
		return mustMarshal(t, map[string]any{
			"auths": []map[string]any{
				{"auth_index": "a.json", "type": workBuddyDisplayName, "storage_json": json.RawMessage(storage)},
			},
		}), nil
	})
	defer restore()

	accounts, _ := collectCheckinAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v, want 1 (type may be the display name)", accounts)
	}
}

// ---- batch run ----------------------------------------------------------

func TestRunCheckinAggregatesResults(t *testing.T) {
	resetState()

	// Two accounts: one succeeds, one reports already-checked-in.
	storageA, _ := json.Marshal(map[string]any{"accessToken": "at-a", "uid": "u-a", "domain": "cn"})
	storageB, _ := json.Marshal(map[string]any{"accessToken": "at-b", "uid": "u-b", "domain": "cn"})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(auth, "at-b") {
			_, _ = w.Write([]byte(`{"code":40001,"msg":"已签到"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	restoreBase := stubCheckinBase(server.URL)
	defer restoreBase()

	restoreHost := stubHostCall(func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			t.Fatalf("unexpected host method %q", method)
		}
		return mustMarshal(t, map[string]any{
			"auths": []map[string]any{
				{"auth_index": "a.json", "provider": workBuddyProviderKey, "label": "A", "storage_json": json.RawMessage(storageA)},
				{"auth_index": "b.json", "provider": workBuddyProviderKey, "label": "B", "storage_json": json.RawMessage(storageB)},
			},
		}), nil
	})
	defer restoreHost()

	run := runCheckin("manual")
	if run.Total != 2 || run.Succeeded != 2 || run.Failed != 0 {
		t.Fatalf("run = %+v, want 2/2/0", run)
	}
	if len(run.Results) != 2 {
		t.Fatalf("results = %+v", run.Results)
	}

	byLabel := map[string]checkinResult{}
	for _, r := range run.Results {
		byLabel[r.Label] = r
	}
	if !byLabel["A"].Success || byLabel["A"].Already {
		t.Errorf("A = %+v, want fresh success", byLabel["A"])
	}
	if !byLabel["B"].Success || !byLabel["B"].Already {
		t.Errorf("B = %+v, want already-checked-in", byLabel["B"])
	}
}

func TestRunCheckinWithNoAccounts(t *testing.T) {
	resetState()
	restore := stubHostCall(func(_ string, _ any) (json.RawMessage, error) {
		return mustMarshal(t, map[string]any{"auths": []any{}}), nil
	})
	defer restore()

	run := runCheckin("manual")
	if run.Total != 0 {
		t.Fatalf("total = %d, want 0", run.Total)
	}
	if len(run.Results) != 1 || !strings.Contains(run.Results[0].Error, "没有可签到") {
		t.Fatalf("results = %+v", run.Results)
	}
}

func TestRunCheckinRecordsFailure(t *testing.T) {
	resetState()
	storage, _ := json.Marshal(map[string]any{"accessToken": "at", "uid": "u-1", "domain": "cn"})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401}`))
	}))
	defer server.Close()

	restoreBase := stubCheckinBase(server.URL)
	defer restoreBase()
	restoreHost := stubHostCall(func(_ string, _ any) (json.RawMessage, error) {
		return mustMarshal(t, map[string]any{
			"auths": []map[string]any{
				{"auth_index": "a.json", "provider": workBuddyProviderKey, "storage_json": json.RawMessage(storage)},
			},
		}), nil
	})
	defer restoreHost()

	run := runCheckin("manual")
	if run.Failed != 1 {
		t.Fatalf("failed = %d, want 1 (%+v)", run.Failed, run.Results)
	}
	if !strings.Contains(run.Results[0].Message, "HTTP 401") {
		t.Fatalf("message = %q", run.Results[0].Message)
	}
}

func TestRunCheckinKeepsHistory(t *testing.T) {
	resetState()
	restore := stubHostCall(func(_ string, _ any) (json.RawMessage, error) {
		return mustMarshal(t, map[string]any{"auths": []any{}}), nil
	})
	defer restore()

	before := len(state.checkin.snapshot(0))
	runCheckin("manual")
	runCheckin("manual")
	after := state.checkin.snapshot(0)
	if len(after) != before+2 {
		t.Fatalf("history size = %d, want %d", len(after), before+2)
	}
	if after[0].Trigger != "manual" {
		t.Fatalf("newest trigger = %q", after[0].Trigger)
	}
}

// ---- scheduler ----------------------------------------------------------

func TestCheckinDueNow(t *testing.T) {
	now := time.Now()
	// A target one minute in the past is due.
	past := checkinSettings{Hour: now.Hour(), Minute: clampMinute(now.Minute() - 1)}
	if !checkinDueNow(past) {
		t.Error("a past target should be due (unless minute wrapped)")
	}
	// A target later today is not due.
	future := now.Add(2 * time.Hour)
	if checkinDueNow(checkinSettings{Hour: future.Hour(), Minute: future.Minute()}) {
		t.Error("a future target should not be due")
	}
}

// TestCheckinDefaultsSurviveEmptyConfig guards the bug where an absent
// "checkin" section in config.yaml decoded to the zero value, scheduling the
// automatic run at 00:00 instead of the documented 09:00.
func TestCheckinDefaultsSurviveEmptyConfig(t *testing.T) {
	store := newSettingsStore()
	if errDecode := store.decodeLifecycleConfig([]byte("enabled: true\napi_key: sk-x\n")); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	cfg := store.get().Checkin
	if cfg.Hour != 9 || cfg.Minute != 0 {
		t.Fatalf("defaults = %02d:%02d, want 09:00", cfg.Hour, cfg.Minute)
	}
	if cfg.Enabled {
		t.Error("auto check-in must default to disabled")
	}
}

func TestCheckinExplicitValuesAreHonoured(t *testing.T) {
	store := newSettingsStore()
	yamlDoc := "checkin:\n  enabled: true\n  hour: 6\n  minute: 30\n  on_start: true\n"
	if errDecode := store.decodeLifecycleConfig([]byte(yamlDoc)); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	cfg := store.get().Checkin
	if !cfg.Enabled || cfg.Hour != 6 || cfg.Minute != 30 || !cfg.OnStart {
		t.Fatalf("config = %+v", cfg)
	}
}

// TestCheckinExplicitMidnightIsHonoured makes sure an operator who really wants
// 00:00 while enabled can still get it.
func TestCheckinExplicitMidnightIsHonoured(t *testing.T) {
	store := newSettingsStore()
	if errDecode := store.decodeLifecycleConfig([]byte("checkin:\n  enabled: true\n  hour: 0\n  minute: 0\n")); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	cfg := store.get().Checkin
	if !cfg.Enabled || cfg.Hour != 0 || cfg.Minute != 0 {
		t.Fatalf("config = %+v, want enabled at 00:00", cfg)
	}
}

func TestClampHourMinute(t *testing.T) {
	cases := []struct{ in, want int }{{-5, 0}, {0, 0}, {12, 12}, {23, 23}, {99, 23}}
	for _, c := range cases {
		if got := clampHour(c.in); got != c.want {
			t.Errorf("clampHour(%d) = %d, want %d", c.in, got, c.want)
		}
	}
	mcases := []struct{ in, want int }{{-1, 0}, {0, 0}, {59, 59}, {60, 59}}
	for _, c := range mcases {
		if got := clampMinute(c.in); got != c.want {
			t.Errorf("clampMinute(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestNextCheckinTimeDisabled(t *testing.T) {
	if got := nextCheckinTime(checkinSettings{Enabled: false}); got != "" {
		t.Fatalf("disabled schedule should render empty, got %q", got)
	}
	got := nextCheckinTime(checkinSettings{Enabled: true, Hour: 23, Minute: 59})
	if got == "" {
		t.Fatal("enabled schedule must render a time")
	}
	if _, errParse := time.Parse(time.RFC3339, got); errParse != nil {
		t.Fatalf("next run is not RFC3339: %q", got)
	}
}

// ---- management endpoints ----------------------------------------------

func TestManagementRegistersCheckinResource(t *testing.T) {
	resetState()
	reg := managementRegistration()
	// A single menu entry is registered; check-in lives behind its tab bar.
	if len(reg.Resources) != 1 || reg.Resources[0].Path != "home" {
		t.Fatalf("expected exactly one combined resource, got %+v", reg.Resources)
	}
}

func TestCheckinStatusEndpoint(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    "/v0/resource/plugins/workbuddy/checkin/status",
		Headers: http.Header{"Accept": []string{"application/json"}},
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", mr.StatusCode)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(mr.Body, &doc); errUnmarshal != nil {
		t.Fatalf("body not json: %v", errUnmarshal)
	}
	for _, key := range []string{"enabled", "hour", "minute", "running", "history"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("status body missing %q", key)
		}
	}
}

func TestCheckinPageRenders(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    "/v0/resource/plugins/workbuddy/checkin",
		Headers: http.Header{"Accept": []string{"text/html"}},
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", mr.StatusCode)
	}
	body := string(mr.Body)
	for _, want := range []string{"自动签到", "立即签到", "签到 + 刷新积分"} {
		if !strings.Contains(body, want) {
			t.Errorf("combined page missing %q", want)
		}
	}
}

func TestCheckinRunEndpointRequiresPost(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/workbuddy/checkin/run",
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", mr.StatusCode)
	}
}

func TestCheckinRunEndpointRuns(t *testing.T) {
	resetState()
	storage, _ := json.Marshal(map[string]any{"accessToken": "at", "uid": "u-1", "domain": "cn"})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	restoreBase := stubCheckinBase(server.URL)
	defer restoreBase()
	restoreHost := stubHostCall(func(_ string, _ any) (json.RawMessage, error) {
		return mustMarshal(t, map[string]any{
			"auths": []map[string]any{
				{"auth_index": "a.json", "provider": workBuddyProviderKey, "storage_json": json.RawMessage(storage)},
			},
		}), nil
	})
	defer restoreHost()

	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/resource/plugins/workbuddy/checkin/run",
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", mr.StatusCode)
	}
	var run checkinRun
	if errUnmarshal := json.Unmarshal(mr.Body, &run); errUnmarshal != nil {
		t.Fatalf("body not a run: %v", errUnmarshal)
	}
	if run.Total != 1 || run.Succeeded != 1 {
		t.Fatalf("run = %+v", run)
	}
}

func TestCheckinConfigEndpointSaves(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/resource/plugins/workbuddy/checkin/config",
		Body:   []byte(`{"enabled":true,"hour":7,"minute":30,"on_start":true}`),
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", mr.StatusCode, mr.Body)
	}

	cfg := state.settings.get().Checkin
	if !cfg.Enabled || cfg.Hour != 7 || cfg.Minute != 30 || !cfg.OnStart {
		t.Fatalf("config = %+v", cfg)
	}
	// Enabling must start the scheduler.
	state.checkin.mu.Lock()
	started := state.checkin.started
	state.checkin.mu.Unlock()
	if !started {
		t.Error("scheduler should have been started")
	}
	stopCheckinScheduler()
}

func TestCheckinConfigEndpointClampsValues(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/resource/plugins/workbuddy/checkin/config",
		Body:   []byte(`{"hour":99,"minute":99}`),
	})
	cfg := state.settings.get().Checkin
	if cfg.Hour != 23 || cfg.Minute != 59 {
		t.Fatalf("config not clamped: %+v", cfg)
	}
}

func TestCheckinConfigEndpointAcceptsNestedShape(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/resource/plugins/workbuddy/checkin/config",
		Body:   []byte(`{"checkin":{"enabled":true,"hour":6}}`),
	})
	cfg := state.settings.get().Checkin
	if !cfg.Enabled || cfg.Hour != 6 {
		t.Fatalf("config = %+v", cfg)
	}
}

// TestCheckinConfigViaPostJSON covers the endpoint the page's fetch() uses.
func TestCheckinConfigViaPostJSON(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    managementBasePath() + "/" + pluginName + "/checkin/config",
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"enabled":true,"hour":8,"minute":45,"on_start":true}`),
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", mr.StatusCode, mr.Body)
	}
	cfg := state.settings.get().Checkin
	if !cfg.Enabled || cfg.Hour != 8 || cfg.Minute != 45 || !cfg.OnStart {
		t.Fatalf("config = %+v", cfg)
	}
	stopCheckinScheduler()
}

// ---- helpers ------------------------------------------------------------

// stubCheckinBase points every cn-facing base (billing, check-in) at a test
// server, so one httptest server can answer all of them.
func stubCheckinBase(base string) func() {
	return redirectAllCnBases(base)
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	return raw
}
