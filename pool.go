package main

import (
	"sort"
	"sync"
	"time"
)

// credentialLane mirrors one entry of the app's account pool.
//
// In the APK each credential lives in A0.s (a LinkedHashMap keyed by
// providerId + "/" + uid). V1.k.c() mutates it on failure:
//
//	case 0 (auth/invalid):  mark; cooldown = quotaCooldownMillis  (hard)
//	case 1,3 (rate/quota):  mark; cooldown = quotaCooldownMillis
//	case 2 (disabled):      dVar.e = true; dVar.g = reason        (permanent)
//	case 4,5,6:             A0.s.p(errThreshold, errCooldown, ...)  (soft)
//
// V1.o.n() picks the next usable lane via A0.s.t(providerId, triedSet) and
// V1.o.k() gives up after settings.maxRotate attempts per request.
type credentialLane struct {
	// Provider is the provider id owning this credential.
	Provider string `json:"provider"`
	// UID is the stable credential identifier (runtime auth index in CPA).
	UID string `json:"uid"`
	// Label mirrors V1.n.c (user-facing account label).
	Label string `json:"label"`
	// Disabled mirrors V1.n.e.
	Disabled bool `json:"disabled"`
	// StatusMessage mirrors V1.n.g.
	StatusMessage string `json:"status_message"`

	// ConsecutiveErrors counts successive failures since the last success.
	ConsecutiveErrors int `json:"consecutive_errors"`
	// CooldownUntil is the wall-clock instant this lane may be retried.
	CooldownUntil time.Time `json:"cooldown_until"`
	// LastError is the most recent failure reason.
	LastError string `json:"last_error"`
	// LastUsed is the most recent successful use.
	LastUsed time.Time `json:"last_used"`
	// Successes / Failures are lifetime counters.
	Successes int64 `json:"successes"`
	Failures  int64 `json:"failures"`
}

func (c *credentialLane) usable(now time.Time) bool {
	if c.Disabled {
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
		lane = &credentialLane{Provider: provider, UID: uid}
		p.lanes[key] = lane
		p.order = append(p.order, key)
	}
	if label != "" {
		lane.Label = label
	}
	return lane
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
		// same way A0.s materialises an entry on first sight.
		lane = &credentialLane{Provider: provider, UID: uid}
		p.lanes[laneKey(provider, uid)] = lane
		p.order = append(p.order, laneKey(provider, uid))
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
		lane = &credentialLane{Provider: provider, UID: uid}
		p.lanes[laneKey(provider, uid)] = lane
		p.order = append(p.order, laneKey(provider, uid))
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
		lane.ConsecutiveErrors++
		lane.CooldownUntil = now.Add(time.Duration(settings.QuotaCooldownMillis) * time.Millisecond)
		lane.StatusMessage = reason
	default:
		// V1.k.c() cases 4,5,6: soft failure. Park only after errorThreshold
		// consecutive failures, for errorCooldownMillis.
		lane.ConsecutiveErrors++
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

// pick ports A0.s.t(providerId, tried): returns the next usable lane for the
// provider, skipping anything already attempted in this request.
func (p *credentialPool) pick(provider string, tried map[string]struct{}, now time.Time) *credentialLane {
	p.mu.Lock()
	defer p.mu.Unlock()
	var best *credentialLane
	for _, key := range p.order {
		lane := p.lanes[key]
		if lane == nil || lane.Provider != provider {
			continue
		}
		if _, ok := tried[lane.UID]; ok {
			continue
		}
		if !lane.usable(now) {
			continue
		}
		if best == nil || lane.ConsecutiveErrors < best.ConsecutiveErrors {
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
