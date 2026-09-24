package main

import (
	"math/rand"
	"sort"
	"sync"
	"time"
)

// This file ports the weighted random account selection algorithm from
// https://github.com/linguo2625469/workbuddy2api-panel (internal/pool/pick.go).
//
// The source project implements a three-factor weighted random selection with
// anti-convoy protection that is noticeably more sophisticated than simply
// sorting by credits: it spreads load across accounts according to their
// remaining credits, idle time, and historical success rate.
//
// Algorithm summary
//
//  1. Filter to healthy (usable, not cooling) candidates.
//  2. Compute a three-factor weight per candidate:
//     weight = creditsRatio × 10 + idleWeight + successRate × 3
//  3. Sort by weight and keep the Top 5 (or fewer).
//     Equal-weight entries are shuffled before truncation to avoid
//     starvation by uid ordering (the anti-convoy measure).
//  4. Within the Top 5, filter out accounts used within the last
//     minPickGap (anti-thundering-herd — serialised by the pool lock).
//  5. If all Top 5 were just used, LRU-fallback across the full list
//     using a monotonic sequence counter (not wall clock, which can stall).
//  6. Pick one from the eligible set by linear-weighted random.

// weightedSelectionSettings controls the tunables for the picker.
type weightedSelectionSettings struct {
	// Enabled turns on the weighted picker; when off, the old by_credits
	// strategy is used.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// MinPickGap prevents the same account from being picked twice within
	// this window (anti-convoy). Zero uses the default.
	MinPickGap time.Duration `json:"min_pick_gap" yaml:"min_pick_gap"`
	// TopK is the shortlist size. Zero defaults to 5.
	TopK int `json:"top_k" yaml:"top_k"`
	// IdleWeightMax caps the idle bonus.
	IdleWeightMax float64 `json:"idle_weight_max" yaml:"idle_weight_max"`
	// IdleWeightPerHour is the bonus per hour since last use.
	IdleWeightPerHour float64 `json:"idle_weight_per_hour" yaml:"idle_weight_per_hour"`
}

func defaultWeightedSelectionSettings() weightedSelectionSettings {
	return weightedSelectionSettings{
		Enabled:           false,
		MinPickGap:        100 * time.Millisecond,
		TopK:              5,
		IdleWeightMax:     5.0,
		IdleWeightPerHour: 0.5,
	}
}

// pickerState carries the monotonic sequence counter and the RNG.
type pickerState struct {
	mu      sync.Mutex
	pickSeq uint64
	rng     *rand.Rand
}

var globalPicker = &pickerState{rng: rand.New(rand.NewSource(time.Now().UnixNano()))}

// weightedCandidate is one account being evaluated.
type weightedCandidate struct {
	// ID is the auth/credential identifier.
	ID string `json:"id"`
	// UID is the provider-side user id.
	UID string `json:"uid"`
	// Label is the display name.
	Label string `json:"label"`
	// Credits is the remaining quota.
	Credits int64 `json:"credits"`
	// MaxCredits is the maximum credits among all candidates (set by the picker).
	MaxCredits int64 `json:"-"`
	// LastUsed is when this account was last picked.
	LastUsed time.Time `json:"last_used"`
	// UsedSeq is the monotonic sequence number of the last pick.
	UsedSeq uint64 `json:"-"`
	// SuccessCount / ErrorTotal track the lifetime call record.
	SuccessCount int64 `json:"success_count"`
	ErrorTotal   int64 `json:"error_total"`
}

// weight computes the three-factor weight.
//
//	weight = 1 + creditsRatio×10 + idleWeight + successRate×3
func (c *weightedCandidate) weight(now time.Time, idleWeightMax, idleWeightPerHour float64) float64 {
	w := 1.0
	if c.MaxCredits > 0 {
		w += float64(c.Credits) / float64(c.MaxCredits) * 10
	}
	if c.LastUsed.IsZero() {
		w += idleWeightMax
	} else {
		hours := now.Sub(c.LastUsed).Hours()
		idleW := hours * idleWeightPerHour
		if idleW > idleWeightMax {
			idleW = idleWeightMax
		}
		if idleW < 0 {
			idleW = 0
		}
		w += idleW
	}
	total := c.SuccessCount + c.ErrorTotal
	if total > 0 {
		successRate := float64(c.SuccessCount) / float64(total)
		w += successRate * 3
	} else {
		w += 1.5 // neutral trust for new accounts
	}
	return w
}

// computeWeights pre-computes weights for a slice of candidates.
// maxCredits is taken from the full set (before TopK truncation).
func computeWeights(cands []*weightedCandidate, now time.Time, idleWeightMax, idleWeightPerHour float64) {
	var max int64
	for _, c := range cands {
		if c.Credits > max {
			max = c.Credits
		}
	}
	for _, c := range cands {
		c.MaxCredits = max
	}
}

// pickWeighted selects one candidate using the algorithm:
//
//  1. compute maxCredits across all candidates
//  2. pre-compute weights
//  3. shuffle equal-weight entries before TopK truncation (anti-convoy)
//  4. sort by (weight desc, uid)
//  5. keep TopK
//  6. filter recently-used by minPickGap
//  7. all filtered → LRU-fallback by monotonic sequence across full set
//  8. linear weighted random pick
func pickWeighted(cands []*weightedCandidate, now time.Time, cfg weightedSelectionSettings) *weightedCandidate {
	if len(cands) == 0 {
		return nil
	}
	if len(cands) == 1 {
		globalPicker.record(cands[0], now)
		return cands[0]
	}
	topK := cfg.TopK
	if topK <= 0 {
		topK = 5
	}

	// compute max credits and weights
	computeWeights(cands, now, cfg.IdleWeightMax, cfg.IdleWeightPerHour)

	type scored struct {
		c *weightedCandidate
		w float64
	}
	scoreds := make([]scored, 0, len(cands))
	for _, c := range cands {
		scoreds = append(scoreds, scored{c: c, w: c.weight(now, cfg.IdleWeightMax, cfg.IdleWeightPerHour)})
	}

	// shuffle equal-weight entries before TopK truncation (anti-convoy)
	if len(scoreds) > topK {
		eq := false
		for i := 1; i < len(scoreds); i++ {
			if scoreds[i].w == scoreds[0].w {
				eq = true
				break
			}
		}
		if eq {
			globalPicker.mu.Lock()
			shuf := rand.New(rand.NewSource(now.UnixNano()))
			shuf.Shuffle(len(scoreds), func(i, j int) { scoreds[i], scoreds[j] = scoreds[j], scoreds[i] })
			globalPicker.mu.Unlock()
		}
	}

	sort.SliceStable(scoreds, func(i, j int) bool {
		if scoreds[i].w != scoreds[j].w {
			return scoreds[i].w > scoreds[j].w
		}
		return scoreds[i].c.UID < scoreds[j].c.UID
	})

	// TopK truncation
	if len(scoreds) > topK {
		scoreds = scoreds[:topK]
	}

	// Save the full (pre-truncation) list for LRU fallback
	allCands := cands

	// Build eligible (not used within minPickGap)
	eligible := make([]*weightedCandidate, 0, len(scoreds))
	for _, s := range scoreds {
		if now.Sub(s.c.LastUsed) >= cfg.MinPickGap {
			eligible = append(eligible, s.c)
		}
	}

	var picked *weightedCandidate
	if len(eligible) == 0 {
		// LRU fallback across full list by monotonic sequence
		picked = allCands[0]
		for _, c := range allCands[1:] {
			if c.UsedSeq < picked.UsedSeq {
				picked = c
			}
		}
	} else {
		// linear weighted random
		var total float64
		for _, c := range eligible {
			total += c.weight(now, cfg.IdleWeightMax, cfg.IdleWeightPerHour)
		}
		if total <= 0 {
			picked = eligible[0]
		} else {
			globalPicker.mu.Lock()
			r := globalPicker.rng.Float64() * total
			globalPicker.mu.Unlock()
			var acc float64
			for _, c := range eligible {
				acc += c.weight(now, cfg.IdleWeightMax, cfg.IdleWeightPerHour)
				if r < acc {
					picked = c
					break
				}
			}
			if picked == nil {
				picked = eligible[len(eligible)-1]
			}
		}
	}
	globalPicker.record(picked, now)
	return picked
}

func (p *pickerState) record(c *weightedCandidate, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pickSeq++
	c.LastUsed = now
	c.UsedSeq = p.pickSeq
}
