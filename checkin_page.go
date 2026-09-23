package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file adds the check-in UI to the plugin's management surface:
//
//	GET  /checkin           HTML page (manual button + auto switch + results)
//	GET  /checkin/status    JSON status (config, running flag, history)
//	POST /checkin/run       trigger a manual run
//	POST /checkin/config    update the automatic-run settings
//
// Registered as an additional ManagementAPI resource next to /status.

// handleCheckinRequest dispatches the check-in management endpoints.
// It returns (response, handled); handled=false lets the caller fall through.
func handleCheckinRequest(req pluginapi.ManagementRequest) (managementResponse, bool) {
	path := normaliseManagementPath(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))

	switch path {
	case "/checkin":
		if method == http.MethodPost {
			// A form post from the HTML page: treat as run/config depending on
			// the "action" field.
			return handleCheckinPost(req)
		}
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    htmlResponseHeaders(),
			Body:       []byte(checkinPage()),
		}, true

	case "/checkin/status":
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(checkinStatusJSON()),
		}, true

	case "/checkin/run":
		if method != http.MethodPost {
			return managementResponse{
				StatusCode: http.StatusMethodNotAllowed,
				Headers:    jsonResponseHeaders(),
				Body:       mustJSON(map[string]any{"error": "POST required"}),
			}, true
		}
		run := runFromManagement()
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(run),
		}, true

	case "/checkin/config":
		if method != http.MethodPost {
			return managementResponse{
				StatusCode: http.StatusMethodNotAllowed,
				Headers:    jsonResponseHeaders(),
				Body:       mustJSON(map[string]any{"error": "POST required"}),
			}, true
		}
		cfg, errDecode := decodeCheckinConfigBody(req.Body)
		if errDecode != nil {
			return managementResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    jsonResponseHeaders(),
				Body:       mustJSON(map[string]any{"error": errDecode.Error()}),
			}, true
		}
		applied := applyCheckinConfig(cfg)

		// Start the scheduler the first time auto mode is enabled so the loop
		// exists in this process.
		if applied.Enabled {
			startCheckinScheduler()
		}
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(map[string]any{
				"ok":      true,
				"checkin": checkinStatusJSON(),
			}),
		}, true
	}

	return managementResponse{}, false
}

// handleCheckinPost handles an HTML form submission from the check-in page.
func handleCheckinPost(req pluginapi.ManagementRequest) (managementResponse, bool) {
	form := parseFormBody(req.Body, req.Headers)
	action := strings.ToLower(strings.TrimSpace(form.Get("action")))

	switch action {
	case "run":
		run := runFromManagement()
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    htmlResponseHeaders(),
			Body:       []byte(checkinPageWithRun(run)),
		}, true

	case "save":
		cfg := checkinSettings{
			Enabled:                  form.Get("enabled") == "on" || form.Get("enabled") == "true",
			Hour:                     atoiDefault(form.Get("hour"), 9),
			Minute:                   atoiDefault(form.Get("minute"), 0),
			OnStart:                  form.Get("on_start") == "on" || form.Get("on_start") == "true",
			RetryOnDeviceFingerprint: true,
		}
		applied := applyCheckinConfig(cfg)
		if applied.Enabled {
			startCheckinScheduler()
		}
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    htmlResponseHeaders(),
			Body:       []byte(checkinPageWithNotice("设置已保存")),
		}, true

	case "config":
		// JSON-ish form post from a script.
		cfg, errDecode := decodeCheckinConfigBody(req.Body)
		if errDecode != nil {
			return managementResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    jsonResponseHeaders(),
				Body:       mustJSON(map[string]any{"error": errDecode.Error()}),
			}, true
		}
		applied := applyCheckinConfig(cfg)
		if applied.Enabled {
			startCheckinScheduler()
		}
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(map[string]any{"ok": true, "checkin": checkinStatusJSON()}),
		}, true
	}

	// Unknown/missing action: render the page instead of returning nothing, so
	// the operator never sees a blank screen.
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    htmlResponseHeaders(),
		Body:       []byte(checkinPageWithNotice("未识别的操作，已显示当前状态")),
	}, true
}

// normaliseManagementPath strips the plugin-resource prefix so the caller can
// match on the tail (CPA may pass either form).
func normaliseManagementPath(path string) string {
	p := strings.TrimSuffix(strings.TrimSpace(path), "/")
	for _, prefix := range []string{
		"/v0/resource/plugins/" + pluginName,
		"/v0/management/" + pluginName,
		"/v0/resource/plugins/aigw-reverse-proxy",
	} {
		if strings.HasPrefix(p, prefix) {
			return strings.TrimPrefix(p, prefix)
		}
	}
	return p
}

// decodeCheckinConfigBody reads a JSON config patch.
func decodeCheckinConfigBody(body []byte) (checkinSettings, error) {
	cfg := state.settings.get().Checkin
	if len(body) == 0 {
		return cfg, nil
	}
	// Accept both a bare config object and {"checkin": {...}}.
	var wrapper struct {
		Enabled                  *bool `json:"enabled"`
		Hour                     *int  `json:"hour"`
		Minute                   *int  `json:"minute"`
		OnStart                  *bool `json:"on_start"`
		RetryOnDeviceFingerprint *bool `json:"retry_on_device_fingerprint"`
		Checkin                  *struct {
			Enabled                  *bool `json:"enabled"`
			Hour                     *int  `json:"hour"`
			Minute                   *int  `json:"minute"`
			OnStart                  *bool `json:"on_start"`
			RetryOnDeviceFingerprint *bool `json:"retry_on_device_fingerprint"`
		} `json:"checkin"`
	}
	if errUnmarshal := json.Unmarshal(body, &wrapper); errUnmarshal != nil {
		return cfg, errUnmarshal
	}

	apply := func(enabled *bool, hour, minute *int, onStart, retry *bool) {
		if enabled != nil {
			cfg.Enabled = *enabled
		}
		if hour != nil {
			cfg.Hour = *hour
		}
		if minute != nil {
			cfg.Minute = *minute
		}
		if onStart != nil {
			cfg.OnStart = *onStart
		}
		if retry != nil {
			cfg.RetryOnDeviceFingerprint = *retry
		}
	}
	apply(wrapper.Enabled, wrapper.Hour, wrapper.Minute, wrapper.OnStart, wrapper.RetryOnDeviceFingerprint)
	if wrapper.Checkin != nil {
		apply(wrapper.Checkin.Enabled, wrapper.Checkin.Hour, wrapper.Checkin.Minute,
			wrapper.Checkin.OnStart, wrapper.Checkin.RetryOnDeviceFingerprint)
	}
	return cfg, nil
}

// managementFormAction returns the absolute POST target for the check-in form.
//
// The browsable page is served from a *resource* route
// (/v0/resource/plugins/<id>/checkin), and CPA serves resource routes with GET
// only (internal/pluginhost/management.go:295 rejects anything else). A form
// action of "checkin" would therefore resolve to a GET-only URL and the POST
// would be dropped, leaving a blank page.
//
// Management routes accept any method and receive the body
// (internal/pluginhost/management.go:232), so the form must post there.
func managementFormAction() string {
	return managementBasePath() + "/" + pluginName + "/checkin"
}

// managementBasePath mirrors CPA's plugin management mount point.
func managementBasePath() string {
	return "/v0/management"
}

// ---- HTML ----------------------------------------------------------------

// checkinPage renders the check-in UI.
func checkinPage() string {
	return checkinPageWithRun(nil)
}

// checkinPageWithNotice renders the page with a one-line status banner.
func checkinPageWithNotice(notice string) string {
	page := checkinPage()
	if notice == "" {
		return page
	}
	banner := `<div class="card ok">` + html.EscapeString(notice) + `</div>`
	// Inject right after the intro paragraph so it is immediately visible.
	marker := `<div class="muted">手动立即签到，或配置每天自动签到。`
	if idx := strings.Index(page, marker); idx >= 0 {
		// Find the end of that paragraph (first ">") after the marker's closing tag.
		rest := page[idx:]
		if end := strings.Index(rest, "</div>"); end >= 0 {
			insertAt := idx + end + len("</div>")
			return page[:insertAt] + banner + page[insertAt:]
		}
	}
	return banner + page
}

// checkinPageWithRun renders the page, optionally highlighting a fresh run.
func checkinPageWithRun(fresh *checkinRun) string {
	cfg := state.settings.get().Checkin
	status := checkinStatusJSON()
	history := state.checkin.snapshot(10)

	var b strings.Builder
	b.WriteString(checkinPageHead())

	// --- auto settings form -------------------------------------------
	b.WriteString(`<h2>自动签到</h2><form method="post" action="` + managementFormAction() + `" class="card">`)
	b.WriteString(`<input type="hidden" name="action" value="save">`)
	b.WriteString(`<label class="row"><input type="checkbox" name="enabled"`)
	if cfg.Enabled {
		b.WriteString(` checked`)
	}
	b.WriteString(`> 启用每日自动签到</label>`)
	b.WriteString(`<div class="row">每天 <input type="number" name="hour" min="0" max="23" value="` +
		fmt.Sprint(clampHour(cfg.Hour)) + `" style="width:4em"> 时 <input type="number" name="minute" min="0" max="59" value="` +
		fmt.Sprint(clampMinute(cfg.Minute)) + `" style="width:4em"> 分执行</div>`)
	b.WriteString(`<label class="row"><input type="checkbox" name="on_start"`)
	if cfg.OnStart {
		b.WriteString(` checked`)
	}
	b.WriteString(`> 启动时补跑（当天尚未执行时）</label>`)
	b.WriteString(`<button type="submit">保存设置</button>`)
	if next, _ := status["next_run"].(string); next != "" {
		b.WriteString(`<span class="muted" style="margin-left:12px">下次执行：` + html.EscapeString(next) + `</span>`)
	}
	b.WriteString(`</form>`)

	// --- manual run ----------------------------------------------------
	b.WriteString(`<h2>手动签到</h2><form method="post" action="` + managementFormAction() + `" class="card">`)
	b.WriteString(`<input type="hidden" name="action" value="run">`)
	b.WriteString(`<button type="submit"`)
	if running, _ := status["running"].(bool); running {
		b.WriteString(` disabled`)
	}
	b.WriteString(`>立即为所有账号签到</button>`)
	if running, _ := status["running"].(bool); running {
		b.WriteString(` <span class="muted">已有任务在运行…</span>`)
	}
	b.WriteString(`</form>`)

	// --- fresh run result ---------------------------------------------
	if fresh != nil {
		b.WriteString(`<h2>本次结果</h2>`)
		b.WriteString(renderRun(*fresh))
	}

	// --- history -------------------------------------------------------
	b.WriteString(`<h2>历史记录</h2>`)
	if len(history) == 0 {
		b.WriteString(`<p class="muted">还没有签到记录。</p>`)
	} else {
		for _, run := range history {
			b.WriteString(renderRun(run))
		}
	}

	b.WriteString(`</body></html>`)
	return b.String()
}

// renderRun renders one run as a table.
func renderRun(run checkinRun) string {
	var b strings.Builder
	trigger := map[string]string{"manual": "手动", "auto": "自动", "startup": "启动补跑"}[run.Trigger]
	if trigger == "" {
		trigger = run.Trigger
	}
	b.WriteString(`<div class="card"><div class="muted">` +
		html.EscapeString(run.StartedAt.Local().Format("2006-01-02 15:04:05")) +
		` · ` + html.EscapeString(trigger))
	if !run.FinishedAt.IsZero() {
		b.WriteString(` · 耗时 ` + html.EscapeString(run.FinishedAt.Sub(run.StartedAt).Round(time.Millisecond).String()))
	}
	b.WriteString(` · 成功 ` + fmt.Sprint(run.Succeeded) + ` / 失败 ` + fmt.Sprint(run.Failed) + `</div>`)

	if len(run.Results) == 0 {
		b.WriteString(`<p class="muted">无结果</p></div>`)
		return b.String()
	}

	b.WriteString(`<table><tr><th>账号</th><th>UID</th><th>结果</th><th>说明</th><th>码</th></tr>`)
	for _, r := range run.Results {
		class, text := "ok", "成功"
		switch {
		case r.Error != "":
			class, text = "bad", "错误"
		case !r.Success:
			class, text = "bad", "失败"
		case r.Already:
			class, text = "warn", "已签到"
		}
		label := firstNonEmpty(r.Label, r.AuthID)
		b.WriteString(`<tr><td>` + html.EscapeString(label) + `</td>`)
		b.WriteString(`<td><code>` + html.EscapeString(firstNonEmpty(r.UID, r.AuthID)) + `</code></td>`)
		b.WriteString(`<td class="` + class + `">` + text + `</td>`)
		msg := firstNonEmpty(r.Error, r.Message)
		b.WriteString(`<td>` + html.EscapeString(msg) + `</td>`)
		b.WriteString(`<td>` + fmt.Sprint(r.Code) + `</td></tr>`)
	}
	b.WriteString(`</table></div>`)
	return b.String()
}

func checkinPageHead() string {
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>AIGW 反向代理 · 签到</title><style>` +
		`:root{color-scheme:light dark}` +
		`body{font:14px/1.6 -apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;margin:0;padding:24px;max-width:1080px}` +
		`h1{font-size:20px;margin:0 0 4px}h2{font-size:15px;margin:24px 0 8px;opacity:.75}` +
		`table{border-collapse:collapse;width:100%;font-size:13px}` +
		`th,td{text-align:left;padding:6px 10px;border-bottom:1px solid rgba(128,128,128,.25)}` +
		`th{opacity:.6;font-weight:600}` +
		`code{font-family:ui-monospace,Menlo,monospace;font-size:12px}` +
		`.ok{color:#0a0}.bad{color:#c00}.warn{color:#b80}.muted{opacity:.6}` +
		`.card{border:1px solid rgba(128,128,128,.3);border-radius:10px;padding:14px 16px;margin-bottom:12px}` +
		`.row{display:block;margin:6px 0}` +
		`button{padding:6px 14px;border-radius:8px;border:1px solid rgba(128,128,128,.4);cursor:pointer}` +
		`input[type=number]{padding:3px 6px}` +
		`</style></head><body>` +
		`<h1>WorkBuddy 签到</h1>` +
		`<div class="muted">手动立即签到，或配置每天自动签到。签到接口对应源 APK 的 <code>POST /v2/billing/meter/daily-checkin</code>。</div>`
}
