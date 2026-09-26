package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// managementRegistration declares the plugin's management surface, mirroring the
// status information the app showed on its home screen (AppUiState in N1.C0270h:
// running/port/localUrl/pool/today/total/providers/accounts/calls/logs/settings).
//
// CPA mounts resources under /v0/resource/plugins/<plugin-id>/ and management
// routes under /v0/management/.
func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Resources: []pluginapi.ResourceRoute{
			{
				// A single menu entry. Everything — accounts, switching
				// strategy, check-in, credits, usage, settings — lives behind
				// the tab bar on this page, so there is no reason to add more
				// entries to CPA's sidebar.
				//
				// The path must not be "/": CPA normalizes a resource path with
				// strings.TrimRight(path, "/") and rejects the result when it
				// becomes empty (internal/pluginhost/management.go:209), so a
				// root resource is logged as "declared invalid resource route /"
				// and silently dropped — the menu entry then never appears.
				Path:        "home",
				Menu:        "WorkBuddy",
				Description: "WorkBuddy 账号、切换策略、签到、积分与调用统计，全部集中在这一页。",
			},
		},
		Routes: []pluginapi.ManagementRoute{
			{
				Method:      http.MethodGet,
				Path:        "/workbuddy/accounts",
				Description: "WorkBuddy account list as JSON (read from the auth store).",
			},
			// Account-switching strategy endpoints. The panel's strategy
			// selector calls these, so a missing registration makes the
			// buttons silently do nothing.
			{
				Method:      http.MethodGet,
				Path:        "/workbuddy/routing/status",
				Description: "Current account-switching strategy and selection order.",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/routing/config",
				Description: "Update the account-switching strategy.",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/routing/reset",
				Description: "Reset the round-robin rotation cursor.",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/run",
				Description: "Run check-in and quota refresh in one call.",
			},
			{
				Method:      http.MethodGet,
				Path:        "/workbuddy/status",
				Description: "WorkBuddy plugin status as JSON.",
			},
			{
				Method:      http.MethodGet,
				Path:        "/workbuddy/calls",
				Description: "Recent reverse-proxy calls recorded by the WorkBuddy plugin.",
			},
			// The check-in page itself is also mounted on the management path so
			// the browser can land there directly (and after a form POST).
			{
				Method:      http.MethodGet,
				Path:        "/workbuddy/checkin",
				Description: "WorkBuddy check-in page.",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/checkin",
				Description: "Handle check-in form submissions (run / save).",
			},
			{
				Method:      http.MethodGet,
				Path:        "/workbuddy/checkin/status",
				Description: "WorkBuddy check-in configuration and recent results.",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/checkin/run",
				Description: "Run a manual WorkBuddy check-in for every account.",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/checkin/config",
				Description: "Update the automatic check-in schedule.",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/variant",
				Description: "Set variant override (cn / ai / empty=auto).",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/account/toggle",
				Description: "Toggle account enable/disable.",
			},
			{
				Method:      http.MethodGet,
				Path:        "/workbuddy/quota",
				Description: "WorkBuddy quota page.",
			},
			{
				Method:      http.MethodGet,
				Path:        "/workbuddy/quota/status",
				Description: "WorkBuddy quota status as JSON.",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/quota/refresh",
				Description: "Refresh WorkBuddy quota for every account.",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/quota/config",
				Description: "Update the automatic quota refresh interval.",
			},
			{
				Method:      http.MethodGet,
				Path:        "/workbuddy/variant",
				Description: "Read the current supplier scope (全部/国内/国际供应商).",
			},
			// Supplier-scoped authorisation links.
			//
			// CPA's own OAuth entry already follows the 供应商切换 setting, so
			// these cover the remaining case: adding an account for the other
			// supplier without changing that setting.
			{
				Method:      http.MethodGet,
				Path:        "/workbuddy/auth/start",
				Description: "Get a supplier-scoped login link (?variant=cn|ai).",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/auth/start",
				Description: "Get a supplier-scoped login link (?variant=cn|ai).",
			},
			// Growth-task endpoints.
			//
			// CPA dispatches management calls through an exact route table: a
			// path that is not declared here is answered 404 by the host and
			// never reaches the plugin's handler, whatever the handler
			// implements. These four were missing, which is why 「完成成长任务」
			// and 「查询任务明细」 both returned 404 while every other panel
			// action worked.
			{
				Method:      http.MethodGet,
				Path:        "/workbuddy/growth/tasks",
				Description: "Growth task list and progress for one account (live upstream query).",
			},
			{
				Method:      http.MethodGet,
				Path:        "/workbuddy/growth/summary",
				Description: "Growth welfare summary (energy, streak, travel state).",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/growth/run",
				Description: "Run the full growth-task pass for one account or all of them.",
			},
			{
				Method:      http.MethodPost,
				Path:        "/workbuddy/growth/travel",
				Description: "Run the cat-travel pass (depart or claim).",
			},
		},
	}
}

// handleManagement dispatches the plugin's management/resource requests.
func handleManagement(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	path := strings.TrimSuffix(strings.TrimSpace(req.Path), "/")
	// Strip the plugin resource prefix when CPA passes the full path.
	if idx := strings.Index(path, "/workbuddy"); idx >= 0 {
		path = path[idx+len("/workbuddy"):]
	}
	// The combined view and its JSON endpoints.
	if resp, handled := handleMainRequest(pluginapi.ManagementRequest{
		Method:  req.Method,
		Path:    req.Path,
		Headers: req.Headers,
		Query:   req.Query,
		Body:    req.Body,
	}); handled {
		return okEnvelope(resp)
	}
	// Check-in endpoints are handled separately so this dispatch stays readable.
	if resp, handled := handleCheckinRequest(pluginapi.ManagementRequest{
		Method:  req.Method,
		Path:    req.Path,
		Headers: req.Headers,
		Query:   req.Query,
		Body:    req.Body,
	}); handled {
		return okEnvelope(resp)
	}
	// Quota endpoints follow the same pattern.
	if resp, handled := handleQuotaRequest(pluginapi.ManagementRequest{
		Method:  req.Method,
		Path:    req.Path,
		Headers: req.Headers,
		Query:   req.Query,
		Body:    req.Body,
	}); handled {
		return okEnvelope(resp)
	}
	if strings.HasSuffix(path, "/status") {
		path = "/status"
	}
	if strings.HasSuffix(path, "/calls") {
		path = "/calls"
	}
	if path == "" {
		path = "/status"
	}

	switch path {
	case "/calls":
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(map[string]any{"data": state.log.recent(50)}),
		})

	case "/status":
		if strings.Contains(strings.ToLower(headerValue(req.Headers, "accept")), "text/html") {
			return okEnvelope(managementResponse{
				StatusCode: http.StatusOK,
				Headers:    htmlResponseHeaders(),
				Body:       []byte(statusPage()),
			})
		}
		return okEnvelope(managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(statusSnapshot()),
		})

	default:
		return okEnvelope(managementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(map[string]any{"error": "not_found", "path": req.Path}),
		})
	}
}

// managementResponse mirrors pluginhost's rpc management response.
type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

func jsonResponseHeaders() http.Header {
	return http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}
}

func htmlResponseHeaders() http.Header {
	return http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}
}

func mustJSON(v any) []byte {
	raw, errMarshal := json.MarshalIndent(v, "", "  ")
	if errMarshal != nil {
		return []byte(`{"error":"marshal_failed"}`)
	}
	return raw
}

// statusSnapshot is the JSON payload of the status endpoint. It mirrors the
// AppUiState fields the app displayed plus the ported gateway settings.
func statusSnapshot() map[string]any {
	settings := state.settings.get()
	now := time.Now()

	lanes := state.pool.snapshot()
	providers := map[string]map[string]any{}
	for _, lane := range lanes {
		entry, ok := providers[lane.Provider]
		if !ok {
			entry = map[string]any{
				"provider":         lane.Provider,
				"total_accounts":   state.pool.totalCount(lane.Provider),
				"healthy_accounts": state.pool.usableCount(lane.Provider, now),
			}
			providers[lane.Provider] = entry
		}
	}

	providerList := make([]map[string]any, 0, len(providers))
	for _, v := range providers {
		providerList = append(providerList, v)
	}
	sort.Slice(providerList, func(i, j int) bool {
		return fmt.Sprint(providerList[i]["provider"]) < fmt.Sprint(providerList[j]["provider"])
	})

	return map[string]any{
		"plugin": map[string]any{
			"name":              pluginName,
			"version":           pluginVersion,
			"author":            pluginAuthor,
			"source_app":        "AI 聚合网关 0.1.18 (dev.aigw.app)",
			"registrations":     state.settings.registrations.Load(),
			"schema_version":    6,
			"port_owned_by_cpa": true,
		},
		"settings":     settings.marshalForLog(),
		"providers":    providerList,
		"accounts":     lanes,
		"usage":        state.log.totals(),
		"recent_calls": state.log.recent(10),
		"server_time":  now.Format(time.RFC3339),
	}
}

// statusPage renders a small self-contained HTML view for the CPA management UI.
func statusPage() string {
	settings := state.settings.get()
	totals := state.log.totals()
	lanes := state.pool.snapshot()
	now := time.Now()

	var b strings.Builder
	b.WriteString("<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">")
	b.WriteString("<title>WorkBuddy 反向代理插件</title>")
	b.WriteString("<style>")
	b.WriteString(":root{color-scheme:light dark}body{font:14px/1.6 -apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;margin:0;padding:24px;max-width:1080px}")
	b.WriteString("h1{font-size:20px;margin:0 0 4px}h2{font-size:15px;margin:24px 0 8px;opacity:.75}")
	b.WriteString("table{border-collapse:collapse;width:100%;font-size:13px}th,td{text-align:left;padding:6px 10px;border-bottom:1px solid rgba(128,128,128,.25)}")
	b.WriteString("th{opacity:.6;font-weight:600}code{font-family:ui-monospace,Menlo,monospace;font-size:12px}")
	b.WriteString(".ok{color:#0a0}.bad{color:#c00}.warn{color:#b80}.muted{opacity:.6}")
	b.WriteString(".cards{display:flex;gap:12px;flex-wrap:wrap}.card{border:1px solid rgba(128,128,128,.3);border-radius:10px;padding:12px 16px;min-width:120px}")
	b.WriteString(".card b{display:block;font-size:22px;line-height:1.2}")
	b.WriteString("</style></head><body>")

	b.WriteString("<h1>WorkBuddy · 反向代理插件</h1>")
	b.WriteString("<div class=\"muted\">端口 <code>" + fmt.Sprint(settings.Port) + "</code> · 默认供应商 <code>" + html.EscapeString(settings.DefaultProvider) + "</code> · CPA 负责监听</div>")

	b.WriteString("<h2>调用统计</h2><div class=\"cards\">")
	writeCard := func(label string, value any) {
		b.WriteString("<div class=\"card\"><b>" + html.EscapeString(fmt.Sprint(value)) + "</b>" + html.EscapeString(label) + "</div>")
	}
	writeCard("总调用", totals.TotalCalls)
	writeCard("今日", totals.TodayCalls)
	writeCard("失败", totals.TotalFailed)
	writeCard("输入 Tokens", totals.TotalPrompt)
	writeCard("输出 Tokens", totals.TotalCompletion)
	// Mirror N1/R0.java:134 — "已知额度合计" shows "—" when nothing is known.
	if total := totalKnownCredits(); total > 0 {
		writeCard("已知额度合计", total)
	} else {
		writeCard("已知额度合计", "—")
	}
	b.WriteString("</div>")

	b.WriteString("<h2>账号池</h2>")
	if len(lanes) == 0 {
		b.WriteString("<p class=\"muted\">尚无账号记录。首次转发成功后 CPA 会在这里登记所选凭据。</p>")
	} else {
		b.WriteString("<table><tr><th>供应商</th><th>账号</th><th>状态</th><th>连续失败</th><th>成功/失败</th><th>冷却至</th></tr>")
		for _, lane := range lanes {
			status, class := "可用", "ok"
			if lane.Disabled {
				status, class = "已停用", "bad"
			} else if !lane.usable(now) {
				status, class = "冷却中", "warn"
			}
			cooldown := "-"
			if !lane.CooldownUntil.IsZero() {
				cooldown = lane.CooldownUntil.Local().Format("15:04:05")
			}
			b.WriteString("<tr><td>" + html.EscapeString(lane.Provider) + "</td>")
			b.WriteString("<td>" + html.EscapeString(firstNonEmpty(lane.Label, lane.UID)) + "</td>")
			b.WriteString("<td class=\"" + class + "\">" + status + "</td>")
			b.WriteString("<td>" + fmt.Sprint(lane.ConsecutiveErrors) + "</td>")
			b.WriteString("<td>" + fmt.Sprint(lane.Successes) + " / " + fmt.Sprint(lane.Failures) + "</td>")
			b.WriteString("<td>" + cooldown + "</td></tr>")
		}
		b.WriteString("</table>")
	}

	b.WriteString("<h2>最近调用</h2>")
	calls := state.log.recent(20)
	if len(calls) == 0 {
		b.WriteString("<p class=\"muted\">暂无调用记录。</p>")
	} else {
		b.WriteString("<table><tr><th>时间</th><th>供应商</th><th>模型</th><th>状态</th><th>Tokens</th><th>耗时</th></tr>")
		for _, rec := range calls {
			class := "ok"
			if rec.StatusCode >= 400 || rec.Error != "" {
				class = "bad"
			}
			b.WriteString("<tr><td>" + rec.StartedAt.Local().Format("15:04:05") + "</td>")
			b.WriteString("<td>" + html.EscapeString(rec.ProviderID) + "</td>")
			b.WriteString("<td><code>" + html.EscapeString(rec.Model) + "</code></td>")
			b.WriteString("<td class=\"" + class + "\">" + fmt.Sprint(rec.StatusCode) + "</td>")
			b.WriteString("<td>" + fmt.Sprint(rec.PromptTokens) + " / " + fmt.Sprint(rec.CompletionTokens) + "</td>")
			b.WriteString("<td>" + fmt.Sprint(rec.LatencyMillis) + "ms</td></tr>")
		}
		b.WriteString("</table>")
	}

	b.WriteString("</body></html>")
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
