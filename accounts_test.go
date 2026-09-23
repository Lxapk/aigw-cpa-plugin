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

// These tests cover the three requirements that drove this change:
//
//	1. the account list must come from the auth store, so a freshly logged-in
//	   account shows up without any traffic
//	2. only WorkBuddy/codebuddy accounts may appear
//	3. everything lives on one page

// ---- requirement 1: list without traffic -------------------------------

// TestAccountsAppearWithoutAnyTraffic is the regression test for
// "账号不要调用时候才显示": the pool only learns about a credential when it sees
// traffic, so the list must not depend on it.
func TestAccountsAppearWithoutAnyTraffic(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{
			"auth_index":   "codebuddy-u-1.json",
			"provider":     workBuddyProviderKey,
			"label":        "Fresh Login",
			"storage_json": mustStorage(t, map[string]any{"type": workBuddyProviderKey, "accessToken": "at", "uid": "u-1", "domain": "cn"}),
		},
	})

	// The pool is deliberately untouched: no request, no quota refresh.
	if lanes := state.pool.snapshot(); len(lanes) != 0 {
		t.Fatalf("precondition failed: pool should be empty, got %+v", lanes)
	}

	accounts := listWorkBuddyAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v, want the freshly stored one", accounts)
	}
	if accounts[0].Label != "Fresh Login" || accounts[0].UID != "u-1" {
		t.Fatalf("account = %+v", accounts[0])
	}
	if !accounts[0].Usable {
		t.Error("a healthy stored credential should be usable")
	}
	if accounts[0].Region != "cn" {
		t.Errorf("region = %q, want cn", accounts[0].Region)
	}
}

// TestAccountsCacheServesStaleOnHostError keeps the page useful when the host
// call fails transiently.
func TestAccountsCacheServesStaleOnHostError(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey,
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-1"})},
	})
	if len(listWorkBuddyAccounts()) != 1 {
		t.Fatal("first read should populate the cache")
	}

	// Host now fails.
	restore := stubHostCall(func(_ string, _ any) (json.RawMessage, error) {
		return nil, errHostUnavailable
	})
	defer restore()

	state.accounts.invalidate()
	accounts := listWorkBuddyAccounts()
	if len(accounts) != 1 {
		t.Fatalf("expected the cached list on host failure, got %+v", accounts)
	}
	if state.accounts.lastError() == "" {
		t.Error("the failure should be recorded for display")
	}
}

// TestLoginInvalidatesAccountCache ensures a finished login shows up at once.
func TestLoginInvalidatesAccountCache(t *testing.T) {
	resetState()

	calls := 0
	restore := stubHostCall(func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return json.RawMessage(`{}`), nil
		}
		calls++
		// First call: nothing stored. After the "login", one account exists.
		if calls == 1 {
			return mustMarshal(t, map[string]any{"auths": []any{}}), nil
		}
		return mustMarshal(t, map[string]any{
			"auths": []map[string]any{
				{"auth_index": "codebuddy-u-9.json", "provider": workBuddyProviderKey,
					"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-9"})},
			},
		}), nil
	})
	defer restore()

	if got := len(listWorkBuddyAccounts()); got != 0 {
		t.Fatalf("expected an empty first read, got %d", got)
	}
	// saveAuthThroughHost is what a completed login calls.
	refreshAccountsAfterLogin()
	if got := len(listWorkBuddyAccounts()); got != 1 {
		t.Fatalf("after login the list must update immediately, got %d", got)
	}
}

// ---- requirement 2: only WorkBuddy accounts ----------------------------

// TestAccountListExcludesOtherProviders is the regression test for
// "不要显示其他非WorkBuddy账号".
func TestAccountListExcludesOtherProviders(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey,
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-1"})},
		{"auth_index": "anthropic-x.json", "provider": "anthropic",
			"storage_json": mustStorage(t, map[string]any{"access_token": "sk-x"})},
		{"auth_index": "openai-y.json", "provider": "openai",
			"storage_json": mustStorage(t, map[string]any{"access_token": "sk-y"})},
		{"auth_index": "gemini-z.json", "provider": "gemini",
			"storage_json": mustStorage(t, map[string]any{"access_token": "gz"})},
	})

	accounts := listWorkBuddyAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v, want only the codebuddy one", accounts)
	}
	if accounts[0].AuthIndex != "codebuddy-u-1.json" {
		t.Fatalf("wrong account surfaced: %+v", accounts[0])
	}
}

// TestAccountFilterAcceptsProviderOrType checks both host field spellings.
func TestAccountFilterAcceptsProviderOrType(t *testing.T) {
	cases := []struct {
		name  string
		entry hostAuthEntry
		want  bool
	}{
		{"provider internal key", hostAuthEntry{Provider: "codebuddy"}, true},
		{"provider display name", hostAuthEntry{Provider: "WorkBuddy"}, true},
		{"type internal key", hostAuthEntry{Type: "codebuddy"}, true},
		{"type display name", hostAuthEntry{Type: "WorkBuddy"}, true},
		{"file name prefix", hostAuthEntry{AuthIndex: "codebuddy-u-1.json"}, true},
		{"other provider", hostAuthEntry{Provider: "anthropic"}, false},
		{"other type", hostAuthEntry{Type: "openai"}, false},
		{"unrelated file", hostAuthEntry{AuthIndex: "claude.json"}, false},
		{"empty", hostAuthEntry{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isWorkBuddyAuthEntry(c.entry); got != c.want {
				t.Fatalf("isWorkBuddyAuthEntry(%+v) = %v, want %v", c.entry, got, c.want)
			}
		})
	}
}

// TestAccountListShowsUnparsableCodebuddyEntry makes a broken file visible
// rather than silently vanishing.
func TestAccountListShowsUnparsableCodebuddyEntry(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-broken.json", "provider": workBuddyProviderKey,
			"storage_json": json.RawMessage(`{}`)},
	})

	accounts := listWorkBuddyAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v", accounts)
	}
	if accounts[0].Usable {
		t.Error("an unparsable credential must not be reported usable")
	}
	if !strings.Contains(accounts[0].Reason, "无法解析") {
		t.Fatalf("reason = %q", accounts[0].Reason)
	}
}

// ---- account view details ----------------------------------------------

func TestAccountViewMarksExpiredCredential(t *testing.T) {
	resetState()
	past := time.Now().Add(-2 * time.Hour).Unix()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey,
			"storage_json": mustStorage(t, map[string]any{
				"accessToken": "at", "uid": "u-1", "domain": "cn", "expiresAt": past,
			})},
	})

	accounts := listWorkBuddyAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v", accounts)
	}
	if !accounts[0].Expired {
		t.Error("a past expiry must be flagged")
	}
	if accounts[0].Usable {
		t.Error("an expired credential must not be usable")
	}
}

func TestAccountViewReportsGlobalRegion(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-g.json", "provider": workBuddyProviderKey,
			"storage_json": mustStorage(t, map[string]any{
				"accessToken": "at", "uid": "g-1", "domain": "www.workbuddy.ai",
			})},
	})

	accounts := listWorkBuddyAccounts()
	if len(accounts) != 1 || accounts[0].Region != "global" {
		t.Fatalf("accounts = %+v, want global", accounts)
	}
}

func TestAccountViewSkipsDisabled(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey, "disabled": true,
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-1"})},
	})
	accounts := listWorkBuddyAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v", accounts)
	}
	if accounts[0].Usable {
		t.Error("a disabled credential must not be usable")
	}
	if !accounts[0].Disabled {
		t.Error("the disabled flag should be surfaced")
	}
}

func TestAccountViewFoldsInPoolCooldown(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey,
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-1"})},
	})
	// Simulate a rejection seen by the executor.
	state.pool.observe(workBuddyProviderKey, "u-1", "Acct")
	state.pool.failure(workBuddyProviderKey, "u-1", failureQuota, "余额不足", defaultGatewaySettings(), false)

	accounts := listWorkBuddyAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v", accounts)
	}
	if accounts[0].CoolKind != "QUOTA" {
		t.Errorf("coolKind = %q, want QUOTA", accounts[0].CoolKind)
	}
	if accounts[0].Usable {
		t.Error("a cooling credential must not be usable")
	}
	if accounts[0].Reason != "余额不足" {
		t.Errorf("reason = %q", accounts[0].Reason)
	}
}

func TestAccountSummary(t *testing.T) {
	accounts := []workBuddyAccount{
		{Usable: true, CreditsKnown: true, Credits: 10},
		{Usable: false, CreditsKnown: true, Credits: 5},
		{Usable: true},
	}
	total, usable, known, credits := accountSummary(accounts)
	if total != 3 || usable != 2 || known != 2 || credits != 15 {
		t.Fatalf("summary = %d/%d/%d/%d", total, usable, known, credits)
	}
}

// ---- requirement 3: one page -------------------------------------------

// TestSinglePageContainsEverything checks the combined view carries every
// feature that used to live on separate pages.
func TestSinglePageContainsEverything(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey, "label": "Acct One",
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-1", "domain": "cn"})},
	})

	page := mainPage()
	for _, want := range []string{
		"管理密钥",         // key section
		"WorkBuddy 账号", // account list
		"账号总数",         // account summary
		"签到设置",         // check-in schedule
		"额度刷新设置",       // quota schedule
		"调用统计",         // usage
		"网关设置",         // gateway config
		"一键：签到 + 刷新额度",
		"Acct One", // the account is rendered server-side
	} {
		if !strings.Contains(page, want) {
			t.Errorf("combined page missing %q", want)
		}
	}
	// Still no HTML forms (the header-auth constraint).
	if strings.Contains(page, "<form") {
		t.Error("combined page must not use HTML forms")
	}
	// Anchor navigation for the single-page layout.
	if !strings.Contains(page, `href="#sec-accounts"`) {
		t.Error("expected in-page navigation")
	}
}

// TestCombinedPageServedAtRoot verifies the menu entry resolves.
func TestCombinedPageServedAtRoot(t *testing.T) {
	resetState()
	installAuthList(t, nil)

	for _, path := range []string{
		"/v0/resource/plugins/" + pluginName + "/",
		"/v0/resource/plugins/" + pluginName,
	} {
		res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
			Method:  http.MethodGet,
			Path:    path,
			Headers: http.Header{"Accept": []string{"text/html"}},
		})
		var mr managementResponse
		mustDecode(t, res, &mr)
		if mr.StatusCode != http.StatusOK || len(mr.Body) == 0 {
			t.Fatalf("%s -> status=%d len=%d", path, mr.StatusCode, len(mr.Body))
		}
		if !strings.Contains(string(mr.Body), "WorkBuddy 账号") {
			t.Fatalf("%s did not render the combined page", path)
		}
	}
}

// TestAccountsEndpointReturnsJSON covers the in-place refresh endpoint.
func TestAccountsEndpointReturnsJSON(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey, "label": "A",
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-1"})},
		{"auth_index": "anthropic.json", "provider": "anthropic",
			"storage_json": mustStorage(t, map[string]any{"access_token": "x"})},
	})

	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   managementBasePath() + "/" + pluginName + "/accounts",
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", mr.StatusCode)
	}
	var payload struct {
		Accounts     []workBuddyAccount `json:"accounts"`
		Total        int                `json:"total"`
		Usable       int                `json:"usable"`
		TotalCredits int64              `json:"total_credits"`
	}
	if errUnmarshal := json.Unmarshal(mr.Body, &payload); errUnmarshal != nil {
		t.Fatalf("bad json: %v", errUnmarshal)
	}
	if payload.Total != 1 || len(payload.Accounts) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Accounts[0].AuthIndex != "codebuddy-u-1.json" {
		t.Fatalf("foreign account leaked: %+v", payload.Accounts)
	}
}

// TestRunEndpointDoesBoth verifies the single combined action.
func TestRunEndpointDoesBoth(t *testing.T) {
	resetState()

	// Check-in server and quota server are the same host in practice.
	server := newTencentStub(t)
	defer server.Close()
	restoreBase := stubCheckinBase(server.URL)
	defer restoreBase()
	origGlobal := workBuddyGlobalBase()
	setWorkBuddyGlobalBase(server.URL)
	defer setWorkBuddyGlobalBase(origGlobal)

	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey, "label": "A",
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-1", "domain": "cn"})},
	})

	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementBasePath() + "/" + pluginName + "/run",
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", mr.StatusCode, mr.Body)
	}

	var payload struct {
		Checkin *checkinRun          `json:"checkin"`
		Quota   []quotaRefreshResult `json:"quota"`
	}
	if errUnmarshal := json.Unmarshal(mr.Body, &payload); errUnmarshal != nil {
		t.Fatalf("bad json: %v", errUnmarshal)
	}
	if payload.Checkin == nil {
		t.Fatal("check-in results missing")
	}
	if payload.Checkin.Total != 1 {
		t.Fatalf("checkin total = %d, want 1", payload.Checkin.Total)
	}
	if len(payload.Quota) != 1 {
		t.Fatalf("quota results = %+v", payload.Quota)
	}
}

// ---- helpers ------------------------------------------------------------

// installAuthList stubs host.auth.list with the given raw entries.
func installAuthList(t *testing.T, entries []map[string]any) {
	t.Helper()
	restore := stubHostCall(func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			if entries == nil {
				entries = []map[string]any{}
			}
			return mustMarshal(t, map[string]any{"auths": entries}), nil
		case "host.auth.get":
			return json.RawMessage(`{}`), nil
		}
		return json.RawMessage(`{}`), nil
	})
	t.Cleanup(restore)
	state.accounts.invalidate()
}

// mustStorage marshals a credential map into a json.RawMessage.
func mustStorage(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		t.Fatalf("marshal storage: %v", errMarshal)
	}
	return raw
}

// newTencentStub answers both the check-in and quota endpoints.
func newTencentStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "daily-checkin"):
			_, _ = w.Write([]byte(`{"code":0}`))
		case strings.Contains(r.URL.Path, "get-user-resource"):
			_, _ = w.Write([]byte(`{"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":80,"CycleCapacityRemain":80}]}}}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
}
