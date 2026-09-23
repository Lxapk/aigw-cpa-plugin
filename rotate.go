package main

import (
	"time"
)

// This file ports the rotation decision of changexbc/workbuddy-switch
// (crates/wb-switch-core/src/modules/rotate.rs) into the plugin's scheduler.
//
// Why a separate decision function
//
// The reference implementation's insight is that picking the account with the
// most credits is not optimal: credits expire, so the balance that expires
// soonest should be spent first. Its decide_target() is an eight-step gate
// chain where every refusal carries a human-readable reason, which makes the
// behaviour auditable from the panel.
//
// The same chain is reproduced here for the by_expiry strategy.

// rotateDecisionKind classifies the outcome.
type rotateDecisionKind int

const (
	// rotateSkip means keep the current account.
	rotateSkip rotateDecisionKind = iota
	// rotateSwitch means move to TargetID.
	rotateSwitch
)

// rotateCandidate is one selectable account with its credit context.
type rotateCandidate struct {
	// AccountID is the auth id (CPA's storage key).
	AccountID string
	// DisplayName is the user-facing label.
	DisplayName string
	// SoonestExpireAt is the earliest expiry in epoch seconds; 0 = no expiry.
	SoonestExpireAt int64
	// TotalRemaining is the summed remaining credits.
	TotalRemaining float64
	// Valid marks the account as a rotation target: query succeeded, nothing
	// expired, and credits remain.
	Valid bool
}

// rotateDecision is the outcome of the gate chain.
type rotateDecision struct {
	Kind   rotateDecisionKind
	Reason string
	// TargetID is set when Kind == rotateSwitch.
	TargetID string
	// UrgencyDays is the target's remaining days to expiry, for the message.
	UrgencyDays int64
}

// rotateParams carries the tunables, mirroring decide_target's arguments.
type rotateParams struct {
	// CurrentAccountID is the account in use (may not be a candidate).
	CurrentAccountID string
	// LastSwitchAt is when the plugin last changed account; zero = never.
	LastSwitchAt time.Time
	// Cooldown is the minimum time between switches.
	Cooldown time.Duration
	// MinGap is the anti-flapping threshold: a target must expire at least this
	// much sooner than the current account to justify a switch.
	MinGap time.Duration
	// MinUrgency is the urgency threshold: if even the most urgent account has
	// more than this left, there is no need to switch.
	MinUrgency time.Duration
	// MinRemaining filters out targets whose balance is too small to bother.
	MinRemaining float64
	// HasLiveSession blocks switching while a session is running, because a live
	// process holds the old credential and a switch would not take effect.
	HasLiveSession bool
	// Now is the evaluation instant (injected for testability).
	Now time.Time
}

// urgencyKey orders candidates by expiry, ported from Candidate::urgency_key():
//
//	Some(ts) -> (0, ts)   an expiring balance outranks a non-expiring one
//	None     -> (1, 0)    no expiry sorts last
func urgencyKey(c rotateCandidate) (int, int64) {
	if c.SoonestExpireAt > 0 {
		return 0, c.SoonestExpireAt
	}
	return 1, 0
}

// decideRotate reproduces rotate.rs::decide_target.
//
// Steps, in order:
//
//  1. filter to valid candidates      (empty -> skip)
//  2. target = earliest expiry
//  3. urgency: target expires later than MinUrgency -> skip
//  4. already the target -> skip
//  5. cooldown window -> skip
//  6. live session gate -> skip
//  7. value filter: target below MinRemaining -> skip
//  8. anti-flap: target sooner than current by less than MinGap -> skip
//
// The order matters: it is what makes the log read sensibly, and the live
// session check deliberately sits after the cheap comparisons so a pointless
// session lookup is avoided.
func decideRotate(candidates []rotateCandidate, p rotateParams) rotateDecision {
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	nowSec := now.Unix()

	// 1) valid candidates
	valid := make([]rotateCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.Valid {
			valid = append(valid, c)
		}
	}
	if len(valid) == 0 {
		return rotateDecision{Kind: rotateSkip, Reason: "没有可用账号（查询失败/已过期/无剩余积分）"}
	}

	// 2) target = earliest expiry
	target := valid[0]
	for _, c := range valid[1:] {
		tGroup, tTs := urgencyKey(target)
		cGroup, cTs := urgencyKey(c)
		if cGroup < tGroup || (cGroup == tGroup && cTs < tTs) {
			target = c
		}
	}

	// 3) urgency threshold
	var urgencyDays int64
	if target.SoonestExpireAt > 0 {
		remaining := time.Duration(target.SoonestExpireAt-nowSec) * time.Second
		urgencyDays = int64((remaining + 24*time.Hour - 1) / (24 * time.Hour))
		if p.MinUrgency > 0 && remaining > p.MinUrgency {
			return rotateDecision{
				Kind:        rotateSkip,
				UrgencyDays: urgencyDays,
				Reason: "所有账号到期都还早（最紧迫的还剩 " +
					itoa64(uint64(maxInt64(urgencyDays, 0))) + " 天），无需切换",
			}
		}
	}

	// 4) already current
	if p.CurrentAccountID != "" && p.CurrentAccountID == target.AccountID {
		return rotateDecision{
			Kind:        rotateSkip,
			TargetID:    target.AccountID,
			UrgencyDays: urgencyDays,
			Reason:      "当前账号已是最紧迫账号",
		}
	}

	// 5) cooldown
	if !p.LastSwitchAt.IsZero() && p.Cooldown > 0 {
		if p.LastSwitchAt.Add(p.Cooldown).After(now) {
			return rotateDecision{
				Kind:        rotateSkip,
				TargetID:    target.AccountID,
				UrgencyDays: urgencyDays,
				Reason:      "处于切换冷却期",
			}
		}
	}

	// 6) live session gate
	if p.HasLiveSession {
		return rotateDecision{
			Kind:        rotateSkip,
			TargetID:    target.AccountID,
			UrgencyDays: urgencyDays,
			Reason:      "检测到有会话在运行，本次跳过切换",
		}
	}

	// 7) value filter
	if p.MinRemaining > 0 && target.TotalRemaining < p.MinRemaining {
		return rotateDecision{
			Kind:        rotateSkip,
			TargetID:    target.AccountID,
			UrgencyDays: urgencyDays,
			Reason: "目标账号剩余积分不足（" + formatFloat(target.TotalRemaining) +
				"，阈值 " + formatFloat(p.MinRemaining) + "），不值得切换",
		}
	}

	// 8) anti-flap
	if p.CurrentAccountID != "" {
		for _, c := range candidates {
			if c.AccountID != p.CurrentAccountID {
				continue
			}
			if c.SoonestExpireAt > 0 && target.SoonestExpireAt > 0 &&
				target.SoonestExpireAt < c.SoonestExpireAt {
				gap := time.Duration(c.SoonestExpireAt-target.SoonestExpireAt) * time.Second
				if p.MinGap > 0 && gap < p.MinGap {
					return rotateDecision{
						Kind:        rotateSkip,
						TargetID:    target.AccountID,
						UrgencyDays: urgencyDays,
						Reason: "目标到期仅早 " + itoa64(uint64(gap/time.Hour)) +
							" 小时，未达切换阈值（" + itoa64(uint64(p.MinGap/time.Hour)) + " 小时）",
					}
				}
			}
			break
		}
	}

	return rotateDecision{
		Kind:        rotateSwitch,
		TargetID:    target.AccountID,
		UrgencyDays: urgencyDays,
		Reason:      "切换到到期最紧迫的账号",
	}
}

// pickByExpiry is the strategy entry point: it returns the account whose
// credits expire first, applying the gate chain against the given current
// account.
func pickByExpiry(candidates []rotateCandidate, current string, now time.Time) (string, rotateDecision) {
	cfg := state.settings.get().Routing
	decision := decideRotate(candidates, rotateParams{
		CurrentAccountID: current,
		LastSwitchAt:     state.scheduler.lastSwitchAt(),
		Cooldown:         time.Duration(cfg.CooldownSeconds) * time.Second,
		MinGap:           time.Duration(cfg.MinGapHours) * time.Hour,
		MinUrgency:       time.Duration(cfg.MinUrgencyHours) * time.Hour,
		MinRemaining:     cfg.MinRemaining,
		Now:              now,
	})
	if decision.Kind != rotateSwitch {
		return "", decision
	}
	return decision.TargetID, decision
}

// maxInt64 avoids pulling in a math import for one call.
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// formatFloat renders a float without trailing zeros, for messages.
func formatFloat(v float64) string {
	if v == float64(int64(v)) {
		return itoa64(uint64(int64(v)))
	}
	whole := int64(v)
	frac := int((v - float64(whole)) * 100)
	if frac < 0 {
		frac = -frac
	}
	out := itoa64(uint64(whole)) + "."
	if frac < 10 {
		out += "0"
	}
	return out + itoa64(uint64(frac))
}
