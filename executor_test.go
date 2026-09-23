package main

import (
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

func TestModelRouteIgnoresUnknownModel(t *testing.T) {
	resetState()
	workBuddyModelCache.put("cn/u-1", []workBuddyModel{{ID: "known"}})

	res := callOK(t, pluginabi.MethodModelRoute, pluginapi.ModelRouteRequest{
		SourceFormat:   "chat-completions",
		RequestedModel: "some-other-model",
	})
	var out pluginapi.ModelRouteResponse
	mustDecode(t, res, &out)
	if out.Handled {
		t.Fatal("unknown model must not be claimed")
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

func TestExecutorExecuteStreamForwardsSSEFrames(t *testing.T) {
	resetState()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"He\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}],\"usage\":{\"total_tokens\":2}}\n\n"))
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

	if len(out.Chunks) == 0 {
		t.Fatal("expected stream chunks")
	}
	var joined strings.Builder
	for _, c := range out.Chunks {
		joined.Write(c.Payload)
	}
	all := joined.String()
	if !strings.Contains(all, "He") || !strings.Contains(all, "llo") {
		t.Fatalf("chunks lost content: %q", all)
	}
	if !strings.Contains(all, "[DONE]") {
		t.Fatalf("terminal sentinel lost: %q", all)
	}
}

func TestExecutorExecuteStreamEmitsErrorFrame(t *testing.T) {
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
	if !strings.Contains(string(out.Chunks[0].Payload), "upstream_error") {
		t.Fatalf("chunk = %s", out.Chunks[0].Payload)
	}
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
