package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// These tests lock in the two defects that caused
// "auth_not_found: no auth available (providers=codebuddy, ...)" after a
// successful WorkBuddy login:
//
//	1. host.auth.save was called with the wrong field names
//	   (FileName / StorageJSON instead of name / json), so CPA rejected the
//	   request and no credential was ever written.
//	2. The persisted credential JSON had no "type" field, and CPA derives a
//	   credential's provider from metadata["type"], so even a written account
//	   would have been filed under provider "unknown" and never matched.

// ---- defect 1: host.auth.save request shape -----------------------------

// TestSaveAuthThroughHostUsesDocumentedFieldNames asserts the request shape is
// exactly pluginapi.HostAuthSaveRequest{"name","json"}.
func TestSaveAuthThroughHostUsesDocumentedFieldNames(t *testing.T) {
	resetState()

	var captured map[string]json.RawMessage
	restore := stubHostCall(func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.save" {
			t.Fatalf("unexpected host method %q", method)
		}
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			t.Fatalf("marshal payload: %v", errMarshal)
		}
		if errUnmarshal := json.Unmarshal(raw, &captured); errUnmarshal != nil {
			t.Fatalf("unmarshal payload: %v", errUnmarshal)
		}
		return json.RawMessage(`{"name":"x.json","path":"/tmp/x.json"}`), nil
	})
	defer restore()

	creds := &workBuddyCredentials{
		AccessToken: "at", RefreshToken: "rt", Domain: "cn", UID: "u-1", Nickname: "Nick",
	}
	if errSave := saveAuthThroughHost(workBuddyAuthData(creds)); errSave != nil {
		t.Fatalf("save failed: %v", errSave)
	}

	// The two documented keys must be present...
	if _, ok := captured["name"]; !ok {
		t.Fatalf(`missing "name" key; got keys %v`, keysOf(captured))
	}
	if _, ok := captured["json"]; !ok {
		t.Fatalf(`missing "json" key; got keys %v`, keysOf(captured))
	}

	// ...and the old, wrong key names must be gone.
	for _, wrong := range []string{"FileName", "StorageJSON", "Provider", "ID", "Metadata"} {
		if _, present := captured[wrong]; present {
			t.Errorf("request still carries obsolete key %q", wrong)
		}
	}

	// name must end in .json (CPA rejects anything else).
	var name string
	if errUnmarshal := json.Unmarshal(captured["name"], &name); errUnmarshal != nil {
		t.Fatalf("name is not a string: %v", errUnmarshal)
	}
	if !strings.HasSuffix(name, ".json") {
		t.Fatalf("name = %q, must end with .json", name)
	}
}

// TestSaveAuthThroughHostAddsJSONSuffix ensures a missing extension is repaired
// rather than rejected by the host.
func TestSaveAuthThroughHostAddsJSONSuffix(t *testing.T) {
	resetState()

	var name string
	restore := stubHostCall(func(_ string, payload any) (json.RawMessage, error) {
		raw, _ := json.Marshal(payload)
		var doc map[string]json.RawMessage
		_ = json.Unmarshal(raw, &doc)
		_ = json.Unmarshal(doc["name"], &name)
		return json.RawMessage(`{}`), nil
	})
	defer restore()

	auth := workBuddyAuthData(&workBuddyCredentials{AccessToken: "at", UID: "u-1"})
	auth.FileName = "codebuddy-u-1" // deliberately without extension
	if errSave := saveAuthThroughHost(auth); errSave != nil {
		t.Fatalf("save failed: %v", errSave)
	}
	if !strings.HasSuffix(name, ".json") {
		t.Fatalf("name = %q, want .json suffix", name)
	}
}

// TestSaveAuthThroughHostSurfacesHostError makes sure a host-side rejection is
// reported instead of silently swallowed.
func TestSaveAuthThroughHostSurfacesHostError(t *testing.T) {
	resetState()
	restore := stubHostCall(func(_ string, _ any) (json.RawMessage, error) {
		return nil, errHostRPC{Code: "invalid_request", Message: "json is required"}
	})
	defer restore()

	errSave := saveAuthThroughHost(workBuddyAuthData(&workBuddyCredentials{AccessToken: "at", UID: "u"}))
	if errSave == nil {
		t.Fatal("expected the host error to propagate")
	}
	if !strings.Contains(errSave.Error(), "json is required") {
		t.Fatalf("error = %v", errSave)
	}
}

// ---- defect 2: persisted credential must carry "type" -------------------

// TestStorageJSONCarriesProviderType is the fix for the provider mismatch: CPA
// reads metadata["type"] to decide which provider an auth file belongs to
// (internal/pluginhost/auth_callbacks.go:324).
func TestStorageJSONCarriesProviderType(t *testing.T) {
	creds := &workBuddyCredentials{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1893456000,
		Domain: "cn", UID: "u-1", EnterpriseID: "e-1", Nickname: "Nick",
	}

	var doc map[string]any
	if errUnmarshal := json.Unmarshal(creds.storageJSON(), &doc); errUnmarshal != nil {
		t.Fatalf("bad json: %v", errUnmarshal)
	}

	if doc["type"] != workBuddyProviderKey {
		t.Fatalf(`type = %v, want %q — without it CPA files the auth under "unknown" and never matches a codebuddy request`,
			doc["type"], workBuddyProviderKey)
	}

	// The original a2/b.E() fields must all survive alongside "type".
	for _, key := range []string{"accessToken", "refreshToken", "expiresAt", "domain", "uid", "enterpriseId", "nickname"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("storage JSON lost original key %q", key)
		}
	}
}

// TestStorageJSONRoundTrips ensures the extra "type" field does not break the
// plugin's own parser.
func TestStorageJSONRoundTrips(t *testing.T) {
	original := &workBuddyCredentials{
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1893456000,
		Domain: "global", UID: "u-9", EnterpriseID: "e-9", Nickname: "Nine",
	}
	parsed, errParse := parseWorkBuddyCredentials(original.storageJSON())
	if errParse != nil {
		t.Fatalf("round trip failed: %v", errParse)
	}
	if parsed.AccessToken != original.AccessToken ||
		parsed.RefreshToken != original.RefreshToken ||
		parsed.Domain != original.Domain ||
		parsed.UID != original.UID ||
		parsed.EnterpriseID != original.EnterpriseID ||
		parsed.Nickname != original.Nickname ||
		parsed.ExpiresAt != original.ExpiresAt {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", parsed, original)
	}
}

// TestAuthDataFileNameIdentifiesProvider keeps the saved file name aligned with
// the provider key, so CPA's provider filter can see it.
func TestAuthDataFileNameIdentifiesProvider(t *testing.T) {
	auth := workBuddyAuthData(&workBuddyCredentials{AccessToken: "at", UID: "u-1"})
	if !strings.HasPrefix(auth.FileName, workBuddyProviderKey) {
		t.Fatalf("fileName = %q, should start with the provider key", auth.FileName)
	}
	if auth.Provider != workBuddyProviderKey {
		t.Fatalf("provider = %q", auth.Provider)
	}
	// The metadata type must agree with Provider, since CPA reads the former.
	if auth.Metadata["type"] != workBuddyProviderKey {
		t.Fatalf("metadata type = %v", auth.Metadata["type"])
	}
}

// ---- end-to-end: login poll must persist a usable record ----------------

func TestAuthLoginPollPersistsAndReportsSaveFailure(t *testing.T) {
	resetState()

	// Point the poll endpoint at a server that reports a completed login.
	server := workBuddyLoginServer(t, `{"code":0,"data":{"accessToken":"at","refreshToken":"rt","expiresAt":1893456000,"uid":"u-7","nickname":"Seven"}}`)
	defer server.Close()
	restoreEndpoint := pointWorkBuddyAt(server.URL)
	defer restoreEndpoint()

	// Simulate the host rejecting the save so the operator sees why the account
	// did not appear.
	restore := stubHostCall(func(_ string, _ any) (json.RawMessage, error) {
		return nil, errHostRPC{Code: "invalid_request", Message: "json is required"}
	})
	defer restore()

	res := callOK(t, pluginabi.MethodAuthLoginPoll, pluginapi.AuthLoginPollRequest{State: "s"})
	var out pluginapi.AuthLoginPollResponse
	mustDecode(t, res, &out)

	if out.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success (the login itself succeeded)", out.Status)
	}
	if !strings.Contains(out.Message, "写入") {
		t.Fatalf("message should explain the persistence failure, got %q", out.Message)
	}
	// The credential must still be handed back so the operator can retry/save
	// it manually.
	if out.Auth.ID != "u-7" {
		t.Fatalf("auth = %+v", out.Auth)
	}
}

func TestAuthLoginPollSucceedsSilentlyWhenSaveWorks(t *testing.T) {
	resetState()
	server := workBuddyLoginServer(t, `{"code":0,"data":{"accessToken":"at","refreshToken":"rt","expiresAt":1893456000,"uid":"u-8"}}`)
	defer server.Close()
	restoreEndpoint := pointWorkBuddyAt(server.URL)
	defer restoreEndpoint()

	var savedName string
	var savedJSON []byte
	restore := stubHostCall(func(method string, payload any) (json.RawMessage, error) {
		if method != "host.auth.save" {
			t.Fatalf("unexpected host method %q", method)
		}
		raw, _ := json.Marshal(payload)
		var doc map[string]json.RawMessage
		_ = json.Unmarshal(raw, &doc)
		_ = json.Unmarshal(doc["name"], &savedName)
		savedJSON = doc["json"]
		return json.RawMessage(`{}`), nil
	})
	defer restore()

	res := callOK(t, pluginabi.MethodAuthLoginPoll, pluginapi.AuthLoginPollRequest{State: "s"})
	var out pluginapi.AuthLoginPollResponse
	mustDecode(t, res, &out)

	if out.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q", out.Status)
	}
	if strings.Contains(out.Message, "失败") {
		t.Fatalf("unexpected failure message: %q", out.Message)
	}
	if !strings.HasSuffix(savedName, ".json") {
		t.Fatalf("saved name = %q", savedName)
	}
	// The payload handed to the host must contain the provider type.
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(savedJSON, &doc); errUnmarshal != nil {
		t.Fatalf("saved json invalid: %v (%s)", errUnmarshal, savedJSON)
	}
	if doc["type"] != workBuddyProviderKey {
		t.Fatalf(`saved credential type = %v, want %q`, doc["type"], workBuddyProviderKey)
	}
}

// ---- helpers ------------------------------------------------------------

// stubHostCall replaces the host RPC entrypoint for the duration of a test.
func stubHostCall(fn func(method string, payload any) (json.RawMessage, error)) func() {
	original := hostCallFunc
	hostCallFunc = fn
	return func() { hostCallFunc = original }
}

// workBuddyLoginServer starts a server that answers the device-code token
// endpoint with the supplied body, so the poll path can be exercised without
// touching Tencent.
func workBuddyLoginServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v2/plugin/auth/token") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
