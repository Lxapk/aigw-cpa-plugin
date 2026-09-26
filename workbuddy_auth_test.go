package main

import (
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

// ---- provider identity --------------------------------------------------

// TestWorkBuddyProviderKeyMatchesSource locks in the provider key discovered in
// the APK: a2/b.java:313 returns "codebuddy" even though the display name
// (a2/b.java:681) is "WorkBuddy".
func TestWorkBuddyProviderKeyMatchesSource(t *testing.T) {
	if workBuddyProviderKey != "codebuddy" {
		t.Fatalf("provider key = %q, want %q (a2/b.java:313)", workBuddyProviderKey, "codebuddy")
	}
	if workBuddyDisplayName != "WorkBuddy" {
		t.Fatalf("display name = %q, want WorkBuddy (a2/b.java:681)", workBuddyDisplayName)
	}
}

func TestAuthIdentifier(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodAuthIdentifier, nil)
	var out identifierResponse
	mustDecode(t, res, &out)

	// The Auth page labels the OAuth entry with this value, so it must be the
	// display spelling rather than the lower-case routing key.
	if out.Identifier != workBuddyDisplayName {
		t.Fatalf("identifier = %q, want the display name %q", out.Identifier, workBuddyDisplayName)
	}
	if out.Identifier != "WorkBuddy" {
		t.Fatalf("identifier = %q, want WorkBuddy", out.Identifier)
	}
	// CPA normalises the identifier before storing it, so the plugin must keep
	// recognising "workbuddy" as well as the original "codebuddy" key.
	if !isWorkBuddyProvider(out.Identifier) {
		t.Fatalf("identifier %q is no longer recognised by the plugin's own matcher", out.Identifier)
	}
	if !isWorkBuddyProvider("workbuddy") {
		t.Fatal("the lower-cased form CPA persists is not recognised, so existing accounts would vanish")
	}
	if !isWorkBuddyProvider("codebuddy") {
		t.Fatal("the original provider key is not recognised, so existing accounts would vanish")
	}
}

// TestFrontendAuthIdentifierStaysARoutingKey guards the other identifier: CPA
// mounts the client-API-key gate under the plugin's directory-safe name, so it
// must keep the lower-case pluginName.
func TestFrontendAuthIdentifierStaysARoutingKey(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodFrontendAuthIdentifier, nil)
	var out identifierResponse
	mustDecode(t, res, &out)
	if out.Identifier != pluginName {
		t.Fatalf("frontend identifier = %q, want the routing key %q", out.Identifier, pluginName)
	}
}

func TestRegistrationDeclaresAuthProvider(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{SchemaVersion: pluginabi.SchemaVersion})
	var reg registration
	mustDecode(t, res, &reg)
	if !reg.Capabilities.AuthProvider {
		t.Fatal("auth_provider capability must be declared for the login entry to appear")
	}
}

// ---- domain / base-url selection (a2/b.q, a2/b.p) -----------------------

func TestWorkBuddyBaseURLMatchesSource(t *testing.T) {
	// a2/b.q(): global -> workbuddy.ai, otherwise copilot.tencent.com
	// The region comes from the credential's domain (a2/b.java:284).
	orig := workBuddyGlobalBase()
	setWorkBuddyGlobalBase("https://www.workbuddy.ai")
	defer setWorkBuddyGlobalBase(orig)
	if got := workBuddyBaseURL("www.workbuddy.ai"); got != "https://www.workbuddy.ai" {
		t.Errorf("global base = %q", got)
	}
	if got := workBuddyBaseURL("cn"); got != "https://copilot.tencent.com" {
		t.Errorf("cn base = %q", got)
	}
	if got := workBuddyBaseURL(""); got != "https://copilot.tencent.com" {
		t.Errorf("empty base = %q", got)
	}
}

func TestWorkBuddyOriginURLMatchesSource(t *testing.T) {
	// a2/b.p(): global -> workbuddy.ai, otherwise codebuddy.cn
	if got := workBuddyOriginURL("www.workbuddy.ai"); got != "https://www.codebuddy.ai" {
		t.Errorf("global origin = %q, want the codebuddy.ai product domain", got)
	}
	if got := workBuddyOriginURL("cn"); got != "https://www.codebuddy.cn" {
		t.Errorf("cn origin = %q", got)
	}
}

// ---- state response parsing (V1/k.java:390) ----------------------------

func TestParseWorkBuddyStateResponse(t *testing.T) {
	body := []byte(`{"code":0,"msg":"OK","data":{"state":"abc-123","authUrl":"https://copilot.tencent.com/login?state=abc-123"}}`)
	state, authURL, err := parseWorkBuddyStateResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != "abc-123" {
		t.Errorf("state = %q", state)
	}
	if !strings.Contains(authURL, "abc-123") {
		t.Errorf("authUrl = %q", authURL)
	}
}

func TestParseWorkBuddyStateResponseRejectsMissingFields(t *testing.T) {
	// V1/k.java throws "授权响应缺少 state 或 authUrl".
	for name, body := range map[string]string{
		"missing state":   `{"code":0,"data":{"authUrl":"https://x"}}`,
		"missing authUrl": `{"code":0,"data":{"state":"s"}}`,
		"empty data":      `{"code":0,"data":{}}`,
		"invalid json":    `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseWorkBuddyStateResponse([]byte(body)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// ---- credential parsing (a2/b.t) ---------------------------------------

func TestParseWorkBuddyCredentialsCanonicalFields(t *testing.T) {
	raw := []byte(`{
		"accessToken":"at-1","refreshToken":"rt-1","expiresAt":1893456000,
		"domain":"cn","uid":"u-1","enterpriseId":"e-1","nickname":"Tester"
	}`)
	creds, err := parseWorkBuddyCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.AccessToken != "at-1" || creds.RefreshToken != "rt-1" {
		t.Fatalf("tokens wrong: %+v", creds)
	}
	if creds.UID != "u-1" || creds.EnterpriseID != "e-1" || creds.Nickname != "Tester" {
		t.Fatalf("identity wrong: %+v", creds)
	}
	if creds.ExpiresAt != 1893456000 {
		t.Fatalf("expiresAt = %d", creds.ExpiresAt)
	}
}

func TestParseWorkBuddyCredentialsSnakeCaseAliases(t *testing.T) {
	// a2/b.t() accepts access_token / refresh_token / expires_at / user_id / tenant_id.
	raw := []byte(`{
		"access_token":"at-2","refresh_token":"rt-2","expires_at":1893456001,
		"user_id":"u-2","tenant_id":"t-2","name":"Snake"
	}`)
	creds, err := parseWorkBuddyCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.AccessToken != "at-2" || creds.RefreshToken != "rt-2" {
		t.Fatalf("tokens wrong: %+v", creds)
	}
	if creds.UID != "u-2" || creds.EnterpriseID != "t-2" || creds.Nickname != "Snake" {
		t.Fatalf("identity wrong: %+v", creds)
	}
}

func TestParseWorkBuddyCredentialsBackfillsFromJWT(t *testing.T) {
	// a2/b.t(): when uid/enterpriseId are absent they are recovered from the
	// access token's JWT claims.
	token := makeJWT(t, map[string]any{
		"user_id":   "jwt-user",
		"tenant_id": "jwt-tenant",
		"nickname":  "ignored",
	})
	raw, _ := json.Marshal(map[string]any{"accessToken": token})
	creds, err := parseWorkBuddyCredentials(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.UID != "jwt-user" {
		t.Errorf("uid = %q, want jwt-user", creds.UID)
	}
	if creds.EnterpriseID != "jwt-tenant" {
		t.Errorf("enterpriseId = %q, want jwt-tenant", creds.EnterpriseID)
	}
}

func TestParseWorkBuddyCredentialsRequiresAccessToken(t *testing.T) {
	if _, err := parseWorkBuddyCredentials([]byte(`{"refreshToken":"rt"}`)); err == nil {
		t.Fatal("expected error when accessToken is missing")
	}
}

func TestParseWorkBuddyCredentialsDefaultsDomainToCN(t *testing.T) {
	// V1/i.java case 9: region defaults to "cn".
	creds, err := parseWorkBuddyCredentials([]byte(`{"accessToken":"at"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds.Domain != "cn" {
		t.Fatalf("domain = %q, want cn", creds.Domain)
	}
}

// ---- storage + auth id (a2/b.E) ----------------------------------------

func TestWorkBuddyStorageJSONShape(t *testing.T) {
	// a2/b.E() writes exactly these keys.
	creds := &workBuddyCredentials{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 123,
		Domain: "cn", UID: "u", EnterpriseID: "e", Nickname: "n",
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(creds.storageJSON(), &doc); errUnmarshal != nil {
		t.Fatalf("bad json: %v", errUnmarshal)
	}
	for _, key := range []string{"accessToken", "refreshToken", "expiresAt", "domain", "uid", "enterpriseId", "nickname"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("storage JSON missing key %q", key)
		}
	}
}

func TestWorkBuddyAuthIDFallback(t *testing.T) {
	withUID := &workBuddyCredentials{UID: "u-1", AccessToken: "at"}
	if got := withUID.authID(); got != "u-1" {
		t.Errorf("authID = %q, want u-1", got)
	}

	// a2/b.E(): uid empty -> "codebuddy-" + hashCode(accessToken)
	noUID := &workBuddyCredentials{AccessToken: "at"}
	got := noUID.authID()
	if !strings.HasPrefix(got, "codebuddy-") {
		t.Fatalf("authID = %q, want codebuddy- prefix", got)
	}
}

func TestJavaHashCodeCompatibility(t *testing.T) {
	// Java: "abc".hashCode() == 96354
	if got := hashString("abc"); got != 96354 {
		t.Fatalf("hashString(abc) = %d, want 96354", got)
	}
	if got := hashString(""); got != 0 {
		t.Fatalf("hashString(\"\") = %d, want 0", got)
	}
}

func TestWorkBuddyAuthDataMapsToHostRecord(t *testing.T) {
	creds := &workBuddyCredentials{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1893456000,
		Domain: "cn", UID: "u-1", Nickname: "Nick",
	}
	auth := workBuddyAuthData(creds)
	if auth.Provider != workBuddyProviderKey {
		t.Errorf("provider = %q", auth.Provider)
	}
	if auth.ID != "u-1" {
		t.Errorf("id = %q", auth.ID)
	}
	if auth.Label != "Nick" {
		t.Errorf("label = %q", auth.Label)
	}
	if !strings.HasSuffix(auth.FileName, ".json") {
		t.Errorf("fileName = %q", auth.FileName)
	}
	if len(auth.StorageJSON) == 0 {
		t.Error("storage JSON must be populated for the executor side")
	}
	if auth.NextRefreshAfter.IsZero() {
		t.Error("NextRefreshAfter must reflect expiresAt")
	}
	if auth.Metadata["display_name"] != workBuddyDisplayName {
		t.Errorf("metadata display_name = %v", auth.Metadata["display_name"])
	}
}

// ---- request headers (a2/b.p) ------------------------------------------

func TestApplyWorkBuddyHeadersMatchesSource(t *testing.T) {
	creds := &workBuddyCredentials{
		AccessToken: "tok", Domain: "cn", UID: "u-1", EnterpriseID: "e-1",
	}
	h := http.Header{}
	applyWorkBuddyHeaders(h, creds)

	checks := map[string]string{
		"Authorization":    "Bearer tok",
		"Accept":           "application/json",
		"Content-Type":     "application/json",
		"X-Requested-With": "XMLHttpRequest",
		"User-Agent":       codebuddyUA,
		"Origin":           "https://www.codebuddy.cn",
		"Referer":          "https://www.codebuddy.cn/",
		"X-Product":        "SaaS",
		"X-User-Id":        "u-1",
		"X-Enterprise-Id":  "e-1",
		"X-Tenant-Id":      "e-1",
		"X-Domain":         "cn",
	}
	for key, want := range checks {
		if got := h.Get(key); got != want {
			t.Errorf("header %s = %q, want %q", key, got, want)
		}
	}
}

func TestApplyWorkBuddyHeadersGlobalOrigin(t *testing.T) {
	// The region is decided by the credential's domain suffix (a2/b.java:284),
	// not by a magic "global" string.
	creds := &workBuddyCredentials{AccessToken: "tok", Domain: "www.workbuddy.ai"}
	h := http.Header{}
	applyWorkBuddyHeaders(h, creds)
	// The international *product* domain is codebuddy.ai (variant.rs::
	// productDomain). WorkBuddy clients write www.workbuddy.ai, which CodeBuddy
	// tooling would classify as self-hosted, so it is mapped across.
	if got := h.Get("Origin"); got != "https://www.codebuddy.ai" {
		t.Fatalf("global origin = %q, want https://www.codebuddy.ai", got)
	}
}

// TestIsWorkBuddyGlobalDomainMatchesSource ports a2/b.java:284 D().
func TestIsWorkBuddyGlobalDomainMatchesSource(t *testing.T) {
	cases := map[string]bool{
		"workbuddy.ai":     true,
		"www.workbuddy.ai": true,
		"WWW.WorkBuddy.AI": true,
		"  workbuddy.ai  ": true,
		"api.workbuddy.ai": true,
		// Anything else non-empty is cn.
		"codebuddy.cn":      false,
		"www.codebuddy.cn":  false,
		"cn":                false,
		"workbuddy.ai.evil": false, // suffix must be the real domain
		"notworkbuddy.ai":   false,
		"":                  false,
	}
	for in, want := range cases {
		if got := isWorkBuddyGlobalDomain(in); got != want {
			t.Errorf("isWorkBuddyGlobalDomain(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestGlobalDomainSelectsAllBases ties the domain rule to every endpoint base.
func TestGlobalDomainSelectsAllBases(t *testing.T) {
	origGlobal := workBuddyGlobalBase()
	origCopilot := copilotHostValue()
	restoreCn := redirectAllCnBases("https://CN-CHECKIN")
	setWorkBuddyGlobalBase("https://GLOBAL")
	setCopilotHost("https://CN-CHAT")
	defer func() {
		setWorkBuddyGlobalBase(origGlobal)
		setCopilotHost(origCopilot)
		restoreCn()
	}()

	global := "www.workbuddy.ai"
	cn := "codebuddy.cn"

	// Chat / models base (a2/b.java:717 q()).
	if got := workBuddyBaseURL(global); got != "https://GLOBAL" {
		t.Errorf("global chat base = %q", got)
	}
	if got := workBuddyBaseURL(cn); got != "https://CN-CHAT" {
		t.Errorf("cn chat base = %q", got)
	}
	// Quota base (a2/b.java:406).
	if got := workBuddyQuotaBase(global); got != "https://GLOBAL" {
		t.Errorf("global quota base = %q", got)
	}
	if got := workBuddyQuotaBase(cn); got != "https://CN-CHECKIN" {
		t.Errorf("cn quota base = %q", got)
	}
	// Check-in base (smali a2/b.smali:2660).
	if got := workBuddyCheckinBase(global); got != "https://GLOBAL" {
		t.Errorf("global checkin base = %q", got)
	}
	if got := workBuddyCheckinBase(cn); got != "https://CN-CHECKIN" {
		t.Errorf("cn checkin base = %q", got)
	}
	// Origin/Referer uses the variant's product domain (variant.rs).
	if got := workBuddyOriginURL(global); got != "https://www.codebuddy.ai" {
		t.Errorf("global origin = %q, want https://www.codebuddy.ai", got)
	}
	if got := workBuddyOriginURL(cn); got != "https://www.codebuddy.cn" {
		t.Errorf("cn origin = %q", got)
	}
}

func TestApplyWorkBuddyHeadersOmitsEmptyIdentity(t *testing.T) {
	creds := &workBuddyCredentials{AccessToken: "tok", Domain: "cn"}
	h := http.Header{}
	applyWorkBuddyHeaders(h, creds)
	for _, key := range []string{"X-User-Id", "X-Enterprise-Id", "X-Tenant-Id"} {
		if h.Get(key) != "" {
			t.Errorf("%s should be omitted when empty, got %q", key, h.Get(key))
		}
	}
}

// ---- poll response handling (N1/B.java) --------------------------------

func TestPollWorkBuddyLoginPendingSentinel(t *testing.T) {
	// N1/B.java: code 11217 means "still waiting", not an error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":11217,"msg":"11217:login ing..."}`))
	}))
	defer server.Close()

	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	creds, err := pollWorkBuddyLogin("state-1", variantCn)
	if err != nil {
		t.Fatalf("pending must not be an error, got %v", err)
	}
	if creds != nil {
		t.Fatalf("pending must return nil creds, got %+v", creds)
	}
}

func TestPollWorkBuddyLoginSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v2/plugin/auth/token") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("state") != "state-ok" {
			t.Errorf("state query = %q", r.URL.Query().Get("state"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"at-x","refreshToken":"rt-x","expiresAt":1893456000,"uid":"u-x","enterpriseId":"e-x","nickname":"User X"}}`))
	}))
	defer server.Close()

	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	creds, err := pollWorkBuddyLogin("state-ok", variantCn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds == nil {
		t.Fatal("expected credentials")
	}
	if creds.AccessToken != "at-x" || creds.UID != "u-x" || creds.Nickname != "User X" {
		t.Fatalf("creds = %+v", creds)
	}
}

func TestPollWorkBuddyLoginErrorCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":40001,"msg":"授权已过期"}`))
	}))
	defer server.Close()

	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	_, err := pollWorkBuddyLogin("state-bad", variantCn)
	if err == nil {
		t.Fatal("expected error for non-zero non-pending code")
	}
	if !strings.Contains(err.Error(), "授权已过期") {
		t.Fatalf("error should carry the provider message, got %v", err)
	}
}

func TestPollWorkBuddyLoginSuccessWithoutData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	// N1/B.java: "登录响应缺少 data".
	if _, err := pollWorkBuddyLogin("s", variantCn); err == nil || !strings.Contains(err.Error(), "缺少 data") {
		t.Fatalf("expected missing-data error, got %v", err)
	}
}

func TestPollWorkBuddyLoginHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	// N1/B.java: "轮询失败（HTTP <code>）".
	if _, err := pollWorkBuddyLogin("s", variantCn); err == nil || !strings.Contains(err.Error(), "轮询失败") {
		t.Fatalf("expected http error, got %v", err)
	}
}

// ---- auth.login.start / poll RPC ---------------------------------------

func TestAuthLoginStartReturnsURLAndState(t *testing.T) {
	resetState()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("state request must be POST, got %s", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/v2/plugin/auth/state") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("platform") != "CLI" {
			t.Errorf("platform query = %q", r.URL.Query().Get("platform"))
		}
		if r.Header.Get("User-Agent") != "WorkBuddy/5.5.6" {
			t.Errorf("UA = %q, want the domestic WorkBuddy agent", r.Header.Get("User-Agent"))
		}
		if r.Header.Get("X-Domain") != "copilot.tencent.com" {
			t.Errorf("X-Domain = %q, want copilot.tencent.com", r.Header.Get("X-Domain"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"state":"st-1","authUrl":"https://copilot.tencent.com/login?state=st-1"}}`))
	}))
	defer server.Close()

	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	res := callOK(t, pluginabi.MethodAuthLoginStart, pluginapi.AuthLoginStartRequest{Provider: workBuddyProviderKey})
	var out pluginapi.AuthLoginStartResponse
	mustDecode(t, res, &out)
	if out.State != "st-1" {
		t.Errorf("state = %q", out.State)
	}
	if !strings.Contains(out.URL, "st-1") {
		t.Errorf("url = %q", out.URL)
	}
	if out.Provider != workBuddyProviderKey {
		t.Errorf("provider = %q", out.Provider)
	}
	if out.ExpiresAt.IsZero() {
		t.Error("ExpiresAt must be set so CPA can time the flow out")
	}
}

func TestAuthLoginStartRejectsUnknownProvider(t *testing.T) {
	resetState()
	env := callErr(t, pluginabi.MethodAuthLoginStart, pluginapi.AuthLoginStartRequest{Provider: "openai"})
	if env.Code != "unsupported_provider" {
		t.Fatalf("code = %q", env.Code)
	}
}

func TestAuthLoginStartAcceptsDisplayName(t *testing.T) {
	resetState()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"state":"s","authUrl":"https://x"}}`))
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	// Users may address the provider by its display name.
	res := callOK(t, pluginabi.MethodAuthLoginStart, pluginapi.AuthLoginStartRequest{Provider: workBuddyDisplayName})
	var out pluginapi.AuthLoginStartResponse
	mustDecode(t, res, &out)
	if out.State != "s" {
		t.Fatalf("state = %q", out.State)
	}
}

func TestAuthLoginPollPendingThenSuccess(t *testing.T) {
	resetState()

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			_, _ = w.Write([]byte(`{"code":11217,"msg":"login ing"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"at","refreshToken":"rt","expiresAt":1893456000,"uid":"u-9","nickname":"Nine"}}`))
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	// Pre-seed the pending login so poll has a known state.
	workBuddyPendingLogins.put(&pendingLogin{State: "s9", StartedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)})

	res := callOK(t, pluginabi.MethodAuthLoginPoll, pluginapi.AuthLoginPollRequest{State: "s9"})
	var out pluginapi.AuthLoginPollResponse
	mustDecode(t, res, &out)
	if out.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("first poll status = %q, want pending", out.Status)
	}

	res = callOK(t, pluginabi.MethodAuthLoginPoll, pluginapi.AuthLoginPollRequest{State: "s9"})
	mustDecode(t, res, &out)
	if out.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("second poll status = %q, want success", out.Status)
	}
	if out.Auth.ID != "u-9" || out.Auth.Provider != workBuddyProviderKey {
		t.Fatalf("auth = %+v", out.Auth)
	}
}

func TestAuthLoginPollRequiresState(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodAuthLoginPoll, pluginapi.AuthLoginPollRequest{})
	var out pluginapi.AuthLoginPollResponse
	mustDecode(t, res, &out)
	if out.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %q, want error", out.Status)
	}
}

func TestAuthLoginPollReportsProviderError(t *testing.T) {
	resetState()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":50000,"msg":"用户取消授权"}`))
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	res := callOK(t, pluginabi.MethodAuthLoginPoll, pluginapi.AuthLoginPollRequest{State: "s"})
	var out pluginapi.AuthLoginPollResponse
	mustDecode(t, res, &out)
	if out.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %q", out.Status)
	}
	if !strings.Contains(out.Message, "用户取消授权") {
		t.Fatalf("message = %q", out.Message)
	}
}

// ---- auth.parse --------------------------------------------------------

func TestAuthParseAcceptsStorageJSON(t *testing.T) {
	resetState()
	raw := []byte(`{"accessToken":"at-p","refreshToken":"rt-p","expiresAt":1893456000,"uid":"u-p","nickname":"P"}`)
	res := callOK(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
		Provider: workBuddyProviderKey,
		RawJSON:  raw,
	})
	var out map[string]any
	mustDecode(t, res, &out)
	if out["Handled"] != true {
		t.Fatalf("Handled = %v", out["Handled"])
	}
	auth, ok := out["Auth"].(map[string]any)
	if !ok {
		t.Fatalf("Auth missing: %v", out)
	}
	if auth["Provider"] != workBuddyProviderKey {
		t.Errorf("provider = %v", auth["Provider"])
	}
}

func TestAuthParseAcceptsBareToken(t *testing.T) {
	resetState()
	token := makeJWT(t, map[string]any{"user_id": "jwt-u", "exp": 1893456000})
	res := callOK(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
		Provider: workBuddyProviderKey,
		RawJSON:  []byte(token),
	})
	var out map[string]any
	mustDecode(t, res, &out)
	if out["Handled"] != true {
		t.Fatalf("Handled = %v", out["Handled"])
	}
	auth, _ := out["Auth"].(map[string]any)
	if auth == nil {
		t.Fatalf("Auth missing: %v", out)
	}
	if auth["ID"] != "jwt-u" {
		t.Errorf("id = %v, want jwt-u", auth["ID"])
	}
}

func TestAuthParseRejectsGarbage(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{
		Provider: workBuddyProviderKey,
		RawJSON:  []byte("这不是凭据"),
	})
	var out map[string]any
	mustDecode(t, res, &out)
	if out["Handled"] != true {
		t.Fatalf("Handled = %v", out["Handled"])
	}
	if !hasErrorField(out) {
		t.Fatalf("expected an Error field, got %v", out)
	}
}

func TestAuthParsePassesThroughForeignProvider(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{Provider: "anthropic"})
	var out map[string]any
	mustDecode(t, res, &out)
	if out["Handled"] != false {
		t.Fatalf("foreign provider must not be handled: %v", out)
	}
}

// ---- auth.refresh ------------------------------------------------------

func TestAuthRefreshUpdatesToken(t *testing.T) {
	resetState()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer old-at" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accessToken":"new-at","expiresIn":3600,"refreshToken":"new-rt"}`))
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	res := callOK(t, pluginabi.MethodAuthRefresh, pluginapi.AuthRefreshRequest{
		AuthID:      "u-1",
		StorageJSON: []byte(`{"accessToken":"old-at","refreshToken":"old-rt","domain":"global","uid":"u-1"}`),
	})
	var out struct {
		Auth             map[string]any `json:"Auth"`
		NextRefreshAfter string         `json:"NextRefreshAfter"`
	}
	mustDecode(t, res, &out)
	if out.Auth == nil {
		t.Fatalf("Auth missing: %s", res)
	}
	storage, _ := out.Auth["StorageJSON"].(string)
	if storage == "" {
		t.Fatalf("storage missing: %v", out.Auth)
	}
	decoded, errDecode := base64.StdEncoding.DecodeString(storage)
	if errDecode != nil {
		t.Fatalf("storage is not base64: %v", errDecode)
	}
	var parsed map[string]any
	_ = json.Unmarshal(decoded, &parsed)
	if parsed["accessToken"] != "new-at" {
		t.Errorf("accessToken = %v, want new-at", parsed["accessToken"])
	}
}

func TestAuthRefreshWithoutRefreshToken(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodAuthRefresh, pluginapi.AuthRefreshRequest{
		StorageJSON: []byte(`{"accessToken":"at"}`),
	})
	var out map[string]any
	mustDecode(t, res, &out)
	if !hasErrorField(out) {
		t.Fatalf("expected Error field, got %v", out)
	}
}

// ---- pending login store ----------------------------------------------

func TestPendingLoginStoreEvictsOldest(t *testing.T) {
	store := newPendingLoginStore()
	store.max = 3
	for _, s := range []string{"a", "b", "c", "d"} {
		store.put(&pendingLogin{State: s, StartedAt: time.Now()})
	}
	if len(store.items) != 3 {
		t.Fatalf("size = %d, want 3", len(store.items))
	}
	if _, ok := store.get("d"); !ok {
		t.Fatal("newest entry must survive")
	}
}

func TestPendingLoginStoreDrop(t *testing.T) {
	store := newPendingLoginStore()
	store.put(&pendingLogin{State: "x", StartedAt: time.Now()})
	store.drop("x")
	if _, ok := store.get("x"); ok {
		t.Fatal("entry should be dropped")
	}
}

// ---- helpers -----------------------------------------------------------

// pointWorkBuddyAt redirects the plugin's Tencent endpoints at a test server.
func pointWorkBuddyAt(base string) func() {
	origClient := workBuddyHTTPClient
	origCopilot := copilotHostValue()
	origGlobal := workBuddyGlobalBase()
	setCopilotHost(base)
	// The global domain branch must hit the same test server, otherwise the
	// refresh flow would attempt a real network call.
	setWorkBuddyGlobalBase(base)
	return func() {
		workBuddyHTTPClient = origClient
		setCopilotHost(origCopilot)
		setWorkBuddyGlobalBase(origGlobal)
	}
}

// callErr invokes handleMethod and expects a whole-envelope error.
func mustDecode(t *testing.T, raw json.RawMessage, out any) {
	t.Helper()
	if errUnmarshal := json.Unmarshal(raw, out); errUnmarshal != nil {
		t.Fatalf("unmarshal %s: %v", raw, errUnmarshal)
	}
}

func hasErrorField(m map[string]any) bool {
	v, ok := m["Error"]
	if !ok {
		return false
	}
	s, isString := v.(string)
	return isString && s != ""
}

// makeJWT builds an unsigned JWT with the given claims (signature is ignored by
// the decoder, which only reads the payload segment).
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadRaw, errMarshal := json.Marshal(claims)
	if errMarshal != nil {
		t.Fatalf("marshal claims: %v", errMarshal)
	}
	payload := base64.RawURLEncoding.EncodeToString(payloadRaw)
	return header + "." + payload + "."
}
