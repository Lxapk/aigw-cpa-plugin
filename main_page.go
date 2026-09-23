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

// renderMainPage builds the plugin's single management page.
//
// Everything the operator needs lives here behind a tab bar — accounts,
// switching strategy, check-in, credits, usage and gateway settings. Before
// this, each feature had its own resource route and menu entry, which meant
// hopping between pages to do one job.
//
// The markup is intentionally plain: CPA renders these pages inside its own
// panel, so the layout must be responsive and must not assume it owns the
// viewport. Styling comes from uiCSS.
func renderMainPage() string {
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

	checkinHistory := state.checkin.snapshot(1)
	recentCalls := state.log.recent(12)

	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>WorkBuddy</title><style>` + uiCSS + `</style></head><body><div class="root">`)

	// ---------------- hero ----------------
	b.WriteString(`<div class="hero"><div>`)
	b.WriteString(`<h1>WorkBuddy</h1>`)
	b.WriteString(`<div class="sub">把 WorkBuddy 账号反代为 CPA 的 OpenAI 兼容接口：账号轮换、模型输出、签到与积分管理。</div>`)
	b.WriteString(`</div><div class="badge"><span class="dot"></span>`)
	if quotaRunning || checkinRunning {
		b.WriteString(`任务执行中…`)
	} else {
		b.WriteString(fmt.Sprintf(`%d 个账号可用`, usable))
	}
	b.WriteString(`</div></div>`)

	// ---------------- tabs ----------------
	b.WriteString(`<div class="tabs">`)
	tab := func(id, label string, first bool) {
		cls := ""
		if first {
			cls = ` class="active"`
		}
		b.WriteString(`<button type="button" data-tab="` + id + `"` + cls +
			` onclick="showTab('` + id + `', this)">` + html.EscapeString(label) + `</button>`)
	}
	tab("tab-accounts", "账号", true)
	tab("tab-switch", "账号切换", false)
	tab("tab-checkin", "签到", false)
	tab("tab-credits", "积分", false)
	tab("tab-usage", "统计", false)
	tab("tab-settings", "设置", false)
	b.WriteString(`</div>`)

	// ---------------- tab: accounts ----------------
	b.WriteString(`<div id="tab-accounts" class="panel active">`)
	b.WriteString(`<div class="grid stats">`)
	stat := func(k string, v any) {
		b.WriteString(`<div class="stat"><div class="v">` +
			html.EscapeString(fmt.Sprint(v)) + `</div><div class="k">` +
			html.EscapeString(k) + `</div></div>`)
	}
	stat("账号总数", total)
	stat("可用账号", usable)
	creditsText := "—"
	if known > 0 {
		creditsText = fmt.Sprint(credits)
	}
	stat("积分合计", creditsText)
	stat("已查积分", fmt.Sprintf("%d / %d", known, total))
	b.WriteString(`</div>`)

	b.WriteString(`<div class="card"><h2>账号列表 <span class="hint">读取自 CPA 认证存储，登录后立即可见</span></h2>`)
	if len(accounts) == 0 {
		b.WriteString(`<div class="empty">还没有 WorkBuddy 账号。请到 CPA 的「认证」页登录。</div>`)
	} else {
		b.WriteString(`<table><thead><tr>`)
		b.WriteString(`<th>账号</th><th>UID</th><th>版本</th><th class="num">积分</th><th>到期</th><th>状态</th></tr></thead><tbody>`)
		for _, a := range accounts {
			pillClass, statusText := "ok", "可用"
			detail := ""
			switch {
			case a.Disabled:
				pillClass, statusText = "bad", "已停用"
				detail = a.Reason
			case a.Expired:
				pillClass, statusText = "bad", "凭据过期"
				detail = "请重新登录"
			case a.CreditsExpired:
				pillClass, statusText = "bad", "积分过期"
			case !a.CooldownUntil.IsZero() && time.Now().Before(a.CooldownUntil):
				pillClass, statusText = "warn", "冷却中"
				detail = a.CooldownUntil.Local().Format("15:04:05")
			}
			cv := "—"
			if a.CreditsKnown {
				cv = fmt.Sprint(a.Credits)
			}
			expiry, expiryClass := "—", "muted"
			switch {
			case a.CreditsExpired:
				expiry, expiryClass = "已过期", "bad"
			case a.CreditsExpireAt > 0 && a.CreditsExpiringSoon:
				expiry, expiryClass = fmt.Sprintf("%d 天后", a.CreditsExpireDays), "warn"
			case a.CreditsExpireAt > 0:
				expiry, expiryClass = fmt.Sprintf("%d 天后", a.CreditsExpireDays), "muted"
			}
			b.WriteString(`<tr><td><strong>` + html.EscapeString(a.Label) + `</strong></td>`)
			b.WriteString(`<td><code>` + html.EscapeString(firstNonEmpty(a.UID, a.AuthIndex)) + `</code></td>`)
			b.WriteString(`<td><span class="pill idle">` + html.EscapeString(a.Variant) + `</span></td>`)
			b.WriteString(`<td class="num">` + html.EscapeString(cv) + `</td>`)
			b.WriteString(`<td class="` + expiryClass + `">` + html.EscapeString(expiry) + `</td>`)
			b.WriteString(`<td><span class="pill ` + pillClass + `">` + statusText + `</span>`)
			if detail != "" {
				b.WriteString(` <span class="muted small">` + html.EscapeString(detail) + `</span>`)
			}
			b.WriteString(`</td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
	}
	if warn := state.accounts.lastError(); warn != "" {
		b.WriteString(`<div class="note bad">读取账号列表失败：` + html.EscapeString(warn) + `</div>`)
	}
	b.WriteString(`</div>`)

	b.WriteString(`<div class="card"><h2>一键操作</h2><div class="row">`)
	b.WriteString(`<button type="button" id="btnRun" onclick="runAll()"`)
	if quotaRunning || checkinRunning {
		b.WriteString(` disabled`)
	}
	b.WriteString(`>签到 + 刷新积分</button>`)
	b.WriteString(`<button type="button" class="ghost" onclick="refreshAccounts()">刷新列表</button>`)
	b.WriteString(`<span class="muted small" id="runMsg"></span></div>`)
	b.WriteString(`<div id="runResult"></div></div>`)
	b.WriteString(`</div>`)

	// ---------------- tab: switching strategy ----------------
	routing := routingStatusJSON()
	b.WriteString(`<div id="tab-switch" class="panel">`)
	b.WriteString(`<div class="card"><h2>账号切换策略 <span class="hint">请求如何在这些账号之间分配</span></h2>`)
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
		b.WriteString(`><span><span class="name">` + html.EscapeString(label) + `</span><br>` +
			`<span class="desc">` + html.EscapeString(desc) + `</span></span></label>`)
	}
	b.WriteString(`<div class="row">`)
	b.WriteString(`<button type="button" onclick="saveStrategy()">应用策略</button>`)
	b.WriteString(`<button type="button" class="ghost" onclick="resetRotation()">重置轮巡位置</button>`)
	b.WriteString(`<span class="muted small" id="strategyMsg"></span></div>`)
	b.WriteString(`<div class="note">当前：<b>` + html.EscapeString(fmt.Sprint(routing["strategy_label"])) +
		`</b> · ` + html.EscapeString(nextRotationHint()) + `</div>`)
	b.WriteString(`</div>`)

	if rows, okRows := routing["order"].([]map[string]any); okRows && len(rows) > 0 {
		b.WriteString(`<div class="card"><h2>选择顺序预览</h2>`)
		b.WriteString(`<table><thead><tr><th class="num">#</th><th>账号</th><th class="num">积分</th><th class="num">已选中</th></tr></thead><tbody>`)
		for _, row := range rows {
			cv := "—"
			if knownValue, _ := row["known"].(bool); knownValue {
				cv = fmt.Sprint(row["credits"])
			}
			b.WriteString(`<tr><td class="num">` + fmt.Sprint(row["position"]) + `</td>`)
			b.WriteString(`<td>` + html.EscapeString(fmt.Sprint(row["label"])) + `</td>`)
			b.WriteString(`<td class="num">` + html.EscapeString(cv) + `</td>`)
			b.WriteString(`<td class="num">` + fmt.Sprint(row["picks"]) + `</td></tr>`)
		}
		b.WriteString(`</tbody></table></div>`)
	} else {
		b.WriteString(`<div class="card"><div class="empty">暂无可用账号，无法预览顺序。</div></div>`)
	}
	b.WriteString(`</div>`)

	// ---------------- tab: check-in ----------------
	b.WriteString(`<div id="tab-checkin" class="panel">`)
	b.WriteString(`<div class="card"><h2>自动签到</h2>`)
	b.WriteString(`<div class="row tight"><label class="field"><input type="checkbox" id="ckEnabled"`)
	if settings.Checkin.Enabled {
		b.WriteString(` checked`)
	}
	b.WriteString(`> 启用每日自动签到</label></div>`)
	b.WriteString(`<div class="row tight"><label class="field">每天 <input type="number" id="ckHour" min="0" max="23" value="` +
		fmt.Sprint(clampHour(settings.Checkin.Hour)) + `"> 时 <input type="number" id="ckMinute" min="0" max="59" value="` +
		fmt.Sprint(clampMinute(settings.Checkin.Minute)) + `"> 分执行</label></div>`)
	b.WriteString(`<div class="row tight"><label class="field"><input type="checkbox" id="ckOnStart"`)
	if settings.Checkin.OnStart {
		b.WriteString(` checked`)
	}
	b.WriteString(`> 启动时补跑（当天尚未执行时）</label></div>`)
	b.WriteString(`<div class="row"><button type="button" onclick="saveCheckinSettings()">保存</button>`)
	b.WriteString(`<button type="button" class="ghost" onclick="runCheckin()">立即签到</button></div>`)
	b.WriteString(`</div>`)

	b.WriteString(`<div class="card"><h2>最近一次签到</h2>`)
	if len(checkinHistory) == 0 {
		b.WriteString(`<div class="empty">还没有签到记录。</div>`)
	} else {
		b.WriteString(renderRun(checkinHistory[0]))
	}
	b.WriteString(`</div></div>`)

	// ---------------- tab: credits ----------------
	b.WriteString(`<div id="tab-credits" class="panel">`)
	b.WriteString(`<div class="card"><h2>自动刷新积分</h2>`)
	b.WriteString(`<div class="row tight"><label class="field"><input type="checkbox" id="qEnabled"`)
	if settings.Quota.Enabled {
		b.WriteString(` checked`)
	}
	b.WriteString(`> 启用定时刷新积分</label></div>`)
	b.WriteString(`<div class="row tight"><label class="field">每 <input type="number" id="qInterval" min="5" max="1440" value="` +
		fmt.Sprint(clampIntervalMinutes(settings.Quota.IntervalMinutes)) + `"> 分钟刷新一次</label></div>`)
	b.WriteString(`<div class="row tight"><label class="field"><input type="checkbox" id="qOnStart"`)
	if settings.Quota.RefreshOnStart {
		b.WriteString(` checked`)
	}
	b.WriteString(`> 启动时刷新一次</label></div>`)
	b.WriteString(`<div class="row"><button type="button" onclick="saveQuotaSettings()">保存</button>`)
	b.WriteString(`<button type="button" class="ghost" onclick="refreshQuota()">立即刷新积分</button></div>`)
	b.WriteString(`<div class="note">积分决定账号选用顺序：源应用按剩余积分从多到少选用。</div>`)
	b.WriteString(`</div>`)

	b.WriteString(`<div class="card"><h2>账号积分</h2>`)
	state.quota.mu.Lock()
	lastRun := append([]quotaRefreshResult(nil), state.quota.lastRun...)
	state.quota.mu.Unlock()
	if len(lastRun) == 0 {
		b.WriteString(`<div class="empty">点「立即刷新积分」查询各账号的剩余积分与到期时间。</div>`)
	} else {
		b.WriteString(renderQuotaResults(lastRun))
	}
	b.WriteString(`</div></div>`)

	// ---------------- tab: usage ----------------
	b.WriteString(`<div id="tab-usage" class="panel">`)
	b.WriteString(`<div class="grid stats">`)
	stat("总调用", totals.TotalCalls)
	stat("今日", totals.TodayCalls)
	stat("失败", totals.TotalFailed)
	stat("输入 Tokens", totals.TotalPrompt)
	stat("输出 Tokens", totals.TotalCompletion)
	b.WriteString(`</div>`)
	b.WriteString(`<div class="card"><h2>最近调用</h2>`)
	if len(recentCalls) == 0 {
		b.WriteString(`<div class="empty">暂无调用记录。</div>`)
	} else {
		b.WriteString(`<table><thead><tr><th>时间</th><th>供应商</th><th>模型</th><th class="num">状态</th><th class="num">Tokens</th></tr></thead><tbody>`)
		for _, rec := range recentCalls {
			cls := "ok"
			if rec.StatusCode >= 400 || rec.Error != "" {
				cls = "bad"
			}
			b.WriteString(`<tr><td class="mono">` + rec.StartedAt.Local().Format("15:04:05") + `</td>`)
			b.WriteString(`<td>` + html.EscapeString(rec.ProviderID) + `</td>`)
			b.WriteString(`<td><code>` + html.EscapeString(rec.Model) + `</code></td>`)
			b.WriteString(`<td class="num ` + cls + `">` + fmt.Sprint(rec.StatusCode) + `</td>`)
			b.WriteString(`<td class="num">` + fmt.Sprint(rec.PromptTokens) + " / " + fmt.Sprint(rec.CompletionTokens) + `</td></tr>`)
		}
		b.WriteString(`</tbody></table>`)
	}
	b.WriteString(`</div></div>`)

	// ---------------- tab: settings ----------------
	b.WriteString(`<div id="tab-settings" class="panel">`)
	b.WriteString(`<div class="card"><h2>管理密钥 <span class="hint">仅保存在本机浏览器</span></h2>`)
	b.WriteString(`<div class="row"><input type="password" id="mgmtKey" placeholder="CPA management key" style="flex:1 1 320px">`)
	b.WriteString(`<button type="button" onclick="saveKey()">保存到浏览器</button>`)
	b.WriteString(`<button type="button" class="ghost" onclick="clearKey()">清除</button></div>`)
	b.WriteString(`<div class="muted small" id="keyState"></div>`)
	b.WriteString(`<div class="note">密钥仅保存在本机浏览器（localStorage），不会上传到插件或服务器。</div>`)
	b.WriteString(`</div>`)

	b.WriteString(`<div class="card"><h2>网关设置</h2><table>`)
	row := func(k string, v any) {
		b.WriteString(`<tr><td><code>` + html.EscapeString(k) + `</code></td><td>` +
			html.EscapeString(fmt.Sprint(v)) + `</td></tr>`)
	}
	row("default_provider", settings.DefaultProvider)
	row("default_model", settings.DefaultModel)
	row("enforce_frontend_key", settings.EnforceFrontendKey)
	row("allow_no_key", settings.AllowNoKey)
	row("api_key", settings.marshalForLog()["api_key"])
	row("max_rotate", settings.MaxRotate)
	row("error_threshold", settings.ErrorThreshold)
	row("端口 port", settings.Port)
	b.WriteString(`</table><div class="note">客户端鉴权由 CPA 的 <code>api-keys</code> 负责；` +
		`本插件默认不重复校验，避免覆盖你已有的 CPA 密钥。</div></div>`)
	b.WriteString(`</div>`)

	b.WriteString(`</div>` + mainPageScript() + `</body></html>`)
	return b.String()
}

// handleMainRequest serves the combined page and its JSON endpoints.
func handleMainRequest(req pluginapi.ManagementRequest) (managementResponse, bool) {
	path := normaliseManagementPath(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))

	switch path {
	case "", "/", "/home", "/dashboard":
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    htmlResponseHeaders(),
			Body:       []byte(renderMainPage()),
		}, true

	case "/accounts":
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
			// Combined action: check in, then refresh credits.
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
