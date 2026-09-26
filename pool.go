package main

import (
	"sort"
	"sync"
	"time"
)

// coolKind ports W1.b (A0/s.java:969 parses it from the persisted "coolKind"):
//
//	QUOTA(0)  credits exhausted
//	SOFT(1)   transient failure, short cooldown
//	ERROR(2)  hard error
//
// RATE is this plugin's addition. The app folds a 429 into QUOTA, but the two
// have opposite meanings for the remaining balance, so the pool tracks them
// separately instead of zeroing Credits on every throttle.
type coolKind int

const (
	coolKindNone  coolKind = -1
	coolKindQuota coolKind = iota
	coolKindSoft
	coolKindError
	coolKindRate
)

// credentialLane mirrors one entry of the app's account pool.
//
// In the APK each credential lives in A0.s (a LinkedHashMap keyed by
// providerId + "/" + uid) and carries the fields of W1.d:
//
//	f3827b credits       -> Credits          (selection weight, A0/s.java:596)
//	f3828c creditsKnown  -> CreditsKnown
//	f3829d detail        -> Detail
//	f3830e disabled      -> Disabled
//	f          enabled   -> Enabled
//	f3831g reason        -> StatusMessage
//	f3832h untilMillis   -> CooldownUntil
//	f3833i errorCount    -> ConsecutiveErrors
//	f3834j coolKind      -> CoolKind
//
// V1.k.c() mutates it on failure:
//
//	case 0 (auth/invalid):  mark; cooldown = quotaCooldownMillis  (hard)
//	case 1,3 (rate/quota):  mark; cooldown = quotaCooldownMillis
//	case 2 (disabled):      dVar.e = true; dVar.g = reason        (permanent)
//	case 4,5,6:             A0.s.p(errThreshold, errCooldown, ...)  (soft)
type credentialLane struct {
	// Provider is the provider id owning this credential.
	Provider string `json:"provider"`
	// UID is the stable credential identifier (runtime auth index in CPA).
	UID string `json:"uid"`
	// Label mirrors V1.n.c (user-facing account label).
	Label string `json:"label"`
	// Disabled mirrors V1.n.e.
	Disabled bool `json:"disabled"`
	// DisabledByUser tracks manual toggles from the panel.
	DisabledByUser bool `json:"disabled_by_user"`
	// AutoDisabled reports that the pool retired this account itself, after a
	// failure that cannot be recovered by retrying (dead credential, revoked
	// token). It is what the面板 shows as 原因「自动禁用」 and what lets the
	// operator re-enable the account knowingly.
	AutoDisabled bool `json:"auto_disabled"`
	// DisabledReason explains why the account was retired.
	DisabledReason string `json:"disabled_reason,omitempty"`
	// DisabledAt records when that happened.
	DisabledAt time.Time `json:"disabled_at,omitempty"`
	// Variant is the credential's realm, shown in the grouped account list.
	Variant string `json:"variant,omitempty"`
	// Enabled mirrors W1.d.f (defaults to true in the app).
	Enabled bool `json:"enabled"`
	// StatusMessage mirrors V1.n.g / W1.d.f3831g.
	StatusMessage string `json:"status_message"`
	// Detail mirrors W1.d.f3829d.
	Detail string `json:"detail"`

	// Credits is the remaining quota, used as the selection weight
	// (W1.d.f3827b / A0/s.java:596 picks the largest).
	Credits int64 `json:"credits"`
	// CreditsKnown mirrors W1.d.f3828c.
	CreditsKnown bool `json:"credits_known"`

	// ConsecutiveErrors counts successive failures since the last success.
	ConsecutiveErrors int `json:"consecutive_errors"`
	// CooldownUntil is the wall-clock instant this lane may be retried.
	CooldownUntil time.Time `json:"cooldown_until"`
	// CoolKind mirrors W1.d.f3834j.
	CoolKind coolKind `json:"cool_kind"`
	// LastError is the most recent failure reason.
	LastError string `json:"last_error"`
	// LastUsed is the most recent successful use.
	LastUsed time.Time `json:"last_used"`
	// Successes / Failures are lifetime counters.
	Successes int64 `json:"successes"`
	Failures  int64 `json:"failures"`
}

// disableAccount marks a credential for manual enable/disable from the panel.
//
// This is orthogonal to the host's own disabled flag — it persists across
// restarts in the pool state and is checked by usable().
func (p *credentialPool) disableAccount(uid string, disabled bool) {
	p.disableAccountKeyed(uid, "", disabled)
}

// disableAccountKeyed marks a credential for manual enable/disable from the
// panel, matching lanes by uid AND the CPA auth index.
//
// The two identifiers differ by code path: the panel keys accounts by the
// provider-side uid (creds.UID), while the pool lane seen by interception is
// keyed by the CPA auth index (metadata auth_id / AuthIndex). Matching only
// one of them silently created an orphan disabled lane while the live lane
// stayed enabled — the "账号禁用不了" report. Both are applied so the toggle
// always lands on the lane that actually serves traffic.
// disableAccountKeyed applies the panel's enable/disable toggle.
//
// Enabling must also clear the pool's own retirement (Disabled /
// AutoDisabled / cooldown): otherwise a manually re-enabled account stayed
// unusable because the internal bit was still set, and the toggle looked
// broken.
func (p *credentialPool) disableAccountKeyed(uid, authIndex string, disabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	matched := false
	for _, lane := range p.lanes {
		if (uid != "" && lane.UID == uid) || (authIndex != "" && lane.UID == authIndex) {
			p.applyToggleLocked(lane, disabled)
			matched = true
		}
	}
	if matched {
		return
	}
	key := firstNonEmpty(uid, authIndex)
	if key == "" {
		return
	}
	lane := p.laneLocked(workBuddyProviderKey, key)
	p.applyToggleLocked(lane, disabled)
}

// applyToggleLocked writes one enable/disable decision. Caller holds p.mu.
func (p *credentialPool) applyToggleLocked(lane *credentialLane, disabled bool) {
	lane.DisabledByUser = disabled
	if disabled {
		return
	}
	// Re-enabling lifts every internal retirement this lane accumulated.
	lane.Disabled = false
	lane.AutoDisabled = false
	lane.DisabledReason = ""
	lane.DisabledAt = time.Time{}
	lane.ConsecutiveErrors = 0
	lane.CooldownUntil = time.Time{}
	lane.CoolKind = coolKindNone
	lane.StatusMessage = ""
	p.markRecoveredLocked(lane.UID)
}

// autoDisableEvent records one automatic retirement, for the panel's history.
type autoDisableEvent struct {
	UID       string    `json:"uid"`
	Label     string    `json:"label"`
	Reason    string    `json:"reason"`
	Variant   string    `json:"variant,omitempty"`
	At        time.Time `json:"at"`
	Recovered bool      `json:"recovered"`
}

// recordAutoDisableLocked appends an audit entry. Caller holds p.mu.
//
// A wrong retirement is otherwise invisible: the account simply stops being
// used and nothing says why or when.
func (p *credentialPool) recordAutoDisableLocked(lane *credentialLane, reason string) {
	p.autoDisables = append(p.autoDisables, autoDisableEvent{
		UID:     lane.UID,
		Label:   lane.Label,
		Reason:  reason,
		Variant: lane.Variant,
		At:      time.Now(),
	})
	if len(p.autoDisables) > 50 {
		p.autoDisables = p.autoDisables[len(p.autoDisables)-50:]
	}
}

// autoDisableHistory returns the recorded retirements, newest last.
func (p *credentialPool) autoDisableHistory() []autoDisableEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]autoDisableEvent, len(p.autoDisables))
	copy(out, p.autoDisables)
	return out
}

// markRecoveredLocked flags the latest retirement of a uid as undone.
// Caller holds p.mu.
func (p *credentialPool) markRecoveredLocked(uid string) {
	for i := len(p.autoDisables) - 1; i >= 0; i-- {
		if p.autoDisables[i].UID == uid && !p.autoDisables[i].Recovered {
			p.autoDisables[i].Recovered = true
			return
		}
	}
}

// findAccount returns a lane by uid, or nil.
func (p *credentialPool) findAccount(uid string) *credentialLane {
	return p.findAccountKeyed(uid, "")
}

// findAccountKeyedCopy returns a snapshot copy of the first lane matching uid
// or the CPA auth index. See disableAccountKeyed for why both keys are
// consulted.
//
// It returns a value rather than the pooled *credentialLane on purpose: lanes
// are mutated in place under p.mu by success/failure/observe, so handing the
// pointer to a caller that reads fields after the lock is dropped is a data
// race. Callers that only need to read state must use this copy.
func (p *credentialPool) findAccountKeyedCopy(uid, authIndex string) (credentialLane, bool) {
	if uid == "" && authIndex == "" {
		return credentialLane{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, lane := range p.lanes {
		if (uid != "" && lane.UID == uid) || (authIndex != "" && lane.UID == authIndex) {
			return *lane, true
		}
	}
	return credentialLane{}, false
}

// findAccountKeyed returns the first lane matching uid or the CPA auth index,
// or nil. See disableAccountKeyed for why both keys are consulted.
//
// Deprecated: the returned pointer aliases pool state and must not be read
// after p.mu is released. Prefer findAccountKeyedCopy.
func (p *credentialPool) findAccountKeyed(uid, authIndex string) *credentialLane {
	if uid == "" && authIndex == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, lane := range p.lanes {
		if (uid != "" && lane.UID == uid) || (authIndex != "" && lane.UID == authIndex) {
			return lane
		}
	}
	return nil
}

// isAccountDisabled reports whether any pool lane matching uid/authIndex is
// manually disabled by the operator (or permanently parked by the host).
//
// The check runs entirely under p.mu via findAccountKeyedCopy so it never
// races with the writers that mutate lane fields in place.
func (p *credentialPool) isAccountDisabled(uid, authIndex string) bool {
	lane, ok := p.findAccountKeyedCopy(uid, authIndex)
	return ok && (lane.DisabledByUser || lane.Disabled)
}

//	!d.disabled && d.enabled && now >= d.untilMillis
//
// and also checks the operator's manual toggle.
func (c *credentialLane) usable(now time.Time) bool {
	if c.Disabled || c.DisabledByUser || !c.Enabled {
		return false
	}
	return !now.Before(c.CooldownUntil)
}

// credentialPool is the port of A0.s's health bookkeeping plus V1.k.c()'s
// cooldown policy. It is keyed by "provider/uid", exactly like A0.s.m().
type credentialPool struct {
	mu    sync.Mutex
	lanes map[string]*credentialLane
	order []string
	// autoDisables is the audit trail of accounts the pool retired itself.
	autoDisables []autoDisableEvent
}

func newCredentialPool() *credentialPool {
	return &credentialPool{lanes: make(map[string]*credentialLane)}
}

func laneKey(provider, uid string) string { return provider + "/" + uid }

// observe registers (or refreshes) a credential seen on the wire.
func (p *credentialPool) observe(provider, uid, label string) *credentialLane {
	if provider == "" || uid == "" {
		return nil
	}
	key := laneKey(provider, uid)
	p.mu.Lock()
	defer p.mu.Unlock()
	lane, ok := p.lanes[key]
	if !ok {
		// A new lane starts enabled with no known credits, matching W1.d's
		// field initialisers (f = true, f3827b = 0, f3828c = false).
		lane = &credentialLane{Provider: provider, UID: uid, Enabled: true, CoolKind: coolKindNone}
		p.lanes[key] = lane
		p.order = append(p.order, key)
	}
	if label != "" {
		lane.Label = label
	}
	return lane
}

// setCredits records a quota reading, mirroring A0/s.java:947:
//
//	dVar.f3827b = credits
//	dVar.f3828c = creditsKnown
func (p *credentialPool) setCredits(provider, uid string, credits int64, known bool) {
	if provider == "" && uid == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	lane := p.laneLocked(provider, uid)
	lane.Credits = credits
	lane.CreditsKnown = known
}

// laneLocked finds a lane by provider+uid, creating it if needed.
func (p *credentialPool) laneLocked(provider, uid string) *credentialLane {
	key := laneKey(provider, uid)
	lane, ok := p.lanes[key]
	if !ok {
		lane = &credentialLane{Provider: provider, UID: uid, Enabled: true, CoolKind: coolKindNone}
		p.lanes[key] = lane
		p.order = append(p.order, key)
	}
	return lane
}

// setCreditsByAuthID records a reading when only the auth id is known.
// It matches on uid first, then on the auth id itself.
func (p *credentialPool) setCreditsByAuthID(authID, uid string, credits int64, known bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, key := range p.order {
		lane := p.lanes[key]
		if lane == nil {
			continue
		}
		if (uid != "" && lane.UID == uid) || lane.UID == authID || key == workBuddyProviderKey+"/"+authID {
			lane.Credits = credits
			lane.CreditsKnown = known
			return
		}
	}
	// No existing lane: create one so the reading is not lost.
	lane := p.laneLocked(workBuddyProviderKey, firstNonEmpty(uid, authID))
	lane.Credits = credits
	lane.CreditsKnown = known
}

// success ports A0.s.q(providerId, uid): a good call resets the failure state.
func (p *credentialPool) success(provider, uid string) {
	if provider == "" || uid == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	lane := p.lanes[laneKey(provider, uid)]
	if lane == nil {
		// A success can be observed before any interception (e.g. CPA's usage
		// hook is the first place the credential surfaces). Register it, the
		// same way A0.s materialises an entry on first sight: W1.d initialises
		// enabled=true, so the lane must be usable immediately.
		lane = &credentialLane{Provider: provider, UID: uid, Enabled: true, CoolKind: coolKindNone}
		p.lanes[laneKey(provider, uid)] = lane
		p.order = append(p.order, laneKey(provider, uid))
	}
	// Guard against lanes created by older code paths without Enabled set.
	if !lane.Enabled && !lane.Disabled {
		lane.Enabled = true
	}
	lane.ConsecutiveErrors = 0
	lane.CooldownUntil = time.Time{}
	lane.LastError = ""
	lane.LastUsed = time.Now()
	lane.Successes++
}

// failureKind mirrors V1.j / the Y1.j enum used by V1.k.c().
type failureKind int

const (
	// failureAuth maps to V1.j "auth" (credential invalid/expired).
	failureAuth failureKind = iota
	// failureRate maps to V1.j "rate" (rate limited).
	failureRate
	// failureQuota maps to V1.j "quota" (balance/permission exhausted).
	failureQuota
	// failureTransient maps to V1.j "transient" (5xx, connection reset).
	failureTransient
)

// failure ports A0.s.p(...) plus V1.k.c(): applies the correct cooldown for the
// failure class and parks the lane permanently once `disabled` is signalled.
//
// Returns whether the lane was actually retired by this call, so the caller can
// log the retirement once instead of inferring it.
func (p *credentialPool) failure(provider, uid string, kind failureKind, reason string, settings gatewaySettings, permanent bool) bool {
	if provider == "" || uid == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	lane := p.lanes[laneKey(provider, uid)]
	if lane == nil {
		lane = &credentialLane{Provider: provider, UID: uid, Enabled: true, CoolKind: coolKindNone}
		p.lanes[laneKey(provider, uid)] = lane
		p.order = append(p.order, laneKey(provider, uid))
	}
	if !lane.Enabled && !lane.Disabled {
		lane.Enabled = true
	}

	now := time.Now()
	lane.LastError = reason
	lane.Failures++

	if permanent {
		// V1.k.c() case 2: permanent disable.
		//
		// Also stamp the user-visible flag and reason. Previously only the
		// internal Disabled bit was set, so an account the pool had retired for
		// a bad credential still rendered as 启用 in the panel and offered no
		// explanation — the operator had to infer it from the call log.
		lane.Disabled = true
		lane.DisabledByUser = true
		lane.AutoDisabled = true
		lane.DisabledReason = reason
		lane.DisabledAt = now
		lane.StatusMessage = reason
		p.recordAutoDisableLocked(lane, reason)
		return true
	}

	switch kind {
	case failureAuth, failureRate, failureQuota:
		// V1.k.c() cases 0,1,3: immediate hard cooldown.
		// The app labels a quota/rate rejection as QUOTA
		// (V1/k.java:164 passes W1.b.f3822d with quotaCooldownMillis).
		lane.ConsecutiveErrors++
		lane.StatusMessage = reason
		switch kind {
		case failureQuota:
			// A quota rejection means the credits are gone; reflecting that
			// keeps the selection order honest (A0/s.java:596).
			lane.CoolKind = coolKindQuota
			lane.CooldownUntil = now.Add(time.Duration(settings.QuotaCooldownMillis) * time.Millisecond)
			lane.Credits = 0
			lane.CreditsKnown = true
		case failureRate:
			// A 429 is transient: it says nothing about the remaining balance.
			// Zeroing Credits here permanently demoted a merely throttled
			// account under by_credits selection and, because CreditsKnown
			// stayed true, made it look "known empty" rather than unknown.
			// Keep the last known figure and use the rate cooldown.
			lane.CoolKind = coolKindRate
			lane.CooldownUntil = now.Add(time.Duration(settings.RateCooldownMillis) * time.Millisecond)
		default:
			lane.CoolKind = coolKindError
			lane.CooldownUntil = now.Add(time.Duration(settings.QuotaCooldownMillis) * time.Millisecond)
		}
	default:
		// V1.k.c() cases 4,5,6: soft failure. Park only after errorThreshold
		// consecutive failures, for errorCooldownMillis.
		// V1/k.java:168 labels this SOFT.
		lane.ConsecutiveErrors++
		lane.CoolKind = coolKindSoft
		if lane.ConsecutiveErrors >= settings.ErrorThreshold {
			lane.CooldownUntil = now.Add(time.Duration(settings.ErrorCooldownMillis) * time.Millisecond)
			lane.StatusMessage = reason
			lane.ConsecutiveErrors = 0
			return false
		}
		lane.CooldownUntil = now.Add(time.Duration(settings.SoftCooldownMillis) * time.Millisecond)
		lane.StatusMessage = reason
	}
	return false
}

// pick ports A0/s.java:585 t(providerId, exclude):
//
//	now := clock()
//	best := nil
//	for key, d := range accounts {           // LinkedHashMap insertion order
//	    if d.providerId != providerId { continue }
//	    if exclude.contains(d.uid) { continue }
//	    if !d.disabled && d.enabled && now >= d.untilMillis {
//	        if best == nil || d.credits > best.credits { best = d }
//	    }
//	}
//	return best?.account
//
// The selection weight is the remaining quota, so the richest account is used
// first. Ties keep the earlier insertion order, which is why the comparison is
// strict (>).
func (p *credentialPool) pick(provider string, tried map[string]struct{}, now time.Time) *credentialLane {
	p.mu.Lock()
	defer p.mu.Unlock()
	var best *credentialLane
	for _, key := range p.order {
		lane := p.lanes[key]
		if lane == nil || lane.Provider != provider {
			continue
		}
		if tried != nil {
			if _, ok := tried[lane.UID]; ok {
				continue
			}
		}
		if !lane.usable(now) {
			continue
		}
		if best == nil || lane.Credits > best.Credits {
			best = lane
		}
	}
	return best
}

// snapshot returns a stable copy of every lane for the management endpoint.
func (p *credentialPool) snapshot() []credentialLane {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]credentialLane, 0, len(p.lanes))
	for _, key := range p.order {
		if lane := p.lanes[key]; lane != nil {
			out = append(out, *lane)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].UID < out[j].UID
	})
	return out
}

// usableCount reports how many lanes are currently usable for a provider.
func (p *credentialPool) usableCount(provider string, now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, lane := range p.lanes {
		if lane.Provider == provider && lane.usable(now) {
			n++
		}
	}
	return n
}

func (p *credentialPool) totalCount(provider string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, lane := range p.lanes {
		if lane.Provider == provider {
			n++
		}
	}
	return n
}
