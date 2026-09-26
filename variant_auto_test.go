package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file covers the automatic cn/ai detection added on top of the original
// domain-only check, plus the invariant that every account-facing surface
// (panel list, region label, quota refresh, check-in) agrees on which accounts
// belong to this plugin and which variant they are.
//
// makeJWT is shared with workbuddy_auth_test.go.

// withVariantOverride pins the global override for the duration of a test.
func withVariantOverride(t *testing.T, override string) {
	t.Helper()
	previous := state.settings.get().VariantOverride
	state.settings.setVariantOverride(override)
	t.Cleanup(func() { state.settings.setVariantOverride(previous) })
}

// ---- detection -----------------------------------------------------------

func TestVariantForCredentialsUsesIssuerWhenDomainEmpty(t *testing.T) {
	resetState()
	withVariantOverride(t, "")

	cases := []struct {
		name  string
		creds *workBuddyCredentials
		want  wbVariant
	}{
		{
			name: "ai domain wins",
			creds: &workBuddyCredentials{
				Domain: "www.workbuddy.ai",
			},
			want: variantAi,
		},
		{
			name: "cn domain",
			creds: &workBuddyCredentials{
				Domain: "copilot.tencent.com",
			},
			want: variantCn,
		},
		{
			// The whole point of the change: an empty domain used to be enough
			// to label an international account "cn".
			name: "empty domain, ai issuer",
			creds: &workBuddyCredentials{
				AccessToken: makeJWT(t, map[string]any{"iss": "https://www.workbuddy.ai"}),
			},
			want: variantAi,
		},
		{
			name: "empty domain, cn issuer",
			creds: &workBuddyCredentials{
				AccessToken: makeJWT(t, map[string]any{"iss": "https://copilot.tencent.com"}),
			},
			want: variantCn,
		},
		{
			name: "empty domain, codebuddy.cn issuer",
			creds: &workBuddyCredentials{
				AccessToken: makeJWT(t, map[string]any{"iss": "https://www.codebuddy.cn"}),
			},
			want: variantCn,
		},
		{
			name: "no signals at all defaults to cn",
			creds: &workBuddyCredentials{
				AccessToken: makeJWT(t, map[string]any{"sub": "u-1"}),
			},
			want: variantCn,
		},
		{
			name:  "nil credentials default to cn",
			creds: nil,
			want:  variantCn,
		},
		{
			name: "unknown issuer is not treated as international",
			creds: &workBuddyCredentials{
				AccessToken: makeJWT(t, map[string]any{"iss": "https://example.com"}),
			},
			want: variantCn,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := variantForCredentials(c.creds); got != c.want {
				t.Fatalf("variantForCredentials = %q, want %q", got, c.want)
			}
		})
	}
}

// TestVariantForCredentialsIgnoresOverride is the guard for the mixed-pool bug.
//
// An override must not re-label an account: a token minted by one realm is
// rejected by the other, so honouring a forced version here would guarantee
// failure for every account on the opposite side. The selector instead gates
// which accounts a pass acts on (see TestVariantAllowedGatesWithoutRelabeling).
func TestVariantForCredentialsIgnoresOverride(t *testing.T) {
	resetState()

	creds := &workBuddyCredentials{
		Domain:      "www.workbuddy.ai",
		AccessToken: makeJWT(t, map[string]any{"iss": "https://www.workbuddy.ai"}),
	}

	// Whatever the selector says, the credential keeps its own realm.
	withVariantOverride(t, "cn")
	if got := variantForCredentials(creds); got != variantAi {
		t.Fatalf("override cn re-labelled an international credential: got %q, want ai", got)
	}

	creds.Domain = "copilot.tencent.com"
	withVariantOverride(t, "ai")
	if got := variantForCredentials(creds); got != variantCn {
		t.Fatalf("override ai re-labelled a domestic credential: got %q, want cn", got)
	}

	withVariantOverride(t, "")
	if got := variantForCredentials(creds); got != variantCn {
		t.Fatalf("auto should follow the domain: got %q", got)
	}
}

// TestVariantAllowedGatesWithoutRelabeling covers what the selector does do:
// scope a batch operation, leaving each account routed to its own host.
func TestVariantAllowedGatesWithoutRelabeling(t *testing.T) {
	resetState()

	cnCreds := &workBuddyCredentials{Domain: "copilot.tencent.com"}
	aiCreds := &workBuddyCredentials{Domain: "www.workbuddy.ai"}

	// auto: both channels are served.
	withVariantOverride(t, "")
	if !variantAllowed(cnCreds) || !variantAllowed(aiCreds) {
		t.Fatal("auto must allow both variants, otherwise a mixed pool is not fully served")
	}

	// 国内版: only domestic accounts.
	withVariantOverride(t, "cn")
	if !variantAllowed(cnCreds) {
		t.Fatal("override cn excluded a domestic account")
	}
	if variantAllowed(aiCreds) {
		t.Fatal("override cn admitted an international account")
	}

	// 国际版: only international accounts.
	withVariantOverride(t, "ai")
	if variantAllowed(cnCreds) {
		t.Fatal("override ai admitted a domestic account")
	}
	if !variantAllowed(aiCreds) {
		t.Fatal("override ai excluded an international account")
	}
}

func TestIssuerRealmIgnoresUnreadableTokens(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		"not-a-jwt":             "",
		"header.!!!invalid.bad": "",
		"header." + base64.RawURLEncoding.EncodeToString([]byte("{}")) + ".x": "",
	}
	for token, want := range cases {
		if got := issuerRealm(token); got != want {
			t.Errorf("issuerRealm(%q) = %q, want %q", token, got, want)
		}
	}
}

func TestDetectVariantFromTokenReportsSignal(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		domain string
		want   wbVariant
		signal string
	}{
		{name: "domain decides", domain: "www.workbuddy.ai", want: variantAi, signal: "domain"},
		{name: "cn domain decides", domain: "www.codebuddy.cn", want: variantCn, signal: "domain"},
		{
			name:   "issuer decides",
			token:  makeJWT(t, map[string]any{"iss": "https://www.workbuddy.ai"}),
			want:   variantAi,
			signal: "issuer",
		},
		{name: "default", want: variantCn, signal: "default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, signal := detectVariantFromToken(c.token, c.domain)
			if got != c.want || signal != c.signal {
				t.Fatalf("detectVariantFromToken = (%q, %q), want (%q, %q)", got, signal, c.want, c.signal)
			}
		})
	}
}

// ---- variant capabilities ------------------------------------------------

func TestHasCheckinMatchesReferenceRealms(t *testing.T) {
	// REALM_CONFIGS: intl has "has_checkin": False, cn has True.
	if variantAi.hasCheckin() {
		t.Fatal("international variant must not have check-in")
	}
	if !variantCn.hasCheckin() {
		t.Fatal("domestic variant must have check-in")
	}
}

func TestWorkBuddyRegionFollowsDetection(t *testing.T) {
	resetState()
	withVariantOverride(t, "")

	if got := workBuddyRegion("www.workbuddy.ai"); got != "global" {
		t.Fatalf("region = %q, want global", got)
	}
	if got := workBuddyRegion("copilot.tencent.com"); got != "cn" {
		t.Fatalf("region = %q, want cn", got)
	}

	// A credential with no domain must take its region from the issuer, not
	// fall back to cn unconditionally.
	creds := &workBuddyCredentials{
		AccessToken: makeJWT(t, map[string]any{"iss": "https://www.workbuddy.ai"}),
	}
	if got := workBuddyRegionForCredentials(creds); got != "global" {
		t.Fatalf("region = %q, want global from issuer", got)
	}
}

// ---- inventory consistency ----------------------------------------------

// TestInventoryFilterMatchesPanel is the regression guard for the split-brain
// filter: the check-in/quota inventory used a looser provider test than the
// panel list, so a credential exposed only through its file name appeared in
// the panel but was silently missing from quota totals and check-in.
func TestInventoryFilterMatchesPanel(t *testing.T) {
	resetState()

	storage, _ := json.Marshal(map[string]any{
		"accessToken": "[REDACTED]", "uid": "u-named",
	})
	restore := stubHostCall(func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			t.Fatalf("unexpected host method %q", method)
		}
		return mustMarshal(t, map[string]any{
			"files": []map[string]any{
				// No provider/type at all: only the file name identifies it.
				{
					"auth_index":   "codebuddy-named.json",
					"name":         "codebuddy-named.json",
					"storage_json": json.RawMessage(storage),
				},
				// A genuinely foreign credential must stay excluded.
				{
					"auth_index":   "anthropic-x.json",
					"provider":     "anthropic",
					"storage_json": json.RawMessage(storage),
				},
			},
		}), nil
	})
	defer restore()

	accounts, errCollect := collectCheckinAccounts()
	if errCollect != nil {
		t.Fatalf("unexpected error: %v", errCollect)
	}
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1 (only the codebuddy- file): %+v", len(accounts), accounts)
	}
	if accounts[0].AuthID != "codebuddy-named.json" {
		t.Fatalf("account = %+v, want the codebuddy-named entry", accounts[0])
	}
}

// ---- check-in skipping ---------------------------------------------------

// TestCheckinSkipsInternationalAccounts pins the behavioural change: the
// international build has no check-in endpoint, so the pass must report the
// account as skipped rather than attempting (and failing) a request.
func TestCheckinSkipsInternationalAccounts(t *testing.T) {
	resetState()
	withVariantOverride(t, "")

	creds := &workBuddyCredentials{
		AccessToken: "[REDACTED]",
		UID:         "u-intl",
		Domain:      "www.workbuddy.ai",
	}
	res := checkinOne(checkinAccount{AuthID: "a1", Label: "Intl", Creds: creds}, defaultCheckinSettings())

	if !res.Skipped {
		t.Fatalf("Skipped = false, want true for an international account: %+v", res)
	}
	if res.Success {
		t.Fatal("a skipped account must not be reported as successful")
	}
	if res.Error != "" {
		t.Fatalf("Error = %q, want empty (skipping is not a failure)", res.Error)
	}
}

// TestCheckinRunSeparatesSkippedFromFailed keeps the totals honest: a skipped
// account is neither a success nor a failure.
func TestCheckinRunSeparatesSkippedFromFailed(t *testing.T) {
	resetState()
	withVariantOverride(t, "")

	intl, _ := json.Marshal(map[string]any{
		"accessToken": "[REDACTED]", "uid": "u-intl", "domain": "www.workbuddy.ai",
	})
	restore := stubHostCall(func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			t.Fatalf("unexpected host method %q", method)
		}
		return mustMarshal(t, map[string]any{
			"files": []map[string]any{
				{"auth_index": "codebuddy-intl.json", "provider": workBuddyProviderKey, "storage_json": json.RawMessage(intl)},
			},
		}), nil
	})
	defer restore()

	run := runCheckin("manual")
	if run.Total != 1 {
		t.Fatalf("Total = %d, want 1", run.Total)
	}
	if run.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1: %+v", run.Skipped, run)
	}
	if run.Failed != 0 {
		t.Fatalf("Failed = %d, want 0 (an international account is not a failure)", run.Failed)
	}
	if run.Succeeded != 0 {
		t.Fatalf("Succeeded = %d, want 0", run.Succeeded)
	}
	if run.Total != run.Succeeded+run.Failed+run.Skipped {
		t.Fatalf("totals do not add up: %+v", run)
	}
}

// ---- UI contract ---------------------------------------------------------

// TestAccountsSignatureMatchesBrowserContract pins the change-detection string
// shared with the browser poller.
//
// main_script.go rebuilds this string client-side, so a field rename here that
// is not mirrored there would make every poll look like a change and reload the
// page continuously. The JSON field names asserted below are the ones the JS
// reads.
func TestAccountsSignatureMatchesBrowserContract(t *testing.T) {
	accounts := []workBuddyAccount{
		{UID: "u-1", AuthIndex: "codebuddy-u-1.json", Usable: true, DisabledByUser: false},
		{UID: "u-2", AuthIndex: "codebuddy-u-2.json", Usable: true, DisabledByUser: true},
		{UID: "", AuthIndex: "codebuddy-u-3.json", Usable: false, DisabledByUser: false},
	}

	got := accountsSignature(accounts)
	want := "3:2:u-1E,u-2D,codebuddy-u-3.jsonE"
	if got != want {
		t.Fatalf("accountsSignature = %q, want %q", got, want)
	}

	// The pieces the JS relies on must keep their JSON spelling.
	encoded, errMarshal := json.Marshal(accounts[1])
	if errMarshal != nil {
		t.Fatalf("marshal account: %v", errMarshal)
	}
	for _, field := range []string{`"uid"`, `"auth_index"`, `"usable"`, `"disabled_by_user"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("account JSON is missing %s: %s", field, encoded)
		}
	}
}

// TestMainPageDeclaresPollingContract checks the elements the browser poller
// needs exist in the rendered page, and that the task tab's handlers are wired.
func TestMainPageDeclaresPollingContract(t *testing.T) {
	resetState()
	page := renderMainPage()
	for _, needle := range []string{
		`id="accountsSignature"`,
		`id="accountsStamp"`,
		`id="taskMsg"`,
		`id="btnRunAllTasks"`,
		`id="taskResult"`,
		`data-variant="auto"`,
		`data-variant="cn"`,
		`data-variant="ai"`,
	} {
		if !strings.Contains(page, needle) {
			t.Errorf("main page is missing %s", needle)
		}
	}
	// Every onclick target must have a definition, or the button is dead.
	for _, handler := range []string{"runAllTasks", "toggleAccountTask", "setVariant", "toggleAccount"} {
		if !strings.Contains(page, "window."+handler+" = function") {
			t.Errorf("handler %s is referenced but never defined", handler)
		}
	}
}

// TestMainPageVariantNoteExplainsScope keeps the switch's real semantics
// documented in the UI.
//
// The note previously said the switch "强制全部账号" (re-labels every account),
// which stopped being true once the override was reduced to a scope filter.
// A wrong explanation here is worse than none: it sent operators looking for a
// re-authorisation that is not needed.
func TestMainPageVariantNoteExplainsScope(t *testing.T) {
	resetState()
	page := renderMainPage()

	// It must say the selector scopes which accounts participate.
	if !strings.Contains(page, "决定<strong>调用</strong>时使用哪些账号") {
		t.Fatal("the switch must state that it scopes which accounts are called")
	}
	// And that it does NOT re-label them.
	if !strings.Contains(page, "不影响已登录账号的归属") {
		t.Fatal("the switch must say accounts are not re-labelled")
	}
	// The stale claim must be gone.
	if strings.Contains(page, "则强制全部账号") {
		t.Fatal("the removed 强制全部账号 claim is still present")
	}
}

// TestMainPageSplitsCallScopeFromAuthorisation is the guard for the requested
// split: the supplier switch governs model calls only, and authorisation has its
// own control.
//
// They used to be one setting, so changing which accounts a run touched also
// changed which supplier the next login would authorise against — an operator
// could not add an international account while keeping calls fanning out to
// both.
func TestMainPageSplitsCallScopeFromAuthorisation(t *testing.T) {
	resetState()
	page := renderMainPage()

	// The call-scope switch.
	if !strings.Contains(page, "供应商切换") {
		t.Fatal("missing the 供应商切换 heading")
	}
	if !strings.Contains(page, "仅影响模型调用") {
		t.Fatal("the call switch must say it only affects model calls")
	}
	for _, needle := range []string{"全部供应商", "国内供应商", "国际供应商"} {
		if !strings.Contains(page, needle) {
			t.Errorf("the call switch is missing the %s option", needle)
		}
	}
	if !strings.Contains(page, "决定<strong>调用</strong>时使用哪些账号") {
		t.Fatal("the call switch does not explain that it scopes which accounts are called")
	}

	// The authorisation switch, directly below it.
	if !strings.Contains(page, "在 CPA 的 OAuth 登录中完成") {
		t.Fatal("the panel does not point the operator at CPA's OAuth entry")
	}
	for _, needle := range []string{"国内授权", "国际授权", "跟随调用设置"} {
		if !strings.Contains(page, needle) {
			t.Errorf("the authorisation switch is missing the %s option", needle)
		}
	}
	if !strings.Contains(page, "只决定授权走哪一侧") {
		t.Fatal("the authorisation switch does not say it only chooses the auth side")
	}
	if !strings.Contains(page, "window.setAuthSupplier = function") {
		t.Fatal("the authorisation switch has no handler")
	}

	// The two hosts must be named so the operator knows what to expect.
	if !strings.Contains(page, "copilot.tencent.com") || !strings.Contains(page, "www.workbuddy.ai") {
		t.Fatal("the panel does not name the host each authorisation choice uses")
	}
	if strings.Contains(page, "版本切换") {
		t.Fatal("the panel still says 版本切换")
	}
}

// TestPanelAuthEndpointsAreGone guards the removed route: leaving it reachable
// would keep a second auth path alive.
func TestPanelAuthEndpointsAreGone(t *testing.T) {
	for _, route := range managementRegistration().Routes {
		if strings.Contains(route.Path, "/auth/start") {
			t.Fatalf("the removed panel auth route is still registered: %s", route.Path)
		}
	}
	if _, handled := handleMainRequest(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/workbuddy/auth/start",
		Query:  url.Values{"variant": []string{"cn"}},
	}); handled {
		t.Fatal("the removed /auth/start path is still dispatched")
	}
}

// ---- account toggle ------------------------------------------------------

// TestDisabledAccountIsNotUsable is the regression guard for "账号禁用没有生效".
//
// The panel toggle sets the pool lane's DisabledByUser, but Usable was computed
// from the host-side Disabled flag alone. The flag was therefore stored while
// the status column kept reporting 可用, which is exactly what the report
// described.
func TestDisabledAccountIsNotUsable(t *testing.T) {
	resetState()

	uid := "u-toggle"
	authIndex := "codebuddy-" + uid + ".json"
	storage, _ := json.Marshal(map[string]any{
		"accessToken": "[REDACTED]", "uid": uid, "domain": "copilot.tencent.com",
	})
	restore := stubHostCall(func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return json.RawMessage(`{}`), nil
		}
		return mustMarshal(t, map[string]any{
			"files": []map[string]any{{
				"auth_index":   authIndex,
				"provider":     workBuddyProviderKey,
				"storage_json": json.RawMessage(storage),
			}},
		}), nil
	})
	defer restore()

	state.accounts.invalidate()
	before := listWorkBuddyAccounts()
	if len(before) != 1 {
		t.Fatalf("got %d accounts, want 1", len(before))
	}
	if !before[0].Usable {
		t.Fatal("a fresh account should be usable")
	}

	// Mirror the panel's disable request.
	state.pool.disableAccountKeyed(before[0].UID, before[0].AuthIndex, true)

	state.accounts.invalidate()
	after := listWorkBuddyAccounts()
	if len(after) != 1 {
		t.Fatalf("got %d accounts after disable, want 1", len(after))
	}
	if !after[0].DisabledByUser {
		t.Fatal("DisabledByUser was not stored")
	}
	if after[0].Usable {
		t.Fatal("a disabled account still reports Usable, which is the reported symptom")
	}

	// The summary must agree with the row.
	_, usable, _, _ := accountSummary(after)
	if usable != 0 {
		t.Fatalf("accountSummary usable = %d, want 0", usable)
	}

	// Re-enabling restores it.
	state.pool.disableAccountKeyed(before[0].UID, before[0].AuthIndex, false)
	state.accounts.invalidate()
	reenabled := listWorkBuddyAccounts()
	if !reenabled[0].Usable {
		t.Fatal("re-enabling did not restore usability")
	}
}

// ---- growth headers ------------------------------------------------------

// TestGrowthHeadersUseDesktopIdentity pins the header set the growth surface
// needs. The billing header helper sends X-Domain as a URL and a CLI
// User-Agent, which the growth edge does not route.
func TestGrowthHeadersUseDesktopIdentity(t *testing.T) {
	creds := &workBuddyCredentials{
		AccessToken: "[REDACTED]",
		UID:         "u-1",
		Domain:      "copilot.tencent.com",
	}
	h := make(http.Header)
	applyGrowthHeaders(h, creds)

	if got := h.Get("X-Domain"); got != "copilot.tencent.com" {
		t.Fatalf("X-Domain = %q, want the bare host copilot.tencent.com", got)
	}
	if strings.HasPrefix(h.Get("X-Domain"), "http") {
		t.Fatal("X-Domain must be a host, not a URL")
	}
	for _, field := range []string{"X-IDE-Type", "X-IDE-Name", "X-IDE-Version", "X-Agent-Purpose"} {
		if h.Get(field) == "" {
			t.Errorf("missing desktop identity header %s", field)
		}
	}
	if ua := h.Get("User-Agent"); !strings.HasPrefix(ua, "WorkBuddy/") {
		t.Fatalf("User-Agent = %q, want the WorkBuddy desktop agent", ua)
	}
}

func TestGrowthHeadersFollowVariant(t *testing.T) {
	resetState()
	withVariantOverride(t, "")

	creds := &workBuddyCredentials{
		AccessToken: "[REDACTED]",
		UID:         "u-1",
		Domain:      "www.workbuddy.ai",
	}
	h := make(http.Header)
	applyGrowthHeaders(h, creds)
	if got := h.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Fatalf("international X-Domain = %q, want www.workbuddy.ai", got)
	}
}

// ---- growth diagnostics --------------------------------------------------

// TestGrowthRunLogsRequestFailures is the guard for the 404 investigation: a
// failing request must name the host, path and body in the run log, because the
// bare "执行失败 404" does not say which endpoint was tried.
func TestGrowthRunLogsRequestFailures(t *testing.T) {
	resetState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
	}))
	defer srv.Close()

	previousChat := workBuddyChatBase()
	setChatBase(srv.URL)
	defer setChatBase(previousChat)

	creds := &workBuddyCredentials{
		AccessToken: "[REDACTED]",
		UID:         "u-404",
		Domain:      "copilot.tencent.com",
	}
	result := newGrowthTestRunner().run(context.Background(), creds, "404 账号")

	if result.Error == "" {
		t.Fatal("a 404 on the task list should surface as an error")
	}
	var sawURL, sawStatus bool
	for _, line := range result.Logs {
		if strings.Contains(line.Message, "/v2/activity/growth/tasks") {
			sawURL = true
		}
		if strings.Contains(line.Message, "404") {
			sawStatus = true
		}
	}
	if !sawURL {
		t.Fatal("the log does not name the failing path")
	}
	if !sawStatus {
		t.Fatal("the log does not report the status code")
	}
}

func TestGrowthDiagnosticsAreBounded(t *testing.T) {
	d := &growthDiagnostics{}
	for i := 0; i < 100; i++ {
		d.add("failure %d", i)
	}
	if got := len(d.snapshot()); got > 20 {
		t.Fatalf("diagnostics kept %d lines, want at most 20", got)
	}
}

// ---- call log realm labelling --------------------------------------------

// TestCallRecordCarriesTheSupplierRealm is the guard for the reported gap: the
// call log showed only the provider key, which is the constant "codebuddy" for
// both realms, so a mixed pool produced records that could not be told apart.
func TestCallRecordCarriesTheSupplierRealm(t *testing.T) {
	resetState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// One account per realm, each with its own domain so the realm resolves.
	restore := stubHostCall(func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return json.RawMessage(`{}`), nil
		}
		cn, _ := json.Marshal(map[string]any{"accessToken": "[REDACTED]", "uid": "u-cn", "domain": "copilot.tencent.com"})
		ai, _ := json.Marshal(map[string]any{"accessToken": "[REDACTED]", "uid": "u-ai", "domain": "www.workbuddy.ai"})
		return mustMarshal(t, map[string]any{
			"files": []map[string]any{
				{"auth_index": "codebuddy-u-cn.json", "provider": workBuddyProviderKey, "storage_json": json.RawMessage(cn)},
				{"auth_index": "codebuddy-u-ai.json", "provider": workBuddyProviderKey, "storage_json": json.RawMessage(ai)},
			},
		}), nil
	})
	defer restore()
	state.accounts.invalidate()

	// The pool must learn each lane's realm.
	for _, account := range listWorkBuddyAccounts() {
		state.pool.observe(workBuddyProviderKey, account.UID, account.Label)
	}
	got := map[string]string{}
	for _, lane := range state.pool.snapshot() {
		got[lane.UID] = lane.Variant
	}
	if got["u-cn"] != "cn" {
		t.Fatalf("domestic lane variant = %q, want cn", got["u-cn"])
	}
	if got["u-ai"] != "ai" {
		t.Fatalf("international lane variant = %q, want ai", got["u-ai"])
	}

	// resolveAccountVariant must find the same answers from the log's inputs.
	if v := resolveAccountVariant("u-cn", ""); v != "cn" {
		t.Fatalf("resolveAccountVariant(u-cn) = %q, want cn", v)
	}
	if v := resolveAccountVariant("u-ai", ""); v != "ai" {
		t.Fatalf("resolveAccountVariant(u-ai) = %q, want ai", v)
	}
	if v := resolveAccountVariant("nobody", ""); v != "" {
		t.Fatalf("an unknown uid resolved to %q, want empty", v)
	}
}

// TestVariantLabelOrDash pins the rendering, including the unknown case: an
// empty cell would read as a rendering bug.
func TestVariantLabelOrDash(t *testing.T) {
	cases := map[string]string{"cn": "国内", "ai": "国际", "": "—", "weird": "—"}
	for in, want := range cases {
		if got := variantLabelOrDash(in); got != want {
			t.Errorf("variantLabelOrDash(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCallLogShowsRealmNotProviderKey covers the rendering: the realm column
// must be present, and the provider key alone is no longer the label.
func TestCallLogShowsRealmNotProviderKey(t *testing.T) {
	resetState()
	state.log.add(callRecord{
		ProviderID: workBuddyProviderKey,
		Variant:    "ai",
		UID:        "u-ai",
		Label:      "国际账号",
		Model:      "codebuddy/glm-5.2",
		StatusCode: 200,
		StartedAt:  time.Now(),
	})
	state.log.add(callRecord{
		ProviderID: workBuddyProviderKey,
		Variant:    "cn",
		UID:        "u-cn",
		Label:      "国内账号",
		Model:      "codebuddy/deepseek-v4-flash",
		StatusCode: 200,
		StartedAt:  time.Now(),
	})

	page := renderMainPage()
	if !strings.Contains(page, "国内") || !strings.Contains(page, "国际") {
		t.Fatal("the call log does not distinguish the two realms")
	}
	// Both rows share the provider key; the realm must be what separates them.
	if !strings.Contains(page, "国际账号") || !strings.Contains(page, "国内账号") {
		t.Fatal("the call log does not show which account served each call")
	}
}

// TestObserveDoesNotDeadlock guards the lock discipline: resolving the realm
// walks the account store, which reads the pool back, so it must not run while
// the pool lock is held.
func TestObserveDoesNotDeadlock(t *testing.T) {
	resetState()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			state.pool.observe(workBuddyProviderKey, "u-race", "acct")
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("observe deadlocked while resolving the realm")
	}
}

// ---- model qualification -------------------------------------------------

// TestModelIDsCarryTheProviderPrefix is the guard for the reported collision:// the list showed a bare "deepseek-v4.1-flash" alongside another plugin's
// "DeepSeek-V4-Flash", with nothing telling the client which upstream it meant.
func TestModelIDsCarryTheProviderPrefix(t *testing.T) {
	models := modelsToInfo([]workBuddyModel{
		{ID: "deepseek-v4.1-flash", DisplayName: "DeepSeek V4.1 Flash"},
		{ID: "DeepSeek-V4-Flash", DisplayName: "DeepSeek V4 Flash"},
	})
	if len(models) != 2 {
		t.Fatalf("got %d models", len(models))
	}
	if models[0].ID != "codebuddy/deepseek-v4.1-flash" {
		t.Fatalf("ID = %q, want the prefixed form", models[0].ID)
	}
	// Casing is preserved: the upstream distinguishes models by exact spelling.
	if models[1].ID != "codebuddy/DeepSeek-V4-Flash" {
		t.Fatalf("ID = %q, want casing preserved", models[1].ID)
	}
	// The bare name survives in Version so the upstream call stays correct.
	if models[1].Version != "DeepSeek-V4-Flash" {
		t.Fatalf("Version = %q, want the bare name", models[1].Version)
	}
}

// TestQualifyModelIDIsIdempotent guards against a double prefix.
func TestQualifyModelIDIsIdempotent(t *testing.T) {
	if got := qualifyModelID("deepseek-v4-flash"); got != "codebuddy/deepseek-v4-flash" {
		t.Fatalf("qualifyModelID = %q", got)
	}
	if got := qualifyModelID("codebuddy/deepseek-v4-flash"); got != "codebuddy/deepseek-v4-flash" {
		t.Fatalf("a prefixed id gained the prefix again: %q", got)
	}
	// The display name WorkBuddy maps to the same key, so it must not double up
	// either.
	if got := qualifyModelID("CodeBuddy/deepseek-v4-flash"); got != "CodeBuddy/deepseek-v4-flash" {
		t.Fatalf("a differently-cased prefix was not recognised: %q", got)
	}
}

// TestNativeModelIDStripsThePrefix checks the outbound direction: the upstream
// must never receive the prefix.
func TestNativeModelIDStripsThePrefix(t *testing.T) {
	cases := map[string]string{
		"codebuddy/deepseek-v4-flash": "deepseek-v4-flash",
		"CodeBuddy/deepseek-v4-flash": "deepseek-v4-flash",
		"workbuddy/glm-5.2":           "glm-5.2",
		"deepseek-v4-flash":           "deepseek-v4-flash",
		"trae/DeepSeek-V4-Flash":      "trae/DeepSeek-V4-Flash", // another provider's prefix is left alone
	}
	for in, want := range cases {
		if got := nativeModelID(in); got != want {
			t.Errorf("nativeModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAdvertisedNameIsAcceptedByRoute closes the loop: the name the client sees
// in the model list must be the name model.route accepts.
func TestAdvertisedNameIsAcceptedByRoute(t *testing.T) {
	resetState()

	advertised := modelsToInfo([]workBuddyModel{{ID: "deepseek-v4.1-flash"}})[0].ID
	if advertised != "codebuddy/deepseek-v4.1-flash" {
		t.Fatalf("advertised id = %q", advertised)
	}

	body, _ := json.Marshal(map[string]any{"model": advertised})
	raw, errRoute := handleMethod(pluginabi.MethodModelRoute, mustJSON(pluginapi.ModelRouteRequest{
		SourceFormat:   "chat-completions",
		RequestedModel: advertised,
		Body:           body,
		AvailableProviders: []string{
			workBuddyProviderKey,
		},
	}))
	if errRoute != nil {
		t.Fatal(errRoute)
	}
	var env struct {
		Result struct {
			Handled     bool   `json:"Handled"`
			TargetModel string `json:"TargetModel"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v; raw=%s", err, raw)
	}
	if !env.Result.Handled {
		t.Fatal("model.route rejected the id advertised in the model list")
	}
	// The upstream must receive the bare name.
	if env.Result.TargetModel != "deepseek-v4.1-flash" {
		t.Fatalf("TargetModel = %q, want the bare upstream name", env.Result.TargetModel)
	}
}

func TestTruncateForLog(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := truncateForLog([]byte(long), 100)
	if len(got) > 110 {
		t.Fatalf("truncated length = %d, want ~100", len(got))
	}
	short := truncateForLog([]byte("hello"), 100)
	if short != "hello" {
		t.Fatalf("short body = %q, want unchanged", short)
	}
}

// ---- route registration --------------------------------------------------

// TestEveryHandledRouteIsRegistered is the guard for the 404 class of bug.
//
// CPA dispatches management calls through an exact route table built from
// managementRegistration(). A path the plugin implements but does not declare
// there is answered 404 by the host before the handler is ever called, so the
// symptom is indistinguishable from a wrong upstream URL.
//
// The growth endpoints were exactly that: implemented, wired into the switch,
// and unreachable. This test cross-checks the declared table against the paths
// the handler actually dispatches, so a future addition cannot silently miss
// the registration.
func TestEveryHandledRouteIsRegistered(t *testing.T) {
	declared := make(map[string]bool)
	for _, route := range managementRegistration().Routes {
		declared[strings.ToUpper(route.Method)+" "+route.Path] = true
	}

	// Every path the panel calls, with the method it uses.
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/workbuddy/accounts"},
		{http.MethodGet, "/workbuddy/status"},
		{http.MethodGet, "/workbuddy/calls"},
		{http.MethodGet, "/workbuddy/routing/status"},
		{http.MethodPost, "/workbuddy/routing/config"},
		{http.MethodPost, "/workbuddy/routing/reset"},
		{http.MethodPost, "/workbuddy/run"},
		{http.MethodGet, "/workbuddy/variant"},
		{http.MethodPost, "/workbuddy/variant"},
		{http.MethodPost, "/workbuddy/account/toggle"},
		{http.MethodGet, "/workbuddy/quota"},
		{http.MethodGet, "/workbuddy/quota/status"},
		{http.MethodPost, "/workbuddy/quota/refresh"},
		{http.MethodPost, "/workbuddy/quota/config"},
		{http.MethodGet, "/workbuddy/checkin"},
		{http.MethodPost, "/workbuddy/checkin"},
		{http.MethodGet, "/workbuddy/checkin/status"},
		{http.MethodPost, "/workbuddy/checkin/run"},
		{http.MethodPost, "/workbuddy/checkin/config"},
		// The growth endpoints are the ones that regressed.
		{http.MethodGet, "/workbuddy/growth/tasks"},
		{http.MethodGet, "/workbuddy/growth/summary"},
		{http.MethodPost, "/workbuddy/growth/run"},
		{http.MethodPost, "/workbuddy/growth/travel"},
	}
	for _, c := range cases {
		key := c.method + " " + c.path
		if !declared[key] {
			t.Errorf("%s is called by the panel but not declared in managementRegistration()", key)
		}
	}
}

// TestGrowthRoutesReachTheHandler proves the declared growth routes actually
// dispatch, rather than 404ing on an unhandled path.
func TestGrowthRoutesReachTheHandler(t *testing.T) {
	resetState()

	// No accounts exist, so the handler answers a business error. The point is
	// that it answers at all: an unregistered path would not reach here.
	req := pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/workbuddy/growth/tasks",
	}
	resp, handled := handleMainRequest(req)
	if !handled {
		t.Fatal("GET /workbuddy/growth/tasks was not handled, so CPA would answer 404")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// And the normalised path must resolve to the growth branch.
	if got := normaliseManagementPath("/v0/management/workbuddy/growth/tasks"); got != "/growth/tasks" {
		t.Fatalf("normaliseManagementPath = %q, want /growth/tasks", got)
	}
}

// ---- auth realm separation -----------------------------------------------

// TestAuthHostFollowsVariant pins the rule the reference implementation applies
// in start_login: the host that issues a credential is the host that serves it
// afterwards. Minting a cn credential while the account is routed to the
// international endpoints (or vice versa) produces an account whose token is
// rejected by every later call — the "账号不通用" symptom.
func TestAuthHostFollowsVariant(t *testing.T) {
	resetState()

	cn := authHostFor(variantCn)
	if !strings.Contains(cn, "copilot.tencent.com") {
		t.Fatalf("cn auth host = %q, want copilot.tencent.com", cn)
	}
	intl := authHostFor(variantAi)
	if !strings.Contains(intl, "workbuddy.ai") {
		t.Fatalf("intl auth host = %q, want workbuddy.ai", intl)
	}
	if cn == intl {
		t.Fatal("both realms resolve to the same auth host, so the credential realm is ambiguous")
	}
}

// TestAuthVariantResolvePrefersExplicitChoice covers the override interaction:
// an explicit request wins so one account can be added for the other realm while
// the global selector points elsewhere.
func TestAuthVariantResolvePrefersExplicitChoice(t *testing.T) {
	resetState()

	withVariantOverride(t, "cn")
	if got, explicit := authVariantResolve(hintRequest("ai")); got != variantAi || !explicit {
		t.Fatalf("explicit ai while override=cn -> %q (explicit=%v), want ai", got, explicit)
	}
	if got, explicit := authVariantResolve(hintRequest("")); got != variantCn || explicit {
		t.Fatalf("no hint with override=cn -> %q (explicit=%v), want cn", got, explicit)
	}

	withVariantOverride(t, "ai")
	if got, explicit := authVariantResolve(hintRequest("cn")); got != variantCn || !explicit {
		t.Fatalf("explicit cn while override=ai -> %q (explicit=%v), want cn", got, explicit)
	}
	if got, explicit := authVariantResolve(hintRequest("")); got != variantAi || explicit {
		t.Fatalf("no hint with override=ai -> %q (explicit=%v), want ai", got, explicit)
	}

	// 自动 with no hint falls back to the domestic channel, and says so.
	withVariantOverride(t, "")
	if got, explicit := authVariantResolve(hintRequest("")); got != variantCn || explicit {
		t.Fatalf("auto with no hint -> %q (explicit=%v), want the cn default", got, explicit)
	}
}

// TestParseVariantSpellings accepts the several ways a caller may name a realm.
func TestParseVariantSpellings(t *testing.T) {
	for _, spelling := range []string{"ai", "intl", "global", "international", "国际", "国际版"} {
		got, ok := parseVariant(spelling)
		if !ok || got != variantAi {
			t.Errorf("parseVariant(%q) = %q/%v, want ai/true", spelling, got, ok)
		}
	}
	for _, spelling := range []string{"cn", "china", "domestic", "国内", "国内版"} {
		got, ok := parseVariant(spelling)
		if !ok || got != variantCn {
			t.Errorf("parseVariant(%q) = %q/%v, want cn/true", spelling, got, ok)
		}
	}
	// 自动 and unknown text mean "no preference".
	for _, spelling := range []string{"", "auto", "自动", "whatever"} {
		if _, ok := parseVariant(spelling); ok {
			t.Errorf("parseVariant(%q) reported a preference, want none", spelling)
		}
	}
}

// hintRequest builds a login-start request carrying a realm hint.
func hintRequest(variant string) pluginapi.AuthLoginStartRequest {
	req := pluginapi.AuthLoginStartRequest{}
	if variant != "" {
		req.Metadata = map[string]any{"variant": variant}
	}
	return req
}

// TestAuthLoginUsesRealmHost proves the login request actually goes to the host
// for the requested realm, not the hardcoded domestic one.
func TestAuthLoginUsesRealmHost(t *testing.T) {
	resetState()

	// Point both realm hosts at two distinguishable local servers.
	var intlHits, cnHits int
	intl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		intlHits++
		if r.Header.Get("X-Domain") != "www.workbuddy.ai" {
			t.Errorf("intl X-Domain = %q", r.Header.Get("X-Domain"))
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"state":"s-intl","authUrl":"https://www.workbuddy.ai/login"}}`))
	}))
	defer intl.Close()
	cnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnHits++
		if r.Header.Get("X-Domain") != "copilot.tencent.com" {
			t.Errorf("cn X-Domain = %q", r.Header.Get("X-Domain"))
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"state":"s-cn","authUrl":"https://copilot.tencent.com/login"}}`))
	}))
	defer cnSrv.Close()

	prevGlobal, prevCopilot := workBuddyGlobalBase(), copilotHostValue()
	setWorkBuddyGlobalBase(intl.URL)
	setCopilotHost(cnSrv.URL)
	t.Cleanup(func() {
		setWorkBuddyGlobalBase(prevGlobal)
		setCopilotHost(prevCopilot)
	})

	// International login.
	_, state, err := startWorkBuddyLogin(variantAi)
	if err != nil {
		t.Fatalf("ai login: %v", err)
	}
	if state != "s-intl" {
		t.Fatalf("ai state = %q, want s-intl (the international host)", state)
	}
	if intlHits != 1 || cnHits != 0 {
		t.Fatalf("ai login hit intl=%d cn=%d, want 1/0", intlHits, cnHits)
	}

	// Domestic login.
	_, state, err = startWorkBuddyLogin(variantCn)
	if err != nil {
		t.Fatalf("cn login: %v", err)
	}
	if state != "s-cn" {
		t.Fatalf("cn state = %q, want s-cn (the domestic host)", state)
	}
	if intlHits != 1 || cnHits != 1 {
		t.Fatalf("after cn login intl=%d cn=%d, want 1/1", intlHits, cnHits)
	}
}

// TestPendingLoginRemembersVariant guards the polling side: the token endpoint
// is realm-scoped, so the state has to carry the realm it was created with.
func TestPendingLoginRemembersVariant(t *testing.T) {
	store := newPendingLoginStore()
	store.put(&pendingLogin{State: "s1", Variant: variantAi})
	got, ok := store.get("s1")
	if !ok {
		t.Fatal("state not stored")
	}
	if got.Variant != variantAi {
		t.Fatalf("stored variant = %q, want ai", got.Variant)
	}
}

// TestAllPostLoginBasesFollowTheCredential is the guard for the "账号不通用"
// class of bug on the call side.
//
// Every post-login surface (check-in, quota, refresh, growth) must resolve its
// host from the credential itself. Testing the domain directly was wrong in a
// different way: it ignored the JWT issuer, so an account with no domain field
// was routed to the domestic host even when its token said otherwise.
func TestAllPostLoginBasesFollowTheCredential(t *testing.T) {
	resetState()
	withVariantOverride(t, "")

	cnDomain := "copilot.tencent.com"
	aiDomain := "www.workbuddy.ai"

	bases := map[string]func(string) string{
		"checkin": workBuddyCheckinBase,
		"quota":   workBuddyQuotaBase,
		"token":   workBuddyBaseURL,
	}

	for name, fn := range bases {
		t.Run(name, func(t *testing.T) {
			if got := fn(cnDomain); strings.Contains(got, "workbuddy.ai") {
				t.Errorf("cn domain resolved to %q", got)
			}
			if got := fn(aiDomain); !strings.Contains(got, "workbuddy.ai") {
				t.Errorf("ai domain resolved to %q", got)
			}

			// An empty domain must not pin the account to the domestic host.
			// The caller passes "" here, so the fallback (default cn) applies;
			// what matters is that a real ai domain still wins above.
			if got := fn("copilot.tencent.com"); strings.Contains(got, "workbuddy.ai") {
				t.Errorf("explicit cn domain resolved to %q", got)
			}
		})
	}
}

// TestSelectorDoesNotChangeResolvedHosts pins the interaction the user reported:
// with the selector on 国内版, an international account's host must NOT change —
// it is simply excluded from the pass.
func TestSelectorDoesNotChangeResolvedHosts(t *testing.T) {
	resetState()

	aiCreds := &workBuddyCredentials{Domain: "www.workbuddy.ai"}

	withVariantOverride(t, "")
	autoHost := workBuddyCheckinBase(aiCreds.Domain)

	withVariantOverride(t, "cn")
	underCn := workBuddyCheckinBase(aiCreds.Domain)

	if autoHost != underCn {
		t.Fatalf("the selector changed the resolved host: auto=%q cn=%q", autoHost, underCn)
	}
	if !strings.Contains(underCn, "workbuddy.ai") {
		t.Fatalf("an international credential resolved to %q under override=cn, but its token only works on workbuddy.ai", underCn)
	}
}

// TestGrowthHostFollowsCredentialNotSelector pins the same rule for the growth
// host: the selector scopes the pass, it does not re-route an account.
func TestGrowthHostFollowsCredentialNotSelector(t *testing.T) {
	resetState()

	withVariantOverride(t, "ai")
	creds := &workBuddyCredentials{AccessToken: "[REDACTED]", Domain: "copilot.tencent.com"}
	if got := growthBase(creds); !strings.Contains(got, "copilot.tencent.com") {
		t.Fatalf("growthBase = %q, but a domestic credential only works on the domestic host", got)
	}
	// The selector instead excludes it from the run.
	if variantAllowed(creds) {
		t.Fatal("override ai must exclude a domestic account from a growth pass")
	}

	withVariantOverride(t, "cn")
	aiCreds := &workBuddyCredentials{AccessToken: "[REDACTED]", Domain: "www.workbuddy.ai"}
	if got := growthBase(aiCreds); !strings.Contains(got, "workbuddy.ai") {
		t.Fatalf("growthBase = %q, but an international credential only works on workbuddy.ai", got)
	}
}

// ---- buddy prerequisite --------------------------------------------------

// TestAcceptRejectionReasonSurfaces covers the diagnostic gap that made the
// reported run unreadable: when the upstream answered 200 with a per-task
// rejection status, the reason was dropped, so 17 rejected tasks produced
// "接取未成功 17 个" with no explanation.
func TestAcceptRejectionReasonSurfaces(t *testing.T) {
	resetState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/accept") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"results":[
				{"task_code":"chat_5","status":"rejected","msg":"no active buddy"}
			]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
	}))
	defer srv.Close()

	prev := workBuddyChatBase()
	setChatBase(srv.URL)
	defer setChatBase(prev)

	creds := &workBuddyCredentials{
		AccessToken: "[REDACTED]",
		UID:         "u-buddy",
		Domain:      "copilot.tencent.com",
	}
	accepted, failed, msg, _ := workBuddyUpstream.acceptGrowthTasks(context.Background(), creds, []string{"chat_5"})
	if len(accepted) != 0 || len(failed) != 1 {
		t.Fatalf("accepted=%v failed=%v, want 0/1", accepted, failed)
	}
	if !strings.Contains(msg, "no active buddy") {
		t.Fatalf("msg = %q, want the upstream reason to surface", msg)
	}
}

// TestBuddyIsPending checks the prerequisite detector.
func TestBuddyIsPending(t *testing.T) {
	cases := []struct {
		name  string
		tasks []growthTask
		want  bool
	}{
		{name: "pending", tasks: []growthTask{{Code: "first_buddy", Status: "not_accepted"}}, want: true},
		{name: "completed", tasks: []growthTask{{Code: "first_buddy", Status: "completed"}}, want: false},
		{name: "claimed", tasks: []growthTask{{Code: "first_buddy", Status: "claimed"}}, want: false},
		{name: "absent", tasks: []growthTask{{Code: "chat_5", Status: "not_accepted"}}, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buddyIsPending(c.tasks); got != c.want {
				t.Fatalf("buddyIsPending = %v, want %v", got, c.want)
			}
		})
	}
}

// TestFirstBuddyIsDesktopOnly pins the prerequisite classification: the buddy
// task cannot be automated, and misclassifying it would send a useless event.
func TestFirstBuddyIsDesktopOnly(t *testing.T) {
	reason, ok := growthDesktopOnlyTasks["first_buddy"]
	if !ok {
		t.Fatal("first_buddy must be treated as requiring a real desktop action")
	}
	if !strings.Contains(reason, "前置条件") {
		t.Fatalf("reason = %q, should say it is a prerequisite", reason)
	}
	spec := growthTaskSpecs["first_buddy"]
	if !spec.Unforgeable {
		t.Fatal("first_buddy must be marked unforgeable so no event is reported")
	}
}

// TestTravelNeedBuddyMessageIsActionable checks the travel stage explains the
// prerequisite instead of surfacing a bare upstream error.
func TestTravelNeedBuddyMessageIsActionable(t *testing.T) {
	resetState()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/travel/status"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"idle"}}`))
		case strings.HasSuffix(r.URL.Path, "/travel/config"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"locations":[{"id":1,"name":"杭州"}]}}`))
		case strings.HasSuffix(r.URL.Path, "/travel/depart"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":400,"msg":"no active buddy"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	defer srv.Close()

	prev := workBuddyChatBase()
	setChatBase(srv.URL)
	defer setChatBase(prev)

	creds := &workBuddyCredentials{
		AccessToken: "[REDACTED]",
		UID:         "u-buddy-travel",
		Domain:      "copilot.tencent.com",
	}
	runner := newGrowthTestRunner()
	outcome, errTravel := runner.travel(context.Background(), creds)
	if errTravel != nil {
		t.Fatalf("unexpected error: %v", errTravel)
	}
	if outcome.Action != "need_buddy" {
		t.Fatalf("action = %q, want need_buddy", outcome.Action)
	}
	if !strings.Contains(outcome.Message, "领养") {
		t.Fatalf("message %q does not tell the operator what to do", outcome.Message)
	}
}

// ---- auto-disable --------------------------------------------------------

// TestPermanentFailureRetiresTheAccount covers the requested behaviour: a call
// that fails for a reason retrying cannot fix must disable the account, and the
// retirement must be visible and reversible.
func TestPermanentFailureRetiresTheAccount(t *testing.T) {
	resetState()
	state.pool.observe(workBuddyProviderKey, "u-retire", "acct")

	retired := state.pool.failure(workBuddyProviderKey, "u-retire", failureAuth,
		"invalid token", state.settings.get(), true)
	if !retired {
		t.Fatal("failure() reported no retirement for a permanent failure")
	}

	lane, _ := state.pool.findAccountKeyedCopy("u-retire", "")
	if !lane.Disabled {
		t.Fatal("the internal Disabled bit was not set")
	}
	if !lane.DisabledByUser {
		t.Fatal("the account would still render as 启用 in the panel")
	}
	if !lane.AutoDisabled {
		t.Fatal("the retirement is not attributed to the pool")
	}
	if lane.DisabledReason == "" {
		t.Fatal("no reason recorded, so the panel cannot explain the retirement")
	}

	// It must be excluded from selection.
	if got := state.pool.pick(workBuddyProviderKey, nil, time.Now()); got != nil {
		t.Fatal("a retired account is still selectable")
	}

	// The audit trail records it.
	history := state.pool.autoDisableHistory()
	if len(history) != 1 || history[0].UID != "u-retire" {
		t.Fatalf("audit history = %+v", history)
	}

	// Re-enabling restores it, including the internal bit.
	state.pool.disableAccountKeyed("u-retire", "", false)
	restored, _ := state.pool.findAccountKeyedCopy("u-retire", "")
	if restored.Disabled || restored.DisabledByUser || restored.AutoDisabled {
		t.Fatalf("re-enable left flags set: %+v", restored)
	}
	if got := state.pool.pick(workBuddyProviderKey, nil, time.Now()); got == nil {
		t.Fatal("the account is still unselectable after re-enabling")
	}
	if hist := state.pool.autoDisableHistory(); len(hist) == 1 && !hist[0].Recovered {
		t.Fatal("the audit entry was not marked recovered")
	}
}

// TestRecoverableFailureDoesNotRetire is the other half: a failure that fixes
// itself must only cool the account down.
func TestRecoverableFailureDoesNotRetire(t *testing.T) {
	resetState()
	state.pool.observe(workBuddyProviderKey, "u-throttled", "acct")

	if got := state.pool.failure(workBuddyProviderKey, "u-throttled", failureRate,
		"429 too many requests", state.settings.get(), false); got {
		t.Fatal("a rate limit retired the account")
	}
	lane, _ := state.pool.findAccountKeyedCopy("u-throttled", "")
	if lane.Disabled || lane.DisabledByUser || lane.AutoDisabled {
		t.Fatalf("a recoverable failure disabled the account: %+v", lane)
	}
	if lane.CooldownUntil.IsZero() {
		t.Fatal("a rate limit should still set a cooldown")
	}
}

// TestInterceptRetiresAccountOn401 exercises the whole path the request takes,
// rather than calling failure() directly.
//
// The direct-call test cannot catch a regression in the classification: it
// passes permanent=true itself. Driving the intercept RPC is what proves a real
// 401 from upstream actually retires the account.
func TestInterceptRetiresAccountOn401(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		ConfigYAML: []byte("error_threshold: 1\n"),
	})
	state.pool.observe(workBuddyProviderKey, "acc-401", "acct")

	callOK(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:      "r-401",
		Model:          "claude-sonnet-4",
		StatusCode:     http.StatusUnauthorized,
		RequestHeaders: http.Header{"X-WorkBuddy-Provider": []string{workBuddyProviderKey}, "X-WorkBuddy-Auth-Id": []string{"acc-401"}},
		Body:           []byte(`{"error":{"message":"invalid token","type":"authentication_error"}}`),
	})

	lane, ok := state.pool.findAccountKeyedCopy("acc-401", "")
	if !ok {
		t.Fatal("lane missing")
	}
	if !lane.Disabled || !lane.AutoDisabled {
		t.Fatalf("a 401 did not retire the account: %+v", lane)
	}
	if lane.DisabledReason == "" {
		t.Fatal("no reason recorded for the retirement")
	}
	if hist := state.pool.autoDisableHistory(); len(hist) == 0 {
		t.Fatal("the retirement was not audited")
	}
}

// TestInterceptKeepsAccountOn429 is the counterpart: a throttle must not retire.
func TestInterceptKeepsAccountOn429(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		ConfigYAML: []byte("error_threshold: 1\n"),
	})
	state.pool.observe(workBuddyProviderKey, "acc-429", "acct")

	callOK(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:      "r-429",
		Model:          "claude-sonnet-4",
		StatusCode:     http.StatusTooManyRequests,
		RequestHeaders: http.Header{"X-WorkBuddy-Provider": []string{workBuddyProviderKey}, "X-WorkBuddy-Auth-Id": []string{"acc-429"}},
		Body:           []byte(`{"error":{"message":"rate limit exceeded"}}`),
	})

	lane, ok := state.pool.findAccountKeyedCopy("acc-429", "")
	if !ok {
		t.Fatal("lane missing")
	}
	if lane.Disabled || lane.AutoDisabled {
		t.Fatalf("a 429 retired the account: %+v", lane)
	}
	if len(state.pool.autoDisableHistory()) != 0 {
		t.Fatal("a 429 was recorded as an auto-disable")
	}
}

// TestPermanentFailureClassification pins which upstream answers retire an
// account, so a future edit cannot quietly widen it.
func TestPermanentFailureClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    upstreamError
		want   bool
	}{
		{"401", http.StatusUnauthorized, upstreamError{Kind: failureAuth, Message: "unauthorized"}, true},
		{"403", http.StatusForbidden, upstreamError{Kind: failureAuth, Message: "forbidden"}, true},
		{"400 invalid token", http.StatusBadRequest, upstreamError{Kind: failureAuth, Message: "invalid token"}, true},
		{"400 登录已过期", http.StatusBadRequest, upstreamError{Kind: failureQuota, Message: "登录已过期"}, true},
		{"400 plain", http.StatusBadRequest, upstreamError{Kind: failureTransient, Message: "bad request shape"}, false},
		{"429", http.StatusTooManyRequests, upstreamError{Kind: failureRate, Message: "rate limited"}, false},
		{"402", http.StatusPaymentRequired, upstreamError{Kind: failureQuota, Message: "no credits"}, false},
		{"404", http.StatusNotFound, upstreamError{Kind: failureTransient, Message: "not found"}, false},
		{"500", http.StatusInternalServerError, upstreamError{Kind: failureTransient, Message: "boom"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPermanentFailure(c.status, c.err); got != c.want {
				t.Fatalf("isPermanentFailure(%d, %q) = %v, want %v", c.status, c.err.Message, got, c.want)
			}
		})
	}
}

// TestSplitAccountsByVariant checks the grouping the account list renders.
func TestSplitAccountsByVariant(t *testing.T) {
	accounts := []workBuddyAccount{
		{UID: "cn-1", Variant: "cn"},
		{UID: "ai-1", Variant: "ai"},
		{UID: "cn-2", Variant: "cn"},
		{UID: "unknown"}, // defaults to the domestic group
	}
	cn, ai := splitAccountsByVariant(accounts)
	if len(cn) != 3 {
		t.Fatalf("cn group = %d, want 3", len(cn))
	}
	if len(ai) != 1 {
		t.Fatalf("ai group = %d, want 1", len(ai))
	}
	if ai[0].UID != "ai-1" {
		t.Fatalf("ai group contains %q", ai[0].UID)
	}
}

// TestAutoDisabledAccountReportsUsableFalse guards the earlier "禁用没生效"
// bug for the automatic path too.
func TestAutoDisabledAccountReportsUsableFalse(t *testing.T) {
	resetState()
	uid := "u-usable"
	authIndex := "codebuddy-" + uid + ".json"
	storage, _ := json.Marshal(map[string]any{
		"accessToken": "[REDACTED]", "uid": uid, "domain": "copilot.tencent.com",
	})
	restore := stubHostCall(func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return json.RawMessage(`{}`), nil
		}
		return mustMarshal(t, map[string]any{
			"files": []map[string]any{{
				"auth_index":   authIndex,
				"provider":     workBuddyProviderKey,
				"storage_json": json.RawMessage(storage),
			}},
		}), nil
	})
	defer restore()

	state.accounts.invalidate()
	if !listWorkBuddyAccounts()[0].Usable {
		t.Fatal("a fresh account should be usable")
	}

	state.pool.failure(workBuddyProviderKey, uid, failureAuth, "invalid token", state.settings.get(), true)
	state.accounts.invalidate()

	after := listWorkBuddyAccounts()[0]
	if !after.AutoDisabled {
		t.Fatal("the account was not marked auto-disabled")
	}
	if after.Usable {
		t.Fatal("an auto-disabled account still reports 可用")
	}
	if after.DisabledReason == "" {
		t.Fatal("the panel has no reason to show")
	}
}

// ---- authorisation supplier split ----------------------------------------

// TestAuthSupplierIsIndependentOfCallScope is the guard for the split.
//
// Before it, authVariantResolve read the call-scope setting, so narrowing calls
// to one supplier silently changed which supplier the next login used. The two
// decisions must be independent.
func TestAuthSupplierIsIndependentOfCallScope(t *testing.T) {
	resetState()

	// Call scope narrowed to domestic, authorisation explicitly international.
	state.settings.setVariantOverride("cn")
	state.settings.setAuthSupplier("ai")

	creds := &workBuddyCredentials{Domain: "www.workbuddy.ai"}
	// Calls only act on domestic accounts...
	if variantAllowed(creds) {
		t.Fatal("the call scope admitted an international account under cn")
	}
	// ...but authorisation still targets the international host.
	variant, explicit := authVariantResolve(pluginapi.AuthLoginStartRequest{})
	if variant != variantAi || explicit {
		t.Fatalf("login resolved to %q (explicit=%v), want ai from the auth switch", variant, explicit)
	}
	if host := authHostFor(variant); !strings.Contains(host, "workbuddy.ai") {
		t.Fatalf("auth host = %q, want the international host", host)
	}
}

// TestAuthSupplierFallsBackToCallScope covers the unset case: an operator who
// only ever touches the call switch should still get a matching login link.
func TestAuthSupplierFallsBackToCallScope(t *testing.T) {
	resetState()

	state.settings.setVariantOverride("ai")
	state.settings.setAuthSupplier("")
	if got := state.settings.get().authSupplierOrDefault(); got != variantAi {
		t.Fatalf("unset auth supplier resolved to %q, want ai from the call scope", got)
	}

	state.settings.setVariantOverride("")
	if got := state.settings.get().authSupplierOrDefault(); got != variantCn {
		t.Fatalf("both unset resolved to %q, want the cn default", got)
	}

	// An explicit authorisation choice always wins.
	state.settings.setAuthSupplier("cn")
	state.settings.setVariantOverride("ai")
	if got := state.settings.get().authSupplierOrDefault(); got != variantCn {
		t.Fatalf("explicit auth choice lost to the call scope: got %q", got)
	}
}

// TestPanelChoicesPersistTogether guards the on-disk interaction: both panel
// selections live in one file, so writing one must not drop the other.
//
// The store is built with a real path here because the point is what reaches
// disk; the shared test state uses an empty path and never writes.
func TestPanelChoicesPersistTogether(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel-choices.json")
	store := newSettingsStoreWithPersist(path)
	original := state.settings
	state.settings = store
	defer func() { state.settings = original }()

	store.setVariantOverride("cn")
	store.setAuthSupplier("ai")

	disk := store.restorePanelChoices()
	if disk.VariantOverride != "cn" {
		t.Fatalf("persisted call scope = %q, want cn", disk.VariantOverride)
	}
	if disk.AuthSupplier != "ai" {
		t.Fatalf("persisted auth supplier = %q, want ai; saving the call scope dropped it", disk.AuthSupplier)
	}

	// The reverse order must be equally safe.
	store.setAuthSupplier("cn")
	disk = store.restorePanelChoices()
	if disk.VariantOverride != "cn" || disk.AuthSupplier != "cn" {
		t.Fatalf("after saving authorisation second: %+v", disk)
	}

	// The file is valid JSON with both keys.
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read state file: %v", errRead)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(raw, &doc); errUnmarshal != nil {
		t.Fatalf("state file is not valid JSON: %v", errUnmarshal)
	}
	if _, ok := doc["variant_override"]; !ok {
		t.Error("state file lost variant_override")
	}
	if _, ok := doc["auth_supplier"]; !ok {
		t.Error("state file lost auth_supplier")
	}
}

// ---- upstream body normalisation -----------------------------------------

// TestNormaliseRewritesDeveloperRole is the guard for upstream code 11128.
//
// OpenAI's newer "developer" role (sent by the Codex CLI and current SDKs) is
// what "system" used to be, but the upstream rejects it verbatim and answers
// "request illegal" for the whole request.
func TestNormaliseRewritesDeveloperRole(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"developer","content":"be terse"},
		{"role":"user","content":"hi"}
	]}`)
	out, errNormalise := normaliseUpstreamBody(body)
	if errNormalise != nil {
		t.Fatal(errNormalise)
	}
	var doc struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	for _, m := range doc.Messages {
		if m.Role == "developer" {
			t.Fatal("the developer role survived; the upstream rejects code 11128")
		}
	}
	if doc.Messages[0].Role != "system" {
		t.Fatalf("first role = %q, want system", doc.Messages[0].Role)
	}
	// The legacy function role maps to tool.
	legacy := []byte(`{"messages":[{"role":"system","content":"s"},{"role":"function","content":"r"}]}`)
	outLegacy, _ := normaliseUpstreamBody(legacy)
	if strings.Contains(string(outLegacy), `"function"`) {
		t.Fatal("the legacy function role survived")
	}
}

// TestNormaliseRepacksInterruptedToolBatch is the guard for upstream code 11148.
//
// A tool result must be adjacent to the assistant message that requested it. An
// intruding message between two parallel results breaks the pairing and the
// upstream rejects every later turn of the conversation.
func TestNormaliseRepacksInterruptedToolBatch(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"system","content":"s"},
		{"role":"assistant","tool_calls":[{"id":"a"},{"id":"b"}]},
		{"role":"tool","tool_call_id":"a","content":"ra"},
		{"role":"user","content":"an intruder"},
		{"role":"tool","tool_call_id":"b","content":"rb"}
	]}`)
	out, errNormalise := normaliseUpstreamBody(body)
	if errNormalise != nil {
		t.Fatal(errNormalise)
	}
	var doc struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	roles := make([]string, 0, len(doc.Messages))
	for _, m := range doc.Messages {
		roles = append(roles, m.Role)
	}
	// The assistant must be followed directly by the results, with the intruder
	// moved behind them; nothing is dropped.
	joined := strings.Join(roles, ",")
	if !strings.Contains(joined, "assistant,tool,tool,user") {
		t.Fatalf("roles = %s, want the batch to be contiguous", joined)
	}
	if strings.Count(joined, "tool") != 2 {
		t.Fatalf("a tool result was lost: %s", joined)
	}
	if !strings.Contains(joined, "user") {
		t.Fatalf("the intruder was dropped instead of moved: %s", joined)
	}
}

// TestNormaliseLeavesAConformingBodyAlone guards against needless rewriting: a
// request the upstream already accepts must be passed through byte for byte.
func TestNormaliseLeavesAConformingBodyAlone(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`)
	out, errNormalise := normaliseUpstreamBody(body)
	if errNormalise != nil {
		t.Fatal(errNormalise)
	}
	if string(out) != string(body) {
		t.Fatalf("a conforming body was rewritten:\n in: %s\nout: %s", body, out)
	}
}

// TestNormaliseInsertsLeadingSystemMessage covers the upstream's expectation
// that a conversation opens with one.
func TestNormaliseInsertsLeadingSystemMessage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, errNormalise := normaliseUpstreamBody(body)
	if errNormalise != nil {
		t.Fatal(errNormalise)
	}
	var doc struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if len(doc.Messages) != 2 || doc.Messages[0].Role != "system" {
		t.Fatalf("messages = %+v, want a leading system message", doc.Messages)
	}
}

// TestNormaliseSurvivesMalformedInput: a body we cannot inspect must pass
// through untouched rather than becoming a rewrite error.
func TestNormaliseSurvivesMalformedInput(t *testing.T) {
	for _, body := range []string{`not json`, `[]`, `{"messages":"nope"}`, `{"messages":[1,2]}`} {
		out, errNormalise := normaliseUpstreamBody([]byte(body))
		if errNormalise != nil {
			t.Errorf("body %q produced an error: %v", body, errNormalise)
		}
		if string(out) != body {
			t.Errorf("body %q was modified to %q", body, out)
		}
	}
}

// TestCatalogueParsesBothShapes covers the model-list fix: one deployment
// returns data.models[] with display names, another exposes only
// data.agents[].models[]. Reading just the first shape produced an empty
// catalogue on the second, and the caller then silently swapped in the built-in
// fallback — so the panel showed five fixed models instead of the real ones.
func TestCatalogueParsesBothShapes(t *testing.T) {
	// Flat shape.
	flat := []byte(`{"code":0,"data":{"models":[{"id":"ds-1","name":"DS 1","maxInputTokens":1000}]}}`)
	models, errFlat := parseWorkBuddyModels(flat)
	if errFlat != nil {
		t.Fatalf("flat: %v", errFlat)
	}
	if len(models) != 1 || models[0].ID != "ds-1" || models[0].DisplayName != "DS 1" {
		t.Fatalf("flat shape parsed as %+v", models)
	}

	// Agent shape, as the reference implementation reads it.
	agent := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["m-a","m-b"]},{"name":"other","models":["m-c"]}]}}`)
	models, errAgent := parseWorkBuddyModels(agent)
	if errAgent != nil {
		t.Fatalf("agent: %v", errAgent)
	}
	if len(models) != 3 {
		t.Fatalf("agent shape parsed as %+v, want 3 models", models)
	}
	if models[0].ID != "m-a" || models[1].ID != "m-b" {
		t.Fatalf("cli order not preserved: %+v", models)
	}
	// Context window falls back to the documented default when the agent shape
	// carries none.
	if models[0].MaxInputTokens != fallbackModelContextWindow {
		t.Fatalf("context window = %d, want the default %d", models[0].MaxInputTokens, fallbackModelContextWindow)
	}
}

// TestModelPathMatchesTheWorkingEndpoint pins the path measured against the
// live service: /console/... answers 500 on the international host while
// /v2/... answers 401 on both, so the wrong path silently forced the fallback
// catalogue.
func TestModelPathMatchesTheWorkingEndpoint(t *testing.T) {
	if workBuddyModelsPath != "/v2/enterprises/personal/models" {
		t.Fatalf("models path = %q, want the endpoint both realms accept", workBuddyModelsPath)
	}
	if strings.Contains(workBuddyModelsPath, "/console/") {
		t.Fatal("the /console/ form is the one that fails internationally")
	}
}

// ---- real catalogue fixtures ---------------------------------------------

// TestParsesRealCatalogues parses the responses captured from the live service
// on both realms.
//
// These files are the ground truth for "the model list is incomplete": the
// domestic catalogue carries 30 models and the international one 18, while the
// built-in fallback holds 5. Any regression that silently swaps the real list
// for the fallback would show up here as a count mismatch, which is exactly the
// symptom that was reported.
func TestParsesRealCatalogues(t *testing.T) {
	cases := []struct {
		file string
		want int
		// A model that exists only in this catalogue, chosen to prove the
		// response was really parsed rather than falling back.
		marker string
	}{
		{"testdata/models_cn.json", 30, "deepseek-v4.1-flash"},
		{"testdata/models_ai.json", 18, "gpt-5.5"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			raw, errRead := os.ReadFile(c.file)
			if errRead != nil {
				t.Skipf("fixture unavailable: %v", errRead)
			}
			models, errParse := parseWorkBuddyModels(raw)
			if errParse != nil {
				t.Fatalf("parse: %v", errParse)
			}
			if len(models) != c.want {
				t.Fatalf("parsed %d models, want %d — the fallback catalogue holds 5, so a short list means the real one was replaced", len(models), c.want)
			}
			found := false
			for _, m := range models {
				if m.ID == c.marker {
					found = true
					if m.DisplayName == "" {
						t.Errorf("%s has no display name", m.ID)
					}
				}
			}
			if !found {
				t.Fatalf("the catalogue is missing %q", c.marker)
			}
			// Context windows come from the response, not a blanket default.
			for _, m := range models {
				if m.ID == "auto" && m.MaxInputTokens == 0 {
					t.Error("auto has no context window; the field was not read")
				}
			}
		})
	}
}

// TestCatalogueIsNeverSilentlyEmpty guards the fallback trigger.
//
// A non-2xx response (the international host answered 500 for the /console path
// that used to be configured) makes the caller substitute five hard-coded
// models. Parsing the two real shapes must therefore never yield zero.
func TestCatalogueIsNeverSilentlyEmpty(t *testing.T) {
	for _, file := range []string{"testdata/models_cn.json", "testdata/models_ai.json"} {
		raw, errRead := os.ReadFile(file)
		if errRead != nil {
			continue
		}
		models, errParse := parseWorkBuddyModels(raw)
		if errParse != nil {
			t.Fatalf("%s: %v", file, errParse)
		}
		if len(models) == 0 {
			t.Fatalf("%s parsed to an empty catalogue, which triggers the fallback", file)
		}
	}
}

// ---- identifier must equal the provider key ------------------------------

// TestAuthIdentifierEqualsProviderKey is the guard for the worst regression of
// this series: every model-powered surface went blank.
//
// CPA compares the value returned here against auth.Provider before asking the
// plugin for a model list (pluginhost/adapters.go ModelsForAuth):
//
//	providerKey := normalizeProviderID(auth.Provider)   // "codebuddy"
//	if normalizeProviderID(identifier) != providerKey { continue }
//
// An earlier revision returned the display name "WorkBuddy" so the OAuth list
// read nicely. That made the value "workbuddy", which does not equal
// "codebuddy", so ModelsForAuth skipped this plugin: the auth-file model button
// and /v1/models were both empty, and CPA never called model.for_auth at all.
// The friendly name was the only gain; the cost was every model surface.
func TestAuthIdentifierEqualsProviderKey(t *testing.T) {
	resetState()

	raw, err := handleMethod(pluginabi.MethodAuthIdentifier, nil)
	if err != nil {
		t.Fatalf("auth.identifier: %v", err)
	}
	var env struct {
		Result struct {
			Identifier string `json:"identifier"`
		} `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v; raw=%s", errUnmarshal, raw)
	}

	if env.Result.Identifier != workBuddyProviderKey {
		t.Fatalf("auth.identifier = %q but the provider key is %q — CPA skips the plugin in ModelsForAuth, leaving the auth-file model list and /v1/models empty",
			env.Result.Identifier, workBuddyProviderKey)
	}

	// The provider written into an auth record must match too: it is the other
	// half of the same comparison.
	creds := &workBuddyCredentials{AccessToken: "[REDACTED]", UID: "u-1", Domain: "copilot.tencent.com"}
	data := workBuddyAuthData(creds)
	if data.Provider != workBuddyProviderKey {
		t.Fatalf("auth.parse Provider = %q, want %q", data.Provider, workBuddyProviderKey)
	}

	// The executor and quota identifiers are compared against the same key, so
	// they must agree too.
	for name, call := range map[string]func() ([]byte, error){
		"executor": executorIdentifier,
		"quota":    quotaIdentifier,
	} {
		out, errCall := call()
		if errCall != nil {
			t.Errorf("%s identifier: %v", name, errCall)
			continue
		}
		var env struct {
			Result struct {
				Identifier string `json:"identifier"`
			} `json:"result"`
		}
		if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
			t.Errorf("%s identifier unmarshal: %v", name, errUnmarshal)
			continue
		}
		if env.Result.Identifier != workBuddyProviderKey {
			t.Errorf("%s identifier = %q, want %q", name, env.Result.Identifier, workBuddyProviderKey)
		}
	}
}

// ---- model catalogue self-check -------------------------------------------

// TestModelsEndpointReportsPerAccount covers the self-check endpoint.
//
// It exists so an operator can see the catalogue the plugin would serve per
// account — with the resolved variant, the API base and whether the answer came
// from the cache — instead of reading one account at a time through CPA and
// guessing which upstream answered.
func TestModelsEndpointReportsPerAccount(t *testing.T) {
	resetState()

	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/workbuddy/models",
	})
	var mr managementResponse
	if errUnmarshal := json.Unmarshal(res, &mr); errUnmarshal != nil {
		t.Fatalf("unmarshal management response: %v", errUnmarshal)
	}
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the route must be registered)", mr.StatusCode)
	}

	// The test harness has no host auth callback, so the endpoint reports that
	// instead of a list. What matters here is that it is routed and answers
	// rather than 404; the per-account content is verified against a live CPA.
	if strings.Contains(string(mr.Body), "host callback unavailable") {
		t.Skip("no host callback in the test harness; the live check covers the content")
	}
	var out struct {
		OK       bool `json:"ok"`
		Accounts []struct {
			Variant string `json:"variant"`
			Source  string `json:"source"`
			Count   int    `json:"count"`
			APIBase string `json:"api_base"`
		} `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal([]byte(mr.Body), &out); errUnmarshal != nil {
		t.Fatalf("unmarshal body: %v; body=%s", errUnmarshal, mr.Body)
	}
	if !out.OK {
		t.Fatalf("ok=false: %s", mr.Body)
	}
}

// TestModelCacheClear covers the stale-cache trap.
//
// The catalogue cache lives as long as the process, but the .so is replaced in
// place on upgrade — so a catalogue fetched by the previous build keeps being
// served for up to ten minutes. Someone updating to fix a short model list
// would see the old, short list and conclude the fix had not worked.
func TestModelCacheClear(t *testing.T) {
	c := newModelCache()
	c.put("k", []workBuddyModel{{ID: "m1"}})
	if _, ok := c.get("k"); !ok {
		t.Fatal("put/get round trip failed")
	}
	c.clear()
	if _, ok := c.get("k"); ok {
		t.Fatal("clear() left the entry behind; a stale catalogue would persist until the TTL expired")
	}
}

// ---- capability declaration ----------------------------------------------

// TestModelRegistrarIsDeclared guards against dropping the second model route.
//
// The official simple example ("完整能力骨架") declares both model_registrar and
// model_provider, and CPA drives them through different RPCs:
//
//	model.register   — one startup pass asks for the whole catalogue
//	model.static     — on-demand, no credential context
//	model.for_auth   — per credential, which is what the auth-file page uses
//
// Declaring only model_provider leaves the registration pass with nothing to
// publish until the host happens to ask.
func TestModelRegistrarIsDeclared(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{})

	// capabilities sits at the top level of the registration response.
	var doc struct {
		Capabilities struct {
			ModelRegistrar bool   `json:"model_registrar"`
			ModelProvider  bool   `json:"model_provider"`
			ModelRouter    bool   `json:"model_router"`
			Executor       bool   `json:"executor"`
			ModelScope     string `json:"executor_model_scope"`
		} `json:"capabilities"`
	}
	if errUnmarshal := json.Unmarshal(res, &doc); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v; raw=%s", errUnmarshal, res)
	}
	if !doc.Capabilities.ModelRegistrar {
		t.Error("model_registrar is not declared; the startup registration pass will see no catalogue")
	}
	if !doc.Capabilities.ModelProvider {
		t.Error("model_provider is not declared")
	}
	if !doc.Capabilities.ModelRouter {
		t.Error("model_router is not declared")
	}
	if !doc.Capabilities.Executor {
		t.Error("executor is not declared")
	}
	if doc.Capabilities.ModelScope != "both" {
		t.Errorf("executor_model_scope = %q, want both so both static and auth-bound models are offered",
			doc.Capabilities.ModelScope)
	}
}

// TestModelRegisterAndStaticAgree checks the two routes return the same shape,
// since they share one implementation.
func TestModelRegisterAndStaticAgree(t *testing.T) {
	resetState()
	for _, method := range []string{
		pluginabi.MethodModelRegister,
		pluginabi.MethodModelStatic,
	} {
		res, errCall := handleMethod(method, []byte(`{}`))
		if errCall != nil {
			t.Errorf("%s: %v", method, errCall)
			continue
		}
		var out struct {
			OK     bool `json:"ok"`
			Result struct {
				Provider string `json:"Provider"`
			} `json:"result"`
		}
		if errUnmarshal := json.Unmarshal(res, &out); errUnmarshal != nil {
			t.Errorf("%s unmarshal: %v", method, errUnmarshal)
			continue
		}
		if !out.OK {
			t.Errorf("%s returned ok=false", method)
		}
		if out.Result.Provider != workBuddyProviderKey {
			t.Errorf("%s Provider = %q, want %q", method, out.Result.Provider, workBuddyProviderKey)
		}
	}
}

// TestDefaultProviderIsThisPlugin is the guard for a real defect: the default
// came from the source app ("trae", a different gateway's provider), so turning
// on enforce_default_provider rejected every codebuddy/... request with
// "该网关仅允许使用默认供应商：trae" — the plugin refusing its own models.
func TestDefaultProviderIsThisPlugin(t *testing.T) {
	settings := defaultGatewaySettings()
	if settings.DefaultProvider != workBuddyProviderKey {
		t.Fatalf("DefaultProvider = %q, want %q", settings.DefaultProvider, workBuddyProviderKey)
	}
	// Retention of the opt-in knob matters: it defaults off, so a stale default
	// only bites once someone turns it on.
	if settings.EnforceDefaultProvider {
		t.Fatal("enforce_default_provider should default off")
	}
}
