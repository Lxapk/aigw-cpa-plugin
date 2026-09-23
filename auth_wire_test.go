package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests pin the host.auth.* wire formats.
//
// Both field names were originally guessed wrong, and the failure mode is
// silent: json.Unmarshal leaves the field zero, so the plugin reported "no
// WorkBuddy accounts" even though the credential was stored and usable, and
// CPA then failed every request with
//
//	auth_not_found: no auth available (providers=codebuddy, ...)
//
// The authoritative shapes are:
//
//	internal/pluginhost.rpcHostAuthListResponse{ Files []HostAuthFileEntry `json:"files"` }
//	internal/pluginhost.rpcHostAuthGetResponse{ JSON json.RawMessage `json:"json"` }

// TestDecodeAuthListAcceptsFilesKey covers the real CPA response shape.
func TestDecodeAuthListAcceptsFilesKey(t *testing.T) {
	raw := json.RawMessage(`{
		"files": [
			{"auth_index":"codebuddy-u-1.json","name":"codebuddy-u-1.json",
			 "provider":"codebuddy","label":"Acct One","status":"ready"},
			{"auth_index":"anthropic-x.json","name":"anthropic-x.json",
			 "provider":"anthropic","label":"Other"}
		]
	}`)
	entries := decodeAuthEntries(raw)
	if len(entries) != 2 {
		t.Fatalf("decoded %d entries, want 2 (the host nests them under \"files\")", len(entries))
	}
	if entries[0].AuthIndex != "codebuddy-u-1.json" {
		t.Errorf("auth_index = %q", entries[0].AuthIndex)
	}
	if entries[0].Provider != workBuddyProviderKey {
		t.Errorf("provider = %q", entries[0].Provider)
	}
	if entries[0].Label != "Acct One" {
		t.Errorf("label = %q", entries[0].Label)
	}
}

// TestDecodeAuthListFallbacks keeps the tolerant shapes working.
func TestDecodeAuthListFallbacks(t *testing.T) {
	cases := map[string]string{
		"auths":      `{"auths":[{"auth_index":"a.json","provider":"codebuddy"}]}`,
		"items":      `{"items":[{"auth_index":"a.json","provider":"codebuddy"}]}`,
		"bare array": `[{"auth_index":"a.json","provider":"codebuddy"}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			entries := decodeAuthEntries(json.RawMessage(body))
			if len(entries) != 1 || entries[0].AuthIndex != "a.json" {
				t.Fatalf("entries = %+v", entries)
			}
		})
	}
}

// TestDecodeAuthListEmptyShapes makes sure absent data yields nothing rather
// than a spurious entry.
func TestDecodeAuthListEmptyShapes(t *testing.T) {
	for name, body := range map[string]string{
		"empty files":  `{"files":[]}`,
		"empty object": `{}`,
		"empty string": ``,
		"null":         `null`,
	} {
		t.Run(name, func(t *testing.T) {
			if entries := decodeAuthEntries(json.RawMessage(body)); len(entries) != 0 {
				t.Fatalf("entries = %+v, want none", entries)
			}
		})
	}
}

// TestCredentialJSONPrefersJSONKey is the fix for the second wrong field name:
// host.auth.get returns the body under "json", not "storage_json".
func TestCredentialJSONPrefersJSONKey(t *testing.T) {
	var resp hostAuthGetResponse
	body := `{"auth_index":"codebuddy-u-1.json","name":"codebuddy-u-1.json","json":{"accessToken":"at","uid":"u-1"}}`
	if errUnmarshal := json.Unmarshal([]byte(body), &resp); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	got := resp.credentialJSON()
	if len(got) == 0 {
		t.Fatal("credential body not read from the \"json\" key")
	}
	var doc map[string]any
	if errUnmarshal := json.Unmarshal(got, &doc); errUnmarshal != nil {
		t.Fatalf("body is not json: %v", errUnmarshal)
	}
	if doc["accessToken"] != "at" {
		t.Fatalf("body = %s", got)
	}
}

// TestCredentialJSONFallback keeps the tolerant key working.
func TestCredentialJSONFallback(t *testing.T) {
	var resp hostAuthGetResponse
	if errUnmarshal := json.Unmarshal([]byte(`{"storage_json":{"accessToken":"x"}}`), &resp); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	if len(resp.credentialJSON()) == 0 {
		t.Fatal("storage_json fallback not honoured")
	}
}

// TestAccountListReadsCredentialBody exercises the full path: the list gives
// metadata only, so the plugin must fetch each body via host.auth.get.
func TestAccountListReadsCredentialBody(t *testing.T) {
	resetState()

	const authIndex = "codebuddy-u-1.json"
	storage := map[string]any{
		"type": workBuddyProviderKey, "accessToken": "at-1",
		"uid": "u-1", "domain": "cn", "nickname": "Nick",
	}

	restore := stubHostCall(func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			// Metadata only, exactly like listAuthFiles.
			return json.RawMessage(`{"files":[{"auth_index":"` + authIndex +
				`","name":"` + authIndex + `","provider":"codebuddy","label":"Nick","status":"ready"}]}`), nil
		case "host.auth.get":
			sent, _ := json.Marshal(payload)
			if !strings.Contains(string(sent), authIndex) {
				t.Errorf("host.auth.get called without the right auth_index: %s", sent)
			}
			// Body under "json", like rpcHostAuthGetResponse.
			body, _ := json.Marshal(map[string]any{
				"auth_index": authIndex, "name": authIndex, "json": storage,
			})
			return body, nil
		}
		return json.RawMessage(`{}`), nil
	})
	defer restore()
	state.accounts.invalidate()

	accounts := listWorkBuddyAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v, want the stored credential", accounts)
	}
	a := accounts[0]
	if a.UID != "u-1" {
		t.Errorf("uid = %q (credential body was not read)", a.UID)
	}
	if a.Label != "Nick" {
		t.Errorf("label = %q", a.Label)
	}
	if a.Domain != "cn" || a.Variant != "cn" {
		t.Errorf("domain/variant = %q/%q", a.Domain, a.Variant)
	}
	if !a.Usable {
		t.Errorf("a healthy credential should be usable: %+v", a)
	}
}

// TestCheckinAccountCollectionUsesFilesKey makes sure the check-in path sees the
// same accounts the panel does.
func TestCheckinAccountCollectionUsesFilesKey(t *testing.T) {
	resetState()

	const authIndex = "codebuddy-u-9.json"
	storage := map[string]any{"accessToken": "at-9", "uid": "u-9", "domain": "cn"}

	restore := stubHostCall(func(method string, _ any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.RawMessage(`{"files":[{"auth_index":"` + authIndex +
				`","provider":"codebuddy","label":"Nine"}]}`), nil
		case "host.auth.get":
			body, _ := json.Marshal(map[string]any{"auth_index": authIndex, "json": storage})
			return body, nil
		}
		return json.RawMessage(`{}`), nil
	})
	defer restore()

	accounts, errCollect := collectCheckinAccounts()
	if errCollect != nil {
		t.Fatalf("collect: %v", errCollect)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v, want 1 (list nests under \"files\")", accounts)
	}
	if accounts[0].Creds.UID != "u-9" {
		t.Fatalf("creds = %+v (body not fetched)", accounts[0].Creds)
	}
}
