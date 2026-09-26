package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ---- capability registration -------------------------------------------

func TestRegistrationDeclaresScheduler(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodPluginRegister, lifecycleRequest{SchemaVersion: pluginabi.SchemaVersion})
	var raw struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	mustDecode(t, res, &raw)
	if v, ok := raw.Capabilities["scheduler"]; !ok || v != true {
		t.Fatalf("scheduler capability must be declared, got %v", raw.Capabilities["scheduler"])
	}
}

// ---- strategy parsing --------------------------------------------------

func TestNormalizeStrategy(t *testing.T) {
	cases := map[string]schedulerStrategy{
		"":            strategyByCredits,
		"by_credits":  strategyByCredits,
		"credits":     strategyByCredits,
		"按额度":         strategyByCredits,
		"BY_CREDITS":  strategyByCredits,
		"round_robin": strategyRoundRobin,
		"round-robin": strategyRoundRobin,
		"rr":          strategyRoundRobin,
		"轮巡":          strategyRoundRobin,
		"random":      strategyRandom,
		"rand":        strategyRandom,
		"随机":          strategyRandom,
		"nonsense":    strategyByCredits, // unknown falls back to the app default
	}
	for in, want := range cases {
		if got := normalizeStrategy(in); got != want {
			t.Errorf("normalizeStrategy(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestWeightedStrategyIsGone is the guard for the removal.
//
// The three-factor weighted strategy shared its entire call path with
// by_credits — same SchedulerPick RPC, same candidate set, same response — and
// its enable switch was never read, so it was a second name for the same
// behaviour plus an inert setting. A configuration saved under the old version
// must now fall back to the default rather than resolving to a strategy that no
// longer exists.
func TestWeightedStrategyIsGone(t *testing.T) {
	for _, spelling := range []string{"weighted", "WEIGHTED", "Weighted", "三因子", "加权"} {
		if got := normalizeStrategy(spelling); got != strategyByCredits {
			t.Errorf("normalizeStrategy(%q) = %v, want the by_credits default", spelling, got)
		}
	}
	for _, s := range allSchedulerStrategies {
		if string(s) == "weighted" {
			t.Fatal("weighted is still advertised in allSchedulerStrategies")
		}
	}
	if len(allSchedulerStrategies) != 4 {
		t.Fatalf("strategy count = %d, want 4 after removing weighted", len(allSchedulerStrategies))
	}
	// Labels must not mention the removed strategy.
	for _, s := range allSchedulerStrategies {
		if strings.Contains(s.label(), "三因子") {
			t.Fatalf("label %q still refers to the removed strategy", s.label())
		}
	}
}

func TestStrategyLabels(t *testing.T) {
	cases := map[schedulerStrategy]string{
		strategyByCredits:  "按额度",
		strategyRoundRobin: "轮巡",
		strategyRandom:     "随机",
	}
	for s, want := range cases {
		if got := s.label(); got != want {
			t.Errorf("%v.label() = %q, want %q", s, got, want)
		}
	}
}

// ---- candidate filtering ----------------------------------------------

func TestCollectCandidatesFiltersUnusable(t *testing.T) {
	resetState()
	s := newSchedulerState()

	req := pluginapi.SchedulerPickRequest{
		Provider: workBuddyProviderKey,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "ok", Provider: workBuddyProviderKey, Status: "active"},
			{ID: "disabled", Provider: workBuddyProviderKey, Status: "disabled"},
			{ID: "failed", Provider: workBuddyProviderKey, Status: "error"},
			{ID: "expired", Provider: workBuddyProviderKey, Status: "expired"},
			{ID: "meta-disabled", Provider: workBuddyProviderKey, Metadata: map[string]any{"disabled": true}},
			{ID: "", Provider: workBuddyProviderKey},
		},
	}
	got := s.collectCandidates(req)
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("candidates = %+v, want only ok", got)
	}
}

func TestCollectCandidatesHonoursTriedSet(t *testing.T) {
	resetState()
	s := newSchedulerState()

	req := pluginapi.SchedulerPickRequest{
		Provider: workBuddyProviderKey,
		Options: pluginapi.SchedulerOptions{
			Metadata: map[string]any{"tried_auth_ids": []any{"a"}},
		},
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "a", Provider: workBuddyProviderKey, Status: "active"},
			{ID: "b", Provider: workBuddyProviderKey, Status: "active"},
		},
	}
	got := s.collectCandidates(req)
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("candidates = %+v, want only b", got)
	}
}

func TestCollectCandidatesHonoursPoolCooldown(t *testing.T) {
	resetState()
	s := newSchedulerState()
	state.pool.observe(workBuddyProviderKey, "cooling", "C")
	state.pool.failure(workBuddyProviderKey, "cooling", failureTransient, "boom", defaultGatewaySettings(), false)

	req := pluginapi.SchedulerPickRequest{
		Provider: workBuddyProviderKey,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "cooling", Provider: workBuddyProviderKey, Status: "active"},
			{ID: "fresh", Provider: workBuddyProviderKey, Status: "active"},
		},
	}
	got := s.collectCandidates(req)
	if len(got) != 1 || got[0].ID != "fresh" {
		t.Fatalf("candidates = %+v, want the non-cooling one", got)
	}
}

func TestIsUnusableSchedulerStatus(t *testing.T) {
	usable := []string{"", "active", "ready", "ok", "healthy", "available", "valid", "weird"}
	for _, s := range usable {
		if isUnusableSchedulerStatus(s) {
			t.Errorf("%q should be usable", s)
		}
	}
	unusable := []string{"disabled", "unavailable", "failed", "invalid", "error", "expired"}
	for _, s := range unusable {
		if !isUnusableSchedulerStatus(s) {
			t.Errorf("%q should be unusable", s)
		}
	}
}

// ---- strategies --------------------------------------------------------

func TestPickByCreditsChoosesRichest(t *testing.T) {
	got := pickByCredits([]schedulerCandidate{
		{ID: "a", Credits: 10},
		{ID: "b", Credits: 900},
		{ID: "c", Credits: 100},
	})
	if got != "b" {
		t.Fatalf("pickByCredits = %q, want b", got)
	}
}

func TestPickByCreditsTieKeepsFirst(t *testing.T) {
	// A0/s.java:596 uses a strict >, so the earlier candidate wins a tie.
	got := pickByCredits([]schedulerCandidate{{ID: "first", Credits: 5}, {ID: "second", Credits: 5}})
	if got != "first" {
		t.Fatalf("pickByCredits = %q, want first", got)
	}
}

func TestPickRoundRobinRotates(t *testing.T) {
	s := newSchedulerState()
	cands := []schedulerCandidate{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		seen[s.pickRoundRobin("codebuddy", cands)]++
	}
	// Two full cycles: each candidate chosen exactly twice.
	for _, id := range []string{"a", "b", "c"} {
		if seen[id] != 2 {
			t.Fatalf("round robin distribution = %v, want 2 each", seen)
		}
	}
}

func TestPickRoundRobinIndependentPerProvider(t *testing.T) {
	s := newSchedulerState()
	cands := []schedulerCandidate{{ID: "a"}, {ID: "b"}}

	// Advancing one provider must not move the other.
	first := s.pickRoundRobin("p1", cands)
	otherFirst := s.pickRoundRobin("p2", cands)
	if first != otherFirst {
		t.Fatalf("cursors should be independent: p1=%q p2=%q", first, otherFirst)
	}
}

func TestPickRandomCoversAllCandidates(t *testing.T) {
	s := newSchedulerState()
	cands := []schedulerCandidate{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	seen := map[string]bool{}
	for i := 0; i < 200 && len(seen) < 3; i++ {
		seen[s.pickRandom(cands)] = true
	}
	if len(seen) != 3 {
		t.Fatalf("random strategy only produced %v", seen)
	}
}

func TestStrategyPreviewOrder(t *testing.T) {
	accounts := []workBuddyAccount{
		{Label: "A", Credits: 1, Usable: true, AuthIndex: "a"},
		{Label: "B", Credits: 99, Usable: true, AuthIndex: "b"},
		{Label: "C", Credits: 50, Usable: true, AuthIndex: "c"},
		{Label: "D", Credits: 999, Usable: false, AuthIndex: "d"}, // unusable, excluded
	}

	byCredits := strategyPreview(strategyByCredits, accounts)
	if len(byCredits) != 3 {
		t.Fatalf("by_credits preview = %+v, want 3 usable", byCredits)
	}
	if byCredits[0].Label != "B" || byCredits[1].Label != "C" || byCredits[2].Label != "A" {
		t.Fatalf("by_credits order = %v", labelsOf(byCredits))
	}

	rr := strategyPreview(strategyRoundRobin, accounts)
	if rr[0].Label != "A" {
		t.Fatalf("round_robin order should be alphabetical, got %v", labelsOf(rr))
	}
}

func labelsOf(accounts []workBuddyAccount) []string {
	out := make([]string, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, a.Label)
	}
	return out
}

// ---- four-strategy end-to-end probe --------------------------------------
// TestAllFourStrategiesAreActuallyEffective drives each strategy through the
// real SchedulerPick RPC and asserts the observable behaviour, rather than
// trusting that the switch statement routes correctly.
//
// The panel offers these four, so every one must demonstrably do something
// different; a strategy that silently falls through to the default would look
// identical to a working one in the UI.
func TestAllFourStrategiesAreActuallyEffective(t *testing.T) {
	candidates := func() []pluginapi.SchedulerAuthCandidate {
		return []pluginapi.SchedulerAuthCandidate{
			{ID: "a", Provider: workBuddyProviderKey, Status: "active"},
			{ID: "b", Provider: workBuddyProviderKey, Status: "active"},
			{ID: "c", Provider: workBuddyProviderKey, Status: "active"},
		}
	}
	seedQuota := func() {
		state.quota.mu.Lock()
		state.quota.byAuth["a"] = &workBuddyQuota{Credits: 10, Known: true}
		state.quota.byAuth["b"] = &workBuddyQuota{Credits: 500, Known: true}
		state.quota.byAuth["c"] = &workBuddyQuota{Credits: 300, Known: true}
		state.quota.mu.Unlock()
	}

	t.Run("按额度 by_credits", func(t *testing.T) {
		resetState()
		applyRoutingConfig(routingSettings{Strategy: strategyByCredits})
		seedQuota()
		res := callOK(t, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{
			Provider: workBuddyProviderKey, Candidates: candidates(),
		})
		var out pluginapi.SchedulerPickResponse
		mustDecode(t, res, &out)
		if !out.Handled || out.AuthID != "b" {
			t.Fatalf("pick = %+v, want b (500 credits)", out)
		}
	})

	t.Run("按到期 by_expiry", func(t *testing.T) {
		resetState()
		applyRoutingConfig(routingSettings{Strategy: strategyByExpiry})
		// The soonest-expiring balance must win even though it is not the
		// richest, which is what distinguishes this strategy from by_credits.
		state.quota.mu.Lock()
		state.quota.byAuth["a"] = &workBuddyQuota{Credits: 10, Known: true,
			Summary: creditSummary{SoonestExpireAt: time.Now().Add(2 * time.Hour).Unix()}}
		state.quota.byAuth["b"] = &workBuddyQuota{Credits: 500, Known: true,
			Summary: creditSummary{SoonestExpireAt: time.Now().Add(720 * time.Hour).Unix()}}
		state.quota.byAuth["c"] = &workBuddyQuota{Credits: 300, Known: true,
			Summary: creditSummary{SoonestExpireAt: time.Now().Add(360 * time.Hour).Unix()}}
		state.quota.mu.Unlock()

		res := callOK(t, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{
			Provider: workBuddyProviderKey, Candidates: candidates(),
		})
		var out pluginapi.SchedulerPickResponse
		mustDecode(t, res, &out)
		if !out.Handled {
			t.Fatal("by_expiry did not handle the request")
		}
		if out.AuthID == "b" {
			t.Fatal("by_expiry picked the richest account; it is behaving like by_credits")
		}
	})

	t.Run("轮巡 round_robin", func(t *testing.T) {
		resetState()
		applyRoutingConfig(routingSettings{Strategy: strategyRoundRobin})
		res := callOK(t, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{
			Provider: workBuddyProviderKey, Candidates: candidates(),
		})
		var out pluginapi.SchedulerPickResponse
		mustDecode(t, res, &out)
		// Round-robin is delegated to CPA's built-in scheduler; the plugin must
		// name it rather than picking on its own.
		if out.DelegateBuiltin != pluginapi.SchedulerBuiltinRoundRobin {
			t.Fatalf("delegate = %q, want the builtin round-robin", out.DelegateBuiltin)
		}
	})

	t.Run("随机 random", func(t *testing.T) {
		resetState()
		applyRoutingConfig(routingSettings{Strategy: strategyRandom})
		seen := map[string]int{}
		for i := 0; i < 200; i++ {
			res := callOK(t, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{
				Provider: workBuddyProviderKey, Candidates: candidates(),
			})
			var out pluginapi.SchedulerPickResponse
			mustDecode(t, res, &out)
			if !out.Handled {
				t.Fatal("random did not handle the request")
			}
			seen[out.AuthID]++
		}
		if len(seen) < 2 {
			t.Fatalf("200 picks hit only %d account(s): %v — random is not random", len(seen), seen)
		}
		t.Logf("200 次随机分布: %v", seen)
	})
}

// TestAutoScopeServesBothRealms is the guard for 「全部供应商」.
//
// In auto mode a domestic and an international account must both be selectable;
// if the scope silently excluded one realm, half a mixed pool would never be
// used and the operator would see accounts that never get traffic.
func TestAutoScopeServesBothRealms(t *testing.T) {
	resetState()
	withVariantOverride(t, "")

	cn := &workBuddyCredentials{Domain: "copilot.tencent.com"}
	ai := &workBuddyCredentials{Domain: "www.workbuddy.ai"}
	if !variantAllowed(cn) {
		t.Fatal("auto excluded a domestic account")
	}
	if !variantAllowed(ai) {
		t.Fatal("auto excluded an international account")
	}

	// And the narrowed scopes still work as documented.
	withVariantOverride(t, "cn")
	if !variantAllowed(cn) || variantAllowed(ai) {
		t.Fatal("cn scope did not narrow to domestic accounts only")
	}
	withVariantOverride(t, "ai")
	if variantAllowed(cn) || !variantAllowed(ai) {
		t.Fatal("ai scope did not narrow to international accounts only")
	}
}

// ---- scheduler.pick RPC -------------------------------------------------

func TestSchedulerPickIgnoresForeignProvider(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{
		Provider:   "anthropic",
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "x", Provider: "anthropic"}},
	})
	var out pluginapi.SchedulerPickResponse
	mustDecode(t, res, &out)
	if out.Handled {
		t.Fatal("must not schedule for another provider")
	}
}

func TestSchedulerPickByCredits(t *testing.T) {
	resetState()
	applyRoutingConfig(routingSettings{Strategy: strategyByCredits})

	state.quota.mu.Lock()
	state.quota.byAuth["a"] = &workBuddyQuota{Credits: 10, Known: true}
	state.quota.byAuth["b"] = &workBuddyQuota{Credits: 500, Known: true}
	state.quota.mu.Unlock()

	res := callOK(t, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{
		Provider: workBuddyProviderKey,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "a", Provider: workBuddyProviderKey, Status: "active"},
			{ID: "b", Provider: workBuddyProviderKey, Status: "active"},
		},
	})
	var out pluginapi.SchedulerPickResponse
	mustDecode(t, res, &out)
	if !out.Handled || out.AuthID != "b" {
		t.Fatalf("pick = %+v, want b (most credits)", out)
	}
}

// TestSchedulerPickRoundRobinDelegates checks that the round_robin strategy
// hands the decision to CPA's own scheduler rather than running a private
// cursor: the host's version accounts for priorities and quota state the plugin
// cannot see, and it survives plugin reloads.
func TestSchedulerPickRoundRobinDelegates(t *testing.T) {
	resetState()
	applyRoutingConfig(routingSettings{Strategy: strategyRoundRobin})

	payload, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: workBuddyProviderKey,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "a", Provider: workBuddyProviderKey, Status: "active"},
			{ID: "b", Provider: workBuddyProviderKey, Status: "active"},
		},
	})

	res := callOK(t, pluginabi.MethodSchedulerPick, json.RawMessage(payload))
	var out pluginapi.SchedulerPickResponse
	mustDecode(t, res, &out)
	if !out.Handled {
		t.Fatal("should be handled")
	}
	if out.DelegateBuiltin != pluginapi.SchedulerBuiltinRoundRobin {
		t.Fatalf("delegate = %q, want %q", out.DelegateBuiltin, pluginapi.SchedulerBuiltinRoundRobin)
	}
	if out.AuthID != "" {
		t.Fatalf("a delegated pick must not also name an auth, got %q", out.AuthID)
	}
}

// TestInternalRoundRobinStillWorks covers the private cursor helper, which
// remains available even though the strategy now delegates.
func TestInternalRoundRobinStillWorks(t *testing.T) {
	s := newSchedulerState()
	cands := []schedulerCandidate{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		seen[s.pickRoundRobin("codebuddy", cands)]++
	}
	for _, id := range []string{"a", "b", "c"} {
		if seen[id] != 2 {
			t.Fatalf("distribution = %v, want 2 each", seen)
		}
	}
}

func TestSchedulerPickRandomStaysInSet(t *testing.T) {
	resetState()
	applyRoutingConfig(routingSettings{Strategy: strategyRandom})

	payload, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: workBuddyProviderKey,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "a", Provider: workBuddyProviderKey, Status: "active"},
			{ID: "b", Provider: workBuddyProviderKey, Status: "active"},
		},
	})
	for i := 0; i < 20; i++ {
		res := callOK(t, pluginabi.MethodSchedulerPick, json.RawMessage(payload))
		var out pluginapi.SchedulerPickResponse
		mustDecode(t, res, &out)
		if out.AuthID != "a" && out.AuthID != "b" {
			t.Fatalf("random pick returned %q", out.AuthID)
		}
	}
}

func TestSchedulerPickNoCandidatesDefers(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{
		Provider:   workBuddyProviderKey,
		Candidates: nil,
	})
	var out pluginapi.SchedulerPickResponse
	mustDecode(t, res, &out)
	if out.Handled {
		t.Fatal("with no candidates the host should decide")
	}
}

func TestSchedulerPickHandlesEmptyProviderList(t *testing.T) {
	resetState()
	// No provider info, but the candidates are ours: still handle it.
	payload, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "a", Provider: workBuddyProviderKey, Status: "active"},
		},
	})
	res := callOK(t, pluginabi.MethodSchedulerPick, json.RawMessage(payload))
	var out pluginapi.SchedulerPickResponse
	mustDecode(t, res, &out)
	if !out.Handled || out.AuthID != "a" {
		t.Fatalf("pick = %+v", out)
	}
}

func TestSchedulerRecordsPicks(t *testing.T) {
	resetState()
	payload, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider:   workBuddyProviderKey,
		Candidates: []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: workBuddyProviderKey, Status: "active"}},
	})
	for i := 0; i < 3; i++ {
		callOK(t, pluginabi.MethodSchedulerPick, json.RawMessage(payload))
	}
	counts := state.scheduler.pickCounts()
	if counts["a"] != 3 {
		t.Fatalf("pick counts = %v, want a=3", counts)
	}
}

// ---- settings ----------------------------------------------------------

func TestRoutingDefaultsToByCredits(t *testing.T) {
	store := newSettingsStore()
	if errDecode := store.decodeLifecycleConfig([]byte("enabled: true\n")); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if got := store.get().Routing.Strategy; got != strategyByCredits {
		t.Fatalf("default strategy = %v, want by_credits", got)
	}
}

func TestRoutingExplicitFromYAML(t *testing.T) {
	store := newSettingsStore()
	if errDecode := store.decodeLifecycleConfig([]byte("routing:\n  strategy: round_robin\n")); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if got := store.get().Routing.Strategy; got != strategyRoundRobin {
		t.Fatalf("strategy = %v, want round_robin", got)
	}
}

// ---- management endpoints ----------------------------------------------

func TestRoutingStatusEndpoint(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-u-1.json", "provider": workBuddyProviderKey, "label": "A",
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-1"})},
	})

	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   managementBasePath() + "/" + pluginName + "/routing/status",
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", mr.StatusCode)
	}
	var doc struct {
		Routing struct {
			Strategy string           `json:"strategy"`
			Options  []map[string]any `json:"options"`
			Order    []map[string]any `json:"order"`
		} `json:"routing"`
		Scheduler map[string]any `json:"scheduler"`
	}
	if errUnmarshal := json.Unmarshal(mr.Body, &doc); errUnmarshal != nil {
		t.Fatalf("bad json: %v", errUnmarshal)
	}
	if doc.Routing.Strategy != string(strategyByCredits) {
		t.Errorf("strategy = %q", doc.Routing.Strategy)
	}
	if len(doc.Routing.Options) != len(allSchedulerStrategies) {
		t.Errorf("expected %d strategy options, got %d", len(allSchedulerStrategies), len(doc.Routing.Options))
	}
	if len(doc.Routing.Order) != 1 {
		t.Errorf("expected 1 account in the order preview, got %+v", doc.Routing.Order)
	}
}

func TestRoutingConfigEndpoint(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementBasePath() + "/" + pluginName + "/routing/config",
		Body:   []byte(`{"strategy":"random"}`),
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (%s)", mr.StatusCode, mr.Body)
	}
	if got := state.settings.get().Routing.Strategy; got != strategyRandom {
		t.Fatalf("strategy = %v, want random", got)
	}
}

func TestRoutingConfigAcceptsNestedAndChinese(t *testing.T) {
	resetState()
	callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementBasePath() + "/" + pluginName + "/routing/config",
		Body:   []byte(`{"routing":{"strategy":"轮巡"}}`),
	})
	if got := state.settings.get().Routing.Strategy; got != strategyRoundRobin {
		t.Fatalf("strategy = %v, want round_robin", got)
	}
}

func TestRoutingResetEndpoint(t *testing.T) {
	resetState()
	// Advance the cursor.
	state.scheduler.pickRoundRobin(workBuddyProviderKey, []schedulerCandidate{{ID: "a"}, {ID: "b"}})
	if nextRotationHint() == "尚未开始轮巡" {
		t.Fatal("precondition: rotation should have advanced")
	}

	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementBasePath() + "/" + pluginName + "/routing/reset",
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", mr.StatusCode)
	}
	if nextRotationHint() != "尚未开始轮巡" {
		t.Fatalf("rotation should be reset, got %q", nextRotationHint())
	}
}

func TestMainPageShowsStrategySection(t *testing.T) {
	resetState()
	installAuthList(t, nil)
	page := renderMainPage()
	for _, want := range []string{"账号切换策略", "按额度", "轮巡", "随机", "应用策略", "重置轮巡位置"} {
		if !strings.Contains(page, want) {
			t.Errorf("combined page missing %q", want)
		}
	}
	// The strategy view is a tab on the combined page, not a separate screen.
	if !strings.Contains(page, `data-tab="tab-switch"`) {
		t.Error("expected a tab for the switching strategy")
	}
}
