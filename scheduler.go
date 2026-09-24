package main

import (
	"encoding/json"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file implements CPA's Scheduler capability, which lets the plugin decide
// which credential a request uses.
//
// Why this matters
//
// CPA normally picks the auth itself. The Scheduler capability hands the plugin
// the candidate list and lets it return one, which is what makes the account
// selection strategy configurable from the panel.
//
// Three strategies are offered:
//
//	by_credits  largest remaining quota first  — the source app's behaviour
//	              (A0/s.java:585 t() keeps the candidate with the greatest
//	               credits after filtering)
//	round_robin strictly rotating position      — predictable, spreads load
//	random      uniformly at random             — avoids hotspots entirely
//
// All three honour the same availability filter, mirroring A0/s.java:596:
//
//	not disabled, not in a cooldown window, status not failed
//
// and they never pick a credential the request has already tried.

// schedulerStrategy names the supported selection modes.
type schedulerStrategy string

const (
	// strategyByExpiry spends the soonest-expiring credits first, which is the
	// reference implementation's rotation policy.
	strategyByExpiry schedulerStrategy = "by_expiry"
	// strategyWeighted uses three-factor weighted random selection.
	strategyWeighted schedulerStrategy = "weighted"
	// strategyByCredits prefers the account with the most remaining quota.
	strategyByCredits schedulerStrategy = "by_credits"
	// strategyRoundRobin rotates through the candidates deterministically.
	strategyRoundRobin schedulerStrategy = "round_robin"
	// strategyRandom picks uniformly at random.
	strategyRandom schedulerStrategy = "random"
)

// allSchedulerStrategies lists the strategies in display order.
var allSchedulerStrategies = []schedulerStrategy{
	strategyWeighted,
	strategyByExpiry,
	strategyByCredits,
	strategyRoundRobin,
	strategyRandom,
}

// normalizeStrategy coerces user input, defaulting to by_credits (the app's
// behaviour) when unrecognised.
func normalizeStrategy(s string) schedulerStrategy {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(strategyRoundRobin), "round-robin", "roundrobin", "rr", "轮巡":
		return strategyRoundRobin
	case string(strategyRandom), "rand", "随机":
		return strategyRandom
	case string(strategyByCredits), "credits", "quota", "按额度", "额度":
		return strategyByCredits
	case "weighted", "三因子", "加权":
		return strategyWeighted
	case "by_expiry", "expiry", "expire", "soonest", "按到期", "到期", "紧迫":
		return strategyByExpiry
	}
	return strategyByCredits
}

func (s schedulerStrategy) label() string {
	switch s {
	case strategyRoundRobin:
		return "轮巡"
	case strategyRandom:
		return "随机"
	case strategyByExpiry:
		return "按到期"
	case strategyWeighted:
		return "三因子加权"
	default:
		return "按额度"
	}
}

// schedulerState holds the rotating cursor used by round_robin.
type schedulerState struct {
	mu sync.Mutex
	// cursor is consumed by round_robin. Keyed by provider so two providers do
	// not advance each other's position.
	cursor map[string]uint64
	// picks counts how many times each auth was chosen, for the panel.
	picks map[string]uint64
	// rng is shared by the random strategy.
	rng *rand.Rand
	// lastSwitch records the most recent by_expiry switch, for the cooldown
	// gate (rotate.rs: last_switch_at_ms).
	lastSwitch time.Time
	// lastPickedID is the account most recently handed to a request, used as
	// the "current account" in the rotation gate chain.
	lastPickedID string
}

func newSchedulerState() *schedulerState {
	return &schedulerState{
		cursor: make(map[string]uint64),
		picks:  make(map[string]uint64),
		// Seeded from the clock; the exact sequence does not matter, only that
		// it is not identical across restarts.
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// ---- candidate filtering ------------------------------------------------

// schedulerCandidate is one selectable auth, already filtered and decorated.
type schedulerCandidate struct {
	ID       string
	Credits  int64
	Known    bool
	Cooldown time.Time
	HasCool  bool
}

// collectCandidates ports A0/s.java:596's availability guard and folds in the
// quota reading so strategies can rank by it.
//
// A candidate is dropped when the host says it is unusable, when the pool has
// it in a cooldown window, or when it has already been tried for this request.
func (s *schedulerState) collectCandidates(req pluginapi.SchedulerPickRequest) []schedulerCandidate {
	tried := triedAuthSet(req.Options.Metadata)
	now := time.Now()

	open := make([]pluginapi.SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, c := range req.Candidates {
		if c.ID == "" {
			continue
		}
		if _, seen := tried[c.ID]; seen {
			continue
		}
		// Host-reported status: skip anything explicitly failed or disabled.
		if isUnusableSchedulerStatus(c.Status) {
			continue
		}
		// Host may also surface the disabled flag in metadata.
		if boolFromAny(c.Metadata["disabled"]) {
			continue
		}
		open = append(open, c)
	}
	if len(open) == 0 {
		return nil
	}

	out := make([]schedulerCandidate, 0, len(open))
	for _, c := range open {
		cand := schedulerCandidate{ID: c.ID}

		// Quota: prefer a recorded reading, else fall back to the pool lane.
		state.quota.mu.Lock()
		if q, ok := state.quota.byAuth[c.ID]; ok && q != nil && q.Known {
			cand.Credits = q.Credits
			cand.Known = true
		}
		state.quota.mu.Unlock()

		if !cand.Known {
			for _, lane := range state.pool.snapshot() {
				if lane.UID != c.ID && laneKey(lane.Provider, lane.UID) != c.ID {
					continue
				}
				// Respect a pool-level cooldown even if the host does not know
				// about it (the pool sees failures the host does not).
				if !lane.CooldownUntil.IsZero() && now.Before(lane.CooldownUntil) {
					cand.Cooldown = lane.CooldownUntil
					cand.HasCool = true
				}
				if lane.CreditsKnown {
					cand.Credits = lane.Credits
					cand.Known = true
				}
				break
			}
		}
		if cand.HasCool {
			continue
		}
		out = append(out, cand)
	}
	return out
}

// triedAuthSet reads the already-attempted auth ids from scheduler metadata.
//
// CPA passes request-scoped state through SchedulerOptions.Metadata; the key
// names below cover the shapes the host uses.
func triedAuthSet(meta map[string]any) map[string]struct{} {
	out := map[string]struct{}{}
	if meta == nil {
		return out
	}
	for _, key := range []string{"tried_auth_ids", "tried", "excluded_auth_ids", "tried_auths"} {
		raw, ok := meta[key]
		if !ok {
			continue
		}
		switch v := raw.(type) {
		case []string:
			for _, id := range v {
				out[id] = struct{}{}
			}
		case []any:
			for _, item := range v {
				if s, okString := item.(string); okString {
					out[s] = struct{}{}
				}
			}
		}
	}
	return out
}

// isUnusableSchedulerStatus drops candidates the host already considers bad.
func isUnusableSchedulerStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "active", "ready", "ok", "healthy", "available", "valid":
		return false
	case "disabled", "unavailable", "failed", "invalid", "error", "expired":
		return true
	}
	// Unknown statuses are kept; the executor will surface any real failure.
	return false
}

func boolFromAny(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

// ---- strategies ---------------------------------------------------------

// pickByCredits returns the candidate with the greatest remaining quota,
// mirroring A0/s.java:596's strict-greater comparison (ties keep host order).
func pickByCredits(candidates []schedulerCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if c.Credits > best.Credits {
			best = c
		}
	}
	return best.ID
}

// pickRoundRobin advances a per-provider cursor and returns that position.
//
// The candidate list is sorted first so the rotation is stable regardless of
// the order the host supplies.
func (s *schedulerState) pickRoundRobin(provider string, candidates []schedulerCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)

	s.mu.Lock()
	pos := s.cursor[provider]
	s.cursor[provider] = pos + 1
	s.mu.Unlock()

	return ids[int(pos%uint64(len(ids)))]
}

// pickRandom returns a uniformly random candidate.
func (s *schedulerState) pickRandom(candidates []schedulerCandidate) string {
	if len(candidates) == 0 {
		return ""
	}
	s.mu.Lock()
	idx := s.rng.Intn(len(candidates))
	s.mu.Unlock()
	return candidates[idx].ID
}

// ---- RPC ----------------------------------------------------------------

// schedulerPick answers scheduler.pick.
func schedulerPick(request []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	if !schedulerOwnsProvider(req) {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	candidates := state.scheduler.collectCandidates(req)
	if len(candidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	strategy := state.settings.get().Routing.Strategy
	weightedCfg := state.settings.get().Weighted
	var chosen string
	var delegate string

	switch strategy {
	case strategyByExpiry:
		chosen, _ = pickByExpiryScheduler(req, candidates)
	case strategyWeighted:
		chosen = pickWeightedFromCandidates(candidates, weightedCfg)
	case strategyRoundRobin:
		delegate = pluginapi.SchedulerBuiltinRoundRobin
	case strategyRandom:
		chosen = state.scheduler.pickRandom(candidates)
	default:
		chosen = pickByCredits(candidates)
	}

	if delegate != "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{
			Handled:         true,
			DelegateBuiltin: delegate,
		})
	}
	if chosen == "" {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	state.scheduler.recordPick(chosen)
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: chosen})
}

// pickWeightedFromCandidates converts scheduler candidates to weighted ones
// and runs the algorithm.
func pickWeightedFromCandidates(candidates []schedulerCandidate, cfg weightedSelectionSettings) string {
	if len(candidates) == 0 {
		return ""
	}
	now := time.Now()

	// Build weighted candidates, pulling credits and success/error from the pool.
	ws := make([]*weightedCandidate, 0, len(candidates))
	for _, c := range candidates {
		wc := &weightedCandidate{
			ID:      c.ID,
			UID:     c.ID,
			Label:   c.ID,
			Credits: c.Credits,
		}
		// Find pool lane for success/error counters
		for _, lane := range state.pool.snapshot() {
			if lane.UID != c.ID && laneKey(lane.Provider, lane.UID) != c.ID {
				continue
			}
			wc.SuccessCount = lane.Successes
			wc.ErrorTotal = lane.Failures
			wc.LastUsed = lane.LastUsed
			break
		}
		ws = append(ws, wc)
	}

	picked := pickWeighted(ws, now, cfg)
	if picked == nil {
		return ""
	}
	return picked.ID
}

// pickByExpiryScheduler adapts scheduler candidates to the rotation gate chain.
//
// The "current account" is whatever the plugin last handed out; the first pick
// has none, so the chain switches unconditionally (subject to the gates).
func pickByExpiryScheduler(req pluginapi.SchedulerPickRequest, candidates []schedulerCandidate) (string, rotateDecision) {
	rot := make([]rotateCandidate, 0, len(candidates))
	for _, c := range candidates {
		rc := rotateCandidate{
			AccountID:      c.ID,
			DisplayName:    c.ID,
			TotalRemaining: float64(c.Credits),
			// Default to valid. A candidate with no quota record still has to
			// be selectable, otherwise by_expiry would refuse to serve any
			// request until a refresh had run for every account.
			Valid: true,
		}
		// Attach the expiry from the recorded credit summary, if any.
		state.quota.mu.Lock()
		q, hasQuota := state.quota.byAuth[c.ID]
		state.quota.mu.Unlock()
		if !hasQuota {
			// Fall back to a uid-keyed reading.
			if alt, okAlt := lookupQuotaByUID(c.ID); okAlt {
				q, hasQuota = alt, true
			}
		}
		if hasQuota && q != nil {
			rc.SoonestExpireAt = q.soonestExpireAt()
			// An expired balance disqualifies the account as a target, matching
			// Candidate::valid in the reference implementation. A missing
			// reading does not: it only means the expiry is unknown.
			if q.expired() || !q.Known {
				rc.Valid = false
			}
		}
		rot = append(rot, rc)
	}

	current := state.scheduler.lastPicked()
	target, decision := pickByExpiry(rot, current, time.Now())
	if decision.Kind == rotateSwitch {
		state.scheduler.noteSwitch(time.Now())
	}
	return target, decision
}

// lookupQuotaByUID finds a recorded credit summary by uid or auth id.
func lookupQuotaByUID(id string) (*workBuddyQuota, bool) {
	return lookupQuotaAny(id)
}

// lookupQuotaAny finds a recorded credit summary under any of the identifiers a
// credential may be keyed by.
//
// Three keys are in play and they differ by code path:
//
//	host.auth.list -> entry.AuthIndex   (the runtime index, e.g. e420b8fe...)
//	executor       -> auth.AuthIndex    (the credential's uid)
//	models         -> creds.AuthKey()   ("<domain>/<uid>")
//
// Accepting all of them keeps a successful query from being invisible to the
// panel, which is exactly what happened before this helper existed: the quota
// refresh stored the reading under the auth index while the account table looked
// it up by uid, so the panel showed credits 0 despite a successful query.
func lookupQuotaAny(ids ...string) (*workBuddyQuota, bool) {
	state.quota.mu.Lock()
	defer state.quota.mu.Unlock()

	for _, id := range ids {
		id = trimSpace(id)
		if id == "" {
			continue
		}
		if q, ok := state.quota.byAuth[id]; ok && q != nil {
			return q, true
		}
	}
	// Fall back to matching the "<domain>/<uid>" keys.
	for _, id := range ids {
		id = trimSpace(id)
		if id == "" {
			continue
		}
		for key, q := range state.quota.byAuth {
			if q == nil {
				continue
			}
			if strings.HasSuffix(key, "/"+id) {
				return q, true
			}
		}
	}
	return nil, false
}

// schedulerOwnsProvider reports whether this plugin should schedule the request.
//
// When the provider list is empty the host has not resolved a provider yet, so
// the plugin answers only for its own provider; if none of the listed providers
// is ours, the request belongs to someone else.
func schedulerOwnsProvider(req pluginapi.SchedulerPickRequest) bool {
	if isWorkBuddyProvider(req.Provider) {
		return true
	}
	for _, p := range req.Providers {
		if isWorkBuddyProvider(p) {
			return true
		}
	}
	// A single provider that is not ours, or an explicit list without ours.
	if req.Provider != "" || len(req.Providers) > 0 {
		return false
	}
	// No provider information: decide from the candidates.
	for _, c := range req.Candidates {
		if isWorkBuddyProvider(c.Provider) {
			return true
		}
	}
	return false
}

// schedulerProviderKey returns a stable key for the round-robin cursor.
func schedulerProviderKey(req pluginapi.SchedulerPickRequest) string {
	if req.Provider != "" {
		return req.Provider
	}
	for _, p := range req.Providers {
		if isWorkBuddyProvider(p) {
			return p
		}
	}
	return workBuddyProviderKey
}

func (s *schedulerState) recordPick(authID string) {
	if authID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.picks[authID]++
	s.lastPickedID = authID
}

// lastPicked returns the account most recently selected.
func (s *schedulerState) lastPicked() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastPickedID
}

// pickCounts returns a copy of the per-auth selection counters.
func (s *schedulerState) pickCounts() map[string]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]uint64, len(s.picks))
	for k, v := range s.picks {
		out[k] = v
	}
	return out
}

// resetCursor clears the round-robin position so the next pick starts at the
// top. Exposed on the panel.
func (s *schedulerState) resetCursor() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursor = make(map[string]uint64)
	s.lastSwitch = time.Time{}
}

// lastSwitchAt returns when by_expiry last changed account.
func (s *schedulerState) lastSwitchAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSwitch
}

// noteSwitch records that by_expiry moved to a different account.
func (s *schedulerState) noteSwitch(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSwitch = at
}
