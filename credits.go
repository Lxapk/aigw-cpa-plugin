package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// This file ports the credit model of changexbc/workbuddy-switch
// (crates/wb-switch-core/src/modules/credits.rs).
//
// The APK only exposed a single legacy endpoint returning a summed remainder.
// The reference implementation uses three newer endpoints that additionally
// carry expiry timestamps, which is what makes "which account should I use
// next" answerable: credits expire, so the soonest-expiring balance should be
// spent first.
//
// Endpoints (with the variant's billing-path fallback applied):
//
//	POST {base}/billing/meter/get-user-resource-summary         current cycle usage
//	POST {base}/billing/meter/get-user-resource-paid-packages   paid packages
//	POST {base}/billing/meter/get-user-resource-free-packages   free packages
//
// plus the legacy fallback used when the new endpoints are unavailable:
//
//	POST {base}/billing/meter/get-user-resource                 summed remainder
//
// Timing constants mirror the reference implementation:
//
//	EXPIRING_SOON_DAYS          = 7    "expiring soon"
//	EXPIRY_CYCLE_OVERRIDE_DAYS  = 365  DeductionEndTime treated as a placeholder
//	FAR_FUTURE_EXPIRY_DAYS      = 730  beyond this the expiry is "no expiry"
const (
	resourceSummaryPath = "/billing/meter/get-user-resource-summary"
	resourcePaidPath    = "/billing/meter/get-user-resource-paid-packages"
	resourceFreePath    = "/billing/meter/get-user-resource-free-packages"
	legacyResourcePath  = "/v2/billing/meter/get-user-resource"

	expiringSoonDays        = 7
	expiryCycleOverrideDays = 365
	farFutureExpiryDays     = 730

	// productCode is the fixed product code from the APK's F() and the
	// reference implementation.
	productCode = "p_tcaca"
)

// creditResource is one package's worth of credits, ported from
// credits.rs::resource_summary.
type creditResource struct {
	PackageCode string `json:"package_code,omitempty"`
	PackageName string `json:"package_name,omitempty"`
	// Total is the package's full allowance.
	Total float64 `json:"total"`
	// Remaining is what is left.
	Remaining float64 `json:"remaining"`
	// Used is Total - Remaining (recomputed when the response omits it).
	Used float64 `json:"used"`
	// Status is the provider's raw status code, when present.
	Status int64 `json:"status,omitempty"`

	// ExpireAt is the earliest expiry for this resource, in epoch seconds.
	// Zero means "no expiry" (a far-future placeholder was filtered out).
	ExpireAt int64 `json:"expire_at,omitempty"`
	// Expired reports ExpireAt <= now.
	Expired bool `json:"expired"`
	// ExpiringSoon reports now < ExpireAt <= now + 7d.
	ExpiringSoon bool `json:"expiring_soon"`
}

// creditSummary is the aggregated view across all resources, mirroring the
// fields the reference implementation's credits summary exposes.
type creditSummary struct {
	// Resources lists every package slot discovered.
	Resources []creditResource `json:"resources"`
	// Total / Remaining / Used are the sums across resources.
	Total     float64 `json:"total"`
	Remaining float64 `json:"remaining"`
	Used      float64 `json:"used"`
	// SoonestExpireAt is the earliest non-zero ExpireAt, in epoch seconds.
	// Zero when no resource expires (or none is known).
	SoonestExpireAt int64 `json:"soonest_expire_at,omitempty"`
	// ExpiredRemaining is the sum of already-expired remaining credits.
	ExpiredRemaining float64 `json:"expired_remaining"`
	// Expired reports whether any resource has expired.
	Expired bool `json:"expired"`
	// ExpiringSoon reports whether the soonest expiry is within 7 days.
	ExpiringSoon bool `json:"expiring_soon"`
	// Known reports whether a query succeeded.
	Known bool `json:"known"`
	// Error carries the failure reason when Known is false.
	Error string `json:"error,omitempty"`
}

// usable reports whether this summary qualifies an account as a rotation
// target: the query succeeded, nothing is expired, and credits remain.
//
// Mirrors the reference implementation's Candidate::valid.
func (c creditSummary) usable() bool {
	return c.Known && !c.Expired && c.Remaining > 0
}

// ---------------------------------------------------------------------------
// expiry resolution
// ---------------------------------------------------------------------------

// resolveExpireAt ports credits.rs::resolve_expire_at.
//
// Preference order:
//
//  1. DeductionEndTime / deductionEndTime / ExpiredTime / expiredTime
//  2. CycleEndTime / cycleEndTime, when the first is more than 365 days later
//     (the provider uses a 2049-style placeholder alongside a real cycle end)
//  3. whichever single value is present
//
// A result further than 730 days out is treated as "no expiry" so placeholders
// never surface in the UI.
func resolveExpireAt(raw map[string]any, now time.Time) int64 {
	// firstTimestamp normalises everything to epoch **seconds**, so all
	// comparisons below are in seconds.
	deduction := firstTimestamp(raw, "DeductionEndTime", "deductionEndTime", "ExpiredTime", "expiredTime")
	cycle := firstTimestamp(raw, "CycleEndTime", "cycleEndTime")

	overrideSeconds := int64(expiryCycleOverrideDays) * 24 * 3600

	var expireAt int64
	switch {
	case deduction != 0 && cycle != 0 && deduction-cycle > overrideSeconds:
		expireAt = cycle
	case deduction != 0:
		expireAt = deduction
	case cycle != 0:
		expireAt = cycle
	}
	if expireAt == 0 {
		return 0
	}

	farFutureSeconds := int64(farFutureExpiryDays) * 24 * 3600
	if expireAt-now.Unix() > farFutureSeconds {
		return 0
	}
	return expireAt
}

// firstTimestamp returns the first present timestamp, normalised to epoch
// seconds (expiry is compared against seconds elsewhere in the plugin).
func firstTimestamp(raw map[string]any, keys ...string) int64 {
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		if ms := parseTimestampMillis(v); ms != 0 {
			return ms / 1000
		}
	}
	return 0
}

// parseTimestampMillis accepts epoch millis, epoch seconds, and RFC3339 strings.
func parseTimestampMillis(v any) int64 {
	switch t := v.(type) {
	case float64:
		return normaliseEpoch(int64(t))
	case int64:
		return normaliseEpoch(t)
	case int:
		return normaliseEpoch(int64(t))
	case json.Number:
		if n, errInt := t.Int64(); errInt == nil {
			return normaliseEpoch(n)
		}
	case string:
		s := trimSpace(t)
		if s == "" {
			return 0
		}
		if n, errInt := parseInt64(s); errInt == nil {
			return normaliseEpoch(n)
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if parsed, errParse := time.Parse(layout, s); errParse == nil {
				return parsed.UnixMilli()
			}
		}
	}
	return 0
}

// normaliseEpoch promotes a seconds value to milliseconds so the rest of the
// code can work in one unit. Anything below year 3000 in seconds is treated as
// seconds.
func normaliseEpoch(n int64) int64 {
	if n == 0 {
		return 0
	}
	// ~1e11 ms is year 1973; real millisecond stamps are far larger than that
	// while second stamps are far smaller.
	if n < 100_000_000_000 {
		return n * 1000
	}
	return n
}

// ---------------------------------------------------------------------------
// resource extraction
// ---------------------------------------------------------------------------

// resourceSummaryFromRaw converts one raw package/resource object.
func resourceSummaryFromRaw(raw map[string]any, now time.Time) creditResource {
	res := creditResource{
		PackageCode: firstString(raw, "PackageCode", "packageCode"),
		PackageName: firstString(raw, "PackageName", "packageName"),
	}

	// Usage may nest under a slice detail object.
	slice, _ := firstMap(raw, "SlicePeriodUsageDetails", "slicePeriodUsageDetails")

	rawRemaining, hasRemaining := firstNumberFrom(raw, slice,
		"RemainingAmount", "remainingAmount", "Remaining", "remaining",
		"CycleCapacityRemain", "cycleCapacityRemain")
	rawUsed, hasUsed := firstNumberFrom(raw, slice,
		"UsedAmount", "usedAmount", "Used", "used")
	rawTotal, hasTotal := firstNumberFrom(raw, slice,
		"TotalAmount", "totalAmount", "Total", "total",
		"CycleCapacitySize", "cycleCapacitySize")

	total := rawTotal
	if !hasTotal {
		if hasRemaining && hasUsed {
			total = rawRemaining + rawUsed
		} else if hasRemaining {
			total = rawRemaining
		} else if hasUsed {
			total = rawUsed
		}
	}
	if total < 0 {
		total = 0
	}

	remaining := rawRemaining
	if !hasRemaining {
		remaining = clampNonNegative(total - rawUsed)
	}
	if remaining < 0 {
		remaining = 0
	}

	used := rawUsed
	if !hasUsed {
		used = clampNonNegative(total - remaining)
	}
	if used < 0 {
		used = 0
	}

	res.Total = total
	res.Remaining = remaining
	res.Used = used

	if status, okStatus := firstNumberFrom(raw, nil, "Status", "status"); okStatus {
		res.Status = int64(status)
	}

	res.ExpireAt = resolveExpireAt(raw, now)
	if res.ExpireAt > 0 {
		nowSec := now.Unix()
		res.Expired = res.ExpireAt <= nowSec
		res.ExpiringSoon = !res.Expired &&
			res.ExpireAt-nowSec <= int64(expiringSoonDays)*24*3600
	}
	return res
}

// collectResources walks a response body looking for package/resource arrays.
//
// The three endpoints differ in shape (a summary object, or a packages list),
// so the walker is deliberately permissive and keeps descending into every
// object.
//
// Two rules keep it accurate:
//
//   - a container holding a resource list is not itself a resource, even when
//     it carries amount fields of its own ({"TotalAmount":..,"Resources":[..]})
//   - a leaf object with amount fields is a resource even without an explicit
//     package code, as long as it sits inside a resource container — the
//     provider omits the code on nested entries
func collectResources(body []byte, now time.Time) []creditResource {
	var doc any
	if len(body) == 0 || json.Unmarshal(body, &doc) != nil {
		return nil
	}

	var out []creditResource
	var walk func(node any, depth int, inResourceContainer bool)
	walk = func(node any, depth int, inResourceContainer bool) {
		if depth > 12 {
			return
		}
		switch t := node.(type) {
		case map[string]any:
			nested := holdsNestedResources(t)
			if looksLikeResource(t, inResourceContainer) && !nested {
				out = append(out, resourceSummaryFromRaw(t, now))
			}
			// Descend, marking children of a resource container as being
			// inside one so bare amount objects are accepted there.
			childInContainer := nested || inResourceContainer
			for _, v := range t {
				walk(v, depth+1, childInContainer)
			}
		case []any:
			for _, v := range t {
				walk(v, depth+1, inResourceContainer)
			}
		}
	}
	walk(doc, 0, false)
	return out
}

// holdsNestedResources reports whether an object contains a child list or object
// of resources, which makes it a container rather than a leaf resource.
func holdsNestedResources(m map[string]any) bool {
	for _, key := range []string{"Resources", "resources", "Packages", "packages", "Items", "items"} {
		if v, ok := m[key]; ok {
			switch t := v.(type) {
			case []any:
				return len(t) > 0
			case map[string]any:
				return len(t) > 0
			}
		}
	}
	return false
}

// looksLikeResource decides whether an object carries credit figures.
//
// inContainer widens the test: entries nested inside a resource container need
// no explicit package code, because the provider omits it there.
func looksLikeResource(m map[string]any, inContainer bool) bool {
	hasAmount := false
	for _, k := range []string{
		"RemainingAmount", "remainingAmount", "Remaining", "remaining",
		"CycleCapacityRemain", "cycleCapacityRemain",
		"TotalAmount", "totalAmount", "Total", "total",
		"CycleCapacitySize", "cycleCapacitySize",
	} {
		if _, ok := m[k]; ok {
			hasAmount = true
			break
		}
	}
	if !hasAmount {
		return false
	}
	if inContainer {
		return true
	}
	// At the top level, require an expiry or a package identity too, so
	// unrelated numeric objects are not mistaken for resources.
	for _, k := range []string{
		"DeductionEndTime", "deductionEndTime", "ExpiredTime", "expiredTime",
		"CycleEndTime", "cycleEndTime", "PackageCode", "packageCode",
	} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// aggregateResources folds resources into one summary.
//
// Duplicates are collapsed first: the three endpoints can each mention the same
// package (summary carries the current cycle, the paid/free endpoints carry the
// package list), and counting a package three times would inflate the total.
// Two resources are considered the same when their package code and name match
// and their figures agree.
func aggregateResources(resources []creditResource, now time.Time) creditSummary {
	resources = dedupeResources(resources)

	sum := creditSummary{Resources: resources, Known: true}
	nowSec := now.Unix()

	for _, r := range resources {
		sum.Total += r.Total
		sum.Remaining += r.Remaining
		sum.Used += r.Used
		if r.Expired {
			sum.Expired = true
			sum.ExpiredRemaining += r.Remaining
		}
		if r.ExpireAt > 0 {
			if sum.SoonestExpireAt == 0 || r.ExpireAt < sum.SoonestExpireAt {
				sum.SoonestExpireAt = r.ExpireAt
			}
		}
	}
	if sum.SoonestExpireAt > 0 {
		sum.ExpiringSoon = !sum.Expired &&
			sum.SoonestExpireAt-nowSec <= int64(expiringSoonDays)*24*3600
	}
	return sum
}

// dedupeResources removes repeats that survive across the three endpoints.
func dedupeResources(in []creditResource) []creditResource {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]creditResource, 0, len(in))
	for _, r := range in {
		key := r.PackageCode + "|" + r.PackageName + "|" +
			formatFloat(r.Total) + "|" + formatFloat(r.Remaining) + "|" + formatFloat(r.Used)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

// fetchCredits queries the three new endpoints, falling back to the legacy one.
//
// Path candidates come from the variant's billingPaths(); only a 404 advances
// to the next candidate, mirroring the reference implementation's
// post_with_fallback + is_route_missing.
func (c *workBuddyClient) fetchCredits(ctx context.Context, creds *workBuddyCredentials) creditSummary {
	variant := variantForDomain(creds.Domain)
	now := time.Now()

	paths := []string{resourceSummaryPath, resourcePaidPath, resourceFreePath}
	var all []creditResource
	anySuccess := false
	lastErr := ""

	for _, path := range paths {
		body, errReq := c.postBillingWithFallback(ctx, creds, variant, path, map[string]any{})
		if errReq != nil {
			lastErr = errReq.Error()
			continue
		}
		if len(body) == 0 {
			continue
		}
		anySuccess = true
		all = append(all, collectResources(body, now)...)
	}

	if anySuccess && len(all) > 0 {
		return aggregateResources(all, now)
	}

	// Fall back to the legacy summed endpoint, which the APK used.
	legacy := c.fetchLegacyCredits(ctx, creds)
	if legacy.Known && legacy.Remaining == 0 && len(all) == 0 {
		return legacy
	}
	if len(all) > 0 {
		return aggregateResources(all, now)
	}
	if !legacy.Known {
		legacy.Error = firstNonEmpty(lastErr, legacy.Error)
	}
	return legacy
}

// postBillingWithFallback tries each variant path, advancing only on 404.
func (c *workBuddyClient) postBillingWithFallback(
	ctx context.Context,
	creds *workBuddyCredentials,
	variant wbVariant,
	path string,
	body map[string]any,
) ([]byte, error) {
	for _, candidate := range variant.billingPaths(path) {
		raw, status, errPost := c.postBilling(ctx, creds, variant, candidate, body)
		if errPost != nil {
			return nil, errPost
		}
		// Only a missing route justifies trying the next candidate.
		if status != http.StatusNotFound {
			return raw, nil
		}
	}
	return nil, nil
}

// postBilling performs one billing POST.
func (c *workBuddyClient) postBilling(
	ctx context.Context,
	creds *workBuddyCredentials,
	variant wbVariant,
	path string,
	body map[string]any,
) ([]byte, int, error) {
	if creds == nil || creds.AccessToken == "" {
		return nil, 0, errors.New("缺少访问令牌")
	}
	endpoint := variant.apiBase() + path

	payload, errMarshal := json.Marshal(body)
	if errMarshal != nil {
		return nil, 0, errMarshal
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if errRequest != nil {
		return nil, 0, errRequest
	}
	applyWorkBuddyHeadersVariant(req.Header, creds, variant)

	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return nil, 0, fmt.Errorf("请求失败: %w", errDo)
	}
	defer resp.Body.Close()

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取响应失败: %w", errRead)
	}
	return raw, resp.StatusCode, nil
}

// fetchLegacyCredits ports the APK's summed query (a2/b.java:406) so older
// deployments still produce a figure.
func (c *workBuddyClient) fetchLegacyCredits(ctx context.Context, creds *workBuddyCredentials) creditSummary {
	variant := variantForDomain(creds.Domain)
	body, status, errPost := c.postBilling(ctx, creds, variant, legacyResourcePath, map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              productCode,
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": time.Now().Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   time.Now().Add(100 * 365 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	if errPost != nil {
		return creditSummary{Error: errPost.Error()}
	}
	if status < 200 || status >= 300 {
		return creditSummary{Error: fmt.Sprintf("上游 HTTP %d", status)}
	}

	sum, errLegacy := parseLegacyRemainder(body)
	if errLegacy != nil {
		return creditSummary{Error: errLegacy.Error()}
	}
	return sum
}

// parseLegacyRemainder sums the APK-era response shape.
func parseLegacyRemainder(body []byte) (creditSummary, error) {
	var doc struct {
		Data *struct {
			Response *struct {
				Data *struct {
					Accounts []struct {
						CycleCapacitySize   *json.Number `json:"CycleCapacitySize"`
						CycleCapacityRemain *json.Number `json:"CycleCapacityRemain"`
						CapacityRemain      *json.Number `json:"CapacityRemain"`
					} `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	if len(body) == 0 || json.Unmarshal(body, &doc) != nil {
		return creditSummary{Error: "查询额度失败"}, nil
	}
	if doc.Data == nil {
		return creditSummary{Error: "响应缺少 data"}, nil
	}

	var sum float64
	if doc.Data.Response != nil && doc.Data.Response.Data != nil {
		for _, acc := range doc.Data.Response.Data.Accounts {
			size := numberOrZero(acc.CycleCapacitySize)
			remain := numberOrZero(acc.CycleCapacityRemain)
			if size <= 0 && remain <= 0 {
				remain = numberOrZero(acc.CapacityRemain)
			}
			if remain > 0 {
				sum += remain
			}
		}
	}
	return creditSummary{Remaining: sum, Total: sum, Known: true}, nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func clampNonNegative(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func parseInt64(s string) (int64, error) {
	var n int64
	neg := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 && (c == '-' || c == '+') {
			neg = c == '-'
			continue
		}
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int64(c-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

func numberOrZero(n *json.Number) float64 {
	if n == nil {
		return 0
	}
	if v, errInt := n.Int64(); errInt == nil {
		return float64(v)
	}
	if v, errFloat := n.Float64(); errFloat == nil {
		return v
	}
	return 0
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, okString := v.(string); okString && trimSpace(s) != "" {
				return trimSpace(s)
			}
		}
	}
	return ""
}

func firstMap(m map[string]any, keys ...string) (map[string]any, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if mm, okMap := v.(map[string]any); okMap {
				return mm, true
			}
		}
	}
	return nil, false
}

// firstNumberFrom looks up the first numeric value across the primary map and
// an optional nested one.
func firstNumberFrom(primary, nested map[string]any, keys ...string) (float64, bool) {
	for _, src := range []map[string]any{primary, nested} {
		if src == nil {
			continue
		}
		for _, k := range keys {
			v, ok := src[k]
			if !ok {
				continue
			}
			if f, okNum := toFloat(v); okNum {
				return f, true
			}
		}
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int64:
		return float64(t), true
	case int:
		return float64(t), true
	case json.Number:
		if f, errFloat := t.Float64(); errFloat == nil {
			return f, true
		}
	case string:
		if f, errParse := parseFloat(trimSpace(t)); errParse == nil {
			return f, true
		}
	}
	return 0, false
}

func parseFloat(s string) (float64, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	var (
		intPart, fracPart float64
		seenDot, neg      bool
		started           bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-' && i == 0:
			neg = true
		case c == '+' && i == 0:
		case c >= '0' && c <= '9':
			started = true
			if seenDot {
				fracPart = fracPart*10 + float64(c-'0')
			} else {
				intPart = intPart*10 + float64(c-'0')
			}
		case c == '.' && !seenDot:
			seenDot = true
		default:
			return 0, errors.New("not a number")
		}
	}
	if !started {
		return 0, errors.New("not a number")
	}
	value := intPart
	if seenDot {
		scale := 1.0
		for i := 0; i < len(s); i++ {
			if s[i] == '.' {
				scale = 1
				for j := i + 1; j < len(s); j++ {
					scale *= 10
				}
				break
			}
		}
		value += fracPart / scale
	}
	if neg {
		value = -value
	}
	return value, nil
}
