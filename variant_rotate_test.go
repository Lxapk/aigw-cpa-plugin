package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// ---- variant (variant.rs) ----------------------------------------------

func TestVariantForDomain(t *testing.T) {
	cases := map[string]wbVariant{
		"":                  variantCn, // default
		"cn":                variantCn,
		"workbuddy.cn":      variantCn,
		"www.codebuddy.cn":  variantCn,
		"workbuddy.ai":      variantAi,
		"www.workbuddy.ai":  variantAi,
		"WWW.WorkBuddy.AI":  variantAi,
		"api.workbuddy.ai":  variantAi,
		"workbuddy.ai.evil": variantCn, // suffix must be the real domain
		"notworkbuddy.ai":   variantCn,
	}
	for in, want := range cases {
		if got := variantForDomain(in); got != want {
			t.Errorf("variantForDomain(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestVariantApiBaseCorrectsInternationalDomain is the fix for a real mistake:
// the international API base is workbuddy.ai while the international *product*
// domain is codebuddy.ai. Conflating them breaks the Origin header.
func TestVariantApiBaseCorrectsInternationalDomain(t *testing.T) {
	origGlobal := workBuddyGlobalBase()
	setWorkBuddyGlobalBase("https://www.workbuddy.ai")
	defer setWorkBuddyGlobalBase(origGlobal)

	restoreCn := redirectAllCnBases("https://www.codebuddy.cn")
	defer restoreCn()

	if got := variantAi.apiBase(); got != "https://www.workbuddy.ai" {
		t.Errorf("ai apiBase = %q, want https://www.workbuddy.ai", got)
	}
	if got := variantCn.apiBase(); got != "https://www.codebuddy.cn" {
		t.Errorf("cn apiBase = %q", got)
	}
}

func TestVariantProductDomain(t *testing.T) {
	// The international product domain is codebuddy.**ai**, not workbuddy.ai.
	if got := variantAi.productDomain(); got != "https://www.codebuddy.ai" {
		t.Errorf("ai productDomain = %q, want https://www.codebuddy.ai", got)
	}
	if got := variantCn.productDomain(); got != "https://www.codebuddy.cn" {
		t.Errorf("cn productDomain = %q", got)
	}
}

func TestVariantOAuthPlatform(t *testing.T) {
	if got := variantCn.oauthPlatform(); got != "workbuddy" {
		t.Errorf("cn platform = %q, want workbuddy", got)
	}
	if got := variantAi.oauthPlatform(); got != "workbuddy-ai" {
		t.Errorf("ai platform = %q, want workbuddy-ai", got)
	}
}

// TestBillingPathsDropsV2ForInternational is the key path fix: the
// international service serves /billing/meter/... while the domestic one
// serves /v2/billing/meter/...
func TestBillingPathsDropsV2ForInternational(t *testing.T) {
	const path = "/v2/billing/meter/get-user-resource-summary"

	cn := variantCn.billingPaths(path)
	if len(cn) != 1 || cn[0] != path {
		t.Fatalf("cn paths = %v, want the path unchanged", cn)
	}

	ai := variantAi.billingPaths(path)
	if len(ai) != 2 {
		t.Fatalf("ai paths = %v, want [primary fallback]", ai)
	}
	if ai[0] != "/billing/meter/get-user-resource-summary" {
		t.Errorf("ai primary = %q, want the /v2-less form", ai[0])
	}
	if ai[1] != path {
		t.Errorf("ai fallback = %q, want the original", ai[1])
	}
}

func TestBillingPathsNonMeterUnchanged(t *testing.T) {
	// A path that is not under the billing prefix is passed through as-is.
	ai := variantAi.billingPaths("/v2/plugin/auth/state")
	if len(ai) != 1 || ai[0] != "/v2/plugin/auth/state" {
		t.Fatalf("ai paths = %v", ai)
	}
}

func TestProductDomainForMapsWorkBuddyDomains(t *testing.T) {
	cases := []struct {
		in   string
		v    wbVariant
		want string
	}{
		{"www.workbuddy.cn", variantCn, "https://www.codebuddy.cn"},
		{"workbuddy.cn", variantCn, "https://www.codebuddy.cn"},
		{"www.workbuddy.ai", variantAi, "https://www.codebuddy.ai"},
		{"workbuddy.ai", variantAi, "https://www.codebuddy.ai"},
		// Empty falls back to the variant default.
		{"", variantCn, "https://www.codebuddy.cn"},
		{"", variantAi, "https://www.codebuddy.ai"},
		// Unknown domains pass through untouched.
		{"enterprise.example.com", variantCn, "enterprise.example.com"},
		{"www.codebuddy.cn", variantCn, "www.codebuddy.cn"},
	}
	for _, c := range cases {
		if got := productDomainFor(c.in, c.v); got != c.want {
			t.Errorf("productDomainFor(%q, %v) = %q, want %q", c.in, c.v, got, c.want)
		}
	}
}

// TestBillingFallbackOnlyOn404 verifies that a non-404 does not advance the
// candidate list (reference: "只有 HTTP 404 才允许换下一个候选").
func TestBillingFallbackOnlyOn404(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized) // 401, not 404
		_, _ = w.Write([]byte(`{"code":401}`))
	}))
	defer server.Close()

	origGlobal := workBuddyGlobalBase()
	setWorkBuddyGlobalBase(server.URL)
	defer setWorkBuddyGlobalBase(origGlobal)

	creds := &workBuddyCredentials{AccessToken: "tok", Domain: "www.workbuddy.ai"}
	_, _ = workBuddyUpstream.postBillingWithFallback(testContext(), creds, variantAi,
		"/v2/billing/meter/get-user-resource-summary", map[string]any{})

	if len(paths) != 1 {
		t.Fatalf("only a 404 may advance the candidate list; got %v", paths)
	}
}

// TestBillingFallbackAdvancesOn404 is the positive case.
func TestBillingFallbackAdvancesOn404(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if len(paths) == 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	origGlobal := workBuddyGlobalBase()
	setWorkBuddyGlobalBase(server.URL)
	defer setWorkBuddyGlobalBase(origGlobal)

	creds := &workBuddyCredentials{AccessToken: "tok", Domain: "www.workbuddy.ai"}
	body, errPost := workBuddyUpstream.postBillingWithFallback(testContext(), creds, variantAi,
		"/v2/billing/meter/get-user-resource-summary", map[string]any{})
	if errPost != nil {
		t.Fatalf("unexpected error: %v", errPost)
	}
	if len(paths) != 2 {
		t.Fatalf("expected the fallback to be attempted; got %v", paths)
	}
	if paths[0] != "/billing/meter/get-user-resource-summary" {
		t.Errorf("first candidate = %q", paths[0])
	}
	if paths[1] != "/v2/billing/meter/get-user-resource-summary" {
		t.Errorf("fallback candidate = %q", paths[1])
	}
	if !strings.Contains(string(body), `"code":0`) {
		t.Errorf("body = %s", body)
	}
}

// ---- credit expiry resolution (credits.rs) -----------------------------

func TestResolveExpireAtPrefersDeductionEnd(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// DeductionEndTime ~30 days out, CycleEndTime far beyond: deduction wins.
	deduction := now.Add(30 * 24 * time.Hour).UnixMilli()
	cycle := now.Add(300 * 24 * time.Hour).UnixMilli()

	got := resolveExpireAt(map[string]any{
		"DeductionEndTime": float64(deduction),
		"CycleEndTime":     float64(cycle),
	}, now)
	if got != deduction/1000 {
		t.Fatalf("expireAt = %d, want %d", got, deduction/1000)
	}
}

func TestResolveExpireAtCycleOverridesPlaceholder(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// DeductionEndTime is a 2049-style placeholder (>365d later); the real
	// expiry is CycleEndTime.
	placeholder := now.Add(20 * 365 * 24 * time.Hour).UnixMilli()
	cycle := now.Add(10 * 24 * time.Hour).UnixMilli()

	got := resolveExpireAt(map[string]any{
		"DeductionEndTime": float64(placeholder),
		"CycleEndTime":     float64(cycle),
	}, now)
	if got != cycle/1000 {
		t.Fatalf("expireAt = %d, want the cycle end %d", got, cycle/1000)
	}
}

func TestResolveExpireAtFarFutureBecomesNoExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// Beyond 730 days -> treated as no expiry.
	wayOut := now.Add(1000 * 24 * time.Hour).UnixMilli()
	if got := resolveExpireAt(map[string]any{"DeductionEndTime": float64(wayOut)}, now); got != 0 {
		t.Fatalf("expireAt = %d, want 0 (no expiry)", got)
	}
}

func TestResolveExpireAtAcceptsSecondsAndStrings(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	want := now.Add(10 * 24 * time.Hour)

	if got := resolveExpireAt(map[string]any{"ExpiredTime": float64(want.Unix())}, now); got != want.Unix() {
		t.Errorf("seconds form = %d, want %d", got, want.Unix())
	}
	if got := resolveExpireAt(map[string]any{"expiredTime": want.Format(time.RFC3339)}, now); got != want.Unix() {
		t.Errorf("RFC3339 form = %d, want %d", got, want.Unix())
	}
}

func TestResourceSummaryComputesExpiryFlags(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	// Expiring in 3 days -> expiringSoon.
	soon := resourceSummaryFromRaw(map[string]any{
		"PackageCode":      "p1",
		"TotalAmount":      float64(100),
		"RemainingAmount":  float64(50),
		"DeductionEndTime": float64(now.Add(3 * 24 * time.Hour).UnixMilli()),
	}, now)
	if !soon.ExpiringSoon || soon.Expired {
		t.Fatalf("resource = %+v, want expiringSoon", soon)
	}
	if soon.Remaining != 50 || soon.Used != 50 {
		t.Fatalf("amounts = %+v", soon)
	}

	// Already past -> expired.
	past := resourceSummaryFromRaw(map[string]any{
		"TotalAmount":      float64(10),
		"RemainingAmount":  float64(4),
		"DeductionEndTime": float64(now.Add(-1 * time.Hour).UnixMilli()),
	}, now)
	if !past.Expired {
		t.Fatalf("resource = %+v, want expired", past)
	}

	// Far future -> no expiry at all.
	far := resourceSummaryFromRaw(map[string]any{
		"TotalAmount":      float64(10),
		"RemainingAmount":  float64(10),
		"DeductionEndTime": float64(now.Add(3000 * 24 * time.Hour).UnixMilli()),
	}, now)
	if far.ExpireAt != 0 || far.Expired || far.ExpiringSoon {
		t.Fatalf("resource = %+v, want no expiry", far)
	}
}

func TestAggregateResourcesSumsAndDedupes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	resources := []creditResource{
		{PackageCode: "p1", Total: 100, Remaining: 50, Used: 50,
			ExpireAt: now.Add(2 * 24 * time.Hour).Unix()},
		{PackageCode: "p2", Total: 200, Remaining: 150, Used: 50,
			ExpireAt: now.Add(30 * 24 * time.Hour).Unix()},
		// Exact duplicate of p1 -> collapsed.
		{PackageCode: "p1", Total: 100, Remaining: 50, Used: 50,
			ExpireAt: now.Add(2 * 24 * time.Hour).Unix()},
	}
	sum := aggregateResources(resources, now)

	if len(sum.Resources) != 2 {
		t.Fatalf("resources = %d, want 2 after dedupe", len(sum.Resources))
	}
	if sum.Remaining != 200 {
		t.Fatalf("remaining = %v, want 200", sum.Remaining)
	}
	if sum.SoonestExpireAt != now.Add(2*24*time.Hour).Unix() {
		t.Fatalf("soonest = %d", sum.SoonestExpireAt)
	}
	if !sum.ExpiringSoon {
		t.Error("should be expiringSoon (2 days)")
	}
	if !sum.usable() {
		t.Error("a fresh non-expired balance should be usable")
	}
}

func TestAggregateResourcesMarksExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	sum := aggregateResources([]creditResource{
		{Total: 10, Remaining: 3, Expired: true,
			ExpireAt: now.Add(-1 * time.Hour).Unix()},
		{Total: 20, Remaining: 20,
			ExpireAt: now.Add(30 * 24 * time.Hour).Unix()},
	}, now)

	if !sum.Expired {
		t.Error("any expired resource should mark the summary expired")
	}
	if sum.ExpiredRemaining != 3 {
		t.Fatalf("expiredRemaining = %v, want 3", sum.ExpiredRemaining)
	}
	if sum.usable() {
		t.Error("an expired summary must not qualify as a rotation target")
	}
}

func TestCollectResourcesWalksNestedShapes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"data":{"Data":{"Packages":[
		{"PackageCode":"a","Resources":[{"TotalAmount":10,"RemainingAmount":7}]},
		{"PackageCode":"b","SlicePeriodUsageDetails":{"TotalAmount":5,"RemainingAmount":2,
		 "DeductionEndTime":1893456000000}}
	]}}}`)
	got := collectResources(body, now)
	if len(got) != 2 {
		t.Fatalf("collected %d resources, want 2: %+v", len(got), got)
	}
	// Both entries must be present with their amounts; the nested slice figures
	// are picked up from SlicePeriodUsageDetails.
	var sawTen, sawFive bool
	for _, r := range got {
		if r.Total == 10 && r.Remaining == 7 {
			sawTen = true
		}
		if r.Total == 5 && r.Remaining == 2 {
			sawFive = true
		}
	}
	if !sawTen {
		t.Errorf("top-level resource not parsed: %+v", got)
	}
	if !sawFive {
		t.Errorf("nested SlicePeriodUsageDetails not parsed: %+v", got)
	}
}

func TestCollectResourcesIgnoresUnrelatedObjects(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"code":0,"data":{"count":3,"items":[{"foo":"bar"}]}}`)
	if got := collectResources(body, now); len(got) != 0 {
		t.Fatalf("collected %+v, want nothing", got)
	}
}

// TestQuotaSettingsExplicitGates verifies the rotation gates are configurable.
func TestQuotaSettingsExplicitGates(t *testing.T) {
	store := newSettingsStore()
	yamlDoc := "routing:\n  strategy: by_expiry\n  cooldown_seconds: 60\n  min_gap_hours: 1\n  min_urgency_hours: 2\n  min_remaining: 50\n"
	if errDecode := store.decodeLifecycleConfig([]byte(yamlDoc)); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	r := store.get().Routing
	if r.Strategy != strategyByExpiry || r.CooldownSeconds != 60 ||
		r.MinGapHours != 1 || r.MinUrgencyHours != 2 || r.MinRemaining != 50 {
		t.Fatalf("routing = %+v", r)
	}
}

// ---- panel display ------------------------------------------------------

// TestPanelShowsCreditExpiry checks the account table surfaces the expiry so
// "which account should I spend first" is answerable at a glance.
func TestPanelShowsCreditExpiry(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-soon.json", "provider": workBuddyProviderKey, "label": "Soon",
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-soon", "domain": "cn"})},
		{"auth_index": "codebuddy-later.json", "provider": workBuddyProviderKey, "label": "Later",
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-later", "domain": "cn"})},
	})

	now := time.Now()
	state.quota.mu.Lock()
	state.quota.byAuth["codebuddy-soon.json"] = &workBuddyQuota{
		Known: true, Credits: 10,
		Summary: creditSummary{Known: true, SoonestExpireAt: now.Add(3 * 24 * time.Hour).Unix(), ExpiringSoon: true},
	}
	state.quota.byAuth["codebuddy-later.json"] = &workBuddyQuota{
		Known: true, Credits: 900,
		Summary: creditSummary{Known: true, SoonestExpireAt: now.Add(30 * 24 * time.Hour).Unix()},
	}
	state.quota.mu.Unlock()

	accounts := listWorkBuddyAccounts()
	var soon, later *workBuddyAccount
	for i := range accounts {
		switch accounts[i].Label {
		case "Soon":
			soon = &accounts[i]
		case "Later":
			later = &accounts[i]
		}
	}
	if soon == nil || later == nil {
		t.Fatalf("accounts = %+v", accounts)
	}
	if !soon.CreditsExpiringSoon {
		t.Errorf("Soon should be expiringSoon: %+v", soon)
	}
	if soon.CreditsExpireDays < 2 || soon.CreditsExpireDays > 3 {
		t.Errorf("Soon expireDays = %d, want ~3", soon.CreditsExpireDays)
	}
	if later.CreditsExpiringSoon {
		t.Errorf("Later should not be expiringSoon: %+v", later)
	}
	if soon.Variant != "cn" {
		t.Errorf("variant = %q, want cn", soon.Variant)
	}

	page := renderMainPage()
	for _, want := range []string{"积分", "到期", "天后", "版本"} {
		if !strings.Contains(page, want) {
			t.Errorf("panel missing %q", want)
		}
	}
}

// TestPanelMarksExpiredCredits checks an expired balance is flagged.
func TestPanelMarksExpiredCredits(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-x.json", "provider": workBuddyProviderKey, "label": "X",
			"storage_json": mustStorage(t, map[string]any{"accessToken": "at", "uid": "u-x", "domain": "cn"})},
	})
	state.quota.mu.Lock()
	state.quota.byAuth["codebuddy-x.json"] = &workBuddyQuota{
		Known: true, Credits: 5,
		Summary: creditSummary{Known: true, Expired: true,
			SoonestExpireAt: time.Now().Add(-time.Hour).Unix()},
	}
	state.quota.mu.Unlock()

	accounts := listWorkBuddyAccounts()
	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v", accounts)
	}
	if !accounts[0].CreditsExpired {
		t.Error("expired credits should be flagged")
	}
	if accounts[0].Usable {
		t.Error("an account with only expired credits must not be usable")
	}
	if !strings.Contains(renderMainPage(), "已过期") {
		t.Error("panel should show the expired marker")
	}
}

// TestRoutingPreviewSortedByExpiry checks the by_expiry preview order.
func TestRoutingPreviewSortedByExpiry(t *testing.T) {
	resetState()
	now := time.Now()
	accounts := []workBuddyAccount{
		{Label: "Far", Usable: true, AuthIndex: "far", Credits: 900,
			CreditsExpireAt: now.Add(30 * 24 * time.Hour).Unix()},
		{Label: "Soon", Usable: true, AuthIndex: "soon", Credits: 10,
			CreditsExpireAt: now.Add(2 * 24 * time.Hour).Unix()},
		{Label: "Never", Usable: true, AuthIndex: "never", Credits: 500},
	}
	order := strategyPreview(strategyByExpiry, accounts)
	got := labelsOf(order)
	want := []string{"Soon", "Far", "Never"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("by_expiry preview order = %v, want %v", got, want)
		}
	}
}

// TestPanelShowsVariantDistinguishesBuilds keeps cn and ai accounts visually
// separable.
func TestPanelShowsVariantDistinguishesBuilds(t *testing.T) {
	resetState()
	installAuthList(t, []map[string]any{
		{"auth_index": "codebuddy-cn.json", "provider": workBuddyProviderKey, "label": "CN",
			"storage_json": mustStorage(t, map[string]any{"accessToken": "a", "uid": "u1", "domain": "cn"})},
		{"auth_index": "codebuddy-ai.json", "provider": workBuddyProviderKey, "label": "AI",
			"storage_json": mustStorage(t, map[string]any{"accessToken": "b", "uid": "u2", "domain": "www.workbuddy.ai"})},
	})
	accounts := listWorkBuddyAccounts()
	variants := map[string]string{}
	for _, a := range accounts {
		variants[a.Label] = a.Variant
	}
	if variants["CN"] != "cn" {
		t.Errorf("CN variant = %q", variants["CN"])
	}
	if variants["AI"] != "ai" {
		t.Errorf("AI variant = %q", variants["AI"])
	}
}

func cand(id string, expireIn time.Duration, now time.Time, remaining float64) rotateCandidate {
	c := rotateCandidate{AccountID: id, DisplayName: id, TotalRemaining: remaining, Valid: true}
	if expireIn > 0 {
		c.SoonestExpireAt = now.Add(expireIn).Unix()
	}
	return c
}

func TestDecideRotatePicksSoonestExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	candidates := []rotateCandidate{
		cand("far", 30*24*time.Hour, now, 100),
		cand("soon", 2*24*time.Hour, now, 10),
		cand("middle", 10*24*time.Hour, now, 50),
	}
	d := decideRotate(candidates, rotateParams{Now: now})
	if d.Kind != rotateSwitch || d.TargetID != "soon" {
		t.Fatalf("decision = %+v, want switch to soon", d)
	}
}

func TestDecideRotateNoExpirySortsLast(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	candidates := []rotateCandidate{
		{AccountID: "never", Valid: true, TotalRemaining: 999}, // no expiry
		cand("expiring", 5*24*time.Hour, now, 1),
	}
	d := decideRotate(candidates, rotateParams{Now: now})
	if d.TargetID != "expiring" {
		t.Fatalf("decision = %+v, want the expiring account over the never-expiring one", d)
	}
}

func TestDecideRotateSkipsWhenAllFarOut(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := decideRotate(
		[]rotateCandidate{cand("a", 30*24*time.Hour, now, 100)},
		rotateParams{Now: now, MinUrgency: 24 * time.Hour},
	)
	if d.Kind != rotateSkip || !strings.Contains(d.Reason, "都还早") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDecideRotateSkipsWhenTargetIsCurrent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := decideRotate(
		[]rotateCandidate{cand("a", 2*24*time.Hour, now, 100)},
		rotateParams{Now: now, CurrentAccountID: "a"},
	)
	if d.Kind != rotateSkip || !strings.Contains(d.Reason, "已是最紧迫") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDecideRotateSkipsDuringCooldown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := decideRotate(
		[]rotateCandidate{cand("a", 2*24*time.Hour, now, 100)},
		rotateParams{
			Now:              now,
			CurrentAccountID: "b",
			LastSwitchAt:     now.Add(-1 * time.Minute),
			Cooldown:         10 * time.Minute,
		},
	)
	if d.Kind != rotateSkip || !strings.Contains(d.Reason, "冷却") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDecideRotateSkipsOnLiveSession(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := decideRotate(
		[]rotateCandidate{cand("a", 2*24*time.Hour, now, 100)},
		rotateParams{Now: now, HasLiveSession: true},
	)
	if d.Kind != rotateSkip || !strings.Contains(d.Reason, "会话在运行") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDecideRotateSkipsOnMinRemaining(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := decideRotate(
		[]rotateCandidate{cand("a", 2*24*time.Hour, now, 5)},
		rotateParams{Now: now, MinRemaining: 100},
	)
	if d.Kind != rotateSkip || !strings.Contains(d.Reason, "剩余积分不足") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDecideRotateSkipsOnAntiFlap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	candidates := []rotateCandidate{
		cand("cur", 10*24*time.Hour, now, 100),
		cand("target", 10*24*time.Hour-2*time.Hour, now, 100), // only 2h sooner
	}
	d := decideRotate(candidates, rotateParams{
		Now:              now,
		CurrentAccountID: "cur",
		MinGap:           6 * time.Hour,
	})
	if d.Kind != rotateSkip || !strings.Contains(d.Reason, "未达切换阈值") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestDecideRotateSwitchesWhenGapIsEnough(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	candidates := []rotateCandidate{
		cand("cur", 10*24*time.Hour, now, 100),
		cand("target", 2*24*time.Hour, now, 100), // 8 days sooner
	}
	d := decideRotate(candidates, rotateParams{
		Now:              now,
		CurrentAccountID: "cur",
		MinGap:           6 * time.Hour,
	})
	if d.Kind != rotateSwitch || d.TargetID != "target" {
		t.Fatalf("decision = %+v, want switch to target", d)
	}
}

func TestDecideRotateSkipsWithoutValidCandidates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := decideRotate([]rotateCandidate{
		{AccountID: "bad", Valid: false},
	}, rotateParams{Now: now})
	if d.Kind != rotateSkip || !strings.Contains(d.Reason, "没有可用账号") {
		t.Fatalf("decision = %+v", d)
	}
}

// ---- by_expiry strategy integration ------------------------------------

func TestStrategyByExpiryWiring(t *testing.T) {
	resetState()
	applyRoutingConfig(routingSettings{
		Strategy:        strategyByExpiry,
		CooldownSeconds: 0,
		MinGapHours:     0,
		MinUrgencyHours: 0,
	})

	now := time.Now()
	state.quota.mu.Lock()
	state.quota.byAuth["far"] = &workBuddyQuota{
		Known:   true,
		Credits: 900,
		Summary: creditSummary{Known: true, SoonestExpireAt: now.Add(30 * 24 * time.Hour).Unix()},
	}
	state.quota.byAuth["soon"] = &workBuddyQuota{
		Known:   true,
		Credits: 10,
		Summary: creditSummary{Known: true, SoonestExpireAt: now.Add(1 * 24 * time.Hour).Unix()},
	}
	state.quota.mu.Unlock()

	payload, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: workBuddyProviderKey,
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "far", Provider: workBuddyProviderKey, Status: "active"},
			{ID: "soon", Provider: workBuddyProviderKey, Status: "active"},
		},
	})
	res := callOK(t, pluginabi.MethodSchedulerPick, json.RawMessage(payload))
	var out pluginapi.SchedulerPickResponse
	mustDecode(t, res, &out)
	if !out.Handled || out.AuthID != "soon" {
		t.Fatalf("pick = %+v, want soon (earliest expiry, not most credits)", out)
	}
}

func TestStrategyByExpiryRegistered(t *testing.T) {
	found := false
	for _, s := range allSchedulerStrategies {
		if s == strategyByExpiry {
			found = true
		}
	}
	if !found {
		t.Fatal("by_expiry must be listed as a strategy")
	}
	if got := normalizeStrategy("按到期"); got != strategyByExpiry {
		t.Errorf("normalizeStrategy(按到期) = %v", got)
	}
	if got := strategyByExpiry.label(); got != "按到期" {
		t.Errorf("label = %q", got)
	}
}

func TestRoutingSettingsGatesDefault(t *testing.T) {
	store := newSettingsStore()
	if errDecode := store.decodeLifecycleConfig([]byte("enabled: true\n")); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	r := store.get().Routing
	if r.CooldownSeconds != 300 || r.MinGapHours != 6 || r.MinUrgencyHours != 24 {
		t.Fatalf("gate defaults = %+v", r)
	}
}

func TestRoutingSettingsExplicitGates(t *testing.T) {
	store := newSettingsStore()
	yamlDoc := "routing:\n  strategy: by_expiry\n  cooldown_seconds: 60\n  min_gap_hours: 1\n  min_urgency_hours: 2\n  min_remaining: 50\n"
	if errDecode := store.decodeLifecycleConfig([]byte(yamlDoc)); errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	r := store.get().Routing
	if r.Strategy != strategyByExpiry || r.CooldownSeconds != 60 ||
		r.MinGapHours != 1 || r.MinUrgencyHours != 2 || r.MinRemaining != 50 {
		t.Fatalf("routing = %+v", r)
	}
}
