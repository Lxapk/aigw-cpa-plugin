package main

import (
	"context"
	"sort"
	"sync"
	"time"
)

// This file holds the entry points that the task engine, the panel and the
// scheduled night pass all share, plus the small store that lets the panel show
// the last run without re-running anything.

// growthRunTimeout bounds one account's growth pass.
//
// The pass makes a variable number of upstream calls (one per report, plus
// polling and claims), so a fixed timeout is safer than relying on the
// individual request timeouts: without it a wedged endpoint keeps the task
// engine's run slot occupied indefinitely.
const growthRunTimeout = 10 * time.Minute

// runGrowthPass executes the full growth pass for one credential.
//
// This is the single entry point used by the scheduler, the manual run button
// and the night pass, so the spacing and the stage ordering are identical on
// every path.
func runGrowthPass(ctx context.Context, account checkinAccount) growthRunResult {
	runner := &growthRunner{
		client:  workBuddyUpstream,
		limiter: growthLimit,
		gap:     workBuddyGrowthGap,
	}
	return runner.run(ctx, account.Creds, firstNonEmpty(account.Label, account.Creds.Nickname, account.AuthID))
}

// growthLimit is the process-wide per-credential call spacer.
var growthLimit = newGrowthLimiter()

// ---- last-run store -------------------------------------------------------

// growthStore keeps the most recent result per account so the panel can render
// it without re-running, and so a scheduled pass is visible to the operator.
type growthStore struct {
	mu      sync.RWMutex
	byUID   map[string]growthRunResult
	history []growthHistoryEntry
}

// growthHistoryEntry is one completed pass, for the run history list.
type growthHistoryEntry struct {
	UID        string    `json:"uid"`
	Label      string    `json:"label"`
	OK         bool      `json:"ok"`
	Earned     int       `json:"earned_credit"`
	Claimed    int       `json:"claimed"`
	Lit        int       `json:"lit"`
	Skipped    int       `json:"skipped"`
	Failed     int       `json:"failed"`
	Trigger    string    `json:"trigger"`
	FinishedAt time.Time `json:"finished_at"`
	Error      string    `json:"error,omitempty"`
}

const growthHistoryMax = 50

func newGrowthStore() *growthStore {
	return &growthStore{byUID: make(map[string]growthRunResult)}
}

func (s *growthStore) record(uid string, result growthRunResult) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byUID[uid] = result
	s.history = append([]growthHistoryEntry{{
		UID:        uid,
		Label:      result.Label,
		OK:         result.OK,
		Earned:     result.Earned,
		Claimed:    result.Claimed,
		Lit:        result.Lit,
		Skipped:    result.Skipped,
		Failed:     result.Failed,
		Trigger:    result.Trigger,
		FinishedAt: result.FinishedAt,
		Error:      result.Error,
	}}, s.history...)
	if len(s.history) > growthHistoryMax {
		s.history = s.history[:growthHistoryMax]
	}
}

// last returns the stored result for one account.
func (s *growthStore) last(uid string) (growthRunResult, bool) {
	if s == nil {
		return growthRunResult{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	result, ok := s.byUID[uid]
	return result, ok
}

// snapshot returns every stored result, ordered by label.
func (s *growthStore) snapshot() []growthRunResult {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]growthRunResult, 0, len(s.byUID))
	for _, result := range s.byUID {
		out = append(out, result)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// recentHistory returns the newest entries first.
func (s *growthStore) recentHistory() []growthHistoryEntry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]growthHistoryEntry, len(s.history))
	copy(out, s.history)
	return out
}

// recordGrowthResult stores a task-engine result.
//
// The task engine reports through runActivity; the manual and night paths call
// recordGrowthResultWithTrigger directly.
func recordGrowthResult(uid string, result growthRunResult) {
	if state.growth == nil {
		return
	}
	result.Trigger = "auto"
	state.growth.record(uid, result)
}

// recordGrowthResultWithTrigger stores a result together with what started it.
func recordGrowthResultWithTrigger(uid string, result growthRunResult, trigger string) {
	if state.growth == nil {
		return
	}
	result.Trigger = trigger
	state.growth.record(uid, result)
}
