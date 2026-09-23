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

// This file renders the plugin's single combined page.
//
// Everything an operator needs lives here:
//
//	1. management key (browser-local)
//	2. account list      — read straight from the auth store, so a fresh login
//	                       shows up without any traffic
//	3. check-in          — manual run + daily schedule
//	4. quota             — manual refresh + interval + per-account credits
//	5. usage summary and recent calls
//
// The check-in and quota pages remain reachable at their own paths for
// bookmarks and scripts, but the combined page is what the CPA menu links to.

// handleMainRequest serves the combined page and its JSON endpoints.
func handleMainRequest(req pluginapi.ManagementRequest) (managementResponse, bool) {
	path := normaliseManagementPath(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))

	switch path {
	case "", "/", "/home", "/dashboard":
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    htmlResponseHeaders(),
			Body:       []byte(mainPage()),
		}, true

	case "/accounts":
		// JSON account list, used by the page to refresh in place.
		accounts := listWorkBuddyAccounts()
		total, usable, known, credits := accountSummary(accounts)
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body: mustJSON(map[string]any{
				"accounts":      accounts,
				"total":         total,
				"usable":        usable,
				"credits_known": known,
				"total_credits": credits,
				"fetched_at":    time.Now().Format(time.RFC3339),
				"warning":       state.accounts.lastError(),
			}),
		}, true

	case "/routing/status":
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body: mustJSON(map[string]any{
				"routing":   routingStatusJSON(),
				"scheduler": schedulerSnapshot(),
			}),
		}, true
	}

	if method == http.MethodPost {
		switch path {
		case "/routing/config":
			var body struct {
				Strategy string `json:"strategy"`
				Routing  *struct {
					Strategy string `json:"strategy"`
				} `json:"routing"`
			}
			if len(req.Body) > 0 {
				if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal != nil {
					return managementResponse{
						StatusCode: http.StatusBadRequest,
						Headers:    jsonResponseHeaders(),
						Body:       mustJSON(map[string]any{"error": errUnmarshal.Error()}),
					}, true
				}
			}
			value := body.Strategy
			if value == "" && body.Routing != nil {
				value = body.Routing.Strategy
			}
			applied := applyRoutingConfig(routingSettings{Strategy: normalizeStrategy(value)})
			return managementResponse{
				StatusCode: http.StatusOK,
				Headers:    jsonResponseHeaders(),
				Body: mustJSON(map[string]any{
					"ok":      true,
					"routing": routingStatusJSON(),
					"applied": string(applied.Strategy),
				}),
			}, true

		case "/routing/reset":
			state.scheduler.resetCursor()
			return managementResponse{
				StatusCode: http.StatusOK,
				Headers:    jsonResponseHeaders(),
				Body:       mustJSON(map[string]any{"ok": true, "routing": routingStatusJSON()}),
			}, true

		case "/run":
			// Combined action: check in, then refresh quota. One button to do
			// both, which is the common intent.
			run := runFromManagement()
			results, errQuota := runQuotaRefresh("manual")
			refreshAccountsAfterLogin()
			payload := map[string]any{"checkin": run}
			if errQuota == nil {
				payload["quota"] = results
			} else {
				payload["quota_error"] = errQuota.Error()
			}
			return managementResponse{
				StatusCode: http.StatusOK,
				Headers:    jsonResponseHeaders(),
				Body:       mustJSON(payload),
			}, true
		}
	}

	return managementResponse{}, false
}

// ---- HTML ----------------------------------------------------------------

// mainPage renders the combined view.
func mainPage() string {
	settings := state.settings.get()
	accounts := listWorkBuddyAccounts()
	total, usable, known, credits := accountSummary(accounts)
	totals := state.log.totals()

	state.quota.mu.Lock()
	quotaRunning := state.quota.running
	state.quota.mu.Unlock()
	state.checkin.mu.Lock()
	checkinRunning := state.checkin.running
	state.checkin.mu.Unlock()

	checkinHistory := state.checkin.snapshot(5)
	recentCalls := state.log.recent(10)

	var b strings.Builder
	b.WriteString(mainPageHead())

	// --- key ----------------------------------------------------------
	b.WriteString(`<section id="sec-key"><h2>管理密钥</h2><div class="card">`)
	b.WriteString(`<div class="row"><input type="password" id="mgmtKey" placeholder="CPA management key" ` +
		`style="width:min(420px,70%)"> ` +
		`<button type="button" onclick="saveKey()">保存到浏览器</button> ` +
		`<button type="button" onclick="clearKey()">清除</button></div>`)
	b.WriteString(`<div class="muted" id="keyState"></div>`)
	b.WriteString(`<div class="muted">密钥仅保存在本机浏览器（localStorage），不会上传到插件或服务器。</div>`)
	b.WriteString(`</div></section>`)

	// --- accounts -----------------------------------------------------
	// Rendered server-side from the auth store so the list is populated on
	// first load, right after a login.
	b.WriteString(`<section id="sec-accounts"><h2>WorkBuddy 账号</h2>`)
	b.WriteString(`<div class="cards">`)
	writeCard := func(label string, value any) {
		b.WriteString(`<div class="card"><b>` + html.EscapeString(fmt.Sprint(value)) + `</b>` +
			html.EscapeString(label) + `</div>`)
	}
	writeCard("账号总数", total)
	writeCard("可用", usable)
	creditsText := "—"
	if known > 0 {
		creditsText = fmt.Sprint(credits)
	}
	writeCard("已知积分合计", creditsText)
	writeCard("已查询积分", fmt.Sprintf("%d / %d", known, total))
	// Urgent accounts get their own card so the number is impossible to miss.
	urgent := 0
	for _, a := range accounts {
		if a.CreditsExpiringSoon || a.CreditsExpired {
			urgent++
		}
	}
	if urgent > 0 {
		writeCard("积分即将/已过期", urgent)
	}
	b.WriteString(`</div>`)

	b.WriteString(`<div class="card"><div class="muted">账号列表读取自 CPA 认证存储，登录后立即可见，无需先发起请求。` +
		`仅显示 WorkBuddy / CodeBuddy 账号。积分 7 天内到期会高亮标注，建议优先使用。</div>`)
	if len(accounts) == 0 {
		b.WriteString(`<p class="muted">还没有 WorkBuddy 账号。请到 CPA 的「认证」页登录。</p>`)
	} else {
		b.WriteString(`<table><tr><th>账号</th><th>UID</th><th>版本</th><th>剩余积分</th><th>到期</th><th>状态</th></tr>`)
		for _, a := range accounts {
			status, class := "可用", "ok"
			reason := ""
			switch {
			case a.Disabled:
				status, class = "已停用", "bad"
				reason = a.Reason
			case a.Expired:
				status, class = "凭据已过期", "bad"
				reason = "请重新登录"
			case !a.CooldownUntil.IsZero() && time.Now().Before(a.CooldownUntil):
				status, class = "冷却中", "warn"
				reason = fmt.Sprintf("%s（至 %s）", a.Reason, a.CooldownUntil.Local().Format("15:04:05"))
			case a.Reason != "" && !a.Usable:
				status, class = "不可用", "bad"
				reason = a.Reason
			}
			cv := "—"
			if a.CreditsKnown {
				cv = fmt.Sprint(a.Credits)
			}
			// Expiry column: highlight 7-day urgency and mark expired credits.
			expiry, expiryClass := "—", "muted"
			switch {
			case a.CreditsExpired:
				expiry, expiryClass = "已过期", "bad"
			case a.CreditsExpireAt > 0 && a.CreditsExpiringSoon:
				expiry, expiryClass = fmt.Sprintf("%d 天后 ⚠️", a.CreditsExpireDays), "warn"
			case a.CreditsExpireAt > 0:
				expiry, expiryClass = fmt.Sprintf("%d 天后", a.CreditsExpireDays), "ok"
			}
			b.WriteString(`<tr><td>` + html.EscapeString(a.Label) + `</td>`)
			b.WriteString(`<td><code>` + html.EscapeString(firstNonEmpty(a.UID, a.AuthIndex)) + `</code></td>`)
			b.WriteString(`<td>` + html.EscapeString(a.Variant) + `</td>`)
			b.WriteString(`<td>` + html.EscapeString(cv) + `</td>`)
			b.WriteString(`<td class="` + expiryClass + `">` + html.EscapeString(expiry) + `</td>`)
			b.WriteString(`<td class="` + class + `">` + status)
			if reason != "" {
				b.WriteString(` <span class="muted">` + html.EscapeString(reason) + `</span>`)
			}
			b.WriteString(`</td></tr>`)
		}
		b.WriteString(`</table>`)
	}
	if warn := state.accounts.lastError(); warn != "" {
		b.WriteString(`<div class="bad">读取账号列表失败：` + html.EscapeString(warn) + `</div>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(`<div class="card"><button type="button" id="btnRun" onclick="runAll()"`)
	if quotaRunning || checkinRunning {
		b.WriteString(` disabled`)
	}
	b.WriteString(`>一键：签到 + 刷新额度</button> <span class="muted" id="runMsg"></span></div>`)
	b.WriteString(`<div id="runResult"></div></section>`)

	// --- check-in schedule --------------------------------------------
	b.WriteString(`<section id="sec-checkin"><h2>签到设置</h2><div class="card">`)
	b.WriteString(`<label class="row"><input type="checkbox" id="ckEnabled"`)
	if settings.Checkin.Enabled {
		b.WriteString(` checked`)
	}
	b.WriteString(`> 启用每日自动签到</label>`)
	b.WriteString(`<div class="row">每天 <input type="number" id="ckHour" min="0" max="23" value="` +
		fmt.Sprint(clampHour(settings.Checkin.Hour)) + `" style="width:4em"> 时 ` +
		`<input type="number" id="ckMinute" min="0" max="59" value="` +
		fmt.Sprint(clampMinute(settings.Checkin.Minute)) + `" style="width:4em"> 分执行</div>`)
	b.WriteString(`<label class="row"><input type="checkbox" id="ckOnStart"`)
	if settings.Checkin.OnStart {
		b.WriteString(` checked`)
	}
	b.WriteString(`> 启动时补跑（当天尚未执行时）</label>`)
	b.WriteString(`<button type="button" onclick="saveSettings()">保存</button>`)
	b.WriteString(`</div>`)

	if len(checkinHistory) > 0 {
		b.WriteString(`<div class="card"><div class="muted">最近签到</div>`)
		b.WriteString(renderRun(checkinHistory[0]))
		b.WriteString(`</div>`)
	}
	b.WriteString(`</section>`)

	// --- quota schedule ------------------------------------------------
	b.WriteString(`<section id="sec-quota"><h2>额度刷新设置</h2><div class="card">`)
	b.WriteString(`<label class="row"><input type="checkbox" id="qEnabled"`)
	if settings.Quota.Enabled {
		b.WriteString(` checked`)
	}
	b.WriteString(`> 启用定时刷新额度</label>`)
	b.WriteString(`<div class="row">每 <input type="number" id="qInterval" min="5" max="1440" value="` +
		fmt.Sprint(clampIntervalMinutes(settings.Quota.IntervalMinutes)) + `" style="width:5em"> 分钟刷新一次</div>`)
	b.WriteString(`<label class="row"><input type="checkbox" id="qOnStart"`)
	if settings.Quota.RefreshOnStart {
		b.WriteString(` checked`)
	}
	b.WriteString(`> 启动时刷新一次</label>`)
	b.WriteString(`<button type="button" onclick="saveSettings()">保存</button>`)
	b.WriteString(`<div class="muted">额度决定账号选用顺序：源应用按剩余额度从多到少选用账号。</div>`)
	b.WriteString(`</div></section>`)

	// --- account switching strategy ------------------------------------
	// This is the piece the panel was missing: choosing how requests are
	// distributed across accounts.
	routing := routingStatusJSON()
	b.WriteString(`<section id="sec-routing"><h2>账号切换策略</h2><div class="card">`)
	b.WriteString(`<div class="row">`)
	options, _ := routing["options"].([]map[string]any)
	current, _ := routing["strategy"].(string)
	for _, opt := range options {
		value, _ := opt["value"].(string)
		label, _ := opt["label"].(string)
		desc, _ := opt["description"].(string)
		b.WriteString(`<label class="opt"><input type="radio" name="strategy" value="` +
			html.EscapeString(value) + `"`)
		if value == current {
			b.WriteString(` checked`)
		}
		b.WriteString(`> <b>` + html.EscapeString(label) + `</b>` +
			` <span class="muted">` + html.EscapeString(desc) + `</span></label>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(`<button type="button" onclick="saveStrategy()">应用策略</button> ` +
		`<button type="button" onclick="resetRotation()">重置轮巡位置</button>` +
		` <span class="muted" id="strategyMsg"></span>`)
	b.WriteString(`<div class="muted" style="margin-top:6px">当前：<b>` +
		html.EscapeString(fmt.Sprint(routing["strategy_label"])) + `</b> · ` +
		html.EscapeString(nextRotationHint()) + `</div>`)
	b.WriteString(`</div>`)

	// Selection order preview.
	if rows, okRows := routing["order"].([]map[string]any); okRows && len(rows) > 0 {
		b.WriteString(`<div class="card"><div class="muted">选择顺序预览（按当前策略）</div>`)
		b.WriteString(`<table><tr><th>#</th><th>账号</th><th>剩余积分</th><th>到期</th><th>已选中次数</th></tr>`)
		for _, row := range rows {
			cv := "—"
			if known, _ := row["known"].(bool); known {
				cv = fmt.Sprint(row["credits"])
			}
			expiry, expiryClass := "—", "muted"
			switch {
			case boolFromAny(row["expired"]):
				expiry, expiryClass = "已过期", "bad"
			case boolFromAny(row["expiring_soon"]):
				expiry, expiryClass = fmt.Sprint(row["expire_days"])+" 天后 ⚠️", "warn"
			case row["expire_days"] != nil && fmt.Sprint(row["expire_days"]) != "0":
				expiry, expiryClass = fmt.Sprint(row["expire_days"])+" 天后", "ok"
			}
			b.WriteString(`<tr><td>` + fmt.Sprint(row["position"]) + `</td>`)
			b.WriteString(`<td>` + html.EscapeString(fmt.Sprint(row["label"])) + `</td>`)
			b.WriteString(`<td>` + html.EscapeString(cv) + `</td>`)
			b.WriteString(`<td class="` + expiryClass + `">` + html.EscapeString(expiry) + `</td>`)
			b.WriteString(`<td>` + fmt.Sprint(row["picks"]) + `</td></tr>`)
		}
		b.WriteString(`</table></div>`)
	} else {
		b.WriteString(`<div class="card muted">暂无可用账号，无法预览顺序。</div>`)
	}
	b.WriteString(`</section>`)

	// --- usage ---------------------------------------------------------
	b.WriteString(`<section id="sec-usage"><h2>调用统计</h2><div class="cards">`)
	writeCard("总调用", totals.TotalCalls)
	writeCard("今日", totals.TodayCalls)
	writeCard("失败", totals.TotalFailed)
	writeCard("输入 Tokens", totals.TotalPrompt)
	writeCard("输出 Tokens", totals.TotalCompletion)
	b.WriteString(`</div>`)
	if len(recentCalls) > 0 {
		b.WriteString(`<div class="card"><div class="muted">最近调用</div>`)
		b.WriteString(`<table><tr><th>时间</th><th>供应商</th><th>模型</th><th>状态</th><th>Tokens</th></tr>`)
		for _, rec := range recentCalls {
			class := "ok"
			if rec.StatusCode >= 400 || rec.Error != "" {
				class = "bad"
			}
			b.WriteString(`<tr><td>` + rec.StartedAt.Local().Format("15:04:05") + `</td>`)
			b.WriteString(`<td>` + html.EscapeString(rec.ProviderID) + `</td>`)
			b.WriteString(`<td><code>` + html.EscapeString(rec.Model) + `</code></td>`)
			b.WriteString(`<td class="` + class + `">` + fmt.Sprint(rec.StatusCode) + `</td>`)
			b.WriteString(`<td>` + fmt.Sprint(rec.PromptTokens) + " / " + fmt.Sprint(rec.CompletionTokens) + `</td></tr>`)
		}
		b.WriteString(`</table></div>`)
	}
	b.WriteString(`</section>`)

	// --- gateway settings (read-only summary) --------------------------
	b.WriteString(`<section id="sec-gateway"><h2>网关设置</h2><div class="card"><table>`)
	row := func(k string, v any) {
		b.WriteString(`<tr><td><code>` + html.EscapeString(k) + `</code></td><td>` +
			html.EscapeString(fmt.Sprint(v)) + `</td></tr>`)
	}
	row("api_key", settings.marshalForLog()["api_key"])
	row("allow_no_key", settings.AllowNoKey)
	row("default_provider", settings.DefaultProvider)
	row("default_model", settings.DefaultModel)
	row("max_rotate", settings.MaxRotate)
	row("error_threshold", settings.ErrorThreshold)
	b.WriteString(`</table></div></section>`)

	b.WriteString(mainPageScript())
	return b.String()
}

func mainPageHead() string {
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>AIGW 反向代理</title><style>` +
		`:root{color-scheme:light dark}` +
		`body{font:14px/1.6 -apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;margin:0;padding:24px;max-width:1100px}` +
		`h1{font-size:20px;margin:0 0 4px}h2{font-size:15px;margin:0 0 8px;opacity:.75}` +
		`nav{position:sticky;top:0;background:Canvas;padding:10px 0;margin:8px 0 16px;border-bottom:1px solid rgba(128,128,128,.25);z-index:5}` +
		`nav a{color:inherit;text-decoration:none;opacity:.7;margin-right:16px;font-size:13px}` +
		`nav a:hover{opacity:1}` +
		`table{border-collapse:collapse;width:100%;font-size:13px}` +
		`th,td{text-align:left;padding:6px 10px;border-bottom:1px solid rgba(128,128,128,.25)}` +
		`th{opacity:.6;font-weight:600}` +
		`code{font-family:ui-monospace,Menlo,monospace;font-size:12px}` +
		`.ok{color:#0a0}.bad{color:#c00}.warn{color:#b80}.muted{opacity:.6}` +
		`section{margin-bottom:28px}` +
		`.card{border:1px solid rgba(128,128,128,.3);border-radius:10px;padding:14px 16px;margin-bottom:12px}` +
		`.cards{display:flex;gap:12px;flex-wrap:wrap;margin-bottom:12px}` +
		`.cards .card{min-width:130px;margin-bottom:0}` +
		`.cards b{display:block;font-size:22px;line-height:1.2}` +
		`.row{display:block;margin:6px 0}` +
		`.opt{display:block;margin:8px 0;cursor:pointer}` +
		`button{padding:6px 14px;border-radius:8px;border:1px solid rgba(128,128,128,.4);cursor:pointer}` +
		`button:disabled{opacity:.5;cursor:default}` +
		`input[type=number]{padding:3px 6px}` +
		`input[type=password]{padding:5px 8px;font-family:ui-monospace,Menlo,monospace}` +
		`</style></head><body>` +
		`<h1>AI 聚合网关 · 反向代理</h1>` +
		`<div class="muted">WorkBuddy 账号、签到、额度与调用统计集中在本页。</div>` +
		`<nav><a href="#sec-accounts">账号</a><a href="#sec-routing">切换策略</a>` +
		`<a href="#sec-checkin">签到</a><a href="#sec-quota">额度</a>` +
		`<a href="#sec-usage">统计</a><a href="#sec-gateway">设置</a></nav>`
}
