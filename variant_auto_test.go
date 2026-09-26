package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// ---- model qualification -------------------------------------------------

// TestModelIDsCarryTheProviderPrefix is the guard for the reported collision:
// the list showed a bare "deepseek-v4.1-flash" alongside another plugin's
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
