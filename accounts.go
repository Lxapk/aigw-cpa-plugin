package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file provides the single authoritative WorkBuddy account view.
//
// Why this exists
//
// The plugin's credential pool (pool.go) only learns about an account when it
// sees traffic for it (intercept_response.go) or when a quota refresh runs. That
// means a freshly logged-in account stayed invisible until the first request —
// which is not what the source app does: it lists accounts straight from its
// own store (A0.s), independent of any traffic.
//
// CPA's equivalent of that store is the auth-file inventory behind
// host.auth.list / host.auth.get. This file reads it directly, so the account
// list is correct the moment a login finishes.
//
// Filtering is strict: only credentials whose provider is WorkBuddy/codebuddy
// are surfaced. Other providers never appear, even though CPA may hold many.

// workBuddyAccount is one WorkBuddy credential as presented to the UI.
type workBuddyAccount struct {
	// AuthIndex is CPA's storage key (the auth file name).
	AuthIndex string `json:"auth_index"`
	// Label is the user-facing name.
	Label string `json:"label"`
	// UID mirrors the provider-side user id.
	UID string `json:"uid"`
	// Nickname mirrors W1.d's display name when the provider supplied one.
	Nickname string `json:"nickname,omitempty"`
	// Domain is the stored region domain ("" when unset).
	Domain string `json:"domain"`
	// Region is the derived region key: "cn" or "global" (a2/b.java:284).
	Region string `json:"region"`
	// EnterpriseID is the tenant, when present.
	EnterpriseID string `json:"enterprise_id,omitempty"`
	// ExpiresAt is the credential expiry as epoch seconds (0 when unknown).
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// Expired reports whether the access token looks expired.
	Expired bool `json:"expired"`
	// Disabled mirrors the host-side disabled flag.
	Disabled bool `json:"disabled"`

	// Credits is the latest known remaining quota (0 when never queried).
	Credits int64 `json:"credits"`
	// CreditsKnown reports whether a quota figure is available.
	CreditsKnown bool `json:"credits_known"`
	// CreditsAt is when the quota was last read.
	CreditsAt time.Time `json:"credits_at,omitempty"`

	// CreditsExpireAt is the soonest expiry across this account's credit
	// resources, in epoch seconds (0 = no expiry). Ported from the reference
	// implementation's soonestExpireAt.
	CreditsExpireAt int64 `json:"credits_expire_at,omitempty"`
	// CreditsExpireDays is the whole-day countdown to CreditsExpireAt.
	CreditsExpireDays int64 `json:"credits_expire_days,omitempty"`
	// CreditsExpiringSoon reports CreditsExpireAt within 7 days.
	CreditsExpiringSoon bool `json:"credits_expiring_soon"`
	// CreditsExpired reports that a credit resource has already expired.
	CreditsExpired bool `json:"credits_expired"`
	// CreditPackages lists the package names, for the detail view.
	CreditPackages []string `json:"credit_packages,omitempty"`
	// Variant is "cn" or "ai", shown so the operator can see which service an
	// account belongs to.
	Variant string `json:"variant"`

	// CoolKind / Reason / CooldownUntil come from the pool when it has seen
	// this credential; they are empty otherwise.
	CoolKind      string    `json:"cool_kind,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	Usable        bool      `json:"usable"`
}

// accountStore caches the inventory briefly so a page render does not hit the
// host on every request, while still picking up a fresh login within seconds.
type accountStore struct {
	mu        sync.Mutex
	cached    []workBuddyAccount
	fetchedAt time.Time
	ttl       time.Duration
	lastErr   string
}

func newAccountStore() *accountStore {
	return &accountStore{ttl: 5 * time.Second}
}

// invalidate forces the next read to hit the host.
func (s *accountStore) invalidate() {
	s.mu.Lock()
	s.fetchedAt = time.Time{}
	s.mu.Unlock()
}

// accounts returns the current WorkBuddy inventory, refreshing from the host
// when the cache has expired.
func (s *accountStore) accounts() []workBuddyAccount {
	s.mu.Lock()
	if !s.fetchedAt.IsZero() && time.Since(s.fetchedAt) < s.ttl {
		out := append([]workBuddyAccount(nil), s.cached...)
		s.mu.Unlock()
		return out
	}
	s.mu.Unlock()

	list, errList := loadWorkBuddyAccounts()

	s.mu.Lock()
	defer s.mu.Unlock()
	if errList != nil {
		s.lastErr = errList.Error()
		// Serve the stale copy rather than an empty list on a transient error.
		out := append([]workBuddyAccount(nil), s.cached...)
		return out
	}
	s.cached = list
	s.fetchedAt = time.Now()
	s.lastErr = ""
	return append([]workBuddyAccount(nil), list...)
}

func (s *accountStore) lastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// loadWorkBuddyAccounts reads and filters the host's auth inventory.
func loadWorkBuddyAccounts() ([]workBuddyAccount, error) {
	raw, errList := callHost("host.auth.list", map[string]any{})
	if errList != nil {
		return nil, errList
	}

	entries := decodeAuthEntries(raw)
	out := make([]workBuddyAccount, 0, len(entries))

	for _, entry := range entries {
		// Strict provider filter: only WorkBuddy/codebuddy entries are shown.
		// The host may expose "provider" or "type", and either may carry the
		// internal key or the display name.
		if !isWorkBuddyAuthEntry(entry) {
			continue
		}

		storage := entry.StorageJSON
		if len(storage) == 0 && entry.AuthIndex != "" {
			storage = fetchAuthStorage(entry.AuthIndex)
		}
		creds, errParse := parseWorkBuddyCredentials(storage)
		if errParse != nil {
			// A codebuddy file we cannot parse is still worth showing, so the
			// operator can see something is wrong with it.
			out = append(out, workBuddyAccount{
				AuthIndex: entry.AuthIndex,
				Label:     firstNonEmpty(entry.Label, entry.Name, entry.AuthIndex),
				Region:    workBuddyRegion(""),
				Usable:    false,
				Reason:    "凭据无法解析",
				Disabled:  entry.Disabled,
			})
			continue
		}

		account := workBuddyAccount{
			AuthIndex:    entry.AuthIndex,
			Label:        firstNonEmpty(entry.Label, entry.Name, creds.Nickname, creds.UID, entry.AuthIndex),
			UID:          creds.UID,
			Nickname:     creds.Nickname,
			Domain:       creds.Domain,
			Region:       workBuddyRegion(creds.Domain),
			EnterpriseID: creds.EnterpriseID,
			ExpiresAt:    creds.ExpiresAt,
			Expired:      creds.expired(),
			Disabled:     entry.Disabled,
			Usable:       !entry.Disabled,
		}
		out = append(out, account)
	}

	// Stable, useful ordering: most credits first (mirrors A0/s.java:596),
	// then by label for ties.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Credits != out[j].Credits {
			return out[i].Credits > out[j].Credits
		}
		return out[i].Label < out[j].Label
	})
	return out, nil
}

// isWorkBuddyAuthEntry applies the strict provider filter.
func isWorkBuddyAuthEntry(entry hostAuthEntry) bool {
	// Accept the internal key or the display name, in either field.
	for _, candidate := range []string{entry.Provider, entry.Type} {
		if isWorkBuddyProvider(candidate) {
			return true
		}
	}
	// Some hosts only expose the file name; a "codebuddy-" prefix is ours.
	name := strings.ToLower(firstNonEmpty(entry.AuthIndex, entry.Name))
	return strings.HasPrefix(name, workBuddyProviderKey+"-") ||
		strings.HasPrefix(name, workBuddyProviderKey+"_")
}

// decodeAuthEntries tolerates the shapes host.auth.list may return.
func decodeAuthEntries(raw json.RawMessage) []hostAuthEntry {
	if len(raw) == 0 {
		return nil
	}
	var wrapper struct {
		Auths []hostAuthEntry `json:"auths"`
		Items []hostAuthEntry `json:"items"`
	}
	if errUnmarshal := json.Unmarshal(raw, &wrapper); errUnmarshal == nil {
		if len(wrapper.Auths) > 0 {
			return wrapper.Auths
		}
		if len(wrapper.Items) > 0 {
			return wrapper.Items
		}
	}
	var list []hostAuthEntry
	if errUnmarshal := json.Unmarshal(raw, &list); errUnmarshal == nil {
		return list
	}
	return nil
}

// enrichWithRuntime folds in the pool's cooldown state and the latest quota
// reading, so the list shows operational detail without being dependent on it.
func enrichWithRuntime(accounts []workBuddyAccount) []workBuddyAccount {
	for i := range accounts {
		a := &accounts[i]

		// Variant label for the UI.
		a.Variant = string(variantForDomain(a.Domain))

		// Quota: prefer the newest reading we hold.
		state.quota.mu.Lock()
		if q, ok := state.quota.byAuth[a.AuthIndex]; ok && q != nil && q.Known {
			a.Credits = q.Credits
			a.CreditsKnown = true
			a.CreditsExpireAt = q.soonestExpireAt()
			a.CreditsExpiringSoon = q.expiringSoon()
			a.CreditsExpired = q.expired()
			a.CreditPackages = q.Labels
		}
		state.quota.mu.Unlock()

		// A uid-keyed reading is the fallback when the file name differs.
		if !a.CreditsKnown {
			if q, ok := lookupQuotaByUID(a.UID); ok {
				a.Credits = q.Credits
				a.CreditsKnown = q.Known
				a.CreditsExpireAt = q.soonestExpireAt()
				a.CreditsExpiringSoon = q.expiringSoon()
				a.CreditsExpired = q.expired()
				a.CreditPackages = q.Labels
			}
		}
		if a.CreditsExpireAt > 0 {
			days := (a.CreditsExpireAt - time.Now().Unix()) / 86400
			if days < 0 {
				days = 0
			}
			a.CreditsExpireDays = days
		}

		// Pool: cooldown / cool kind when this credential has been exercised.
		for _, lane := range state.pool.snapshot() {
			if lane.UID != a.UID && lane.UID != a.AuthIndex {
				continue
			}
			if !a.CreditsKnown && lane.CreditsKnown {
				a.Credits = lane.Credits
				a.CreditsKnown = true
			}
			a.CoolKind = coolKindName(lane.CoolKind)
			if lane.StatusMessage != "" {
				a.Reason = lane.StatusMessage
			}
			a.CooldownUntil = lane.CooldownUntil
			break
		}

		// Usability mirrors A0/s.java:596's guard, plus the reference
		// implementation's rule that an expired balance is not a valid target.
		// A credential flagged with a reason we already set (e.g. an unparsable
		// file) stays unusable.
		now := time.Now()
		blockedByReason := a.Reason != "" && !a.CreditsKnown && a.UID == "" && a.CoolKind == ""
		a.Usable = !a.Disabled && !a.Expired && !a.CreditsExpired && !blockedByReason &&
			(a.CooldownUntil.IsZero() || !now.Before(a.CooldownUntil))
	}
	return accounts
}

// listWorkBuddyAccounts is the entry point used by the UI.
func listWorkBuddyAccounts() []workBuddyAccount {
	return enrichWithRuntime(state.accounts.accounts())
}

// expired reports whether the stored access token is past its expiry, allowing
// a small clock skew.
func (c *workBuddyCredentials) expired() bool {
	if c == nil || c.ExpiresAt <= 0 {
		return false
	}
	return time.Now().After(time.Unix(c.ExpiresAt, 0).Add(60 * time.Second))
}

// refreshAccountsAfterLogin invalidates the inventory cache so a just-finished
// login appears immediately.
func refreshAccountsAfterLogin() {
	state.accounts.invalidate()
}

// accountSummary counts accounts for the header line.
func accountSummary(accounts []workBuddyAccount) (total, usable, known int, totalCredits int64) {
	total = len(accounts)
	for _, a := range accounts {
		if a.Usable {
			usable++
		}
		if a.CreditsKnown {
			known++
			if a.Credits > 0 {
				totalCredits += a.Credits
			}
		}
	}
	return total, usable, known, totalCredits
}

// contextWithTimeout is a small helper shared by the refresh paths.
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
