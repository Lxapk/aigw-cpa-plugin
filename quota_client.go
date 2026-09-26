package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file ports the WorkBuddy quota ("剩余额度") query from
// AI 聚合网关 0.1.18 provider a2/b.
//
// Endpoint (a2/b.java:406, and again at :608 for the batched path):
//
//	POST {base}/v2/billing/meter/get-user-resource
//	     base = domain=="global" ? https://www.workbuddy.ai : https://www.codebuddy.cn
//
// Request body (a2/b.java:290 F()):
//
//	PageNumber=1, PageSize=100, ProductCode="p_tcaca", Status=[0,3],
//	PackageEndTimeRangeBegin/End = now .. now + 3185136000000ms
//
// Response shape (note the capitalised nesting):
//
//	data.Response.Data.Accounts[] {
//	    CycleCapacitySize    cycle capacity total
//	    CycleCapacityRemain  cycle capacity remaining
//	    CapacityRemain       fallback remaining
//	}
//
// Aggregation (a2/b.java:432-450):
//
//	sum := 0
//	for acc := range Accounts {
//	    size   := acc.CycleCapacitySize
//	    remain := acc.CycleCapacityRemain
//	    if size <= 0 && remain <= 0 { remain = acc.CapacityRemain }
//	    if remain > 0 { sum += remain }        // only positive remainders count
//	}
//
// Errors (a2/b.java:414-421):
//
//	non-2xx      -> credits 0, "上游 HTTP <code>" + body
//	missing data  -> credits 0, "响应缺少 data"
//	other         -> credits 0, exception message ?: "查询额度失败"

const (
	// workBuddyQuotaPath is the credit/usage endpoint (a2/b.java:406).
	workBuddyQuotaPath = "/v2/billing/meter/get-user-resource"
	// workBuddyQuotaProductCode is the fixed product code from F().
	workBuddyQuotaProductCode = "p_tcaca"
	// workBuddyQuotaPageSize mirrors PageSize=100 in F().
	workBuddyQuotaPageSize = 100
	// workBuddyQuotaRangeMillis mirrors the 3185136000000ms span in F().
	workBuddyQuotaRangeMillis = int64(3185136000000)
)

// workBuddyQuota is the parsed result of one account's credit query.
type workBuddyQuota struct {
	// Credits is the summed remaining capacity across resources.
	Credits int64
	// Known reports whether the provider returned usable numbers.
	Known bool
	// Message mirrors the app's user-facing summary.
	Message string
	// Detail carries per-account breakdown for the UI.
	Detail string
	// HTTPStatus is the upstream status when a call was made.
	HTTPStatus int
	// Err is set when the query failed; Credits is then 0.
	Err string

	// Summary is the full credit model, including expiry. It is what makes
	// "use the soonest-expiring balance first" possible.
	Summary creditSummary

	// Labels are the human-readable package names, for the panel.
	Labels []string
}

// soonestExpireAt exposes the summary's earliest expiry (epoch seconds, 0 when
// nothing expires).
func (q *workBuddyQuota) soonestExpireAt() int64 {
	if q == nil {
		return 0
	}
	return q.Summary.SoonestExpireAt
}

// expiringSoon reports whether the soonest expiry is within 7 days.
func (q *workBuddyQuota) expiringSoon() bool {
	if q == nil {
		return false
	}
	return q.Summary.ExpiringSoon
}

// expired reports whether any resource has already expired.
func (q *workBuddyQuota) expired() bool {
	if q == nil {
		return false
	}
	return q.Summary.Expired
}

// workBuddyQuotaBase ports the base selection for the quota call
// (a2/b.java:406): global -> workbuddy.ai, otherwise codebuddy.cn.
//
// Note this is the codebuddy.cn host, unlike chat/models which use
// copilot.tencent.com.
//
// Resolution goes through the variant layer so a forced override is honoured.
func workBuddyQuotaBase(domain string) string {
	if variantForCredentials(&workBuddyCredentials{Domain: domain}) == variantAi {
		return workBuddyGlobalBase()
	}
	return checkinBaseForTest()
}

// isWorkBuddyGlobalDomain ports a2/b.java:284 D():
//
//	d := cred.domain.toLowerCase()
//	if strings.HasSuffix(d, ".workbuddy.ai") || d == "workbuddy.ai" { return "global" }
//	if len(d) > 0 { return "cn" }
//	return <configured default>
//
// A bare "workbuddy.ai" also counts as global, and the check is a suffix test
// so "www.workbuddy.ai" is accepted.
func isWorkBuddyGlobalDomain(domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "" {
		return false
	}
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// quotaRequestBody builds the F() payload from a2/b.java:290.
func quotaRequestBody(now time.Time) []byte {
	layout := "2006-01-02 15:04:05"
	begin := now.Format(layout)
	end := time.UnixMilli(now.UnixMilli() + workBuddyQuotaRangeMillis).Format(layout)

	doc := map[string]any{
		"PageNumber":               1,
		"PageSize":                 workBuddyQuotaPageSize,
		"ProductCode":              workBuddyQuotaProductCode,
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": begin,
		"PackageEndTimeRangeEnd":   end,
	}
	raw, _ := json.Marshal(doc)
	return raw
}

// fetchQuota performs the credit query for one credential.
//
// It prefers the three new endpoints (which carry expiry timestamps) and falls
// back to the APK-era summed endpoint, per the reference implementation.
func (c *workBuddyClient) fetchQuota(ctx context.Context, creds *workBuddyCredentials) (*workBuddyQuota, error) {
	if creds == nil || creds.AccessToken == "" {
		return nil, errors.New("缺少访问令牌")
	}

	summary := c.fetchCredits(ctx, creds)
	out := &workBuddyQuota{
		Credits: int64(summary.Remaining),
		Known:   summary.Known,
		Summary: summary,
		Err:     summary.Error,
	}
	if summary.Known {
		out.Message = fmt.Sprintf("周期剩余 %d", int64(summary.Remaining))
		out.Detail = fmt.Sprintf("%d 个额度包", len(summary.Resources))
		for _, res := range summary.Resources {
			label := res.PackageName
			if label == "" {
				label = res.PackageCode
			}
			if label != "" {
				out.Labels = append(out.Labels, label)
			}
		}
	}
	return out, nil
}

// interpretQuotaResponse applies the app's parsing and aggregation rules to the
// legacy endpoint. Kept for the parse tests that pin the original behaviour.
func interpretQuotaResponse(statusCode int, body []byte) *workBuddyQuota {
	out := &workBuddyQuota{HTTPStatus: statusCode}

	// a2/b.java:414 — non-2xx.
	if statusCode < 200 || statusCode >= 300 {
		out.Err = fmt.Sprintf("上游 HTTP %d%s", statusCode, truncateString(string(body), 300))
		return out
	}

	sum, errLegacy := parseLegacyRemainder(body)
	if errLegacy != nil {
		out.Err = errLegacy.Error()
		return out
	}
	if sum.Error != "" {
		out.Err = sum.Error
		return out
	}
	out.Credits = int64(sum.Remaining)
	out.Known = sum.Known
	out.Message = fmt.Sprintf("周期剩余 %d", out.Credits)
	out.Summary = sum
	return out
}

// interpretQuotaResponse applies the app's parsing and aggregation rules.
// It is separated from the HTTP call so the decision table is unit-testable.
// quotaNumber reads a json.Number, treating absent/invalid as 0.
func quotaNumber(n *json.Number) int64 {
	if n == nil {
		return 0
	}
	v, errInt := n.Int64()
	if errInt != nil {
		// Some payloads use floats; fall back to a float parse.
		f, errFloat := n.Float64()
		if errFloat != nil {
			return 0
		}
		return int64(f)
	}
	return v
}
