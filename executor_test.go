package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestParseWorkBuddyModelsCLIWhitelist(t *testing.T) {
	// The "cli" agent's model list acts as a whitelist (a2/b.java w()).
	body := []byte(`{"code":0,"data":{
		"agents":[{"name":"cli","models":["a","c"]},{"name":"other","models":["z"]}],
		"models":[{"id":"a"},{"id":"b"},{"id":"c"}]
	}}`)
	models, err := parseWorkBuddyModels(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ids := modelIDs(models)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "c" {
		t.Fatalf("ids = %v, want [a c]", ids)
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
		if r.Header.Get("User-Agent") != codebuddyUA {
			t.Errorf("UA = %q", r.Header.Get("User-Agent"))
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
	// ID must be the provider-native name so no mapping layer is needed.
	if out.Models[0].Name != "m-a" {
		t.Errorf("Name = %q, want m-a", out.Models[0].Name)
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

func TestModelForAuthUnparseableAuthYieldsNothing(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodModelForAuth, pluginapi.AuthModelRequest{
		AuthProvider: workBuddyProviderKey,
		StorageJSON:  []byte(`not json`),
	})
	var out pluginapi.ModelResponse
	mustDecode(t, res, &out)
	if len(out.Models) != 0 {
		t.Fatalf("models = %+v", out.Models)
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

	storage, _ := json.Marshal(map[string]any{"accessToken": "tok", "domain": "cn"})
	payload, _ := json.Marshal(map[string]any{
		"OriginalRequest": []byte(`{"model":"m","stream":true,"messages":[]}`),
		"StorageJSON":     storage,
		"Stream":          true,
	})

	res := callOK(t, pluginabi.MethodExecutorExecuteStream, json.RawMessage(payload))
	var out struct {
		Chunks []struct {
			Payload []byte `json:"Payload"`
		} `json:"chunks"`
	}
	mustDecode(t, res, &out)

	// Two content frames survive; the heartbeat and [DONE] are dropped.
	if len(out.Chunks) != 2 {
		t.Fatalf("got %d chunks, want 2: %q", len(out.Chunks), chunkPayloads(out.Chunks))
	}

	for i, c := range out.Chunks {
		p := c.Payload
		// CPA's writer adds the SSE framing itself, so the payload must be bare
		// JSON. A "data:" prefix here produces:
		//   Unexpected JSON token at offset 5: Expected EOF after parsing
		if bytes.HasPrefix(p, []byte("data:")) {
			t.Fatalf("chunk %d still carries the SSE prefix: %q", i, p)
		}
		if bytes.ContainsAny(p, "\r\n") {
			t.Fatalf("chunk %d carries newlines: %q", i, p)
		}
		if !json.Valid(p) {
			t.Fatalf("chunk %d is not valid JSON: %q", i, p)
		}
	}

	joined := string(out.Chunks[0].Payload) + string(out.Chunks[1].Payload)
	if !strings.Contains(joined, "He") || !strings.Contains(joined, "llo") {
		t.Fatalf("content lost: %q", joined)
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

func TestExecutorExecuteStreamEmitsBareErrorFrame(t *testing.T) {
	resetState()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream down"}}`))
	}))
	defer server.Close()
	restore := pointWorkBuddyAt(server.URL)
	defer restore()

	storage, _ := json.Marshal(map[string]any{"accessToken": "tok", "domain": "cn"})
	payload, _ := json.Marshal(map[string]any{
		"OriginalRequest": []byte(`{"model":"m","stream":true,"messages":[]}`),
		"StorageJSON":     storage,
		"Stream":          true,
	})

	res := callOK(t, pluginabi.MethodExecutorExecuteStream, json.RawMessage(payload))
	var out struct {
		Chunks []struct {
			Payload []byte `json:"Payload"`
		} `json:"chunks"`
	}
	mustDecode(t, res, &out)
	if len(out.Chunks) == 0 {
		t.Fatal("expected a terminal error chunk")
	}
	p := out.Chunks[0].Payload
	if !json.Valid(p) {
		t.Fatalf("error chunk must be bare JSON, got %q", p)
	}
	if !strings.Contains(string(p), "upstream_error") {
		t.Fatalf("chunk = %s", p)
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
		t.Fatalf("models = %+v", out.Models)
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
