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
type coolKind int

const (
	coolKindNone  coolKind = -1
	coolKindQuota coolKind = iota
	coolKindSoft
	coolKindError
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

// usable ports the guard in A0/s.java:596:
//
//	!d.disabled && d.enabled && now >= d.untilMillis
func (c *credentialLane) usable(now time.Time) bool {
	if c.Disabled || !c.Enabled {
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
func (p *credentialPool) failure(provider, uid string, kind failureKind, reason string, settings gatewaySettings, permanent bool) {
	if provider == "" || uid == "" {
		return
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
		lane.Disabled = true
		lane.StatusMessage = reason
		return
	}

	switch kind {
	case failureAuth, failureRate, failureQuota:
		// V1.k.c() cases 0,1,3: immediate hard cooldown.
		// The app labels a quota/rate rejection as QUOTA
		// (V1/k.java:164 passes W1.b.f3822d with quotaCooldownMillis).
		lane.ConsecutiveErrors++
		lane.CooldownUntil = now.Add(time.Duration(settings.QuotaCooldownMillis) * time.Millisecond)
		lane.StatusMessage = reason
		if kind == failureQuota || kind == failureRate {
			lane.CoolKind = coolKindQuota
			// A quota rejection means the credits are gone; reflecting that
			// keeps the selection order honest (A0/s.java:596).
			lane.Credits = 0
		} else {
			lane.CoolKind = coolKindError
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
			return
		}
		lane.CooldownUntil = now.Add(time.Duration(settings.SoftCooldownMillis) * time.Millisecond)
		lane.StatusMessage = reason
	}
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
