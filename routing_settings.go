package main

import (
	"sort"
	"time"
)

// This file holds the account-routing configuration and the panel-facing view
// of it.

// routingSettings controls how the plugin selects an account per request.
type routingSettings struct {
	// Strategy is one of by_expiry / by_credits / round_robin / random.
	Strategy schedulerStrategy `json:"strategy" yaml:"strategy"`

	// The remaining fields are the rotation gates from the reference
	// implementation's decide_target(). They only affect by_expiry.

	// CooldownSeconds is the minimum time between account switches.
	CooldownSeconds int `json:"cooldown_seconds" yaml:"cooldown_seconds"`
	// MinGapHours is the anti-flapping threshold: a target must expire at least
	// this much sooner than the current account to justify a switch.
	MinGapHours int `json:"min_gap_hours" yaml:"min_gap_hours"`
	// MinUrgencyHours suppresses switching while every account still has more
	// than this much time before expiry.
	MinUrgencyHours int `json:"min_urgency_hours" yaml:"min_urgency_hours"`
	// MinRemaining filters out targets with too few credits to be worth using.
	MinRemaining float64 `json:"min_remaining" yaml:"min_remaining"`
}

func defaultRoutingSettings() routingSettings {
	// by_credits is the source app's behaviour (A0/s.java:596), so it is the
	// safe default; by_expiry must be opted into because it changes the
	// account mid-flight.
	return routingSettings{
		Strategy:        strategyByCredits,
		CooldownSeconds: 300, // 5 minutes between switches
		MinGapHours:     6,   // ignore sub-6h expiry differences
		MinUrgencyHours: 24,  // no switching while everything has >1 day left
		MinRemaining:    0,   // value filter off by default
	}
}

// applyDefaults coerces an empty or unrecognised strategy to the default and
// restores the gate defaults when a field is left unset.
func (r *routingSettings) applyDefaults() {
	d := defaultRoutingSettings()
	r.Strategy = normalizeStrategy(string(r.Strategy))
	if r.CooldownSeconds < 0 {
		r.CooldownSeconds = d.CooldownSeconds
	}
	if r.MinGapHours < 0 {
		r.MinGapHours = d.MinGapHours
	}
	if r.MinUrgencyHours < 0 {
		r.MinUrgencyHours = d.MinUrgencyHours
	}
	if r.MinRemaining < 0 {
		r.MinRemaining = d.MinRemaining
	}
	// A zero cooldown/gap/urgency means "the operator cleared it"; only restore
	// when the whole block is absent (all three zero), so explicit zeros work.
	if r.CooldownSeconds == 0 && r.MinGapHours == 0 && r.MinUrgencyHours == 0 {
		r.CooldownSeconds = d.CooldownSeconds
		r.MinGapHours = d.MinGapHours
		r.MinUrgencyHours = d.MinUrgencyHours
	}
}

// applyRoutingConfig validates and stores the routing settings.
func applyRoutingConfig(cfg routingSettings) routingSettings {
	cfg.applyDefaults()
	state.settings.setRouting(cfg)
	return cfg
}

// routingStatusJSON describes the current strategy for the panel.
func routingStatusJSON() map[string]any {
	cfg := state.settings.get().Routing
	accounts := listWorkBuddyAccounts()
	picks := state.scheduler.pickCounts()

	// Selection order preview: exactly the order the active strategy would use.
	order := strategyPreview(cfg.Strategy, accounts)

	options := make([]map[string]any, 0, len(allSchedulerStrategies))
	for _, s := range allSchedulerStrategies {
		options = append(options, map[string]any{
			"value":       string(s),
			"label":       s.label(),
			"description": strategyDescription(s),
		})
	}

	rows := make([]map[string]any, 0, len(order))
	for i, a := range order {
		rows = append(rows, map[string]any{
			"position":      i + 1,
			"label":         a.Label,
			"auth_id":       a.AuthIndex,
			"uid":           a.UID,
			"credits":       a.Credits,
			"known":         a.CreditsKnown,
			"usable":        a.Usable,
			"picks":         picks[a.AuthIndex],
			"expire_days":   a.CreditsExpireDays,
			"expiring_soon": a.CreditsExpiringSoon,
			"expired":       a.CreditsExpired,
		})
	}

	return map[string]any{
		"strategy":          string(cfg.Strategy),
		"strategy_label":    cfg.Strategy.label(),
		"options":           options,
		"order":             rows,
		"accounts":          len(accounts),
		"cooldown_seconds":  cfg.CooldownSeconds,
		"min_gap_hours":     cfg.MinGapHours,
		"min_urgency_hours": cfg.MinUrgencyHours,
		"min_remaining":     cfg.MinRemaining,
		"last_switch":       state.scheduler.lastSwitchAt().Format(time.RFC3339),
	}
}

// strategyDescription explains each mode, shown in the panel.
func strategyDescription(s schedulerStrategy) string {
	switch s {
	case strategyRoundRobin:
		return "按顺序轮流使用每个账号，请求分布最均匀"
	case strategyRandom:
		return "每次随机挑选，避免总是命中同一个账号"
	case strategyByExpiry:
		return "优先使用积分最快到期的账号（避免积分过期浪费），带冷却与防抖动"
	default:
		return "优先使用剩余积分最多的账号（源应用的行为）"
	}
}

// strategyPreview renders the order the active strategy would produce.
//
// For by_credits this is simply "most credits first". For round_robin it is the
// rotation start order (identical to a sorted list, since the cursor advances
// from the start each cycle). For random there is no fixed order, so the list is
// presented alphabetically and the panel notes that the pick is random.
func strategyPreview(strategy schedulerStrategy, accounts []workBuddyAccount) []workBuddyAccount {
	usable := make([]workBuddyAccount, 0, len(accounts))
	for _, a := range accounts {
		if a.Usable {
			usable = append(usable, a)
		}
	}

	switch strategy {
	case strategyRoundRobin, strategyRandom:
		// No meaningful order to show; present alphabetically.
		sort.SliceStable(usable, func(i, j int) bool { return usable[i].Label < usable[j].Label })
	case strategyByExpiry:
		// Soonest expiry first; no-expiry balances sort last (rotate.rs::
		// urgency_key).
		sort.SliceStable(usable, func(i, j int) bool {
			ai, aj := usable[i].CreditsExpireAt, usable[j].CreditsExpireAt
			if (ai == 0) != (aj == 0) {
				return ai != 0
			}
			if ai != aj {
				return ai < aj
			}
			return usable[i].Label < usable[j].Label
		})
	default: // by_credits
		sort.SliceStable(usable, func(i, j int) bool {
			if usable[i].Credits != usable[j].Credits {
				return usable[i].Credits > usable[j].Credits
			}
			return usable[i].Label < usable[j].Label
		})
	}
	return usable
}

// nextRotationHint tells the panel which account round_robin would use next.
func nextRotationHint() string {
	state.scheduler.mu.Lock()
	defer state.scheduler.mu.Unlock()
	var total uint64
	for _, v := range state.scheduler.cursor {
		total += v
	}
	if total == 0 {
		return "尚未开始轮巡"
	}
	return "已轮转 " + itoa64(total) + " 次"
}

// itoa64 renders a uint64 without pulling in strconv at call sites.
func itoa64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// schedulerSnapshot is a small debug view returned by the status endpoint.
func schedulerSnapshot() map[string]any {
	state.scheduler.mu.Lock()
	defer state.scheduler.mu.Unlock()
	cursors := make(map[string]uint64, len(state.scheduler.cursor))
	for k, v := range state.scheduler.cursor {
		cursors[k] = v
	}
	picks := make(map[string]uint64, len(state.scheduler.picks))
	for k, v := range state.scheduler.picks {
		picks[k] = v
	}
	return map[string]any{
		"cursors":    cursors,
		"pick_count": picks,
		"as_of":      time.Now().Format(time.RFC3339),
	}
}
