package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// These tests lock in the "blank page after clicking 保存/签到" defect.
//
// Root cause: the check-in page is served from a *resource* route
// (/v0/resource/plugins/<id>/checkin) and CPA serves resource routes with GET
// only — internal/pluginhost/management.go:295 returns false for any other
// method, so the POST never reached the plugin and the browser got an empty
// body. Forms must post to the *management* mount instead, which accepts any
// method and forwards the body (management.go:232).

// TestCheckinFormPostsToManagementMount is the regression test: a relative
// action would resolve against the GET-only resource path.
func TestCheckinFormPostsToManagementMount(t *testing.T) {
	resetState()
	page := checkinPage()

	want := `action="/v0/management/` + pluginName + `/checkin"`
	if !strings.Contains(page, want) {
		t.Fatalf("check-in form must post to %s (a relative action lands on the GET-only resource route and yields a blank page)\npage excerpt:\n%s",
			want, excerptAround(page, "action=", 200))
	}
	// No relative form action may remain.
	if strings.Contains(page, `action="checkin"`) {
		t.Fatal(`page still contains a relative action="checkin"`)
	}
}

// TestCheckinManagementPathIsRegistered makes sure the mount the form targets
// actually exists for both GET (page) and POST (submission).
func TestCheckinManagementPathIsRegistered(t *testing.T) {
	resetState()
	reg := managementRegistration()

	byMethod := map[string]bool{}
	for _, r := range reg.Routes {
		if r.Path == "/aigw-reverse-proxy/checkin" {
			byMethod[strings.ToUpper(r.Method)] = true
		}
	}
	if !byMethod[http.MethodGet] {
		t.Error("GET /aigw-reverse-proxy/checkin must be registered so the page is reachable")
	}
	if !byMethod[http.MethodPost] {
		t.Error("POST /aigw-reverse-proxy/checkin must be registered so the form submits")
	}
}

// TestCheckinPageReachableViaManagementPath renders the page through the same
// path the browser uses after a form post.
func TestCheckinPageReachableViaManagementPath(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    "/v0/management/" + pluginName + "/checkin",
		Headers: http.Header{"Accept": []string{"text/html"}},
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", mr.StatusCode)
	}
	if len(mr.Body) == 0 {
		t.Fatal("page body is empty — this is the blank-screen symptom")
	}
	if !strings.Contains(string(mr.Body), "立即为所有账号签到") {
		t.Fatalf("page did not render the manual button: %s", excerptAround(string(mr.Body), "手动", 200))
	}
}

// TestCheckinPostViaManagementPathReturnsHTML ensures a form submission renders
// a real page rather than an empty body.
func TestCheckinPostViaManagementPathReturnsHTML(t *testing.T) {
	resetState()

	form := url.Values{"action": {"save"}, "hour": {"10"}, "minute": {"15"}}
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    "/v0/management/" + pluginName + "/checkin",
		Headers: http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}},
		Body:    []byte(form.Encode()),
	})
	var mr managementResponse
	mustDecode(t, res, &mr)

	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", mr.StatusCode)
	}
	if len(mr.Body) == 0 {
		t.Fatal("POST returned an empty body — the browser would show a blank page")
	}
	if ct := mr.Headers.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	body := string(mr.Body)
	if !strings.Contains(body, "设置已保存") {
		t.Fatalf("no confirmation banner in response: %s", excerptAround(body, "已保存", 200))
	}
	if !strings.Contains(body, "立即为所有账号签到") {
		t.Fatal("response is not a full page (manual button missing)")
	}

	cfg := state.settings.get().Checkin
	if cfg.Hour != 10 || cfg.Minute != 15 {
		t.Fatalf("config not applied: %+v", cfg)
	}
}

// TestCheckinUnknownActionStillRendersPage guards the other way to a blank
// screen: a POST with no recognisable action used to return an empty 400.
func TestCheckinUnknownActionStillRendersPage(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    "/v0/management/" + pluginName + "/checkin",
		Headers: http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}},
		Body:    []byte("action=whatever"),
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a rendered page", mr.StatusCode)
	}
	if len(mr.Body) == 0 {
		t.Fatal("empty body would show a blank page")
	}
	if !strings.Contains(string(mr.Body), "未识别的操作") {
		t.Fatalf("expected a fallback notice, got %s", excerptAround(string(mr.Body), "未识别", 200))
	}
}

// TestCheckinRunFormReturnsFullPage verifies the manual-run submission renders
// the result table rather than nothing.
func TestCheckinRunFormReturnsFullPage(t *testing.T) {
	resetState()
	restore := stubHostCall(func(_ string, _ any) (json.RawMessage, error) {
		return mustMarshal(t, map[string]any{"auths": []any{}}), nil
	})
	defer restore()

	form := url.Values{"action": {"run"}}
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    "/v0/management/" + pluginName + "/checkin",
		Headers: http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}},
		Body:    []byte(form.Encode()),
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", mr.StatusCode)
	}
	body := string(mr.Body)
	if len(body) == 0 {
		t.Fatal("empty body")
	}
	if !strings.Contains(body, "本次结果") {
		t.Fatalf("run result section missing: %s", excerptAround(body, "本次", 200))
	}
}

// TestResourceRouteStaysGetOnly documents the constraint that forced the
// management mount: the resource page is still served for GET.
func TestResourceRouteStaysGetOnly(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    "/v0/resource/plugins/" + pluginName + "/checkin",
		Headers: http.Header{"Accept": []string{"text/html"}},
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK || len(mr.Body) == 0 {
		t.Fatalf("resource GET should render the page (status=%d len=%d)", mr.StatusCode, len(mr.Body))
	}
}

// excerptAround helps produce readable failure output.
func excerptAround(s, needle string, span int) string {
	idx := strings.Index(s, needle)
	if idx < 0 {
		if len(s) > span {
			return s[:span] + "..."
		}
		return s
	}
	start := idx - span/2
	if start < 0 {
		start = 0
	}
	end := idx + span/2
	if end > len(s) {
		end = len(s)
	}
	return "..." + s[start:end] + "..."
}
