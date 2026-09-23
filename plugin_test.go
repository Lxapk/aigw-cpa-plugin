package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// callOK invokes handleMethod and unwraps the success envelope.
func callOK(t *testing.T, method string, payload any) json.RawMessage {
	t.Helper()
	var raw []byte
	if payload != nil {
		var errMarshal error
		raw, errMarshal = json.Marshal(payload)
		if errMarshal != nil {
			t.Fatalf("marshal payload: %v", errMarshal)
		}
	}
	out, errHandle := handleMethod(method, raw)
	if errHandle != nil {
		t.Fatalf("%s: handle error: %v", method, errHandle)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		t.Fatalf("%s: bad envelope %s: %v", method, out, errUnmarshal)
	}
	if !env.OK {
		t.Fatalf("%s: envelope not ok: %s", method, out)
	}
	return env.Result
}

func callErr(t *testing.T, method string, payload any) *envelopeError {
	t.Helper()
	var raw []byte
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	out, errHandle := handleMethod(method, raw)
	if errHandle != nil {
		t.Fatalf("%s: unexpected handle error: %v", method, errHandle)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(out, &env); errUnmarshal != nil {
		t.Fatalf("%s: bad envelope: %v", method, errUnmarshal)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("%s: expected error envelope, got %s", method, out)
	}
	return env.Error
}

// ---- registration ------------------------------------------------------

func TestRegistrationDeclaresPortedCapabilities(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		SchemaVersion: pluginabi.SchemaVersion,
	})
	var reg registration
	if errUnmarshal := json.Unmarshal(res, &reg); errUnmarshal != nil {
		t.Fatalf("unmarshal registration: %v", errUnmarshal)
	}
	if reg.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", reg.SchemaVersion, pluginabi.SchemaVersion)
	}
	if reg.Metadata.Name != pluginName {
		t.Fatalf("name = %q, want %q", reg.Metadata.Name, pluginName)
	}
	caps := reg.Capabilities
	if !caps.FrontendAuthProvider || !caps.RequestInterceptor ||
		!caps.ResponseInterceptor || !caps.StreamChunkInterceptor ||
		!caps.UsagePlugin || !caps.ManagementAPI {
		t.Fatalf("missing capabilities: %+v", caps)
	}
	if len(reg.Metadata.ConfigFields) == 0 {
		t.Fatal("no config fields declared")
	}
}

// ---- settings defaults (V1.s parity) -----------------------------------

func TestDefaultSettingsMatchSourceApp(t *testing.T) {
	d := defaultGatewaySettings()
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"port", d.Port, 8790},
		{"allow_no_key", d.AllowNoKey, true},
		{"expose_lan", d.ExposeLAN, true},
		{"only_usable_models", d.OnlyUsableModels, false},
		{"refresh_skew_seconds", d.RefreshSkewSeconds, int64(86400)},
		{"max_rotate", d.MaxRotate, 3},
		{"quota_cooldown_millis", d.QuotaCooldownMillis, int64(43200000)},
		{"soft_cooldown_millis", d.SoftCooldownMillis, int64(60000)},
		{"error_threshold", d.ErrorThreshold, 3},
		{"error_cooldown_millis", d.ErrorCooldownMillis, int64(600000)},
		{"log_retention_days", d.LogRetentionDays, 30},
		{"default_provider", d.DefaultProvider, "trae"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLifecycleConfigOverride(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		ConfigYAML: []byte("enabled: true\npriority: 5\nport: 9100\napi_key: sk-test\ndefault_provider: OpenAI\nmax_rotate: 7\n"),
	})
	got := state.settings.get()
	if got.Port != 9100 {
		t.Errorf("port = %d, want 9100", got.Port)
	}
	if got.APIKey != "sk-test" {
		t.Errorf("api_key = %q", got.APIKey)
	}
	if got.DefaultProvider != "openai" {
		t.Errorf("default_provider = %q, want lower-cased openai", got.DefaultProvider)
	}
	if got.MaxRotate != 7 {
		t.Errorf("max_rotate = %d, want 7", got.MaxRotate)
	}
}

// ---- frontend auth (port of V1/o.j) ------------------------------------

func TestFrontendAuthAllowNoKeyBypasses(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		ConfigYAML: []byte("allow_no_key: true\napi_key: sk-secret\n"),
	})
	res := callOK(t, pluginabi.MethodFrontendAuthAuthenticate, pluginapi.FrontendAuthRequest{
		Method: "POST", Path: "/v1/chat/completions",
	})
	var out pluginapi.FrontendAuthResponse
	_ = json.Unmarshal(res, &out)
	if !out.Authenticated {
		t.Fatal("allow_no_key should authenticate anonymous callers")
	}
}

func TestFrontendAuthRejectsMissingAndWrongKey(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		ConfigYAML: []byte("allow_no_key: false\napi_key: sk-secret\n"),
	})

	// Missing header -> 401 invalid_api_key.
	errEnv := callErr(t, pluginabi.MethodFrontendAuthAuthenticate, pluginapi.FrontendAuthRequest{
		Method: "POST", Path: "/v1/chat/completions",
	})
	if errEnv.Code != "invalid_api_key" || errEnv.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("missing key: got %+v", errEnv)
	}

	// Wrong token -> 401.
	errEnv = callErr(t, pluginabi.MethodFrontendAuthAuthenticate, pluginapi.FrontendAuthRequest{
		Method:  "POST",
		Path:    "/v1/chat/completions",
		Headers: http.Header{"Authorization": []string{"Bearer sk-nope"}},
	})
	if errEnv.Code != "invalid_api_key" {
		t.Fatalf("wrong key: got %+v", errEnv)
	}

	// Non-Bearer scheme -> rejected.
	errEnv = callErr(t, pluginabi.MethodFrontendAuthAuthenticate, pluginapi.FrontendAuthRequest{
		Method:  "POST",
		Path:    "/v1/chat/completions",
		Headers: http.Header{"Authorization": []string{"Basic sk-secret"}},
	})
	if errEnv.Code != "invalid_api_key" {
		t.Fatalf("basic scheme: got %+v", errEnv)
	}
}

func TestFrontendAuthAcceptsCorrectKeyCaseInsensitiveScheme(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		ConfigYAML: []byte("allow_no_key: false\napi_key: sk-secret\n"),
	})
	res := callOK(t, pluginabi.MethodFrontendAuthAuthenticate, pluginapi.FrontendAuthRequest{
		Method:  "POST",
		Path:    "/v1/chat/completions",
		Headers: http.Header{"Authorization": []string{"bEaReR sk-secret"}},
	})
	var out pluginapi.FrontendAuthResponse
	_ = json.Unmarshal(res, &out)
	if !out.Authenticated {
		t.Fatal("correct key with mixed-case scheme should authenticate (V1/o.j uses equalsIgnoreCase)")
	}
}

func TestFrontendAuthOpenPaths(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		ConfigYAML: []byte("allow_no_key: false\napi_key: sk-secret\n"),
	})
	for _, p := range []string{"/healthz", "/authorize"} {
		res := callOK(t, pluginabi.MethodFrontendAuthAuthenticate, pluginapi.FrontendAuthRequest{
			Method: http.MethodGet, Path: p,
		})
		var out pluginapi.FrontendAuthResponse
		_ = json.Unmarshal(res, &out)
		if !out.Authenticated {
			t.Fatalf("%s should be open", p)
		}
	}
}

// ---- routing (port of V1/o.k steps 6-8) --------------------------------

func TestResolveRouteExplicitPrefix(t *testing.T) {
	s := defaultGatewaySettings()
	known := []string{"trae", "openai"}
	res, ok := resolveRoute("openai/gpt-4o-mini", s, known)
	if !ok {
		t.Fatal("route not resolved")
	}
	if res.Provider != "openai" || res.Model != "gpt-4o-mini" || !res.Explicit {
		t.Fatalf("got %+v", res)
	}
}

func TestResolveRouteFallsBackToDefaultProvider(t *testing.T) {
	s := defaultGatewaySettings() // default "trae"
	known := []string{"trae", "openai"}
	res, ok := resolveRoute("glm-5.2", s, known)
	if !ok {
		t.Fatal("route not resolved")
	}
	if res.Provider != "trae" || res.Model != "glm-5.2" || res.Explicit {
		t.Fatalf("got %+v", res)
	}
}

func TestResolveRouteLeadingSlashIsNotAPrefix(t *testing.T) {
	// V1/o.k() uses indexOf(model,'/') > 0, so "/model" is NOT explicit.
	s := defaultGatewaySettings()
	res, ok := resolveRoute("/gpt-4", s, []string{"trae"})
	if !ok {
		t.Fatal("route not resolved")
	}
	if res.Explicit {
		t.Fatalf("leading slash must not be treated as a provider prefix: %+v", res)
	}
	if res.Model != "/gpt-4" {
		t.Fatalf("model should keep the leading slash, got %q", res.Model)
	}
}

func TestResolveRouteUnknownPrefixFallsThrough(t *testing.T) {
	s := defaultGatewaySettings()
	res, ok := resolveRoute("nope/gpt-4", s, []string{"trae"})
	if !ok {
		t.Fatal("route not resolved")
	}
	if res.Provider != "trae" || res.Explicit {
		t.Fatalf("unknown provider prefix should fall back to default: %+v", res)
	}
}

func TestResolveRouteUsesFirstProviderWhenDefaultAbsent(t *testing.T) {
	s := defaultGatewaySettings()
	s.DefaultProvider = "missing"
	res, ok := resolveRoute("glm-5.2", s, []string{"openai", "trae"})
	if !ok {
		t.Fatal("route not resolved")
	}
	if res.Provider != "openai" {
		t.Fatalf("want first provider openai, got %+v", res)
	}
}

func TestRewriteModelBody(t *testing.T) {
	body := []byte(`{"model":"openai/gpt-4o","stream":true,"messages":[]}`)
	out, changed := rewriteModelBody(body, "gpt-4o")
	if !changed {
		t.Fatal("expected rewrite")
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatalf("bad json: %v", errUnmarshal)
	}
	if doc["model"] != "gpt-4o" {
		t.Fatalf("model = %v", doc["model"])
	}
	if doc["stream"] != true {
		t.Fatal("stream flag lost")
	}
	if _, ok := doc["messages"]; !ok {
		t.Fatal("messages lost")
	}
}

func TestRewriteModelBodyNoopWhenEqual(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","stream":false}`)
	if _, changed := rewriteModelBody(body, "gpt-4o"); changed {
		t.Fatal("should not rewrite identical model")
	}
}

func TestInterceptRequestTerminatesWithoutModel(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{})

	res := callOK(t, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{
		RequestID: "r1",
		Body:      []byte(`{"messages":[]}`), // no "model"
		Metadata:  map[string]any{"providers": []any{"trae"}},
	})
	var out pluginapi.RequestInterceptResponse
	if errUnmarshal := json.Unmarshal(res, &out); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if !out.Terminate || out.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 terminate, got %+v", out)
	}
	if len(out.ResponseBody) == 0 {
		t.Fatal("expected an error body")
	}
}

func TestInterceptRequestStampsRoutingHeaders(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		ConfigYAML: []byte("default_provider: trae\n"),
	})

	res := callOK(t, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{
		RequestID: "r2",
		Body:      []byte(`{"model":"openai/gpt-4o","stream":true}`),
		Metadata:  map[string]any{"providers": []any{"trae", "openai"}},
	})
	var out pluginapi.RequestInterceptResponse
	if errUnmarshal := json.Unmarshal(res, &out); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if out.Terminate {
		t.Fatal("should not terminate")
	}
	if out.Headers.Get("X-AIGW-Provider") != "openai" {
		t.Fatalf("provider header = %q", out.Headers.Get("X-AIGW-Provider"))
	}
	if out.Headers.Get("X-AIGW-Model") != "gpt-4o" {
		t.Fatalf("model header = %q", out.Headers.Get("X-AIGW-Model"))
	}
	if out.Headers.Get("X-AIGW-Route") != "explicit" {
		t.Fatalf("route header = %q", out.Headers.Get("X-AIGW-Route"))
	}
	// The explicit prefix must be stripped from the forwarded body.
	var doc map[string]any
	_ = json.Unmarshal(out.Body, &doc)
	if doc["model"] != "gpt-4o" {
		t.Fatalf("body model = %v", doc["model"])
	}
}

func TestInterceptRequestEnforceDefaultProvider(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		ConfigYAML: []byte("default_provider: trae\nenforce_default_provider: true\n"),
	})
	res := callOK(t, pluginabi.MethodRequestInterceptBefore, pluginapi.RequestInterceptRequest{
		RequestID: "r3",
		Body:      []byte(`{"model":"openai/gpt-4o"}`),
		Metadata:  map[string]any{"providers": []any{"trae", "openai"}},
	})
	var out pluginapi.RequestInterceptResponse
	_ = json.Unmarshal(res, &out)
	if !out.Terminate || out.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 terminate, got %+v", out)
	}
}

// ---- failure classification (port of V1/o.k step 9 + Y1.j) -------------

func TestClassifyUpstream(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   failureKind
	}{
		{"401 status", 401, `{}`, failureAuth},
		{"403 status", 403, `{}`, failureAuth},
		{"invalid_api_key code", 400, `{"error":{"message":"bad key","code":"invalid_api_key"}}`, failureAuth},
		{"429 status", 429, `{}`, failureRate},
		{"rate_limit_exceeded", 400, `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`, failureRate},
		{"insufficient_quota", 400, `{"error":{"type":"insufficient_quota"}}`, failureQuota},
		{"500 transient", 500, `{}`, failureTransient},
		{"503 transient", 503, `{"error":{"message":"upstream down"}}`, failureTransient},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyUpstream(c.status, []byte(c.body))
			if got.Kind != c.want {
				t.Fatalf("kind = %v, want %v (%+v)", got.Kind, c.want, got)
			}
		})
	}
}

func TestStatusForKind(t *testing.T) {
	cases := map[failureKind]int{
		failureAuth:      http.StatusUnauthorized,
		failureRate:      http.StatusTooManyRequests,
		failureQuota:     http.StatusForbidden,
		failureTransient: http.StatusBadGateway,
	}
	for kind, want := range cases {
		if got := statusForKind(kind); got != want {
			t.Errorf("statusForKind(%v) = %d, want %d", kind, got, want)
		}
	}
}

// ---- credential pool (port of A0.s + V1/k.c) ---------------------------

func TestPoolPickSkipsCoolingAndTried(t *testing.T) {
	p := newCredentialPool()
	s := defaultGatewaySettings()
	p.observe("trae", "a", "acc-a")
	p.observe("trae", "b", "acc-b")

	now := time.Now()
	if lane := p.pick("trae", nil, now); lane == nil {
		t.Fatal("expected a lane")
	}

	// Cool "a" down hard; "b" must be selected.
	p.failure("trae", "a", failureAuth, "bad key", s, false)
	lane := p.pick("trae", nil, now)
	if lane == nil || lane.UID != "b" {
		t.Fatalf("want lane b, got %+v", lane)
	}

	// Both tried -> nothing left.
	tried := map[string]struct{}{"a": {}, "b": {}}
	if lane := p.pick("trae", tried, now); lane != nil {
		t.Fatalf("want nil when all tried, got %+v", lane)
	}
}

func TestPoolSuccessClearsCooldown(t *testing.T) {
	p := newCredentialPool()
	s := defaultGatewaySettings()
	p.observe("trae", "a", "acc-a")
	p.failure("trae", "a", failureTransient, "boom", s, false)
	if lane := p.pick("trae", nil, time.Now()); lane != nil {
		t.Fatal("lane should be cooling down")
	}
	p.success("trae", "a")
	lane := p.pick("trae", nil, time.Now())
	if lane == nil {
		t.Fatal("success must clear the cooldown (A0.s.q)")
	}
	if lane.Successes != 1 {
		t.Fatalf("successes = %d", lane.Successes)
	}
}

func TestPoolSoftFailureParksOnlyAfterThreshold(t *testing.T) {
	p := newCredentialPool()
	s := defaultGatewaySettings()
	s.ErrorThreshold = 3
	p.observe("trae", "a", "acc-a")

	// Two soft failures < threshold: only a short soft cooldown is applied.
	p.failure("trae", "a", failureTransient, "e1", s, false)
	p.failure("trae", "a", failureTransient, "e2", s, false)
	lane := p.pick("trae", nil, time.Now())
	// The lane is in soft cooldown, so it must not be pickable yet.
	if lane != nil {
		t.Fatalf("soft cooldown should hide the lane momentarily: %+v", lane)
	}
	// After the soft cooldown expires it becomes usable again without a hard park.
	lane = p.pick("trae", nil, time.Now().Add(time.Duration(s.SoftCooldownMillis)*time.Millisecond+time.Second))
	if lane == nil {
		t.Fatal("lane should recover after soft cooldown")
	}
}

func TestPoolPermanentDisable(t *testing.T) {
	p := newCredentialPool()
	s := defaultGatewaySettings()
	p.observe("trae", "a", "acc-a")
	p.failure("trae", "a", failureAuth, "revoked", s, true)

	lane := p.pick("trae", nil, time.Now().Add(1000*time.Hour))
	if lane != nil {
		t.Fatalf("disabled lane must never be picked: %+v", lane)
	}
	snap := p.snapshot()
	if len(snap) != 1 || !snap[0].Disabled {
		t.Fatalf("snapshot = %+v", snap)
	}
}

// ---- response / usage accounting ---------------------------------------

func TestInterceptResponseRecordsSuccessAndUsage(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{})
	state.log = newCallLog(10)

	res := callOK(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:      "r1",
		Model:          "gpt-4o",
		RequestedModel: "gpt-4o",
		StatusCode:     http.StatusOK,
		RequestHeaders: http.Header{"X-Aigw-Provider": []string{"openai"}, "X-Aigw-Auth-Id": []string{"acc-1"}},
		Body:           []byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`),
	})
	if len(res) == 0 {
		t.Fatal("empty response envelope")
	}
	totals := state.log.totals()
	if totals.TotalCalls != 1 {
		t.Fatalf("total calls = %d", totals.TotalCalls)
	}
	if totals.TotalPrompt != 10 || totals.TotalCompletion != 5 {
		t.Fatalf("token totals = %+v", totals)
	}
	rec := state.log.recent(1)[0]
	if rec.ProviderID != "openai" || rec.UID != "acc-1" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestInterceptResponseClassifiesFailure(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		ConfigYAML: []byte("error_threshold: 1\n"),
	})
	state.pool.observe("openai", "acc-1", "acct")

	callOK(t, pluginabi.MethodResponseInterceptAfter, pluginapi.ResponseInterceptRequest{
		RequestID:      "r1",
		Model:          "gpt-4o",
		StatusCode:     http.StatusUnauthorized,
		RequestHeaders: http.Header{"X-Aigw-Provider": []string{"openai"}, "X-Aigw-Auth-Id": []string{"acc-1"}},
		Body:           []byte(`{"error":{"message":"invalid key","code":"invalid_api_key"}}`),
	})

	totals := state.log.totals()
	if totals.TotalFailed != 1 {
		t.Fatalf("failed = %d, want 1", totals.TotalFailed)
	}
	if lane := state.pool.pick("openai", nil, time.Now()); lane != nil {
		t.Fatal("credential should be cooling down after 401")
	}
}

func TestStreamChunkUsageAndError(t *testing.T) {
	usageChunk := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n" +
		"data: [DONE]\n\n")
	usage, ok := extractStreamUsage(usageChunk)
	if !ok {
		t.Fatal("usage not found")
	}
	p, c, tot := usage.normalized()
	if p != 7 || c != 3 || tot != 10 {
		t.Fatalf("usage = %d/%d/%d", p, c, tot)
	}

	errChunk := []byte("data: {\"error\":{\"message\":\"upstream exploded\"}}\n\n")
	if msg := extractStreamError(errChunk); msg != "upstream exploded" {
		t.Fatalf("error msg = %q", msg)
	}

	// Keep-alive comments and non-data lines are ignored (V1/o.p).
	if usage, ok := extractStreamUsage([]byte(": keep-alive\n\nevent: ping\n\n")); ok {
		t.Fatalf("unexpected usage %+v", usage)
	}
}

func TestStreamChunkHeaderInit(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{})
	res := callOK(t, pluginabi.MethodResponseInterceptStreamChunk, pluginapi.StreamChunkInterceptRequest{
		RequestID:       "s1",
		ChunkIndex:      pluginapi.StreamChunkHeaderInitIndex,
		Model:           "gpt-4o",
		RequestHeaders:  http.Header{"X-Aigw-Provider": []string{"openai"}},
		ResponseHeaders: http.Header{"Content-Type": []string{"text/event-stream"}},
	})
	var out pluginapi.StreamChunkInterceptResponse
	_ = json.Unmarshal(res, &out)
	if out.DropChunk {
		t.Fatal("header-init must not drop chunks")
	}
}

// ---- usage plugin ------------------------------------------------------

func TestHandleUsageRecordsTotals(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{})

	callOK(t, pluginabi.MethodUsageHandle, pluginapi.UsageRecord{
		Provider:    "openai",
		Model:       "gpt-4o",
		AuthIndex:   "acc-9",
		Stream:      true,
		RequestedAt: time.Now(),
		Latency:     1200 * time.Millisecond,
		Detail: pluginapi.UsageDetail{
			InputTokens:  100,
			OutputTokens: 50,
			TotalTokens:  150,
		},
	})

	totals := state.log.totals()
	if totals.TotalCalls != 1 || totals.TotalPrompt != 100 || totals.TotalCompletion != 50 {
		t.Fatalf("totals = %+v", totals)
	}
	if lane := state.pool.pick("openai", nil, time.Now()); lane == nil {
		t.Fatal("credential should be registered by the usage hook")
	}
}

// ---- management --------------------------------------------------------

func TestManagementRegistrationAndStatus(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{
		ConfigYAML: []byte("api_key: sk-secret\n"),
	})

	res := callOK(t, pluginabi.MethodManagementRegister, pluginapi.ManagementRegistrationRequest{})
	var reg managementRegistrationResponse
	if errUnmarshal := json.Unmarshal(res, &reg); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if len(reg.Resources) == 0 {
		t.Fatal("no resources registered")
	}

	res = callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    "/v0/resource/plugins/aigw-reverse-proxy/status",
		Headers: http.Header{"Accept": []string{"application/json"}},
	})
	var mr managementResponse
	if errUnmarshal := json.Unmarshal(res, &mr); errUnmarshal != nil {
		t.Fatalf("unmarshal management response: %v", errUnmarshal)
	}
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d", mr.StatusCode)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(mr.Body, &doc); errUnmarshal != nil {
		t.Fatalf("status body is not json: %v", errUnmarshal)
	}
	settings, ok := doc["settings"].(map[string]any)
	if !ok {
		t.Fatalf("missing settings: %s", mr.Body)
	}
	if settings["api_key"] != "[REDACTED]" {
		t.Fatalf("api_key must be redacted, got %v", settings["api_key"])
	}
}

func TestManagementStatusHTML(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{})
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    "/v0/resource/plugins/aigw-reverse-proxy/status",
		Headers: http.Header{"Accept": []string{"text/html"}},
	})
	var mr managementResponse
	_ = json.Unmarshal(res, &mr)
	if mr.Headers.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("content type = %q", mr.Headers.Get("Content-Type"))
	}
	if len(mr.Body) == 0 {
		t.Fatal("empty html")
	}
}

// ---- misc --------------------------------------------------------------

func TestUnknownMethodReturnsError(t *testing.T) {
	errEnv := callErr(t, "does.not.exist", nil)
	if errEnv.Code != "unknown_method" {
		t.Fatalf("code = %q", errEnv.Code)
	}
	if errEnv.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("http status = %d", errEnv.HTTPStatus)
	}
}

func TestParseRequestMeta(t *testing.T) {
	meta, ok := parseRequestMeta([]byte(`{"model":"m","stream":true}`))
	if !ok || meta.Model != "m" || !meta.Stream {
		t.Fatalf("meta = %+v ok=%v", meta, ok)
	}
	if _, ok := parseRequestMeta([]byte(`{`)); ok {
		t.Fatal("invalid json must fail")
	}
	if _, ok := parseRequestMeta(nil); ok {
		t.Fatal("empty body must fail")
	}
	// stream absent -> false
	meta, ok = parseRequestMeta([]byte(`{"model":"m"}`))
	if !ok || meta.Stream {
		t.Fatalf("meta = %+v", meta)
	}
}

// resetState reinstalls pristine singleton state between tests.
func resetState() {
	stopCheckinScheduler()
	state = &globalState{
		settings: newSettingsStore(),
		pool:     newCredentialPool(),
		log:      newCallLog(100),
		checkin:  newCheckinState(),
	}
	inflight = newInflightMap()
	streamAccumulators.mu.Lock()
	streamAccumulators.items = make(map[string]*streamAccumulator)
	streamAccumulators.mu.Unlock()
}
