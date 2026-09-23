package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// workBuddyQuota is the parsed result of one account's quota query.
type workBuddyQuota struct {
	// Credits is the summed cycle remaining capacity (d.f3827b in the app).
	Credits int64
	// Known reports whether the provider returned usable numbers.
	Known bool
	// Message mirrors the app's user-facing summary ("周期剩余 N").
	Message string
	// Detail carries per-account breakdown for the UI.
	Detail string
	// HTTPStatus is the upstream status when a call was made.
	HTTPStatus int
	// Err is set when the query failed; Credits is then 0.
	Err string
}

// workBuddyQuotaBase ports the base selection for the quota call
// (a2/b.java:406): global -> workbuddy.ai, otherwise codebuddy.cn.
//
// Note this is the codebuddy.cn host, unlike chat/models which use
// copilot.tencent.com.
func workBuddyQuotaBase(domain string) string {
	if isWorkBuddyGlobalDomain(domain) {
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

// fetchQuota performs the quota query for one credential.
func (c *workBuddyClient) fetchQuota(ctx context.Context, creds *workBuddyCredentials) (*workBuddyQuota, error) {
	if creds == nil || creds.AccessToken == "" {
		return nil, errors.New("缺少访问令牌")
	}

	endpoint := workBuddyQuotaBase(creds.Domain) + workBuddyQuotaPath
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		bytes.NewReader(quotaRequestBody(time.Now())))
	if errRequest != nil {
		return nil, errRequest
	}
	applyWorkBuddyHeaders(req.Header, creds)

	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("查询额度失败: %w", errDo)
	}
	defer resp.Body.Close()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, fmt.Errorf("读取额度响应失败: %w", errRead)
	}

	return interpretQuotaResponse(resp.StatusCode, body), nil
}

// interpretQuotaResponse applies the app's parsing and aggregation rules.
// It is separated from the HTTP call so the decision table is unit-testable.
func interpretQuotaResponse(statusCode int, body []byte) *workBuddyQuota {
	out := &workBuddyQuota{HTTPStatus: statusCode}

	// a2/b.java:414 — non-2xx.
	if statusCode < 200 || statusCode >= 300 {
		out.Err = fmt.Sprintf("上游 HTTP %d%s", statusCode, truncateString(string(body), 300))
		return out
	}

	// The response nests under data.Response.Data.Accounts, all capitalised.
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
		out.Err = "查询额度失败"
		return out
	}
	// a2/b.java:418 — "响应缺少 data".
	if doc.Data == nil {
		out.Err = "响应缺少 data"
		return out
	}

	var sum int64
	var accounts int
	if doc.Data.Response != nil && doc.Data.Response.Data != nil {
		for _, acc := range doc.Data.Response.Data.Accounts {
			accounts++
			size := quotaNumber(acc.CycleCapacitySize)
			remain := quotaNumber(acc.CycleCapacityRemain)
			// a2/b.java:437 — fall back to CapacityRemain when the cycle
			// figures are absent or non-positive.
			if size <= 0 && remain <= 0 {
				remain = quotaNumber(acc.CapacityRemain)
			}
			// a2/b.java:441 — only positive remainders are summed.
			if remain > 0 {
				sum += remain
			}
		}
	}

	out.Credits = sum
	out.Known = true
	out.Message = fmt.Sprintf("周期剩余 %d", sum)
	out.Detail = fmt.Sprintf("%d 个额度包", accounts)
	return out
}

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
