package main

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file implements CPA's QuotaProvider capability for WorkBuddy, plus the
// periodic quota refresh that feeds the account-pool ordering.
//
// Ported from AI 聚合网关 0.1.18:
//
//	a2/b.java:406   m()  quota query
//	N1/C0305z.java       fetch pipeline (engine -> provider.m(account))
//	N1/R0.java:134       UI "已知额度合计"
//	N1/C0272i.java:63    batch refresh toast
//
// In the source app the quota value is not merely informational: it is stored
// on the account entry as d.f3827b and used by the selection loop
// (A0/s.java:596) to prefer the account with the most remaining credits. The
// plugin keeps the same ordering internally so its own pool view and the
// management page agree with the app's behaviour.

// quotaState holds the latest quota per credential plus the refresh schedule.
type quotaState struct {
	mu sync.Mutex
	// byAuth maps auth id -> latest quota reading.
	byAuth map[string]*workBuddyQuota
	// lastRun is the most recent refresh pass.
	lastRun []quotaRefreshResult
	// lastRunAt / running guard concurrent passes.
	lastRunAt time.Time
	running   bool
	// lastAutoHour marks the hour of the last automatic refresh so the
	// interval is honoured without a ticker per credential.
	lastAutoAt time.Time
	stopCh     chan struct{}
	started    bool
}

// quotaRefreshResult is one credential's refresh outcome, shown on the page.
type quotaRefreshResult struct {
	AuthID    string    `json:"auth_id"`
	Label     string    `json:"label"`
	UID       string    `json:"uid"`
	Domain    string    `json:"domain"`
	Region    string    `json:"region"`
	Credits   int64     `json:"credits"`
	Known     bool      `json:"known"`
	Message   string    `json:"message"`
	Error     string    `json:"error,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
}

const quotaHistoryMax = 20

func newQuotaState() *quotaState {
	return &quotaState{byAuth: make(map[string]*workBuddyQuota), stopCh: make(chan struct{})}
}

// ---- region labelling (a2/b.java:284) ------------------------------------

// workBuddyRegion ports D()'s return value for display purposes.
func workBuddyRegion(domain string) string {
	if isWorkBuddyGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// ---- fetching ------------------------------------------------------------

// fetchQuotaForAccounts queries quota for every WorkBuddy credential.
//
// It reuses the same account enumeration as the check-in pass so both features
// always agree on which credentials exist.
func fetchQuotaForAccounts() ([]quotaRefreshResult, error) {
	accounts, errCollect := collectCheckinAccounts()
	if errCollect != nil {
		return nil, errCollect
	}

	results := make([]quotaRefreshResult, 0, len(accounts))
	for _, account := range accounts {
		results = append(results, fetchQuotaOne(account))
	}
	return results, nil
}

// fetchQuotaOne queries a single credential and records the result.
func fetchQuotaOne(account checkinAccount) quotaRefreshResult {
	res := quotaRefreshResult{
		AuthID:    account.AuthID,
		Label:     account.Label,
		UID:       account.Creds.UID,
		Domain:    account.Creds.Domain,
		Region:    workBuddyRegion(account.Creds.Domain),
		FetchedAt: time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	quota, errQuota := workBuddyUpstream.fetchQuota(ctx, account.Creds)
	if errQuota != nil {
		res.Error = errQuota.Error()
		res.Message = "查询额度失败"
		return res
	}

	res.Credits = quota.Credits
	res.Known = quota.Known
	res.Message = quota.Message
	if quota.Err != "" {
		res.Error = quota.Err
	}

	// Feeding the pool keeps the selection ordering aligned with the app:
	// A0/s.java:596 picks the account with the greatest credits.
	state.pool.setCreditsByAuthID(account.AuthID, account.Creds.UID, quota.Credits, quota.Known)

	state.quota.mu.Lock()
	state.quota.byAuth[account.AuthID] = quota
	state.quota.mu.Unlock()

	return res
}

// runQuotaRefresh performs one pass over all credentials.
func runQuotaRefresh(trigger string) ([]quotaRefreshResult, error) {
	state.quota.mu.Lock()
	if state.quota.running {
		state.quota.mu.Unlock()
		return nil, errQuotaBusy
	}
	state.quota.running = true
	state.quota.mu.Unlock()

	defer func() {
		state.quota.mu.Lock()
		state.quota.running = false
		state.quota.lastRunAt = time.Now()
		state.quota.mu.Unlock()
	}()

	results, errFetch := fetchQuotaForAccounts()
	if errFetch != nil {
		return nil, errFetch
	}

	// Newest-first presentation, richest accounts first — matching how the app
	// surfaces "已知额度合计".
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Credits > results[j].Credits
	})

	state.quota.mu.Lock()
	state.quota.lastRun = results
	if trigger == "auto" {
		state.quota.lastAutoAt = time.Now()
	}
	state.quota.mu.Unlock()

	return results, nil
}

// quotaSummary aggregates the latest readings the way N1/R0.java:134 does.
func quotaSummary() (total int64, known int, accounts int) {
	state.quota.mu.Lock()
	defer state.quota.mu.Unlock()
	for _, q := range state.quota.byAuth {
		accounts++
		if q != nil && q.Known {
			known++
			if q.Credits > 0 {
				total += q.Credits
			}
		}
	}
	return total, known, accounts
}

// ---- scheduling ----------------------------------------------------------

// startQuotaScheduler launches the periodic refresh exactly once.
func startQuotaScheduler() {
	state.quota.mu.Lock()
	if state.quota.started {
		state.quota.mu.Unlock()
		return
	}
	state.quota.started = true
	state.quota.mu.Unlock()

	go quotaLoop()
}

// quotaLoop refreshes quota on the configured interval.
func quotaLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// Startup pass so the page has data without waiting a whole interval.
	cfg := state.settings.get().Quota
	if cfg.Enabled && cfg.RefreshOnStart {
		_, _ = runQuotaRefresh("startup")
	}

	for {
		select {
		case <-state.quota.stopCh:
			return
		case <-ticker.C:
			cfg := state.settings.get().Quota
			if !cfg.Enabled {
				continue
			}
			state.quota.mu.Lock()
			last := state.quota.lastAutoAt
			busy := state.quota.running
			state.quota.mu.Unlock()
			if busy {
				continue
			}
			interval := time.Duration(clampIntervalMinutes(cfg.IntervalMinutes)) * time.Minute
			if !last.IsZero() && time.Since(last) < interval {
				continue
			}
			_, _ = runQuotaRefresh("auto")
		}
	}
}

func stopQuotaScheduler() {
	state.quota.mu.Lock()
	started := state.quota.started
	state.quota.started = false
	state.quota.mu.Unlock()
	if !started {
		return
	}
	select {
	case state.quota.stopCh <- struct{}{}:
	default:
	}
}

// ---- QuotaProvider RPC ---------------------------------------------------

// quotaIdentifier answers quota.identifier.
func quotaIdentifier() ([]byte, error) {
	return okEnvelope(identifierResponse{Identifier: workBuddyProviderKey})
}

// quotaDescribe answers quota.describe.
func quotaDescribe() ([]byte, error) {
	return okEnvelope(pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{workBuddyProviderKey},
		DisplayName:        workBuddyDisplayName,
		// The provider exposes no reset endpoint.
		SupportsReset: false,
	})
}

// quotaFetch answers quota.fetch: return one credential's quota.
func quotaFetch(request []byte) ([]byte, error) {
	var req pluginapi.QuotaFetchRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	if req.Provider != "" && !isWorkBuddyProvider(req.Provider) {
		return okEnvelope(pluginapi.QuotaFetchResponse{})
	}

	creds, errParse := parseWorkBuddyCredentials(req.StorageJSON)
	if errParse != nil {
		return okEnvelope(pluginapi.QuotaFetchResponse{})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	quota, errQuota := workBuddyUpstream.fetchQuota(ctx, creds)
	if errQuota != nil || quota == nil {
		return okEnvelope(pluginapi.QuotaFetchResponse{})
	}

	// Record it so the pool ordering and the page stay in sync.
	authID := firstNonEmpty(req.AuthIndex, req.AuthID, creds.authID())
	state.pool.setCreditsByAuthID(authID, creds.UID, quota.Credits, quota.Known)
	state.quota.mu.Lock()
	state.quota.byAuth[authID] = quota
	state.quota.mu.Unlock()

	return okEnvelope(quotaToFetchResponse(quota, creds))
}

// quotaToFetchResponse normalises the provider reading into CPA's shape.
//
// The app only ever surfaces an absolute remaining-credit count
// ("周期剩余 N"), so it is exposed as a single bucket with the cycle window
// label. RemainingFraction is set to 1 when the provider reports credits so the
// host treats the credential as usable.
func quotaToFetchResponse(quota *workBuddyQuota, creds *workBuddyCredentials) pluginapi.QuotaFetchResponse {
	resp := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     workBuddyDisplayName,
			TierName: workBuddyRegion(creds.Domain),
		},
	}

	remaining := quota.Credits
	label := "周期剩余"
	bucket := pluginapi.QuotaBucket{
		Window:      "cycle",
		Description: label,
	}
	if quota.Known {
		bucket.RemainingFraction = 1
		if remaining <= 0 {
			bucket.RemainingFraction = 0
		}
	}

	resp.Groups = []pluginapi.QuotaGroup{{
		DisplayName: workBuddyDisplayName,
		Buckets:     []pluginapi.QuotaBucket{bucket},
	}}
	resp.Summary = []pluginapi.QuotaMetric{{
		Label: label,
		Value: float64(remaining),
		Unit:  "credits",
	}}
	return resp
}

// quotaReset answers quota.reset. The provider exposes no reset endpoint, so
// this reports that it is unsupported rather than mutating anything.
func quotaReset() ([]byte, error) {
	return okEnvelope(pluginapi.QuotaResetResponse{
		Success: false,
		Message: workBuddyDisplayName + " 不支持额度重置",
	})
}

type quotaBusyError struct{}

func (quotaBusyError) Error() string { return "已有额度刷新任务正在运行" }

var errQuotaBusy = quotaBusyError{}

// ---- settings ------------------------------------------------------------

// quotaSettings is the quota half of the plugin configuration.
type quotaSettings struct {
	// Enabled turns the periodic refresh on or off.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// IntervalMinutes is how often to refresh, clamped to [5, 1440].
	IntervalMinutes int `json:"interval_minutes" yaml:"interval_minutes"`
	// RefreshOnStart performs a pass when the plugin loads.
	RefreshOnStart bool `json:"refresh_on_start" yaml:"refresh_on_start"`
}

func defaultQuotaSettings() quotaSettings {
	return quotaSettings{
		Enabled:         false,
		IntervalMinutes: 30,
		RefreshOnStart:  false,
	}
}

// applyDefaults restores sane values when the YAML block is absent.
func (q *quotaSettings) applyDefaults() {
	d := defaultQuotaSettings()
	if q.IntervalMinutes <= 0 || q.IntervalMinutes > 1440 {
		q.IntervalMinutes = d.IntervalMinutes
	}
}

func clampIntervalMinutes(m int) int {
	if m < 5 {
		return 5
	}
	if m > 1440 {
		return 1440
	}
	return m
}

// applyQuotaConfig validates and stores the quota settings.
func applyQuotaConfig(cfg quotaSettings) quotaSettings {
	cfg.applyDefaults()
	state.settings.setQuota(cfg)
	return cfg
}

// quotaStatusJSON is the payload for the quota status endpoint.
func quotaStatusJSON() map[string]any {
	cfg := state.settings.get().Quota
	total, known, accounts := quotaSummary()

	state.quota.mu.Lock()
	lastRun := make([]quotaRefreshResult, len(state.quota.lastRun))
	copy(lastRun, state.quota.lastRun)
	lastRunAt := state.quota.lastRunAt
	running := state.quota.running
	state.quota.mu.Unlock()

	resp := map[string]any{
		"enabled":          cfg.Enabled,
		"interval_minutes": cfg.IntervalMinutes,
		"refresh_on_start": cfg.RefreshOnStart,
		"running":          running,
		"total_credits":    total,
		"accounts_known":   known,
		"accounts_total":   accounts,
		"last_run":         lastRun,
	}
	if !lastRunAt.IsZero() {
		resp["last_run_at"] = lastRunAt.Format(time.RFC3339)
	}
	if cfg.Enabled {
		resp["next_run"] = lastRunAt.Add(time.Duration(clampIntervalMinutes(cfg.IntervalMinutes)) * time.Minute).Format(time.RFC3339)
	}
	return resp
}

// totalKnownCredits is the number shown as "已知额度合计" (N1/R0.java:134).
//
// It sums the latest per-credential readings. When a credential has a pool
// entry but no quota record yet (e.g. credits were learned from a failed
// request), the pool value is used so the figure is never under-reported.
func totalKnownCredits() int64 {
	total, known, _ := quotaSummary()

	// Fold in pool readings that have no quota record.
	state.quota.mu.Lock()
	recorded := make(map[string]struct{}, len(state.quota.byAuth))
	for id := range state.quota.byAuth {
		recorded[id] = struct{}{}
	}
	state.quota.mu.Unlock()

	for _, lane := range state.pool.snapshot() {
		if !lane.CreditsKnown {
			continue
		}
		if _, seen := recorded[lane.UID]; seen {
			continue
		}
		if _, seen := recorded[workBuddyProviderKey+"/"+lane.UID]; seen {
			continue
		}
		_ = known
		if lane.Credits > 0 {
			total += lane.Credits
		}
	}
	return total
}

// poolOrderingHint renders the pool in selection order for the status page,
// mirroring A0/s.java:596 (highest credits first).
func poolOrderingHint() []map[string]any {
	lanes := state.pool.snapshot()
	sort.SliceStable(lanes, func(i, j int) bool {
		return lanes[i].Credits > lanes[j].Credits
	})
	out := make([]map[string]any, 0, len(lanes))
	for _, lane := range lanes {
		out = append(out, map[string]any{
			"uid":           lane.UID,
			"label":         firstNonEmpty(lane.Label, lane.UID),
			"credits":       lane.Credits,
			"credits_known": lane.CreditsKnown,
			"cool_kind":     coolKindName(lane.CoolKind),
			"usable":        lane.usable(time.Now()),
		})
	}
	return out
}

// coolKindName renders the W1.b enum the way the app persisted it.
func coolKindName(k coolKind) string {
	switch k {
	case coolKindQuota:
		return "QUOTA"
	case coolKindSoft:
		return "SOFT"
	case coolKindError:
		return "ERROR"
	}
	return ""
}
