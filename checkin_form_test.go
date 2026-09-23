package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// These tests cover the check-in page's browser-side key handling.
//
// Background: CPA authenticates management calls from a request header
// (Authorization: Bearer <key> / X-Management-Key, see
// internal/api/handlers/management/handler.go:276), and the resource route the
// page loads from is GET-only. An HTML form can therefore neither reach the
// management route nor attach the key. The page instead keeps the key in
// localStorage and calls the endpoints with fetch(), so these tests assert the
// page ships that machinery and that no HTML form remains.

// TestPageKeepsKeyInBrowser asserts the key is client-side only.
func TestPageKeepsKeyInBrowser(t *testing.T) {
	resetState()
	page := checkinPage()

	checks := map[string]string{
		"localStorage key name":   checkinKeyStorageName,
		"save-to-browser button":  "保存到浏览器",
		"key input is a password": `type="password"`,
		"explicit privacy note":   "不会上传到插件或服务器",
	}
	for what, want := range checks {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %s (%q)", what, want)
		}
	}
}

// TestPageHasNoHtmlForm is the regression guard for the blank-page and
// missing-management-key defects: an HTML form submission cannot satisfy both
// the GET-only resource route and the header-based management auth.
func TestPageHasNoHtmlForm(t *testing.T) {
	resetState()
	page := checkinPage()
	if strings.Contains(page, "<form") {
		t.Fatalf("check-in page must not use HTML forms; got:\n%s", excerptAround(page, "<form", 300))
	}
}

// TestPageCallsManagementEndpoints asserts fetch() targets the management mount
// and attaches the key as a header.
func TestPageCallsManagementEndpoints(t *testing.T) {
	resetState()
	page := checkinPage()

	mount := managementBasePath() + "/" + pluginName + "/checkin"
	for _, want := range []string{
		mount + "'",                  // base path constant
		"/config'",                   // save config
		"/run'",                      // manual run
		"'Authorization': 'Bearer '", // header attachment
		"'X-Management-Key': k",      // fallback header CPA also accepts
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

// TestPageReportsMissingKey ensures the user gets a clear instruction instead
// of a bare failure when no key has been saved yet.
func TestPageReportsMissingKey(t *testing.T) {
	resetState()
	page := checkinPage()
	if !strings.Contains(page, "请先在上方保存管理密钥") {
		t.Fatal("page should tell the user to save a key before calling the API")
	}
	if !strings.Contains(page, "missing management key") {
		t.Fatal("page should mention the upstream error users previously hit")
	}
}

// TestPageRendersRunControls verifies the three controls exist.
func TestPageRendersRunControls(t *testing.T) {
	resetState()
	page := checkinPage()
	for _, id := range []string{"mgmtKey", "ckEnabled", "ckHour", "ckMinute", "ckOnStart", "btnRun", "runMsg", "runResult"} {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("page missing control #%s", id)
		}
	}
}

// ---- server-side endpoints still work (used by fetch) -------------------

// TestCheckinManagementEndpointsReachable confirms the endpoints the page calls
// exist and function; CPA gates them behind the key, which fetch supplies.
func TestCheckinManagementEndpointsReachable(t *testing.T) {
	resetState()
	restore := stubHostCall(func(_ string, _ any) (json.RawMessage, error) {
		return mustMarshal(t, map[string]any{"auths": []any{}}), nil
	})
	defer restore()

	// POST /checkin/run
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementBasePath() + "/" + pluginName + "/checkin/run",
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("run status = %d", mr.StatusCode)
	}
	var run checkinRun
	if errUnmarshal := json.Unmarshal(mr.Body, &run); errUnmarshal != nil {
		t.Fatalf("run body not JSON: %v", errUnmarshal)
	}
	if run.Trigger != "manual" {
		t.Fatalf("trigger = %q", run.Trigger)
	}

	// POST /checkin/config
	res = callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   managementBasePath() + "/" + pluginName + "/checkin/config",
		Body:   []byte(`{"enabled":true,"hour":7,"minute":5}`),
	})
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("config status = %d (%s)", mr.StatusCode, mr.Body)
	}
	cfg := state.settings.get().Checkin
	if !cfg.Enabled || cfg.Hour != 7 || cfg.Minute != 5 {
		t.Fatalf("config = %+v", cfg)
	}
	stopCheckinScheduler()

	// GET /checkin/status
	res = callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   managementBasePath() + "/" + pluginName + "/checkin/status",
	})
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", mr.StatusCode)
	}
}

// TestCheckinResourcePageServedForGet keeps the browsable page working.
func TestCheckinResourcePageServedForGet(t *testing.T) {
	resetState()
	res := callOK(t, pluginabi.MethodManagementHandle, pluginapi.ManagementRequest{
		Method:  http.MethodGet,
		Path:    resourceBasePath() + "/" + pluginName + "/checkin",
		Headers: http.Header{"Accept": []string{"text/html"}},
	})
	var mr managementResponse
	mustDecode(t, res, &mr)
	if mr.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", mr.StatusCode)
	}
	if len(mr.Body) == 0 {
		t.Fatal("empty body — blank page symptom")
	}
	if !strings.Contains(string(mr.Body), "立即为所有账号签到") {
		t.Fatal("page did not render the manual button")
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
