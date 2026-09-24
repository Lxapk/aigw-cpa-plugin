package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file holds regression tests for the four operator-facing bug reports
// that drove v0.13.7:
//
//  1. 账号禁用没解开 — pool disable/find must match both the lane uid and the
//     CPA auth index, and the toggle request must carry both keys.
//  2. 任务内读取不到账号 — the task engine resolves accounts from the auth
//     store (collectCheckinAccounts) instead of an empty in-memory view.
//  3. variant 切换保存后回显又变"自动" — the panel override is persisted to a
//     per-user state file and restored on register/reconfigure.
//  4. 设置界面残留无用网关设置 — the management status page no longer renders
//     the 网关设置 block.

// ---- ① account disable is keyed by uid AND auth index -------------------

func TestPoolDisableKeyedMatchesAuthIndexLane(t *testing.T) {
	resetState()
	// The pool lane that actually serves traffic is keyed by the CPA auth
	// index, while the panel knows the credential by its provider-side uid.
	state.pool.observe(workBuddyProviderKey, "auth-1.json", "label")

	// Panel toggle arrives with uid=u-1 + auth_index=auth-1.json.
	state.pool.disableAccountKeyed("u-1", "auth-1.json", true)
	if lane := state.pool.findAccount("auth-1.json"); lane == nil || !lane.DisabledByUser {
		t.Fatalf("lane should be disabled via auth-index match, got %+v", lane)
	}
	if lane := state.pool.findAccountKeyed("u-1", "auth-1.json"); lane == nil || !lane.DisabledByUser {
		t.Fatalf("dual-key find must still hit the same lane, got %+v", lane)
	}
	if !state.pool.isAccountDisabled("u-1", "auth-1.json") {
		t.Fatal("isAccountDisabled must report the lane as disabled")
	}
	// The lane is not usable while disabled.
	now := time.Now()
	if lane := state.pool.findAccountKeyed("u-1", "auth-1.json"); lane.usable(now) {
		t.Fatal("disabled lane must not be usable")
	}

	// Enable again: both keys together must flip the same lane back.
	state.pool.disableAccountKeyed("u-1", "auth-1.json", false)
	if lane := state.pool.findAccountKeyed("u-1", "auth-1.json"); lane == nil || lane.DisabledByUser {
		t.Fatalf("enable via uid must clear DisabledByUser, got %+v", lane)
	}
	if state.pool.isAccountDisabled("u-1", "auth-1.json") {
		t.Fatal("isAccountDisabled must be false after enable")
	}
}

func TestPoolDisableKeyedDoesNotCreateOrphanWhenLaneExists(t *testing.T) {
	resetState()
	state.pool.observe(workBuddyProviderKey, "auth-1.json", "label")
	state.pool.disableAccountKeyed("u-1", "auth-1.json", true)
	// Before the fix the UID-only match missed the auth-index lane and created
	// a second orphan lane that was never used by traffic.
	if lanes := state.pool.snapshot(); len(lanes) != 1 {
		t.Fatalf("expected exactly one lane, got %d: %+v", len(lanes), lanes)
	}
}

func TestAccountToggleHandlerKeyed(t *testing.T) {
	resetState()
	state.pool.observe(workBuddyProviderKey, "auth-1.json", "label")

	// The panel now POSTs both uid and auth_index.
	req := pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   "/account/toggle",
		Body:   mustMarshalRaw(t, map[string]any{"uid": "u-1", "auth_index": "auth-1.json", "action": "disable"}),
	}
	res, ok := handleAccountToggleRequest(req)
	if !ok || res.StatusCode != http.StatusOK {
		t.Fatalf("toggle disable failed: %+v", res)
	}
	if !state.pool.isAccountDisabled("u-1", "auth-1.json") {
		t.Fatal("account must be disabled after keyed toggle")
	}

	// toggle again flips it back on.
	req.Body = mustMarshalRaw(t, map[string]any{"uid": "u-1", "auth_index": "auth-1.json", "action": "toggle"})
	res, ok = handleAccountToggleRequest(req)
	if !ok || res.StatusCode != http.StatusOK {
		t.Fatalf("toggle flip failed: %+v", res)
	}
	if state.pool.isAccountDisabled("u-1", "auth-1.json") {
		t.Fatal("account must be re-enabled by the second toggle")
	}

	// A request with only the auth index must also work.
	req.Body = mustMarshalRaw(t, map[string]any{"uid": "auth-1.json", "auth_index": "", "action": "disable"})
	res, ok = handleAccountToggleRequest(req)
	if !ok || res.StatusCode != http.StatusOK {
		t.Fatalf("auth-index-only toggle failed: %+v", res)
	}
	if !state.pool.isAccountDisabled("u-1", "auth-1.json") {
		t.Fatal("auth-index-only disable must hit the lane")
	}
}

// ---- ② task engine resolves accounts from the auth store ----------------

func TestTaskEngineResolveAccountFromAuthStore(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{
			"auth_index":   "auth-1.json",
			"provider":     workBuddyProviderKey,
			"label":        "Acc One",
			"storage_json": mustStorage(t, map[string]any{"type": workBuddyProviderKey, "accessToken": "[REDACTED]", "uid": "u-1", "domain": "cn"}),
		},
	})

	e := newTaskEngine()
	// uid from the provider side.
	account, ok := e.resolveAccount("u-1")
	if !ok {
		t.Fatal("resolveAccount(u-1) must find the stored credential")
	}
	if account.AuthID != "auth-1.json" || account.Label != "Acc One" {
		t.Fatalf("resolved account = %+v", account)
	}
	// auth index also resolves (both keys map onto the credential).
	if _, ok := e.resolveAccount("auth-1.json"); !ok {
		t.Fatal("resolveAccount(auth-1.json) must also hit")
	}
}

func TestTaskEngineResolveAccountSkipsDisabled(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{
			"auth_index":   "auth-1.json",
			"provider":     workBuddyProviderKey,
			"label":        "Acc One",
			"storage_json": mustStorage(t, map[string]any{"type": workBuddyProviderKey, "accessToken": "[REDACTED]", "uid": "u-1", "domain": "cn"}),
		},
	})

	e := newTaskEngine()
	if _, ok := e.resolveAccount("u-1"); !ok {
		t.Fatal("precondition: account resolvable")
	}

	// Disable via the panel (dual-key path used by the real UI).
	state.pool.disableAccountKeyed("u-1", "auth-1.json", true)
	if _, ok := e.resolveAccount("u-1"); ok {
		t.Fatal("resolveAccount must refuse a manually disabled account")
	}
}

func TestTaskEngineScanDueSkipsDisabledAndEnqueuesEnabled(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{
			"auth_index":   "auth-1.json",
			"provider":     workBuddyProviderKey,
			"label":        "Acc One",
			"storage_json": mustStorage(t, map[string]any{"type": workBuddyProviderKey, "accessToken": "[REDACTED]", "uid": "u-1", "domain": "cn"}),
		},
		{
			"auth_index":   "auth-2.json",
			"provider":     workBuddyProviderKey,
			"label":        "Acc Two",
			"storage_json": mustStorage(t, map[string]any{"type": workBuddyProviderKey, "accessToken": "[REDACTED]", "uid": "u-2", "domain": "cn"}),
		},
	})

	e := newTaskEngine()
	e.initFromAccounts([]workBuddyAccount{
		{UID: "u-1", AuthIndex: "auth-1.json", Label: "Acc One"},
		{UID: "u-2", AuthIndex: "auth-2.json", Label: "Acc Two"},
	})
	// Disable account two (both keys).
	state.pool.disableAccountKeyed("u-2", "auth-2.json", true)

	e.scanDue(time.Now())
	e.mu.Lock()
	uids := make([]string, 0, len(e.queue))
	for _, q := range e.queue {
		uids = append(uids, q.UID)
	}
	e.mu.Unlock()

	if len(uids) != 1 || uids[0] != "u-1" {
		t.Fatalf("scanDue must enqueue only the enabled account, got %v", uids)
	}
}

// ---- ③ variant override survives reconfigure (persistence) --------------

func TestVariantOverridePersistsAcrossReconfigure(t *testing.T) {
	// Hermetic store pointing at a temp file.
	dir := t.TempDir()
	store := newSettingsStoreWithPersist(filepath.Join(dir, "state.json"))

	// Panel forces variant "cn".
	store.setVariantOverride("cn")
	if got := store.get().VariantOverride; got != "cn" {
		t.Fatalf("after setVariantOverride, variant = %q", got)
	}

	// A later reconfigure carries no variant_override in its YAML. Before the
	// fix this silently reset the override back to "" (auto).
	if err := store.decodeLifecycleConfig([]byte("enabled: true\npriority: 1\nport: 8790\n")); err != nil {
		t.Fatalf("decodeLifecycleConfig: %v", err)
	}
	if got := store.get().VariantOverride; got != "cn" {
		t.Fatalf("variant after reconfigure = %q, want persisted cn", got)
	}

	// A new store instance reading the same file (simulates plugin restart)
	// must also restore the override.
	store2 := newSettingsStoreWithPersist(filepath.Join(dir, "state.json"))
	if err := store2.decodeLifecycleConfig([]byte("enabled: true\n")); err != nil {
		t.Fatalf("store2 decodeLifecycleConfig: %v", err)
	}
	if got := store2.get().VariantOverride; got != "cn" {
		t.Fatalf("variant after restart = %q, want persisted cn", got)
	}
}

func TestVariantOverrideExplicitYAMLWins(t *testing.T) {
	dir := t.TempDir()
	store := newSettingsStoreWithPersist(filepath.Join(dir, "state.json"))
	store.setVariantOverride("cn")

	// When the operator explicitly declares variant_override in config.yaml it
	// must win over the persisted panel value.
	if err := store.decodeLifecycleConfig([]byte("enabled: true\nvariant_override: ai\n")); err != nil {
		t.Fatalf("decodeLifecycleConfig: %v", err)
	}
	if got := store.get().VariantOverride; got != "ai" {
		t.Fatalf("explicit YAML variant = %q, want ai", got)
	}
}

func TestVariantOverrideResetPersistsEmpty(t *testing.T) {
	dir := t.TempDir()
	store := newSettingsStoreWithPersist(filepath.Join(dir, "state.json"))
	store.setVariantOverride("cn")
	// Switching back to auto must persist "" so a later reconfigure does not
	// resurrect the old override.
	store.setVariantOverride("")
	if got := store.get().VariantOverride; got != "" {
		t.Fatalf("variant after reset = %q, want empty", got)
	}
	if err := store.decodeLifecycleConfig([]byte("enabled: true\n")); err != nil {
		t.Fatalf("decodeLifecycleConfig: %v", err)
	}
	if got := store.get().VariantOverride; got != "" {
		t.Fatalf("variant after reconfigure = %q, want empty", got)
	}
}

// ---- ④ management status page has no 网关设置 block ----------------------

func TestStatusPageHasNoGatewaySettingsBlock(t *testing.T) {
	resetState()
	page := statusPage()
	for _, needle := range []string{"网关设置", "soft_cooldown_millis", "quota_cooldown_millis", "error_cooldown_millis", "log_retention_days"} {
		if strings.Contains(page, needle) {
			t.Errorf("status page must not contain %q (removed 网关设置 block)", needle)
		}
	}
	for _, needle := range []string{"账号池", "最近调用", "调用统计"} {
		if !strings.Contains(page, needle) {
			t.Errorf("status page should still contain %q", needle)
		}
	}
}

// ---- small helpers -------------------------------------------------------

func mustMarshalRaw(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// ensure the state file helpers actually write under the plugin state dir.
func TestPluginStateDirWritable(t *testing.T) {
	dir := pluginStateDir()
	if dir == "" {
		t.Fatal("pluginStateDir returned empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("pluginStateDir %q cannot be created: %v", dir, err)
	}
	probe := filepath.Join(dir, ".probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatalf("pluginStateDir %q not writable: %v", dir, err)
	}
	_ = os.Remove(probe)
}
