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

// This file adds the quota UI to the plugin's management surface:
//
//	GET  /quota             HTML page (total + per-account credits + controls)
//	GET  /quota/status      JSON status
//	POST /quota/refresh     trigger a manual refresh
//	POST /quota/config      update the automatic refresh interval
//
// The page follows the check-in page's design: the management key lives in
// localStorage and is attached by fetch(), because CPA's management endpoints
// authenticate from a request header and the resource route is GET-only.

// handleQuotaRequest dispatches the quota management endpoints.
func handleQuotaRequest(req pluginapi.ManagementRequest) (managementResponse, bool) {
	path := normaliseManagementPath(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))

	switch path {
	case "/quota":
		if method == http.MethodPost {
			return handleQuotaPost(req)
		}
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    htmlResponseHeaders(),
			Body:       []byte(quotaPage()),
		}, true

	case "/quota/status":
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(quotaStatusJSON()),
		}, true

	case "/quota/refresh":
		if method != http.MethodPost {
			return managementResponse{
				StatusCode: http.StatusMethodNotAllowed,
				Headers:    jsonResponseHeaders(),
				Body:       mustJSON(map[string]any{"error": "POST required"}),
			}, true
		}
		results, errRefresh := runQuotaRefresh("manual")
		if errRefresh != nil {
			return managementResponse{
				StatusCode: http.StatusConflict,
				Headers:    jsonResponseHeaders(),
				Body:       mustJSON(map[string]any{"error": errRefresh.Error()}),
			}, true
		}
		total, known, accounts := quotaSummary()
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body: mustJSON(map[string]any{
				"results":         results,
				"total_credits":   total,
				"accounts_known":  known,
				"accounts_total":  accounts,
				"refreshed_at":    time.Now().Format(time.RFC3339),
			}),
		}, true

	case "/quota/config":
		if method != http.MethodPost {
			return managementResponse{
				StatusCode: http.StatusMethodNotAllowed,
				Headers:    jsonResponseHeaders(),
				Body:       mustJSON(map[string]any{"error": "POST required"}),
			}, true
		}
		cfg, errDecode := decodeQuotaConfigBody(req.Body)
		if errDecode != nil {
			return managementResponse{
				StatusCode: http.StatusBadRequest,
				Headers:    jsonResponseHeaders(),
				Body:       mustJSON(map[string]any{"error": errDecode.Error()}),
			}, true
		}
		applied := applyQuotaConfig(cfg)
		if applied.Enabled {
			startQuotaScheduler()
		}
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(map[string]any{"ok": true, "quota": quotaStatusJSON()}),
		}, true
	}

	return managementResponse{}, false
}

// handleQuotaPost keeps POST /quota working for simple clients; the page uses
// the dedicated /refresh and /config endpoints.
func handleQuotaPost(req pluginapi.ManagementRequest) (managementResponse, bool) {
	cfg, errDecode := decodeQuotaConfigBody(req.Body)
	if errDecode != nil {
		return managementResponse{
			StatusCode: http.StatusBadRequest,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(map[string]any{"error": errDecode.Error()}),
		}, true
	}
	applied := applyQuotaConfig(cfg)
	if applied.Enabled {
		startQuotaScheduler()
	}
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    jsonResponseHeaders(),
		Body:       mustJSON(map[string]any{"ok": true, "quota": quotaStatusJSON()}),
	}, true
}

// decodeQuotaConfigBody reads a JSON config patch, accepting both a bare object
// and {"quota": {...}}.
func decodeQuotaConfigBody(body []byte) (quotaSettings, error) {
	cfg := state.settings.get().Quota
	if len(body) == 0 {
		return cfg, nil
	}

	type patch struct {
		Enabled         *bool `json:"enabled"`
		IntervalMinutes *int  `json:"interval_minutes"`
		IntervalAlt     *int  `json:"intervalMinutes"`
		RefreshOnStart  *bool `json:"refresh_on_start"`
		RefreshAlt      *bool `json:"refreshOnStart"`
	}
	var wrapper struct {
		patch
		Quota *patch `json:"quota"`
	}
	if errUnmarshal := json.Unmarshal(body, &wrapper); errUnmarshal != nil {
		return cfg, errUnmarshal
	}

	apply := func(p *patch) {
		if p == nil {
			return
		}
		if p.Enabled != nil {
			cfg.Enabled = *p.Enabled
		}
		if p.IntervalMinutes != nil {
			cfg.IntervalMinutes = *p.IntervalMinutes
		} else if p.IntervalAlt != nil {
			cfg.IntervalMinutes = *p.IntervalAlt
		}
		if p.RefreshOnStart != nil {
			cfg.RefreshOnStart = *p.RefreshOnStart
		} else if p.RefreshAlt != nil {
			cfg.RefreshOnStart = *p.RefreshAlt
		}
	}
	apply(&wrapper.patch)
	apply(wrapper.Quota)
	return cfg, nil
}

// ---- HTML ----------------------------------------------------------------

// quotaPage renders the quota UI.
func quotaPage() string {
	cfg := state.settings.get().Quota
	total, known, accounts := quotaSummary()
	ordering := poolOrderingHint()

	state.quota.mu.Lock()
	lastRun := append([]quotaRefreshResult(nil), state.quota.lastRun...)
	lastRunAt := state.quota.lastRunAt
	running := state.quota.running
	state.quota.mu.Unlock()

	var b strings.Builder
	b.WriteString(quotaPageHead())

	// --- management key (same mechanism as the check-in page) ----------
	b.WriteString(`<h2>管理密钥</h2><div class="card">`)
	b.WriteString(`<div class="row"><input type="password" id="mgmtKey" placeholder="CPA management key" ` +
		`style="width:min(420px,70%)"> <button type="button" onclick="saveKey()">保存到浏览器</button>` +
		` <button type="button" onclick="clearKey()">清除</button></div>`)
	b.WriteString(`<div class="muted" id="keyState"></div>`)
	b.WriteString(`<div class="muted">密钥仅保存在本机浏览器（localStorage），不会上传到插件或服务器。</div>`)
	b.WriteString(`</div>`)

	// --- summary -------------------------------------------------------
	b.WriteString(`<h2>额度概览</h2><div class="cards">`)
	writeCard := func(label string, value any) {
		b.WriteString(`<div class="card"><b>` + html.EscapeString(fmt.Sprint(value)) + `</b>` +
			html.EscapeString(label) + `</div>`)
	}
	// Mirrors N1/R0.java:134: show the sum, or "—" when nothing is known.
	totalText := "—"
	if known > 0 {
		totalText = fmt.Sprint(total)
	}
	writeCard("已知额度合计", totalText)
	writeCard("已查询账号", fmt.Sprintf("%d / %d", known, accounts))
	if !lastRunAt.IsZero() {
		writeCard("上次刷新", lastRunAt.Local().Format("15:04:05"))
	}
	b.WriteString(`</div>`)

	// --- controls ------------------------------------------------------
	b.WriteString(`<h2>刷新</h2><div class="card">`)
	b.WriteString(`<button type="button" id="btnRefresh" onclick="refreshQuota()"`)
	if running {
		b.WriteString(` disabled`)
	}
	b.WriteString(`>立即刷新全部额度</button>`)
	if running {
		b.WriteString(` <span class="muted">已有任务在运行…</span>`)
	}
	b.WriteString(`<div id="runMsg" class="muted"></div></div>`)

	// --- auto config ---------------------------------------------------
	b.WriteString(`<h2>自动刷新</h2><div class="card">`)
	b.WriteString(`<label class="row"><input type="checkbox" id="qEnabled"`)
	if cfg.Enabled {
		b.WriteString(` checked`)
	}
	b.WriteString(`> 启用定时刷新额度</label>`)
	b.WriteString(`<div class="row">每 <input type="number" id="qInterval" min="5" max="1440" value="` +
		fmt.Sprint(clampIntervalMinutes(cfg.IntervalMinutes)) + `" style="width:5em"> 分钟刷新一次</div>`)
	b.WriteString(`<label class="row"><input type="checkbox" id="qOnStart"`)
	if cfg.RefreshOnStart {
		b.WriteString(` checked`)
	}
	b.WriteString(`> 启动时刷新一次</label>`)
	b.WriteString(`<button type="button" onclick="saveConfig()">保存设置</button>`)
	b.WriteString(`<div class="muted">额度用于给账号排序：源应用按剩余额度从多到少选用账号。</div>`)
	b.WriteString(`</div>`)

	// --- per account ---------------------------------------------------
	b.WriteString(`<div id="runResult">`)
	if len(lastRun) > 0 {
		b.WriteString(`<h2>账号额度</h2>`)
		b.WriteString(renderQuotaResults(lastRun))
	}
	b.WriteString(`</div>`)

	// --- selection order ------------------------------------------------
	b.WriteString(`<h2>账号选用顺序（按额度从多到少）</h2>`)
	if len(ordering) == 0 {
		b.WriteString(`<p class="muted">尚无账号记录。发起一次请求或刷新额度后这里会显示顺序。</p>`)
	} else {
		b.WriteString(`<table><tr><th>#</th><th>账号</th><th>剩余额度</th><th>冷却类型</th><th>可用</th></tr>`)
		for i, row := range ordering {
			usable := "✅"
			if !toBool(row["usable"]) {
				usable = "⏸ 冷却中"
			}
			b.WriteString(`<tr><td>` + fmt.Sprint(i+1) + `</td>`)
			b.WriteString(`<td>` + html.EscapeString(fmt.Sprint(row["label"])) + `</td>`)
			b.WriteString(`<td>` + html.EscapeString(fmt.Sprint(row["credits"])) + `</td>`)
			b.WriteString(`<td>` + html.EscapeString(fmt.Sprint(row["cool_kind"])) + `</td>`)
			b.WriteString(`<td>` + usable + `</td></tr>`)
		}
		b.WriteString(`</table>`)
	}

	b.WriteString(quotaPageScript())
	return b.String()
}

// renderQuotaResults renders one refresh pass as a table.
func renderQuotaResults(results []quotaRefreshResult) string {
	var b strings.Builder
	b.WriteString(`<div class="card"><table><tr><th>账号</th><th>区域</th><th>剩余额度</th><th>说明</th></tr>`)
	for _, r := range results {
		class := "ok"
		if r.Error != "" {
			class = "bad"
		}
		label := firstNonEmpty(r.Label, r.AuthID)
		note := r.Message
		if r.Error != "" {
			note = r.Error
		}
		b.WriteString(`<tr><td>` + html.EscapeString(label) + `</td>`)
		b.WriteString(`<td>` + html.EscapeString(r.Region) + `</td>`)
		b.WriteString(`<td class="` + class + `">` + fmt.Sprint(r.Credits) + `</td>`)
		b.WriteString(`<td>` + html.EscapeString(note) + `</td></tr>`)
	}
	b.WriteString(`</table></div>`)
	return b.String()
}

func toBool(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

func quotaPageHead() string {
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1">` +
		`<title>AIGW 反向代理 · 额度</title><style>` +
		`:root{color-scheme:light dark}` +
		`body{font:14px/1.6 -apple-system,BlinkMacSystemFont,'Segoe UI',system-ui,sans-serif;margin:0;padding:24px;max-width:1080px}` +
		`h1{font-size:20px;margin:0 0 4px}h2{font-size:15px;margin:24px 0 8px;opacity:.75}` +
		`table{border-collapse:collapse;width:100%;font-size:13px}` +
		`th,td{text-align:left;padding:6px 10px;border-bottom:1px solid rgba(128,128,128,.25)}` +
		`th{opacity:.6;font-weight:600}` +
		`code{font-family:ui-monospace,Menlo,monospace;font-size:12px}` +
		`.ok{color:#0a0}.bad{color:#c00}.warn{color:#b80}.muted{opacity:.6}` +
		`.card{border:1px solid rgba(128,128,128,.3);border-radius:10px;padding:14px 16px;margin-bottom:12px}` +
		`.cards{display:flex;gap:12px;flex-wrap:wrap}` +
		`.cards .card{min-width:130px}` +
		`.cards b{display:block;font-size:22px;line-height:1.2}` +
		`.row{display:block;margin:6px 0}` +
		`button{padding:6px 14px;border-radius:8px;border:1px solid rgba(128,128,128,.4);cursor:pointer}` +
		`button:disabled{opacity:.5;cursor:default}` +
		`input[type=number]{padding:3px 6px}` +
		`input[type=password]{padding:5px 8px;font-family:ui-monospace,Menlo,monospace}` +
		`</style></head><body>` +
		`<h1>WorkBuddy 额度</h1>` +
		`<div class="muted">查询各账号剩余额度。对应源 APK 的 <code>POST /v2/billing/meter/get-user-resource</code>；` +
		`额度同时决定账号选用顺序（额度多者优先）。</div>`
}
