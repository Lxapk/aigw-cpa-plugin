package main

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file exposes the growth-task engine over the management API and renders
// the task tab.
//
// The endpoints mirror the reference implementation's panel routes:
//
//	GET  /growth/tasks    task list for one account (or the last run summary)
//	GET  /growth/summary  energy, streak and travel state
//	POST /growth/run      run the full pass for one account or all of them
//	POST /growth/travel   send the cat travelling / collect a finished trip
//
// A run is performed synchronously and its result returned in the response,
// because the operator pressed a button and wants the log. The scheduled path
// records into state.growth instead.

// growthRunTargets resolves the accounts a request applies to.
//
// Only enabled domestic accounts participate. The growth centre does not exist
// for the international realm, so selecting them would produce a pass that
// reports nothing but skips.
func growthRunTargets(uid string) ([]checkinAccount, string) {
	accounts, errCollect := collectCheckinAccounts()
	if errCollect != nil {
		return nil, "读取账号失败: " + errCollect.Error()
	}

	domestic := make([]checkinAccount, 0, len(accounts))
	for _, account := range accounts {
		if !variantForCredentials(account.Creds).hasGrowthCenter() {
			continue
		}
		if accountDisabled(account) {
			continue
		}
		domestic = append(domestic, account)
	}

	if uid == "" || uid == "all" {
		if len(domestic) == 0 {
			return nil, "未找到已启用的国内版账号"
		}
		return domestic, ""
	}

	for _, account := range domestic {
		if accountUID(account) == uid || account.AuthID == uid {
			return []checkinAccount{account}, ""
		}
	}
	return nil, "未找到指定的国内版账号"
}

// handleGrowthTasksRequest answers GET /growth/tasks.
func handleGrowthTasksRequest(req pluginapi.ManagementRequest) managementResponse {
	uid := strings.TrimSpace(req.Query.Get("uid"))

	// Without a uid this reports the stored results, which is what the panel
	// polls; with one it queries the upstream live.
	if uid == "" {
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body: mustJSON(map[string]any{
				"runs":    state.growth.snapshot(),
				"history": state.growth.recentHistory(),
			}),
		}
	}

	account, message := growthRunTarget(uid)
	if account == nil {
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(map[string]any{"ok": false, "error": message}),
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tasks, errFetch := workBuddyUpstream.fetchGrowthTasks(ctx, account.Creds)
	if errFetch != nil {
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body: mustJSON(map[string]any{
				"ok":    false,
				"error": errFetch.Error(),
				"uid":   uid,
			}),
		}
	}
	sortGrowthTasks(tasks)

	summary := workBuddyUpstream.fetchGrowthSummary(ctx, account.Creds)
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    jsonResponseHeaders(),
		Body: mustJSON(map[string]any{
			"ok":      true,
			"uid":     accountUID(*account),
			"label":   firstNonEmpty(account.Label, accountUID(*account)),
			"tasks":   tasks,
			"summary": summary,
		}),
	}
}

// growthRunTarget resolves a single uid for a live query.
func growthRunTarget(uid string) (*checkinAccount, string) {
	targets, message := growthRunTargets(uid)
	if message != "" {
		return nil, message
	}
	if len(targets) == 0 {
		return nil, "未找到指定的国内版账号"
	}
	return &targets[0], ""
}

// handleGrowthRunRequest answers POST /growth/run.
func handleGrowthRunRequest(req pluginapi.ManagementRequest) managementResponse {
	var body struct {
		UID     string `json:"uid"`
		Trigger string `json:"trigger"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}

	targets, message := growthRunTargets(strings.TrimSpace(body.UID))
	if message != "" {
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(map[string]any{"ok": false, "error": message}),
		}
	}

	trigger := firstNonEmpty(strings.TrimSpace(body.Trigger), "manual")
	all := make([]growthRunResult, 0, len(targets))
	var lines []growthRunLog
	totalEarned := 0

	for i, account := range targets {
		uid := accountUID(account)
		label := firstNonEmpty(account.Label, uid)
		lines = append(lines, growthRunLog{
			Level:   "info",
			Message: "====== 正在为账号 [" + label + "] 执行全自动成长任务 (" + itoa(i+1) + "/" + itoa(len(targets)) + ") ======",
		})

		ctx, cancel := context.WithTimeout(context.Background(), growthRunTimeout)
		result := runGrowthPass(ctx, account)
		cancel()

		result.Trigger = trigger
		state.growth.record(uid, result)
		totalEarned += result.Earned
		all = append(all, result)
		lines = append(lines, result.Logs...)

		// Space accounts out as well, so one operator click cannot produce a
		// burst across the whole pool.
		if i < len(targets)-1 {
			time.Sleep(workBuddyGrowthGap)
		}
	}

	lines = append(lines, growthRunLog{
		Level:   "ok",
		Message: "====== 全部 " + itoa(len(targets)) + " 个账号执行完毕，累计新增积分 +" + itoa(totalEarned) + " ======",
	})

	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    jsonResponseHeaders(),
		Body: mustJSON(map[string]any{
			"ok":             true,
			"earned_credit":  totalEarned,
			"accounts_count": len(targets),
			"runs":           all,
			"logs":           lines,
		}),
	}
}

// handleGrowthTravelRequest answers POST /growth/travel.
func handleGrowthTravelRequest(req pluginapi.ManagementRequest) managementResponse {
	var body struct {
		UID string `json:"uid"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}

	targets, message := growthRunTargets(strings.TrimSpace(body.UID))
	if message != "" {
		return managementResponse{
			StatusCode: http.StatusOK,
			Headers:    jsonResponseHeaders(),
			Body:       mustJSON(map[string]any{"ok": false, "error": message}),
		}
	}

	runner := &growthRunner{client: workBuddyUpstream, limiter: growthLimit, gap: workBuddyGrowthGap}
	results := make([]map[string]any, 0, len(targets))
	for _, account := range targets {
		uid := accountUID(account)
		label := firstNonEmpty(account.Label, uid)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		outcome, errTravel := runner.travel(ctx, account.Creds)
		cancel()

		entry := map[string]any{
			"uid":     uid,
			"label":   label,
			"action":  outcome.Action,
			"credit":  outcome.Credit,
			"message": outcome.Message,
			"ok":      outcome.OK,
		}
		if errTravel != nil {
			entry["ok"] = false
			entry["error"] = errTravel.Error()
		}
		results = append(results, entry)
	}

	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    jsonResponseHeaders(),
		Body:       mustJSON(map[string]any{"ok": true, "results": results}),
	}
}

// itoa is a tiny alias so the message formatting above stays readable.
func itoa(n int) string { return strconv.Itoa(n) }

// renderGrowthSection builds the growth-task block shown on the task tab.
//
// It renders the stored per-account results, so a scheduled run is visible
// without the operator pressing anything. The per-task table is rendered
// client-side from /growth/tasks, because it needs a live upstream call and
// must not block the page.
func renderGrowthSection() string {
	var b strings.Builder
	b.WriteString(`<div class="card"><h2>成长任务 <span class="hint">国内版每日任务中心</span></h2>`)

	runs := state.growth.snapshot()
	if len(runs) == 0 {
		b.WriteString(`<div class="empty">还没有运行记录。点「完成成长任务」立即执行，或等待每日自动运行。</div>`)
	} else {
		b.WriteString(`<table><thead><tr><th>账号</th><th>状态</th><th class="num">新增积分</th>` +
			`<th class="num">领奖</th><th class="num">跳过</th><th class="num">失败</th><th>完成时间</th></tr></thead><tbody>`)
		for i := range runs {
			run := runs[i]
			pillClass, text := "ok", "成功"
			if !run.OK || run.Error != "" {
				pillClass, text = "bad", "失败"
				if run.Error == "" {
					text = "部分失败"
				}
			}
			detail := ""
			if run.Error != "" {
				detail = ` <span class="muted small">` + html.EscapeString(run.Error) + `</span>`
			}
			b.WriteString(`<tr>`)
			b.WriteString(`<td><strong>` + html.EscapeString(run.Label) + `</strong>` + detail + `</td>`)
			b.WriteString(`<td><span class="pill ` + pillClass + `">` + text + `</span></td>`)
			b.WriteString(`<td class="num">+` + itoa(run.Earned) + `</td>`)
			b.WriteString(`<td class="num">` + itoa(run.Claimed) + `</td>`)
			b.WriteString(`<td class="num">` + itoa(run.Skipped) + `</td>`)
			b.WriteString(`<td class="num">` + itoa(run.Failed) + `</td>`)
			finished := "—"
			if !run.FinishedAt.IsZero() {
				finished = run.FinishedAt.Format("01-02 15:04")
			}
			b.WriteString(`<td class="muted small">` + finished + `</td>`)
			b.WriteString(`</tr>`)
		}
		b.WriteString(`</tbody></table>`)

		// Show the most recent account's log; it is the actionable part.
		last := runs[len(runs)-1]
		if len(last.Logs) > 0 {
			b.WriteString(`<details><summary class="muted small">最近一次执行日志（` +
				html.EscapeString(last.Label) + `）</summary><pre class="log">`)
			for _, line := range last.Logs {
				b.WriteString(html.EscapeString(growthLogPrefix(line.Level)+line.Message) + "\n")
			}
			b.WriteString(`</pre></details>`)
		}
	}

	b.WriteString(`<div class="row" style="margin-top:10px">`)
	b.WriteString(`<button type="button" class="ghost" onclick="loadGrowthTasks()">查询任务明细</button>`)
	b.WriteString(`<span class="muted small" id="growthMsg"></span>`)
	b.WriteString(`</div>`)
	b.WriteString(`<div id="growthDetail"></div>`)
	b.WriteString(`</div>`)
	return b.String()
}

// growthLogPrefix renders the level marker used in the log block.
func growthLogPrefix(level string) string {
	switch level {
	case "ok":
		return "✓ "
	case "skip":
		return "⏭ "
	case "warn":
		return "! "
	case "error":
		return "✗ "
	}
	return "· "
}

// accountUID returns the identifier to use for a check-in account.
//
// checkinAccount carries the host-side AuthID and the provider-side uid inside
// Creds; the pool and the panel both key on the provider uid when it exists, so
// the task paths follow the same precedence.
func accountUID(account checkinAccount) string {
	if account.Creds != nil && account.Creds.UID != "" {
		return account.Creds.UID
	}
	return account.AuthID
}

// accountDisabled reports whether the operator or the host disabled an account.
//
// collectCheckinAccounts already skips host-disabled entries, so this covers the
// panel toggle, which lives in the credential pool.
func accountDisabled(account checkinAccount) bool {
	return state.pool.isAccountDisabled(accountUID(account), account.AuthID)
}

// runGrowthForAll runs the growth pass for every eligible account.
//
// Used by the combined /run endpoint. It shares the target selection with the
// dedicated /growth/run route so both apply the same realm and enabled filters.
func runGrowthForAll() []growthRunResult {
	targets, message := growthRunTargets("")
	if message != "" {
		return []growthRunResult{{Error: message}}
	}

	out := make([]growthRunResult, 0, len(targets))
	for i, account := range targets {
		ctx, cancel := context.WithTimeout(context.Background(), growthRunTimeout)
		result := runGrowthPass(ctx, account)
		cancel()

		result.Trigger = "manual"
		state.growth.record(accountUID(account), result)
		out = append(out, result)

		if i < len(targets)-1 {
			time.Sleep(workBuddyGrowthGap)
		}
	}
	return out
}
