package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ---- capability registration -------------------------------------------

// TestRegistrationDeclaresModelExecution is the fix for "获取不到模型": before
// v0.3.0 the plugin registered no capability that could surface or serve a
// model, so CPA had nothing to list.
func TestRegistrationDeclaresModelExecution(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{SchemaVersion: pluginabi.SchemaVersion})

	var raw struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	mustDecode(t, res, &raw)

	for _, key := range []string{"model_provider", "model_router", "executor"} {
		v, ok := raw.Capabilities[key]
		if !ok || v != true {
			t.Fatalf("capability %q must be true, got %v", key, raw.Capabilities)
		}
	}
	if got := raw.Capabilities["executor_model_scope"]; got != "both" {
		t.Errorf("executor_model_scope = %v, want both", got)
	}
	for _, key := range []string{"executor_input_formats", "executor_output_formats"} {
		list, ok := raw.Capabilities[key].([]any)
		if !ok || len(list) != 1 || list[0] != "chat-completions" {
			t.Errorf("%s = %v, want [chat-completions]", key, raw.Capabilities[key])
		}
	}
}

// ---- model catalogue parsing (a2/b.java:745 w()) -----------------------

func TestParseWorkBuddyModelsBasic(t *testing.T) {
	body := []byte(`{"code":0,"data":{"models":[
		{"id":"claude-sonnet-4","name":"Claude Sonnet 4","maxInputTokens":200000},
		{"id":"gpt-5","name":"GPT-5","maxInputTokens":128000}
	]}}`)
	models, err := parseWorkBuddyModels(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	if models[0].ID != "claude-sonnet-4" || models[0].DisplayName != "Claude Sonnet 4" {
		t.Errorf("models[0] = %+v", models[0])
	}
	if models[0].MaxInputTokens != 200000 {
		t.Errorf("maxInputTokens = %d", models[0].MaxInputTokens)
	}
}

func TestParseWorkBuddyModelsDisplayNameFallsBackToID(t *testing.T) {
	body := []byte(`{"code":0,"data":{"models":[{"id":"m1"}]}}`)
	models, err := parseWorkBuddyModels(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 1 || models[0].DisplayName != "m1" {
		t.Fatalf("models = %+v", models)
	}
}

func TestParseWorkBuddyModelsSkipsDisabled(t *testing.T) {
	// a2/b.java: disabled == true is filtered out.
	body := []byte(`{"code":0,"data":{"models":[
		{"id":"live"},{"id":"dead","disabled":true}
	]}}`)
	models, err := parseWorkBuddyModels(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "live" {
		t.Fatalf("models = %+v", models)
	}
}

func TestParseWorkBuddyModelsSkipsEmptyAndDuplicateIDs(t *testing.T) {
	body := []byte(`{"code":0,"data":{"models":[
		{"id":""},{"id":"a"},{"id":"a"},{"id":"b"}
	]}}`)
	models, err := parseWorkBuddyModels(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2 (a, b): %+v", len(models), models)
	}
}

// TestParseWorkBuddyModelsCLIOrdering pins the corrected behaviour: the "cli"
// agent's list orders the catalogue, it does not filter it.
//
// It used to be a hard whitelist, which silently dropped models the provider
// had added but not yet listed under "cli" — a model then failed routing with
// "unknown provider for model".
func TestParseWorkBuddyModelsCLIOrdering(t *testing.T) {
	body := []byte(`{"code":0,"data":{
		"agents":[{"name":"cli","models":["a","c"]},{"name":"other","models":["z"]}],
		"models":[{"id":"b"},{"id":"a"},{"id":"c"}]
	}}`)
	models, err := parseWorkBuddyModels(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ids := modelIDs(models)
	// All three survive; the two cli-listed ones come first in their declared
	// order, and the unlisted one follows.
	want := []string{"a", "c", "b"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
}

// TestParseWorkBuddyModelsKeepsUnlistedModel is the regression test for the
// user-visible symptom: a model present in the provider catalogue but absent
// from the "cli" agent list must still be offered.
func TestParseWorkBuddyModelsKeepsUnlistedModel(t *testing.T) {
	body := []byte(`{"code":0,"data":{
		"agents":[{"name":"cli","models":["deepseek-v4-flash"]}],
		"models":[{"id":"deepseek-v4-flash"},{"id":"deepseek-v4.1-flash"}]
	}}`)
	models, err := parseWorkBuddyModels(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, m := range models {
		if m.ID == "deepseek-v4.1-flash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a model not listed under the cli agent was dropped: %v", modelIDs(models))
	}
}

func TestParseWorkBuddyModelsEmptyWhitelistAllowsAll(t *testing.T) {
	// When the cli agent declares no models, the full catalogue is used.
	body := []byte(`{"code":0,"data":{
		"agents":[{"name":"other","models":["z"]}],
		"models":[{"id":"a"},{"id":"b"}]
	}}`)
	models, err := parseWorkBuddyModels(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("ids = %v, want both", modelIDs(models))
	}
}

func TestParseWorkBuddyModelsRejectsBadCode(t *testing.T) {
	// a2/b.java: "模型接口 code=<code>"
	for name, body := range map[string]string{
		"non-zero code": `{"code":401,"msg":"unauthorized"}`,
		"missing code":  `{"data":{"models":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseWorkBuddyModels([]byte(body)); err == nil {
				t.Fatal("expected error")
			} else if !strings.Contains(err.Error(), "code=") {
				t.Fatalf("error should mention code, got %v", err)
			}
		})
	}
}

func TestParseWorkBuddyModelsRejectsInvalidJSON(t *testing.T) {
	// a2/b.java: "模型响应不是合法 JSON"
	if _, err := parseWorkBuddyModels([]byte("<html>502</html>")); err == nil {
		t.Fatal("expected error")
	} else if !strings.Contains(err.Error(), "不是合法 JSON") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseWorkBuddyModelsMissingDataYieldsEmpty(t *testing.T) {
	models, err := parseWorkBuddyModels([]byte(`{"code":0}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("models = %+v, want empty", models)
	}
}

// ---- listModels over HTTP ----------------------------------------------

func TestListModelsSendsExpectedRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, workBuddyModelsPath) {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-User-Id") != "u-1" {
			t.Errorf("X-User-Id = %q", r.Header.Get("X-User-Id"))
		}
		if r.Header.Get("User-Agent") != "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1" {
			t.Errorf("UA = %q, want the desktop agent", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"m1","name":"M1"}]}}`))
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	creds := &workBuddyCredentials{AccessToken: "tok", Domain: "cn", UID: "u-1"}
	models, err := workBuddyUpstream.listModels(testContext(), creds)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "m1" {
		t.Fatalf("models = %+v", models)
	}
}

func TestListModelsRequiresToken(t *testing.T) {
	if _, err := workBuddyUpstream.listModels(testContext(), &workBuddyCredentials{}); err == nil {
		t.Fatal("expected error without access token")
	}
}

func TestListModelsHTTPErrorCarriesStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":401}`))
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	_, err := workBuddyUpstream.listModels(testContext(), &workBuddyCredentials{AccessToken: "t", Domain: "cn"})
	if err == nil {
		t.Fatal("expected error")
	}
	var upErr *workBuddyUpstreamError
	if !asUpstreamError(err, &upErr) {
		t.Fatalf("expected *workBuddyUpstreamError, got %T: %v", err, err)
	}
	if upErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", upErr.StatusCode)
	}
}

// ---- model provider RPC -------------------------------------------------

func TestModelForAuthReturnsCatalogue(t *testing.T) {
	resetState()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
			{"id":"m-a","name":"Model A","maxInputTokens":1000},
			{"id":"m-b"}
		]}}`))
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	storage, _ := json.Marshal(map[string]any{"accessToken": "tok", "uid": "u-1", "domain": "cn"})
	res := callOK(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{
		AuthProvider: workBuddyProviderKey,
		StorageJSON:  storage,
	})

	var out pluginapi.ModelResponse
	mustDecode(t, res, &out)
	if out.Provider != workBuddyProviderKey {
		t.Errorf("provider = %q", out.Provider)
	}
	if len(out.Models) != 2 {
		t.Fatalf("models = %+v", out.Models)
	}
	if out.Models[0].ID != "m-a" || out.Models[0].DisplayName != "Model A" {
		t.Errorf("models[0] = %+v", out.Models[0])
	}
	// ID carries the provider prefix so a client can tell this upstream apart
	// from another plugin serving a same-named model; Version keeps the bare
	// upstream name for diagnostics.
	if out.Models[0].Name != "m-a" {
		t.Errorf("Name = %q, want the bare id m-a", out.Models[0].Name)
	}
	if out.Models[0].Version != "m-a" {
		t.Errorf("Version = %q, want the bare upstream name m-a", out.Models[0].Version)
	}
	if out.Models[0].InputTokenLimit != 1000 {
		t.Errorf("InputTokenLimit = %d", out.Models[0].InputTokenLimit)
	}
}

func TestModelForAuthIgnoresForeignProvider(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{
		AuthProvider: "anthropic",
		StorageJSON:  []byte(`{"accessToken":"x"}`),
	})
	var out pluginapi.ModelResponse
	mustDecode(t, res, &out)
	if len(out.Models) != 0 {
		t.Fatalf("should not answer for a foreign provider: %+v", out)
	}
}

// TestModelForAuthUnparseableAuthYieldsFallback pins the fix for
// "该凭证暂无可用模型": an unreadable credential body must still report the
// built-in model list, because CPA shows the "no models" notice for an empty
// response and the account would look unusable.
func TestModelForAuthUnparseableAuthYieldsFallback(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{
		AuthProvider: workBuddyProviderKey,
		StorageJSON:  []byte(`not json`),
	})
	var out pluginapi.ModelResponse
	mustDecode(t, res, &out)
	if out.Provider != workBuddyProviderKey {
		t.Fatalf("provider = %q", out.Provider)
	}
	if len(out.Models) == 0 {
		t.Fatal("an unreadable credential must still report the built-in models")
	}
	var sawDeepSeek bool
	for _, m := range out.Models {
		if m.ID == "deepseek-v4-flash" {
			sawDeepSeek = true
		}
	}
	if !sawDeepSeek {
		t.Fatalf("fallback list missing deepseek-v4-flash: %+v", out.Models)
	}
}

// TestModelForAuthFallsBackWhenUpstreamFails keeps a usable credential from
// looking empty when the live catalogue call fails.
func TestModelForAuthFallsBackWhenUpstreamFails(t *testing.T) {
	resetState()
	// Point the API host at a dead address so listModels fails.
	orig := copilotHostValue()
	setCopilotHost("http://127.0.0.1:1")
	defer setCopilotHost(orig)

	storage, _ := json.Marshal(map[string]any{
		"type": workBuddyProviderKey, "accessToken": "at", "uid": "u-1", "domain": "cn",
	})
	res := callOK(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{
		AuthProvider: workBuddyProviderKey,
		StorageJSON:  storage,
	})
	var out pluginapi.ModelResponse
	mustDecode(t, res, &out)
	if len(out.Models) == 0 {
		t.Fatal("a failed live query must fall back to the built-in list")
	}
}

func TestModelStaticServesCachedCatalogue(t *testing.T) {
	resetState()
	workBuddyModelCache.put("cn/u-1", []workBuddyModel{{ID: "cached-1", DisplayName: "Cached"}})

	res := callOK(t, pluginabi.MethodModelStatic, pluginapi.StaticModelRequest{})
	var out pluginapi.ModelResponse
	mustDecode(t, res, &out)
	if len(out.Models) != 1 || out.Models[0].ID != "cached-1" {
		t.Fatalf("models = %+v", out.Models)
	}
}

// ---- model router -------------------------------------------------------

func TestModelRouteClaimsKnownModel(t *testing.T) {
	resetState()
	workBuddyModelCache.put("cn/u-1", []workBuddyModel{{ID: "claude-sonnet-4"}})
	// A credential must exist: the router only claims models it can serve.
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey,
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-1"})},
	})

	res := callOK(t, pluginabi.MethodModelRoute, pluginapi.ModelRouteRequest{
		SourceFormat:   "chat-completions",
		RequestedModel: "claude-sonnet-4",
	})
	var out pluginapi.ModelRouteResponse
	mustDecode(t, res, &out)
	if !out.Handled {
		t.Fatal("known model must be claimed")
	}
	if out.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("targetKind = %q, want self", out.TargetKind)
	}
}

// TestModelRouteClaimsUncataloguedModelWhenProviderOwned is the fix for
// "unknown provider for model deepseek-v4.1-flash": the catalogue is fetched
// lazily, so claiming only catalogued names refused to route any request that
// arrived before the first /v1/models call.
func TestModelRouteClaimsUncataloguedModelWhenProviderOwned(t *testing.T) {
	resetState()
	// Catalogue deliberately empty.
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey,
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-1"})},
	})

	res := callOK(t, pluginabi.MethodModelRoute, pluginapi.ModelRouteRequest{
		SourceFormat:   "chat-completions",
		RequestedModel: "deepseek-v4.1-flash",
	})
	var out pluginapi.ModelRouteResponse
	mustDecode(t, res, &out)
	if !out.Handled {
		t.Fatal("a model for a provider we own must be claimed even before the catalogue loads")
	}
	if out.TargetKind != pluginapi.ModelRouteTargetSelf {
		t.Fatalf("targetKind = %q", out.TargetKind)
	}
	if out.TargetModel != "deepseek-v4.1-flash" {
		t.Fatalf("targetModel = %q", out.TargetModel)
	}
}

// TestModelRouteDefersWithoutCredential keeps the plugin out of the way when it
// has no WorkBuddy account at all.
func TestModelRouteDefersWithoutCredential(t *testing.T) {
	resetState()
	installAuthList(t, nil)

	res := callOK(t, pluginabi.MethodModelRoute, pluginapi.ModelRouteRequest{
		SourceFormat:   "chat-completions",
		RequestedModel: "deepseek-v4.1-flash",
	})
	var out pluginapi.ModelRouteResponse
	mustDecode(t, res, &out)
	if out.Handled {
		t.Fatal("with no WorkBuddy credential the host should decide")
	}
}

// TestModelRouteUsesAvailableProviders checks the authoritative signal CPA
// supplies, without consulting the account store.
func TestModelRouteUsesAvailableProviders(t *testing.T) {
	resetState()
	// Account store empty; only AvailableProviders says we have an account.
	res := callOK(t, pluginabi.MethodModelRoute, pluginapi.ModelRouteRequest{
		SourceFormat:       "chat-completions",
		RequestedModel:     "some-model",
		AvailableProviders: []string{"anthropic", workBuddyProviderKey},
	})
	var out pluginapi.ModelRouteResponse
	mustDecode(t, res, &out)
	if !out.Handled {
		t.Fatal("AvailableProviders listing codebuddy should be enough to claim")
	}
}

func TestModelRouteClaimsExplicitPrefix(t *testing.T) {
	resetState()
	// Even without a cached catalogue, an explicit codebuddy/ prefix is ours.
	res := callOK(t, pluginabi.MethodModelRoute, pluginapi.ModelRouteRequest{
		SourceFormat:   "chat-completions",
		RequestedModel: "codebuddy/whatever",
	})
	var out pluginapi.ModelRouteResponse
	mustDecode(t, res, &out)
	if !out.Handled {
		t.Fatal("explicit prefix must be claimed")
	}
	if out.TargetModel != "whatever" {
		t.Fatalf("targetModel = %q, want whatever", out.TargetModel)
	}
}

func TestModelRouteDefersForeignPrefix(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodModelRoute, pluginapi.ModelRouteRequest{
		SourceFormat:   "chat-completions",
		RequestedModel: "openai/gpt-4o",
	})
	var out pluginapi.ModelRouteResponse
	mustDecode(t, res, &out)
	if out.Handled {
		t.Fatal("foreign provider prefix must not be claimed")
	}
}

func TestModelRouteIgnoresNonChatFormat(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodModelRoute, pluginapi.ModelRouteRequest{
		SourceFormat:   "responses",
		RequestedModel: "codebuddy/x",
	})
	var out pluginapi.ModelRouteResponse
	mustDecode(t, res, &out)
	if out.Handled {
		t.Fatal("only chat-completions should be claimed")
	}
}

// ---- model name normalisation (a2/b.java:583 k()) ----------------------

func TestNormalizeWorkBuddyModel(t *testing.T) {
	cases := []struct {
		in, def, want string
	}{
		{"gpt-4", "", "gpt-4"},
		{"  gpt-4  ", "", "gpt-4"},
		{"", "fallback", "fallback"},
		{"auto", "fallback", "fallback"},
		{"auto", "", ""},
	}
	for _, c := range cases {
		if got := normalizeWorkBuddyModel(c.in, c.def); got != c.want {
			t.Errorf("normalize(%q, %q) = %q, want %q", c.in, c.def, got, c.want)
		}
	}
}

func TestRewriteChatModelPreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"old","stream":true,"messages":[{"role":"user","content":"hi"}],"temperature":0.5}`)
	out, err := rewriteChatModel(body, "new")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(out, &doc); errUnmarshal != nil {
		t.Fatalf("bad json: %v", errUnmarshal)
	}
	if doc["model"] != "new" {
		t.Errorf("model = %v", doc["model"])
	}
	if doc["stream"] != true {
		t.Error("stream lost")
	}
	if doc["temperature"] != 0.5 {
		t.Error("temperature lost")
	}
	if _, ok := doc["messages"]; !ok {
		t.Error("messages lost")
	}
}

func TestRewriteChatModelRejectsBadJSON(t *testing.T) {
	if _, err := rewriteChatModel([]byte(`{`), "m"); err == nil {
		t.Fatal("expected error")
	}
}

// ---- executor -----------------------------------------------------------

func TestExecutorIdentifier(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodExecutorIdentifier, nil)
	var out identifierResponse
	mustDecode(t, res, &out)
	if out.Identifier != workBuddyProviderKey {
		t.Fatalf("identifier = %q", out.Identifier)
	}
}

func TestExecutorExecuteForwardsAndRewritesModel(t *testing.T) {
	resetState()

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, workBuddyChatPath) {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	storage, _ := json.Marshal(map[string]any{"accessToken": "tok", "uid": "u-1", "domain": "cn"})
	reqBody := []byte(`{"model":"codebuddy/claude-sonnet-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	payload, _ := json.Marshal(map[string]any{
		"AuthID":          "u-1",
		"AuthProvider":    workBuddyProviderKey,
		"Model":           "codebuddy/claude-sonnet-4",
		"OriginalRequest": reqBody,
		"StorageJSON":     storage,
	})

	res := callOK(t, pluginabi.MethodExecutorExecute, json.RawMessage(payload))
	var out pluginapi.ExecutorResponse
	mustDecode(t, res, &out)

	// The provider-prefix form must be resolved to the bare upstream model.
	if gotBody["model"] != "claude-sonnet-4" {
		t.Fatalf("upstream model = %v, want claude-sonnet-4", gotBody["model"])
	}
	if !strings.Contains(string(out.Payload), "hello") {
		t.Fatalf("payload = %s", out.Payload)
	}
}

func TestExecutorExecuteRequiresBody(t *testing.T) {
	resetState()
	storage, _ := json.Marshal(map[string]any{"accessToken": "tok", "domain": "cn"})
	payload, _ := json.Marshal(map[string]any{"StorageJSON": storage})

	env := callErr(t, pluginabi.MethodExecutorExecute, json.RawMessage(payload))
	if env.Code != "invalid_executor_request" {
		t.Fatalf("code = %q", env.Code)
	}
}

func TestExecutorExecuteRequiresCredentials(t *testing.T) {
	resetState()
	payload, _ := json.Marshal(map[string]any{
		"OriginalRequest": []byte(`{"model":"m","messages":[]}`),
	})
	env := callErr(t, pluginabi.MethodExecutorExecute, json.RawMessage(payload))
	if env.Code != "invalid_executor_request" {
		t.Fatalf("code = %q", env.Code)
	}
}

func TestExecutorExecuteReportsUpstreamError(t *testing.T) {
	resetState()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid token"}}`))
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	storage, _ := json.Marshal(map[string]any{"accessToken": "tok", "domain": "cn"})
	payload, _ := json.Marshal(map[string]any{
		"OriginalRequest": []byte(`{"model":"m","messages":[]}`),
		"StorageJSON":     storage,
	})

	// The exec still succeeds; the upstream status is reported in metadata so
	// the host can decide how to react.
	res := callOK(t, pluginabi.MethodExecutorExecute, json.RawMessage(payload))
	var out struct {
		Metadata struct {
			UpstreamStatus int `json:"upstream_status"`
		} `json:"Metadata"`
	}
	mustDecode(t, res, &out)
	if out.Metadata.UpstreamStatus != http.StatusUnauthorized {
		t.Fatalf("upstream_status = %d, want 401", out.Metadata.UpstreamStatus)
	}
}

func TestExecutorExecuteStreamForwardsBareJSONFrames(t *testing.T) {
	resetState()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(": keep-alive\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"He\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"llo\"}}],\"usage\":{\"total_tokens\":2}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	var emitted [][]byte
	closed := make(chan string, 1)
	orig := hostCallFunc
	hostCallFunc = func(method string, payload any) (json.RawMessage, error) {
		doc, _ := json.Marshal(payload)
		var fields map[string]any
		_ = json.Unmarshal(doc, &fields)
		switch method {
		case "host.stream.emit":
			if raw, okPayload := fields["payload"]; okPayload {
				if encoded, okStr := raw.(string); okStr {
					if decoded, errDecode := base64.StdEncoding.DecodeString(encoded); errDecode == nil {
						emitted = append(emitted, decoded)
					}
				}
			}
		case "host.stream.close":
			select {
			case closed <- fmt.Sprint(fields["error"]):
			default:
			}
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	defer func() { hostCallFunc = orig }()

	if _, errCall := executorExecuteStream(buildStreamExecutorRequest(t, "s-frames")); errCall != nil {
		t.Fatal(errCall)
	}

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("流未关闭")
	}

	// Two content frames survive; the heartbeat and [DONE] are dropped.
	if len(emitted) != 2 {
		t.Fatalf("emit 了 %d 帧，应为 2（心跳与 [DONE] 应被丢弃）", len(emitted))
	}
	for i, p := range emitted {
		// CPA's writer adds the SSE framing itself, so the payload must be bare
		// JSON. A "data:" prefix here produces:
		//   Unexpected JSON token at offset 5: Expected EOF after parsing
		if bytes.HasPrefix(p, []byte("data:")) {
			t.Fatalf("第 %d 帧仍带 SSE 前缀: %q", i, p)
		}
		if bytes.ContainsAny(p, "\r\n") {
			t.Fatalf("第 %d 帧带换行: %q", i, p)
		}
		if !json.Valid(p) {
			t.Fatalf("第 %d 帧不是合法 JSON: %q", i, p)
		}
	}

	joined := string(emitted[0]) + string(emitted[1])
	if !strings.Contains(joined, "He") || !strings.Contains(joined, "llo") {
		t.Fatalf("内容丢失: %q", joined)
	}
}

func TestSSEFrameToBareJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		keep bool
	}{
		{"plain data frame", "data: {\"a\":1}\n", true},
		{"no space after colon", "data:{\"a\":1}\n", true},
		{"trailing crlf", "data: {\"a\":1}\r\n", true},
		{"done sentinel", "data: [DONE]\n", false},
		{"heartbeat comment", ": keep-alive\n", false},
		{"blank separator", "\n", false},
		{"empty", "", false},
		{"non-json garbage", "data: oops\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, keep := sseFrameToBareJSON([]byte(c.in))
			if keep != c.keep {
				t.Fatalf("keep = %v, want %v (payload=%q)", keep, c.keep, got)
			}
			if !keep {
				return
			}
			if !json.Valid(got) {
				t.Fatalf("payload is not valid JSON: %q", got)
			}
			if strings.HasPrefix(string(got), "data:") {
				t.Fatalf("payload still has SSE prefix: %q", got)
			}
			if bytes.ContainsAny(got, "\r\n") {
				t.Fatalf("payload carries newlines: %q", got)
			}
		})
	}
}

// TestSSEFrameToBareJSONUnwrapsDoubledPrefix guards against providers that
// accidentally double-prefix a frame.
func TestSSEFrameToBareJSONUnwrapsDoubledPrefix(t *testing.T) {
	got, keep := sseFrameToBareJSON([]byte("data: data: {\"a\":1}\n"))
	if !keep {
		t.Fatal("expected the frame to survive")
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("payload = %q", got)
	}
}

// 上游报错时必须通过 host.stream.close 带错误关闭，客户端才不会静默等待。
func TestExecutorExecuteStreamEmitsBareErrorFrame(t *testing.T) {
	resetState()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream down"}}`))
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	closed := make(chan string, 1)
	orig := hostCallFunc
	hostCallFunc = func(method string, payload any) (json.RawMessage, error) {
		if method == "host.stream.close" {
			doc, _ := json.Marshal(payload)
			var fields map[string]any
			_ = json.Unmarshal(doc, &fields)
			select {
			case closed <- fmt.Sprint(fields["error"]):
			default:
			}
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	defer func() { hostCallFunc = orig }()

	resp, errCall := executorExecuteStream(buildStreamExecutorRequest(t, "s-err"))
	if errCall != nil {
		t.Fatal(errCall)
	}
	// 调用本身成功返回（chunk 通过回调流走），错误在 close 上体现。
	var env struct {
		OK bool `json:"ok"`
	}
	mustDecode(t, resp, &env)
	if !env.OK {
		t.Fatalf("信封应为 ok；resp=%s", resp)
	}

	select {
	case message := <-closed:
		if message == "" {
			t.Fatal("上游报错时必须带错误关闭流，否则客户端只看到流结束")
		}
		if !strings.Contains(message, "upstream") && !strings.Contains(message, "502") {
			t.Logf("关闭原因: %s", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("上游报错后流未关闭")
	}
}

func chunkPayloads(chunks []struct {
	Payload []byte `json:"Payload"`
}) string {
	var b strings.Builder
	for _, c := range chunks {
		b.Write(c.Payload)
		b.WriteString(" | ")
	}
	return b.String()
}

// ---- helpers ------------------------------------------------------------

func modelIDs(models []workBuddyModel) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

// testContext returns a background context; kept as a helper so tests read the
// same regardless of whether a timeout is added later.
func testContext() context.Context {
	return context.Background()
}

// asUpstreamError reports whether err is a *workBuddyUpstreamError and, if so,
// stores it in target.
func asUpstreamError(err error, target **workBuddyUpstreamError) bool {
	var upErr *workBuddyUpstreamError
	if !errors.As(err, &upErr) {
		return false
	}
	*target = upErr
	return true
}

// ---- model.static lazy catalogue ---------------------------------------

// TestModelStaticFetchesCatalogueWhenCacheEmpty covers the lazy-catalogue fix:
// without it the model list stayed empty until something else triggered a
// fetch, so CPA reported "unknown provider for model <name>" because the router
// refused to claim anything not already cached.
func TestModelStaticFetchesCatalogueWhenCacheEmpty(t *testing.T) {
	resetState()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "personal/models") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash"}
			]}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	orig := copilotHostValue()
	setCopilotHost(server.URL)
	defer setCopilotHost(orig)

	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey,
			"storage_json": mustStorage(t, map[string]any{
				"accessToken": "at", "uid": "u-1", "domain": "cn"})},
	})

	if n := len(workBuddyModelCache.snapshot()); n != 0 {
		t.Fatalf("precondition: cache should be empty, got %d", n)
	}

	res := callOK(t, pluginabi.MethodModelStatic, pluginapi.StaticModelRequest{})
	var out pluginapi.ModelResponse
	mustDecode(t, res, &out)
	if len(out.Models) == 0 {
		t.Fatal("model.static should fetch the catalogue rather than return empty")
	}
	if out.Models[0].ID != "deepseek-v4.1-flash" {
		t.Fatalf("models should carry bare upstream ids, got %+v", out.Models)
	}
}

// TestModelStaticEmptyWithoutCredential keeps the no-account case quiet.
func TestModelStaticEmptyWithoutCredential(t *testing.T) {
	resetState()
	installAuthList(t, nil)

	res := callOK(t, pluginabi.MethodModelStatic, pluginapi.StaticModelRequest{})
	var out pluginapi.ModelResponse
	mustDecode(t, res, &out)
	if len(out.Models) != 0 {
		t.Fatalf("models = %+v, want none", out.Models)
	}
}

// buildStreamExecutorRequest 构造带凭据与 stream_id 的 executor 调用体。
func buildStreamExecutorRequest(t *testing.T, streamID string) []byte {
	t.Helper()
	storage, _ := json.Marshal(map[string]any{"accessToken": "[REDACTED]", "domain": "cn"})
	payload, _ := json.Marshal(map[string]any{
		"OriginalRequest": []byte(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[]}`),
		"StorageJSON":     storage,
		"Stream":          true,
		// executorRequest 的 json tag 是小写下划线形式
		"stream_id": streamID,
	})
	return payload
}

// 端到端：上游逐块发送时，插件应【立即返回】并在后台持续 emit。
//
// 这覆盖了本次修复的核心：
//   - 修复前：executor 收完所有 chunk 才返回，客户端全程零字节 → 超时断开
//   - 修复后：立即返回，后台 goroutine 持续 emit
func TestStreamReturnsImmediatelyAndEmitsInBackground(t *testing.T) {
	resetState()

	const frames = 40 // 远超宿主 16 槽的 emit 缓冲

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < frames; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(2 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	// 记录插件发出的 chunk 与 stream.close。
	var emitted []string
	var closedWith string
	var closed bool
	orig := hostCallFunc
	hostCallFunc = func(method string, payload any) (json.RawMessage, error) {
		doc, _ := json.Marshal(payload)
		var fields map[string]any
		_ = json.Unmarshal(doc, &fields)
		switch method {
		case "host.stream.emit":
			if p, okPayload := fields["payload"]; okPayload {
				emitted = append(emitted, fmt.Sprint(p))
			}
		case "host.stream.close":
			closed = true
			if e, okErr := fields["error"]; okErr {
				closedWith = fmt.Sprint(e)
			}
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	defer func() { hostCallFunc = orig }()

	req := buildStreamExecutorRequest(t, "s-1")

	start := time.Now()
	resp, errCall := executorExecuteStream(req)
	callDuration := time.Since(start)

	if errCall != nil {
		t.Fatalf("executorExecuteStream: %v", errCall)
	}

	// 关键断言 1：调用必须【立即返回】，不能等上游读完。
	if callDuration > 300*time.Millisecond {
		t.Errorf("调用耗时 %v，应该立即返回（修复前是收完所有 chunk 才返回）", callDuration)
	}

	// 关键断言 2：返回的 chunk 列表必须为空 —— 空列表才告诉宿主走回调流。
	var env struct {
		Result streamChunkEnvelope `json:"result"`
	}
	mustDecode(t, resp, &env)
	out := env.Result
	if len(out.Chunks) != 0 {
		t.Errorf("返回了 %d 个 chunk；异步模式应返回空列表让宿主消费回调流", len(out.Chunks))
	}

	// 关键断言 3：后台应把全部帧 emit 出去（含超过 16 槽的部分）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if closed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !closed {
		t.Fatalf("后台未在期限内关闭流（已 emit %d 块）", len(emitted))
	}
	if len(emitted) != frames {
		t.Errorf("emit 了 %d 块，应为 %d 块（宿主缓冲只有 16 槽，同步实现会在第 17 块卡死）",
			len(emitted), frames)
	}
	if closedWith != "" {
		t.Errorf("正常结束不应带错误，实际 %q", closedWith)
	}
}

// 上游零输出时必须带错误关闭，不能静默结束。
func TestStreamClosesWithErrorWhenUpstreamEmpty(t *testing.T) {
	resetState()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	var closed bool
	var closedWith string
	orig := hostCallFunc
	hostCallFunc = func(method string, payload any) (json.RawMessage, error) {
		doc, _ := json.Marshal(payload)
		var fields map[string]any
		_ = json.Unmarshal(doc, &fields)
		if method == "host.stream.close" {
			closed = true
			if e, okErr := fields["error"]; okErr {
				closedWith = fmt.Sprint(e)
			}
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	defer func() { hostCallFunc = orig }()

	resp, errCall := executorExecuteStream(buildStreamExecutorRequest(t, "s-2"))
	if errCall != nil {
		t.Fatal(errCall)
	}
	_ = resp

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !closed {
		time.Sleep(20 * time.Millisecond)
	}
	if !closed {
		t.Fatal("上游零输出时流未关闭，客户端会一直等")
	}
	if closedWith == "" {
		t.Error("零输出必须带错误关闭，否则客户端只看到空响应")
	}
}

// 没有 stream_id 时必须报错 —— 对齐官方 execute_stream 示例：
//
//	streamID := strings.TrimSpace(req.StreamID)
//	if streamID == "" {
//	    return errorEnvelope("executor_error",
//	        "stream_id is required for executor.execute_stream"), nil
//	}
//
// 宿主没有流就无处投递 chunk，报错比猜一个缓冲回退更明确。
func TestStreamRequiresStreamID(t *testing.T) {
	resetState()

	orig := hostCallFunc
	hostCallFunc = func(string, any) (json.RawMessage, error) {
		t.Error("缺少 stream_id 时不应发起任何宿主流调用")
		return json.RawMessage(`{}`), nil
	}
	defer func() { hostCallFunc = orig }()

	resp, errCall := executorExecuteStream(buildStreamExecutorRequest(t, ""))
	if errCall != nil {
		t.Fatal(errCall)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	mustDecode(t, resp, &env)
	if env.OK {
		t.Fatalf("缺少 stream_id 时不应返回 ok；resp=%s", resp)
	}
	if env.Error == nil || env.Error.HTTPStatus != 400 {
		t.Fatalf("应返回 http_status=400 的错误；resp=%s", resp)
	}
}

// 流式响应的初始 header 必须是 text/event-stream，且返回空 chunk 列表
// （空列表告诉宿主改从回调流消费）。对齐官方示例。
func TestStreamResponseShape(t *testing.T) {
	resetState()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"id\":\"c1\"}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	defer pointWorkBuddyAt(server.URL)()

	closed := make(chan string, 1)
	orig := hostCallFunc
	hostCallFunc = func(method string, payload any) (json.RawMessage, error) {
		if method == "host.stream.close" {
			doc, _ := json.Marshal(payload)
			var fields map[string]any
			_ = json.Unmarshal(doc, &fields)
			select {
			case closed <- fmt.Sprint(fields["error"]):
			default:
			}
		}
		return json.RawMessage(`{"ok":true}`), nil
	}
	defer func() { hostCallFunc = orig }()

	resp, errCall := executorExecuteStream(buildStreamExecutorRequest(t, "s-shape"))
	if errCall != nil {
		t.Fatal(errCall)
	}
	var env struct {
		Result struct {
			Headers http.Header                     `json:"headers"`
			Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks"`
		} `json:"result"`
	}
	mustDecode(t, resp, &env)
	if got := env.Result.Headers.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if len(env.Result.Chunks) != 0 {
		t.Errorf("异步模式应返回空 chunk 列表，实际 %d 个", len(env.Result.Chunks))
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("流未关闭")
	}
}

// 走真实的响应拦截路径（而非直接调 pool），确认模型级冷却生效。
func TestInterceptResponseParksModelNotAccount(t *testing.T) {
	resetState()

	// 模拟一次请求的上下文
	requestID := "req-1"
	inflight.put(requestID, requestContext{
		Provider:       "codebuddy",
		Model:          "deepseek-v4.1-flash",
		RequestedModel: "deepseek-v4.1-flash",
		UID:            "u-intercepted",
		Stream:         false,
	})

	body := []byte(`{"error":{"message":"您的使用量已超出频率限制，将在 2030-01-01 12:00:00 UTC+8 重置，您也可以切换其他模型继续使用。","type":"server_error","code":"internal_server_error"}}`)
	payload, _ := json.Marshal(map[string]any{
		"RequestID":       requestID,
		"StatusCode":      502,
		"Body":            body,
		"Model":           "deepseek-v4.1-flash",
		"RequestedModel":  "deepseek-v4.1-flash",
		"RequestHeaders":  map[string][]string{"X-WorkBuddy-Provider": {"codebuddy"}, "X-WorkBuddy-Auth-Id": {"u-intercepted"}, "X-WorkBuddy-Model": {"deepseek-v4.1-flash"}},
		"ResponseHeaders": map[string][]string{},
	})

	if _, errCall := interceptResponse(payload); errCall != nil {
		t.Fatal(errCall)
	}

	lane := state.pool.lanes[laneKey("codebuddy", "u-intercepted")]
	if lane == nil {
		t.Fatal("车道未被创建")
	}
	t.Logf("ModelCooldowns = %v", lane.ModelCooldowns)
	t.Logf("CooldownUntil = %v", lane.CooldownUntil)

	if len(lane.ModelCooldowns) == 0 {
		t.Error("模型级冷却未生效：限流应只停 deepseek-v4.1-flash")
	}
	if !lane.CooldownUntil.IsZero() && time.Now().Before(lane.CooldownUntil) {
		t.Error("账号级冷却不应被触发")
	}
}
