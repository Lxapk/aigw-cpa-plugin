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
	"sync"
)

// This file implements the WorkBuddy daily check-in, ported from
// AI 聚合网关 0.1.18 provider a2/b.
//
// Call chain in the source app:
//
//	UI "批量签到" (N1/C0290r0.java:148)
//	  -> N1/i1.java:35   engine.h("codebuddy", uid, "checkin", cb)
//	  -> N1/C0284o.java  engine.i(providerId, uid, "checkin", cb)
//	  -> V1/k.java:462   provider.e(account, "checkin", cb)
//	  -> a2/b.java:496   e(account, action, cb)      <- 1375 insns, read from smali
//
// The endpoint itself (smali a2/b.smali around line 2643):
//
//	POST {checkinBase}/v2/billing/meter/daily-checkin
//	     body: {}
//	     base = domain=="global" ? https://www.workbuddy.ai : https://www.codebuddy.cn
//
// Note the check-in base differs from the chat/models base: for the cn domain
// chat uses copilot.tencent.com but check-in uses www.codebuddy.cn.
const (
	// workBuddyCheckinPath is the daily check-in endpoint (a2/b.smali:2643).
	workBuddyCheckinPath = "/v2/billing/meter/daily-checkin"
)

// checkinOutcome mirrors Y1.o seen from the caller's perspective.
type checkinOutcome struct {
	// Success reports whether the operation is considered successful. It is
	// true both for a fresh check-in and for "already checked in today", which
	// the source app treats as success (idempotent).
	Success bool
	// Message is the user-facing text, matching the source strings exactly.
	Message string
	// AlreadyCheckedIn distinguishes "今日已签到" from a fresh "签到成功".
	AlreadyCheckedIn bool
	// Code is the upstream response code when available (-1 when unparsable).
	Code int
	// HTTPStatus is the HTTP status of the upstream call.
	HTTPStatus int
	// Raw is the upstream response body, kept for troubleshooting.
	Raw string
}

// workBuddyCheckinBase ports the base selection used by the check-in call
// (smali a2/b.smali:2660-2675): global -> workbuddy.ai, otherwise codebuddy.cn.
//
// The cn branch is redirectable so tests can point it at a local server.
func workBuddyCheckinBase(domain string) string {
	if isWorkBuddyGlobalDomain(domain) {
		return workBuddyGlobalBase()
	}
	return checkinBaseForTest()
}

var (
	workBuddyCheckinMu     sync.RWMutex
	workBuddyCheckinBaseCN = "https://www.codebuddy.cn"
)

func checkinBaseForTest() string {
	workBuddyCheckinMu.RLock()
	defer workBuddyCheckinMu.RUnlock()
	return workBuddyCheckinBaseCN
}

func setCheckinBase(v string) {
	workBuddyCheckinMu.Lock()
	workBuddyCheckinBaseCN = v
	workBuddyCheckinMu.Unlock()
}

// checkin performs one daily check-in for a credential.
//
// Response handling mirrors the source exactly:
//
//	if HTTP 2xx {
//	    code := body.code            // -1 when the field is absent/unparsable
//	    if code == 0 {
//	        success("签到成功")
//	    } else if body contains "已签到" or "already" {
//	        success("今日已签到")     // idempotent
//	    } else {
//	        failure(body.message or "签到失败（code=N）")
//	    }
//	} else {
//	    failure("签到失败（HTTP <status>）：<body>")
//	}
func (c *workBuddyClient) checkin(ctx context.Context, creds *workBuddyCredentials) (*checkinOutcome, error) {
	if creds == nil || creds.AccessToken == "" {
		return nil, errors.New("缺少访问令牌")
	}

	endpoint := workBuddyCheckinBase(creds.Domain) + workBuddyCheckinPath
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if errRequest != nil {
		return nil, errRequest
	}
	applyWorkBuddyHeaders(req.Header, creds)

	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("签到请求失败: %w", errDo)
	}
	defer resp.Body.Close()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if errRead != nil {
		return nil, fmt.Errorf("读取签到响应失败: %w", errRead)
	}

	return interpretCheckinResponse(resp.StatusCode, body), nil
}

// interpretCheckinResponse applies the source app's success/failure rules,
// extended with the codes observed against the live provider.
//
// Live evidence (2026-09-23): a repeat check-in returns HTTP **400** with
//
//	{"code":10001,"msg":"今天已签到，请明天再来"}
//
// The APK's own rule set treated any non-2xx as a hard failure, which
// mislabelled an idempotent repeat as an error. The provider reports the
// semantic outcome in "code", not in the HTTP status, so the envelope is parsed
// first and the status only decides when no idempotency marker is present.
func interpretCheckinResponse(statusCode int, body []byte) *checkinOutcome {
	out := &checkinOutcome{
		HTTPStatus: statusCode,
		Raw:        truncateString(string(body), 2000),
		Code:       -1,
	}

	// Parse the envelope regardless of status.
	if len(body) > 0 {
		var doc struct {
			Code    *json.Number `json:"code"`
			Msg     string       `json:"msg"`
			Message string       `json:"message"`
		}
		if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal == nil {
			if doc.Code != nil {
				if n, errInt := doc.Code.Int64(); errInt == nil {
					out.Code = int(n)
				}
			}
			if strings.TrimSpace(doc.Message) != "" {
				out.Message = strings.TrimSpace(doc.Message)
			} else if strings.TrimSpace(doc.Msg) != "" {
				out.Message = strings.TrimSpace(doc.Msg)
			}
		}
	}

	// code 0 on a 2xx is a fresh success.
	if out.Code == 0 && statusCode >= 200 && statusCode < 300 {
		out.Success = true
		out.Message = "签到成功"
		return out
	}

	// Idempotency markers win over the HTTP status: the live provider answers
	// HTTP 400 for "already checked in today".
	text := string(body)
	if containsSubstringFold(text, "已签到") || containsSubstringFold(text, "already") {
		out.Success = true
		out.AlreadyCheckedIn = true
		out.Message = "今日已签到"
		return out
	}

	// Genuine non-2xx without an idempotency marker.
	if statusCode < 200 || statusCode >= 300 {
		out.Success = false
		if out.Message == "" {
			out.Message = fmt.Sprintf("签到失败（HTTP %d）", statusCode)
		}
		return out
	}

	// 2xx with a non-zero code and no marker: a real failure.
	out.Success = false
	if out.Message == "" {
		out.Message = fmt.Sprintf("签到失败（code=%d）", out.Code)
	}
	return out
}

// containsSubstringFold is a case-insensitive substring check.
//
// Named distinctly from routing.go's containsFold (which tests set membership),
// because both live in this package.
func containsSubstringFold(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
