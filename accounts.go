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
	// VariantLabel is Variant rendered for display ("国内版"/"国际版").
	VariantLabel string `json:"variant_label"`

	// CoolKind / Reason / CooldownUntil come from the pool when it has seen
	// this credential; they are empty otherwise.
	CoolKind      string    `json:"cool_kind,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	Usable        bool      `json:"usable"`
	// DisabledByUser reports the operator's manual enable/disable toggle.
	DisabledByUser bool `json:"disabled_by_user"`

	// credentials is the parsed credential material, kept for internal callers
	// (model catalogue, quota refresh). Unexported so it never reaches JSON.
	credentials *workBuddyCredentials
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
			credentials:  creds,
			AuthIndex:    entry.AuthIndex,
			Label:        firstNonEmpty(entry.Label, entry.Name, creds.Nickname, creds.UID, entry.AuthIndex),
			UID:          creds.UID,
			Nickname:     creds.Nickname,
			Domain:       creds.Domain,
			Region:       workBuddyRegionForCredentials(creds),
			EnterpriseID: creds.EnterpriseID,
			ExpiresAt:    creds.ExpiresAt,
			Expired:      creds.expired(),
			Disabled:     entry.Disabled,
			Usable:       !entry.Disabled,
		}
		out = append(out, account)
	}

	// Collapse duplicates.
	//
	// host.auth.list can surface the same credential twice — once from the auth
	// file on disk and once from the runtime index — which showed up as a doubled
	// account row in the panel. A credential is the same when its uid matches, or,
	// when the uid is unavailable, its auth_index does.
	out = dedupeAccounts(out)

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

// dedupeAccounts removes repeated credentials, keeping the most informative
// entry of each group (the one with a parsed credential body and a label).
func dedupeAccounts(in []workBuddyAccount) []workBuddyAccount {
	if len(in) < 2 {
		return in
	}
	index := make(map[string]int, len(in))
	out := make([]workBuddyAccount, 0, len(in))

	for _, a := range in {
		key := accountIdentity(a)
		if key == "" {
			out = append(out, a)
			continue
		}
		pos, seen := index[key]
		if !seen {
			index[key] = len(out)
			out = append(out, a)
			continue
		}
		// Prefer the richer record: one with credentials, then one with a
		// human label.
		if accountScore(a) > accountScore(out[pos]) {
			out[pos] = a
		}
	}
	return out
}

// accountIdentity derives the dedupe key: the provider uid when present, else
// the auth index.
func accountIdentity(a workBuddyAccount) string {
	if strings.TrimSpace(a.UID) != "" {
		return "uid:" + strings.TrimSpace(a.UID)
	}
	if strings.TrimSpace(a.AuthIndex) != "" {
		return "idx:" + strings.TrimSpace(a.AuthIndex)
	}
	return ""
}

// accountScore ranks how much useful information a record carries.
func accountScore(a workBuddyAccount) int {
	score := 0
	if a.credentials != nil && a.credentials.AccessToken != "" {
		score += 100
	}
	if a.CreditsKnown {
		score += 10
	}
	if a.Nickname != "" {
		score += 2
	}
	// A label equal to the auth index is the least useful fallback.
	if a.Label != "" && a.Label != a.AuthIndex {
		score += 5
	}
	return score
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
//
// CPA nests the entries under "files" (rpcHostAuthListResponse); the other keys
// are fallbacks for hosts that spell it differently. A bare array is also
// accepted.
func decodeAuthEntries(raw json.RawMessage) []hostAuthEntry {
	if len(raw) == 0 {
		return nil
	}
	var wrapper hostAuthListResponse
	if errUnmarshal := json.Unmarshal(raw, &wrapper); errUnmarshal == nil {
		switch {
		case len(wrapper.Files) > 0:
			return wrapper.Files
		case len(wrapper.Auths) > 0:
			return wrapper.Auths
		case len(wrapper.Items) > 0:
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
		//
		// Variant stays the stable machine key ("cn"/"ai") that the script
		// matches on, while VariantLabel carries the human text. The panel used
		// to print the raw key next to buttons that already said 国内版/国际版,
		// so the same account was described two different ways.
		variant := variantForCredentials(a.credentials)
		if a.credentials == nil {
			variant = variantForDomain(a.Domain)
		}
		a.Variant = string(variant)
		a.VariantLabel = variant.label()

		// Quota: look the reading up by every identifier the credential has.
		//
		// The quota writer keys by the auth index it received from
		// host.auth.list, while the executor path keys by the credential's uid
		// (auth.AuthIndex). Looking up only one of them silently loses the
		// figure — the panel then showed credits 0 despite a successful query.
		authKey := ""
		if a.credentials != nil {
			authKey = a.credentials.AuthKey()
		}
		if q, ok := lookupQuotaAny(a.AuthIndex, a.UID, authKey); ok {
			a.Credits = q.Credits
			a.CreditsKnown = q.Known
			a.CreditsExpireAt = q.soonestExpireAt()
			a.CreditsExpiringSoon = q.expiringSoon()
			a.CreditsExpired = q.expired()
			a.CreditPackages = q.Labels
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
			a.DisabledByUser = lane.DisabledByUser
			break
		}

		// Usability mirrors A0/s.java:596's guard, plus the reference
		// implementation's rule that an expired balance is not a valid target.
		// A credential flagged with a reason we already set (e.g. an unparsable
		// file) stays unusable.
		//
		// DisabledByUser must be part of this test: the panel toggle only sets
		// the pool lane, so leaving it out made a disabled account keep
		// reporting 可用 in the very column the operator looks at, which read as
		// "禁用没有生效" even though the flag was stored.
		now := time.Now()
		blockedByReason := a.Reason != "" && !a.CreditsKnown && a.UID == "" && a.CoolKind == ""
		a.Usable = !a.Disabled && !a.DisabledByUser && !a.Expired && !a.CreditsExpired &&
			!blockedByReason && (a.CooldownUntil.IsZero() || !now.Before(a.CooldownUntil))
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
