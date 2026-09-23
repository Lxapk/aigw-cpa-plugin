package main

import (
	"sort"
	"time"
)

// This file holds the account-routing configuration and the panel-facing view
// of it.

// routingSettings controls how the plugin selects an account per request.
type routingSettings struct {
	// Strategy is one of by_credits / round_robin / random.
	Strategy schedulerStrategy `json:"strategy" yaml:"strategy"`
}

func defaultRoutingSettings() routingSettings {
	// by_credits is the source app's behaviour (A0/s.java:596), so it is the
	// safe default.
	return routingSettings{Strategy: strategyByCredits}
}

// applyDefaults coerces an empty or unrecognised strategy to the default.
func (r *routingSettings) applyDefaults() {
	r.Strategy = normalizeStrategy(string(r.Strategy))
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
			"position": i + 1,
			"label":    a.Label,
			"auth_id":  a.AuthIndex,
			"uid":      a.UID,
			"credits":  a.Credits,
			"known":    a.CreditsKnown,
			"usable":   a.Usable,
			"picks":    picks[a.AuthIndex],
		})
	}

	return map[string]any{
		"strategy":       string(cfg.Strategy),
		"strategy_label": cfg.Strategy.label(),
		"options":        options,
		"order":          rows,
		"accounts":       len(accounts),
	}
}

// strategyDescription explains each mode, shown in the panel.
func strategyDescription(s schedulerStrategy) string {
	switch s {
	case strategyRoundRobin:
		return "按顺序轮流使用每个账号，请求分布最均匀"
	case strategyRandom:
		return "每次随机挑选，避免总是命中同一个账号"
	default:
		return "优先使用剩余额度最多的账号（源应用的行为）"
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
	case strategyRoundRobin:
		sort.SliceStable(usable, func(i, j int) bool { return usable[i].Label < usable[j].Label })
	case strategyRandom:
		sort.SliceStable(usable, func(i, j int) bool { return usable[i].Label < usable[j].Label })
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
