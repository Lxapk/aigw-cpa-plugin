package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestVariantForCredentialsHonoursOverrideOverIssuer(t *testing.T) {
	resetState()

	creds := &workBuddyCredentials{
		Domain:      "www.workbuddy.ai",
		AccessToken: makeJWT(t, map[string]any{"iss": "https://www.workbuddy.ai"}),
	}

	withVariantOverride(t, "cn")
	if got := variantForCredentials(creds); got != variantCn {
		t.Fatalf("override cn ignored: got %q", got)
	}

	withVariantOverride(t, "ai")
	creds.Domain = "copilot.tencent.com"
	if got := variantForCredentials(creds); got != variantAi {
		t.Fatalf("override ai ignored: got %q", got)
	}

	withVariantOverride(t, "")
	if got := variantForCredentials(creds); got != variantCn {
		t.Fatalf("auto should follow the domain: got %q", got)
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

// TestMainPageVariantNoteExplainsScope keeps the switch's side effects documented
// in the UI: the override re-routes endpoints without rewriting credentials.
func TestMainPageVariantNoteExplainsScope(t *testing.T) {
	resetState()
	page := renderMainPage()
	if !strings.Contains(page, "不会改写已登录账号的凭据") {
		t.Fatal("variant switch must warn that credentials are not rewritten")
	}
	if !strings.Contains(page, "国际版没有签到接口") {
		t.Fatal("variant switch must mention that the international build has no check-in")
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
