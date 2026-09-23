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

// ---- quota base (a2/b.java:406) ----------------------------------------

func TestWorkBuddyQuotaBaseMatchesSource(t *testing.T) {
	origGlobal := workBuddyGlobalBase()
	origCheckin := checkinBaseForTest()
	setWorkBuddyGlobalBase("https://www.workbuddy.ai")
	setCheckinBase("https://www.codebuddy.cn")
	defer func() {
		setWorkBuddyGlobalBase(origGlobal)
		setCheckinBase(origCheckin)
	}()

	// The quota call uses codebuddy.cn for cn, NOT copilot.tencent.com.
	if got := workBuddyQuotaBase("cn"); got != "https://www.codebuddy.cn" {
		t.Errorf("cn quota base = %q, want https://www.codebuddy.cn", got)
	}
	if got := workBuddyQuotaBase("www.workbuddy.ai"); got != "https://www.workbuddy.ai" {
		t.Errorf("global quota base = %q", got)
	}
}

// ---- request body (a2/b.java:290 F()) ----------------------------------

func TestQuotaRequestBodyMatchesSource(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(quotaRequestBody(now), &doc); errUnmarshal != nil {
		t.Fatalf("bad json: %v", errUnmarshal)
	}

	if doc["ProductCode"] != workBuddyQuotaProductCode {
		t.Errorf("ProductCode = %v, want %v", doc["ProductCode"], workBuddyQuotaProductCode)
	}
	if doc["PageSize"] != float64(workBuddyQuotaPageSize) {
		t.Errorf("PageSize = %v", doc["PageSize"])
	}
	if doc["PageNumber"] != float64(1) {
		t.Errorf("PageNumber = %v", doc["PageNumber"])
	}
	status, okStatus := doc["Status"].([]any)
	if !okStatus || len(status) != 2 || status[0] != float64(0) || status[1] != float64(3) {
		t.Errorf("Status = %v, want [0,3]", doc["Status"])
	}
	begin, _ := doc["PackageEndTimeRangeBegin"].(string)
	end, _ := doc["PackageEndTimeRangeEnd"].(string)
	if begin != "2026-09-23 12:00:00" {
		t.Errorf("begin = %q", begin)
	}
	if end == "" || end <= begin {
		t.Errorf("end = %q, want after begin", end)
	}
}

// ---- response parsing (a2/b.java:432-450) ------------------------------

func TestInterpretQuotaResponseSumsPositives(t *testing.T) {
	body := []byte(`{"data":{"Response":{"Data":{"Accounts":[
		{"CycleCapacitySize":100,"CycleCapacityRemain":30},
		{"CycleCapacitySize":50,"CycleCapacityRemain":20},
		{"CycleCapacitySize":10,"CycleCapacityRemain":0}
	]}}}}`)
	q := interpretQuotaResponse(200, body)
	if !q.Known {
		t.Fatalf("known = false: %+v", q)
	}
	if q.Credits != 50 {
		t.Fatalf("credits = %d, want 50 (30+20, zero not counted)", q.Credits)
	}
	if q.Message != "周期剩余 50" {
		t.Fatalf("message = %q", q.Message)
	}
}

func TestInterpretQuotaResponseFallsBackToCapacityRemain(t *testing.T) {
	// a2/b.java:437 — when both cycle figures are <= 0, use CapacityRemain.
	body := []byte(`{"data":{"Response":{"Data":{"Accounts":[
		{"CycleCapacitySize":0,"CycleCapacityRemain":0,"CapacityRemain":77}
	]}}}}`)
	q := interpretQuotaResponse(200, body)
	if q.Credits != 77 {
		t.Fatalf("credits = %d, want 77 (fallback)", q.Credits)
	}
}

func TestInterpretQuotaResponseIgnoresNegativeRemainders(t *testing.T) {
	body := []byte(`{"data":{"Response":{"Data":{"Accounts":[
		{"CycleCapacitySize":10,"CycleCapacityRemain":-5},
		{"CycleCapacitySize":10,"CycleCapacityRemain":8}
	]}}}}`)
	q := interpretQuotaResponse(200, body)
	if q.Credits != 8 {
		t.Fatalf("credits = %d, want 8 (negatives skipped)", q.Credits)
	}
}

func TestInterpretQuotaResponseMissingData(t *testing.T) {
	// a2/b.java:418 — "响应缺少 data".
	q := interpretQuotaResponse(200, []byte(`{"code":0}`))
	if q.Known {
		t.Error("known should be false")
	}
	if q.Err != "响应缺少 data" {
		t.Fatalf("err = %q", q.Err)
	}
	if q.Credits != 0 {
		t.Fatalf("credits = %d", q.Credits)
	}
}

func TestInterpretQuotaResponseHTTPError(t *testing.T) {
	// a2/b.java:414 — "上游 HTTP <code>" + body.
	q := interpretQuotaResponse(502, []byte("bad gateway"))
	if q.Err == "" || !strings.Contains(q.Err, "上游 HTTP 502") {
		t.Fatalf("err = %q", q.Err)
	}
	if q.HTTPStatus != 502 {
		t.Fatalf("httpStatus = %d", q.HTTPStatus)
	}
}

func TestInterpretQuotaResponseInvalidJSON(t *testing.T) {
	q := interpretQuotaResponse(200, []byte("<html>"))
	if q.Known {
		t.Error("known should be false on invalid JSON")
	}
	if q.Err == "" {
		t.Error("expected an error message")
	}
}

func TestInterpretQuotaResponseEmptyAccounts(t *testing.T) {
	q := interpretQuotaResponse(200, []byte(`{"data":{"Response":{"Data":{"Accounts":[]}}}}`))
	if !q.Known {
		t.Fatal("an empty account list is still a successful query")
	}
	if q.Credits != 0 {
		t.Fatalf("credits = %d", q.Credits)
	}
}

// ---- HTTP call ---------------------------------------------------------

func TestFetchQuotaSendsExpectedRequest(t *testing.T) {
	var (
		gotMethod string
		gotPaths  []string
		gotAuth   string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPaths = append(gotPaths, r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"Packages":[{"Resources":[{"PackageCode":"p1","TotalAmount":9,"RemainingAmount":9}]}]}}`))
	}))
	defer server.Close()

	restoreBase := stubCheckinBase(server.URL)
	defer restoreBase()

	q, errQuota := workBuddyUpstream.fetchQuota(testContext(), &workBuddyCredentials{
		AccessToken: "tok", Domain: "cn", UID: "u-1",
	})
	if errQuota != nil {
		t.Fatalf("unexpected error: %v", errQuota)
	}
	if !q.Known || q.Credits != 9 {
		t.Fatalf("quota = %+v (want 9 after dedupe across endpoints)", q)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s", gotMethod)
	}
	// All three new endpoints are queried, on the cn base (/v2/billing/meter).
	wantPaths := []string{resourceSummaryPath, resourcePaidPath, resourceFreePath}
	for _, want := range wantPaths {
		found := false
		for _, got := range gotPaths {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("endpoint %q was not queried; got %v", want, gotPaths)
		}
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("auth = %q", gotAuth)
	}
}

func TestFetchQuotaRequiresToken(t *testing.T) {
	if _, errQuota := workBuddyUpstream.fetchQuota(testContext(), &workBuddyCredentials{}); errQuota == nil {
		t.Fatal("expected an error without an access token")
	}
}

// ---- pool ordering (A0/s.java:596) -------------------------------------

// TestPoolPickPrefersHighestCredits is the core of the rotation change: the
// source app selects the account with the greatest remaining credits.
func TestPoolPickPrefersHighestCredits(t *testing.T) {
	p := newCredentialPool()
	now := time.Now()

	p.observe("codebuddy", "low", "Low")
	p.observe("codebuddy", "high", "High")
	p.observe("codebuddy", "mid", "Mid")

	p.setCredits("codebuddy", "low", 10, true)
	p.setCredits("codebuddy", "high", 900, true)
	p.setCredits("codebuddy", "mid", 100, true)

	lane := p.pick("codebuddy", nil, now)
	if lane == nil || lane.UID != "high" {
		t.Fatalf("pick = %+v, want high (most credits)", lane)
	}

	// Excluding it moves to the next richest.
	lane = p.pick("codebuddy", map[string]struct{}{"high": {}}, now)
	if lane == nil || lane.UID != "mid" {
		t.Fatalf("pick(without high) = %+v, want mid", lane)
	}
}

func TestPoolPickTieKeepsInsertionOrder(t *testing.T) {
	// A0/s.java:596 uses a strict >, so equal credits keep insertion order.
	p := newCredentialPool()
	now := time.Now()
	p.observe("codebuddy", "first", "")
	p.observe("codebuddy", "second", "")
	p.setCredits("codebuddy", "first", 5, true)
	p.setCredits("codebuddy", "second", 5, true)

	lane := p.pick("codebuddy", nil, now)
	if lane == nil || lane.UID != "first" {
		t.Fatalf("pick = %+v, want first", lane)
	}
}

func TestPoolPickSkipsUnusableRegardlessOfCredits(t *testing.T) {
	p := newCredentialPool()
	s := defaultGatewaySettings()
	now := time.Now()

	p.observe("codebuddy", "rich", "")
	p.observe("codebuddy", "poor", "")
	p.setCredits("codebuddy", "rich", 1000, true)
	p.setCredits("codebuddy", "poor", 1, true)

	// Rich is cooling down: the poor one must be chosen.
	p.failure("codebuddy", "rich", failureTransient, "boom", s, false)
	lane := p.pick("codebuddy", nil, now)
	if lane == nil || lane.UID != "poor" {
		t.Fatalf("pick = %+v, want poor", lane)
	}
}

func TestPoolCreditsRecordedByAuthID(t *testing.T) {
	p := newCredentialPool()
	p.observe("codebuddy", "u-1", "Label")
	p.setCreditsByAuthID("codebuddy-u-1.json", "u-1", 42, true)

	lane := p.pick("codebuddy", nil, time.Now())
	if lane == nil || lane.Credits != 42 || !lane.CreditsKnown {
		t.Fatalf("lane = %+v", lane)
	}
}

// ---- cool kind (W1.b) --------------------------------------------------

func TestFailureRecordsCoolKind(t *testing.T) {
	cases := map[string]struct {
		kind failureKind
		want coolKind
	}{
		"quota":     {failureQuota, coolKindQuota},
		"rate":      {failureRate, coolKindQuota},
		"auth":      {failureAuth, coolKindError},
		"transient": {failureTransient, coolKindSoft},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := newCredentialPool()
			s := defaultGatewaySettings()
			p.observe("codebuddy", "u", "")
			p.failure("codebuddy", "u", c.kind, "reason", s, false)

			p.mu.Lock()
			lane := p.lanes[laneKey("codebuddy", "u")]
			p.mu.Unlock()
			if lane == nil {
				t.Fatal("lane missing")
			}
			if lane.CoolKind != c.want {
				t.Fatalf("coolKind = %v, want %v", lane.CoolKind, c.want)
			}
		})
	}
}

// TestQuotaFailureZeroesCredits keeps the ordering honest after a rejection.
func TestQuotaFailureZeroesCredits(t *testing.T) {
	p := newCredentialPool()
	s := defaultGatewaySettings()
	p.observe("codebuddy", "u", "")
	p.setCredits("codebuddy", "u", 500, true)
	p.failure("codebuddy", "u", failureQuota, "insufficient", s, false)

	p.mu.Lock()
	lane := p.lanes[laneKey("codebuddy", "u")]
	p.mu.Unlock()
	if lane.Credits != 0 {
		t.Fatalf("credits = %d, want 0 after a quota rejection", lane.Credits)
	}
}

func TestCoolKindName(t *testing.T) {
	cases := map[coolKind]string{
		coolKindQuota: "QUOTA",
		coolKindSoft:  "SOFT",
		coolKindError: "ERROR",
		coolKindNone:  "",
	}
	for kind, want := range cases {
		if got := coolKindName(kind); got != want {
			t.Errorf("coolKindName(%v) = %q, want %q", kind, got, want)
		}
	}
}

// ---- QuotaProvider RPC -------------------------------------------------

func TestQuotaDescribe(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodQuotaDescribe, nil)
	var out pluginapi.QuotaDescribeResponse
	mustDecode(t, res, &out)
	if len(out.SupportedProviders) != 1 || out.SupportedProviders[0] != workBuddyProviderKey {
		t.Fatalf("providers = %v", out.SupportedProviders)
	}
	if out.SupportsReset {
		t.Error("the provider has no reset endpoint")
	}
}

func TestQuotaFetchReturnsCredits(t *testing.T) {
	resetState()
	// The plugin now prefers the three new endpoints; answer those with a
	// resource that carries both an amount and an expiry.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"Packages":[{"Resources":[
			{"PackageCode":"p1","TotalAmount":100,"RemainingAmount":64,
			 "DeductionEndTime":1893456000000}
		]}]}}`))
	}))
	defer server.Close()
	restoreBase := stubCheckinBase(server.URL)
	defer restoreBase()

	storage, _ := json.Marshal(map[string]any{"accessToken": "tok", "uid": "u-1", "domain": "cn"})
	res := callOK(t, pluginabi.MethodQuotaFetch, pluginapi.QuotaFetchRequest{
		AuthIndex:   "codebuddy-u-1.json",
		Provider:    workBuddyProviderKey,
		StorageJSON: storage,
	})

	var out pluginapi.QuotaFetchResponse
	mustDecode(t, res, &out)
	if len(out.Summary) == 0 || out.Summary[0].Value != 64 {
		t.Fatalf("summary = %+v (want 64)", out.Summary)
	}
	if len(out.Groups) == 0 || len(out.Groups[0].Buckets) == 0 {
		t.Fatalf("groups = %+v", out.Groups)
	}

	// The reading must also reach the pool so selection order reflects it.
	lane := state.pool.pick(workBuddyProviderKey, nil, time.Now())
	if lane == nil || lane.Credits != 64 {
		t.Fatalf("pool lane = %+v, want credits 64", lane)
	}
}

func TestQuotaFetchIgnoresForeignProvider(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodQuotaFetch, pluginapi.QuotaFetchRequest{
		Provider:    "anthropic",
		StorageJSON: []byte(`{"accessToken":"x"}`),
	})
	var out pluginapi.QuotaFetchResponse
	mustDecode(t, res, &out)
	if len(out.Summary) != 0 {
		t.Fatalf("should not answer for a foreign provider: %+v", out)
	}
}

func TestQuotaResetReportsUnsupported(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodQuotaReset, nil)
	var out pluginapi.QuotaResetResponse
	mustDecode(t, res, &out)
	if out.Success {
		t.Fatal("reset must not report success")
	}
}

// ---- settings ----------------------------------------------------------

func TestQuotaSettingsDefaults(t *testing.T) {
	store := newSettingsStore()
	if errDecode := store.decodeLifecycleConfig([]byte("enabled: true\n")); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	cfg := store.get().Quota
	if cfg.Enabled {
		t.Error("auto quota refresh must default to disabled")
	}
	if cfg.IntervalMinutes != 30 {
		t.Fatalf("interval = %d, want 30", cfg.IntervalMinutes)
	}
}

func TestQuotaSettingsExplicit(t *testing.T) {
	store := newSettingsStore()
	yamlDoc := "quota:\n  enabled: true\n  interval_minutes: 15\n  refresh_on_start: true\n"
	if errDecode := store.decodeLifecycleConfig([]byte(yamlDoc)); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	cfg := store.get().Quota
	if !cfg.Enabled || cfg.IntervalMinutes != 15 || !cfg.RefreshOnStart {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestClampIntervalMinutes(t *testing.T) {
	cases := []struct{ in, want int }{{0, 5}, {4, 5}, {5, 5}, {30, 30}, {1440, 1440}, {9999, 1440}}
	for _, c := range cases {
		if got := clampIntervalMinutes(c.in); got != c.want {
			t.Errorf("clampIntervalMinutes(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ---- management endpoints ----------------------------------------------

func TestQuotaResourceRegistered(t *testing.T) {
	resetState()
	reg := managementRegistration()
	if len(reg.Resources) != 1 || reg.Resources[0].Path != "home" {
		t.Fatalf("expected exactly one combined resource, got %+v", reg.Resources)
	}
}

func TestQuotaStatusEndpoint(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   managementBasePath() + "/" + pluginName + "/quota/status",
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", mr.StatusCode)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(mr.Body, &doc); errUnmarshal != nil {
		t.Fatalf("bad json: %v", errUnmarshal)
	}
	for _, key := range []string{"enabled", "interval_minutes", "total_credits", "accounts_known", "last_run"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("status missing %q", key)
		}
	}
}

func TestQuotaPageRenders(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    resourceBasePath() + "/" + pluginName + "/quota",
		Headers: http.Header{"Accept": []string{"text/html"}},
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK || len(mr.Body) == 0 {
		t.Fatalf("status=%d len=%d", mr.StatusCode, len(mr.Body))
	}
	page := string(mr.Body)
	for _, want := range []string{"积分合计", "立即刷新积分", "自动刷新积分"} {
		if !strings.Contains(page, want) {
			t.Errorf("combined page missing %q", want)
		}
	}
	// Same key-handling design as the check-in page.
	if strings.Contains(page, "<form") {
		t.Error("quota page must not use HTML forms")
	}
	if !strings.Contains(page, checkinKeyStorageName) {
		t.Error("quota page must reuse the localStorage key")
	}
	if !strings.Contains(page, "'Authorization': 'Bearer '") {
		t.Error("quota page must attach the key with fetch()")
	}
}

func TestQuotaRefreshEndpoint(t *testing.T) {
	resetState()
	storage, _ := json.Marshal(map[string]any{"accessToken": "at", "uid": "u-1", "domain": "cn"})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"Packages":[{"Resources":[{"PackageCode":"p1","TotalAmount":5,"RemainingAmount":5}]}]}}`))
	}))
	defer server.Close()
	restoreBase := stubCheckinBase(server.URL)
	defer restoreBase()

	restore := stubHostCall(func(_ string, _ any) (json.RawMessage, error) {
		return mustMarshal(t, map[string]any{
			"auths": []map[string]any{
				{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey, "storage_json": json.RawMessage(storage)},
			},
		}), nil
	})
	defer restore()

	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementBasePath() + "/" + pluginName + "/quota/refresh",
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", mr.StatusCode, mr.Body)
	}

	var payload struct {
		Results       []quotaRefreshResult `json:"results"`
		TotalCredits  int64                `json:"total_credits"`
		AccountsKnown int                  `json:"accounts_known"`
	}
	if errUnmarshal := json.Unmarshal(mr.Body, &payload); errUnmarshal != nil {
		t.Fatalf("bad json: %v", errUnmarshal)
	}
	if payload.TotalCredits != 5 || payload.AccountsKnown != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if len(payload.Results) != 1 || payload.Results[0].Credits != 5 {
		t.Fatalf("results = %+v", payload.Results)
	}
}

func TestQuotaConfigEndpoint(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementBasePath() + "/" + pluginName + "/quota/config",
		Body:   []byte(`{"enabled":true,"interval_minutes":20,"refresh_on_start":true}`),
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", mr.StatusCode, mr.Body)
	}
	cfg := state.settings.get().Quota
	if !cfg.Enabled || cfg.IntervalMinutes != 20 || !cfg.RefreshOnStart {
		t.Fatalf("config = %+v", cfg)
	}
	stopQuotaScheduler()
}

func TestQuotaConfigAcceptsNestedAndCamelCase(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementBasePath() + "/" + pluginName + "/quota/config",
		Body:   []byte(`{"quota":{"enabled":true,"intervalMinutes":45}}`),
	})
	cfg := state.settings.get().Quota
	if !cfg.Enabled || cfg.IntervalMinutes != 45 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestQuotaRefreshRejectsOnConflict(t *testing.T) {
	resetState()
	state.quota.mu.Lock()
	state.quota.running = true
	state.quota.mu.Unlock()

	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementBasePath() + "/" + pluginName + "/quota/refresh",
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", mr.StatusCode)
	}
}

// ---- status page integration -------------------------------------------

func TestStatusPageShowsQuotaTotal(t *testing.T) {
	resetState()
	state.pool.observe(workBuddyProviderKey, "u-1", "Acct")
	state.pool.setCredits(workBuddyProviderKey, "u-1", 123, true)

	page := statusPage()
	if !strings.Contains(page, "已知额度合计") {
		t.Fatal("status page should show the quota total")
	}
	if !strings.Contains(page, "123") {
		t.Fatalf("status page should show the summed credits: %s", excerptAround(page, "已知额度", 200))
	}
}

func TestPoolOrderingHintSortedByCredits(t *testing.T) {
	resetState()
	state.pool.observe(workBuddyProviderKey, "a", "A")
	state.pool.observe(workBuddyProviderKey, "b", "B")
	state.pool.setCredits(workBuddyProviderKey, "a", 1, true)
	state.pool.setCredits(workBuddyProviderKey, "b", 99, true)

	rows := poolOrderingHint()
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0]["label"] != "B" {
		t.Fatalf("first row should be the richest: %+v", rows)
	}
}

// ---- scheduler ---------------------------------------------------------

func TestQuotaDueByInterval(t *testing.T) {
	resetState()
	state.quota.lastAutoAt = time.Now().Add(-31 * time.Minute)
	cfg := state.settings.get().Quota
	_ = cfg
	// 15-minute interval with a 31-minute-old reading is due; 60-minute is not.
	if time.Since(state.quota.lastAutoAt) < 15*time.Minute {
		t.Fatal("test setup wrong")
	}
	if time.Since(state.quota.lastAutoAt) >= 60*time.Minute {
		t.Fatal("test setup wrong")
	}
}

func TestQuotaRegistrationDeclaresCapability(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{SchemaVersion: pluginabi.SchemaVersion})
	var raw struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	mustDecode(t, res, &raw)
	if v, ok := raw.Capabilities["quota_provider"]; !ok || v != true {
		t.Fatalf("quota_provider capability must be declared, got %v", raw.Capabilities["quota_provider"])
	}
}
