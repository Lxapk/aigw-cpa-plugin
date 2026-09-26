package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// This file orchestrates the growth-task lifecycle: accept -> report -> claim,
// then the daily welfare (cat travel, streak, energy).
//
// Ported from run_growth_tasks (wb_tasks.py:428). The stage ordering is not
// incidental; each step can silently no-op if the previous one did not land:
//
//	report  on a task that was never accepted  -> progress stays 0
//	claim   before progress is booked          -> "task not completed"
//	report  with a reused expert id            -> progress advances once only
//
// The pass therefore re-reads the task list after accepting, and re-reads
// progress after reporting before it claims.

const (
	// growthProgressPolls bounds how long a report is given to land.
	//
	// The upstream books progress within a second or two. Polling harder than
	// this would spend minutes on a task that is simply not going to move.
	growthProgressPolls = 6
	// growthClaimSettle is the pause before a claim, letting progress book.
	growthClaimSettle = 1500 * time.Millisecond
)

// growthLimiter spaces upstream calls per credential.
//
// The reference implementation sleeps a flat interval between reports. Enforcing
// it here instead means every caller path (manual run, scheduled run, night run)
// is spaced identically, and a slow claim cannot silently remove the spacing.
type growthLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newGrowthLimiter() *growthLimiter {
	return &growthLimiter{last: make(map[string]time.Time)}
}

// wait blocks until the gap since the previous call for key has elapsed.
func (l *growthLimiter) wait(ctx context.Context, key string, gap time.Duration) error {
	if l == nil || gap <= 0 {
		return nil
	}
	l.mu.Lock()
	previous, seen := l.last[key]
	now := time.Now()
	if !seen || now.Sub(previous) >= gap {
		l.last[key] = now
		l.mu.Unlock()
		return nil
	}
	delay := gap - now.Sub(previous)
	l.last[key] = now.Add(delay)
	l.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

// growthLogger accumulates the human-readable run log.
type growthLogger struct {
	mu   sync.Mutex
	logs []growthRunLog
}

func (g *growthLogger) add(level, format string, args ...any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.logs = append(g.logs, growthRunLog{Level: level, Message: fmt.Sprintf(format, args...)})
}

func (g *growthLogger) snapshot() []growthRunLog {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]growthRunLog, len(g.logs))
	copy(out, g.logs)
	return out
}

// growthRunner executes one credential's growth pass.
type growthRunner struct {
	client  *workBuddyClient
	limiter *growthLimiter
	gap     time.Duration
	// now is injectable so the night window is testable.
	now func() time.Time
	// log receives progress lines; may be nil.
	log func(growthRunLog)
}

// run executes the full pass for one credential.
func (r *growthRunner) run(ctx context.Context, creds *workBuddyCredentials, label string) growthRunResult {
	logger := &growthLogger{}
	result := growthRunResult{
		Label:      label,
		FinishedAt: r.clock(),
	}
	if creds != nil {
		result.UID = creds.UID
	}
	if creds == nil || creds.AccessToken == "" {
		result.Error = "缺少访问令牌"
		logger.add("error", "缺少访问令牌，跳过")
		result.Logs = logger.snapshot()
		return result
	}

	// The growth centre only exists for the domestic realm; the reference
	// implementation returns "国际版不适用国内成长任务中心" and stops. Reporting
	// events to the international host would only draw 404s.
	variant := variantForCredentials(creds)
	if !variant.hasGrowthCenter() {
		logger.add("skip", "国际版不适用国内成长任务中心，跳过")
		result.OK = true
		result.Logs = logger.snapshot()
		if r.log != nil {
			for _, line := range result.Logs {
				r.log(line)
			}
		}
		return result
	}

	name := firstNonEmpty(label, creds.UID)
	logger.add("info", "开始为账号 %s 运行成长任务", name)

	tasks, errFetch := r.fetch(ctx, creds)
	if errFetch != nil {
		logger.add("error", "获取任务清单失败: %v", errFetch)
		result.Error = errFetch.Error()
		result.Logs = logger.snapshot()
		r.emit(logger)
		return result
	}
	if len(tasks) == 0 {
		logger.add("warn", "未获取到任务清单，请检查网络或账号状态")
		result.Logs = logger.snapshot()
		r.emit(logger)
		return result
	}
	result.TaskCount = len(tasks)

	// Stage 1: accept everything that is not accepted yet.
	tasks = r.acceptPending(ctx, creds, tasks, logger)

	// Stage 2: light up and claim.
	for _, task := range tasks {
		if ctx.Err() != nil {
			logger.add("warn", "任务已中断")
			break
		}
		r.processTask(ctx, creds, task, &result, logger)
	}

	// Stage 3: daily welfare — the cat travel reward is independent of the task
	// list, so it runs even when every task was skipped.
	if travel, errTravel := r.travel(ctx, creds); errTravel != nil {
		logger.add("warn", "猫猫旅行: %v", errTravel)
	} else if travel.Message != "" {
		level := "info"
		if travel.OK {
			level = "ok"
		}
		logger.add(level, "猫猫日常: %s", travel.Message)
		result.Earned += travel.Credit
	}

	// Stage 4: refresh the balance so the panel reports the post-run figure.
	if quota, errQuota := r.client.fetchQuota(ctx, creds); errQuota == nil {
		result.Credits = quota.Credits
		logger.add("info", "🎉 全部完成！本次累计新增到账 +%d 积分，当前总剩余 %d 积分", result.Earned, result.Credits)
	} else {
		logger.add("info", "🎉 全部完成！本次累计新增到账 +%d 积分", result.Earned)
	}

	result.OK = true
	result.FinishedAt = r.clock()
	result.Logs = logger.snapshot()
	r.emit(logger)
	return result
}

func (r *growthRunner) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *growthRunner) emit(logger *growthLogger) {
	if r.log == nil {
		return
	}
	for _, line := range logger.snapshot() {
		r.log(line)
	}
}

func (r *growthRunner) spacing(ctx context.Context, creds *workBuddyCredentials) error {
	key := ""
	if creds != nil {
		key = creds.UID
	}
	if key == "" {
		key = "default"
	}
	return r.limiter.wait(ctx, key, r.gap)
}

func (r *growthRunner) fetch(ctx context.Context, creds *workBuddyCredentials) ([]growthTask, error) {
	if errWait := r.spacing(ctx, creds); errWait != nil {
		return nil, errWait
	}
	return r.client.fetchGrowthTasks(ctx, creds)
}

// acceptPending accepts the not-yet-accepted tasks.
//
// It re-reads the list afterwards and retries the remainder once, because the
// upstream occasionally rejects a whole batch transiently. Skipping this check
// is what produces "accepted a batch, lit none of them": reports against an
// unaccepted task accumulate nothing, so the failure only shows up later as a
// claim error.
func (r *growthRunner) acceptPending(ctx context.Context, creds *workBuddyCredentials, tasks []growthTask, logger *growthLogger) []growthTask {
	var pending []string
	for _, task := range tasks {
		if task.Status == "not_accepted" && !task.Unforgeable {
			pending = append(pending, task.Code)
		}
	}
	if len(pending) == 0 {
		return tasks
	}

	logger.add("info", "发现 %d 个待接取任务，正在批量接取…", len(pending))
	if errWait := r.spacing(ctx, creds); errWait != nil {
		logger.add("warn", "任务已中断")
		return tasks
	}
	accepted, failed, msg, _ := r.client.acceptGrowthTasks(ctx, creds, pending)
	if len(accepted) > 0 {
		logger.add("ok", "✓ 已接取 %d 个任务", len(accepted))
	}
	if len(failed) > 0 {
		detail := strings.Join(firstN(failed, 5), ", ")
		if len(failed) > 5 {
			detail += " …"
		}
		line := fmt.Sprintf("! 接取未成功 %d 个: %s", len(failed), detail)
		if msg != "" {
			line += " (" + msg + ")"
		}
		logger.add("warn", "%s", line)
	}

	// Re-read, then retry whatever is still pending.
	fresh, errFetch := r.fetch(ctx, creds)
	if errFetch != nil {
		logger.add("warn", "! 接取后无法获取任务清单，本轮中止")
		return tasks
	}
	still := make([]string, 0, len(fresh))
	for _, task := range fresh {
		if task.Status == "not_accepted" {
			still = append(still, task.Code)
		}
	}
	if len(still) > 0 {
		logger.add("info", "仍有 %d 个未接取，重试接取一次…", len(still))
		if errWait := r.spacing(ctx, creds); errWait == nil {
			accepted, _, _, _ := r.client.acceptGrowthTasks(ctx, creds, still)
			if len(accepted) > 0 {
				logger.add("ok", "✓ 重试接取成功 %d 个", len(accepted))
			}
		}
		fresh, errFetch = r.fetch(ctx, creds)
		if errFetch != nil {
			logger.add("warn", "! 重试后无法获取任务清单，沿用上一份")
			return tasks
		}
	}

	// Warn about anything that will be skipped, so the reason is visible before
	// the per-task lines rather than only as a tally at the end.
	var stubborn []string
	for _, task := range fresh {
		if task.Status == "not_accepted" {
			stubborn = append(stubborn, task.Code)
		}
	}
	if len(stubborn) > 0 {
		logger.add("warn", "! 仍有 %d 个任务处于未接取状态，对未接取任务上报事件不会计入进度，本轮跳过这些任务", len(stubborn))
	}
	return fresh
}

// processTask handles one task: claim if done, skip if impossible, otherwise
// report until the target is met and then claim.
func (r *growthRunner) processTask(ctx context.Context, creds *workBuddyCredentials, task growthTask, result *growthRunResult, logger *growthLogger) {
	spec, known := growthTaskSpecs[task.Code]
	// A task the upstream lists but this build has no recipe for cannot be lit;
	// reporting a heartbeat would be noise.
	if !known || spec.Unforgeable {
		if task.Unforgeable {
			logger.add("skip", "⏭ 任务 [%s] 需真实捐款动作，跳过", task.Name)
		} else {
			logger.add("skip", "⏭ 任务 [%s] 无对应事件配方，跳过", task.Name)
		}
		result.Skipped++
		return
	}
	if task.Status == "claimed" {
		return
	}

	// Already complete: claim it. A task can be completed by the operator
	// using the desktop client, or by the night pass; skipping it because it is
	// "desktop only" would throw that reward away.
	if task.Status == "completed" || task.Current >= task.Target {
		r.claim(ctx, creds, task, result, logger)
		return
	}

	if reason, desktop := growthDesktopOnlyTasks[task.Code]; desktop {
		jump := firstNonEmpty(task.JumpURL, "workbuddy://chat")
		logger.add("skip", "⏭ 任务 [%s] 需真实操作完成: %s（深链 %s），跳过事件伪造", task.Name, reason, jump)
		result.Skipped++
		return
	}

	if growthNightTaskCodes[task.Code] && !inNightWindow(r.clock()) {
		logger.add("skip", "🌙 任务 [%s] 仅 23:00-08:00 上报计数，当前不在窗口，跳过", task.Name)
		result.Skipped++
		return
	}

	if task.Status == "not_accepted" {
		// Reporting against an unaccepted task is wasted: the upstream only
		// accumulates progress for accepted tasks.
		logger.add("skip", "⏭ 任务 [%s] 仍未接取，跳过（先解决接取失败）", task.Name)
		result.Skipped++
		return
	}

	// Report until the target is met.
	need := task.Target - task.Current
	if need < 1 {
		need = 1
	}
	logger.add("info", "正在点亮任务 [%s]（需上报 %d 次）…", task.Name, need)

	kind := spec.Kind
	reportOK := true
	for i := 0; i < need; i++ {
		expert := growthExpertFor(kind, task.Current+i)
		event := buildGrowthEvent(creds, kind, i, expert)
		if errWait := r.spacing(ctx, creds); errWait != nil {
			logger.add("warn", "任务已中断")
			return
		}
		if !r.client.reportGrowthEvents(ctx, creds, []growthEvent{event}) {
			reportOK = false
		}
	}
	if !reportOK {
		logger.add("warn", "! 任务 [%s] 部分事件上报失败（上游拒绝），继续尝试领奖", task.Name)
		result.Failed++
	}

	// Give the upstream a moment to book the progress before claiming.
	if errWait := r.limiter.wait(ctx, limiterKey(creds), growthClaimSettle); errWait != nil {
		logger.add("warn", "任务已中断")
		return
	}

	progress := task.Current
	for attempt := 0; attempt < growthProgressPolls; attempt++ {
		fresh, errFetch := r.client.fetchGrowthTasks(ctx, creds)
		if errFetch != nil {
			break
		}
		for _, candidate := range fresh {
			if candidate.Code != task.Code {
				continue
			}
			progress = candidate.Current
			if progress >= task.Target || candidate.Status == "completed" || candidate.Status == "claimed" {
				attempt = growthProgressPolls
			}
			break
		}
		if progress >= task.Target {
			break
		}
		// Poll faster while progress is actually moving; a task that has not
		// budged after two checks is not going to.
		if attempt >= 2 {
			break
		}
		if errWait := r.limiter.wait(ctx, limiterKey(creds), growthClaimSettle); errWait != nil {
			return
		}
	}

	if progress < task.Target {
		logger.add("warn", "? 任务 [%s] 已上报但进度 %d/%d 未达成，领奖顺延到下次运行", task.Name, progress, task.Target)
		return
	}

	r.claim(ctx, creds, task, result, logger)
}

// claim collects a task reward and records the outcome.
func (r *growthRunner) claim(ctx context.Context, creds *workBuddyCredentials, task growthTask, result *growthRunResult, logger *growthLogger) {
	if errWait := r.spacing(ctx, creds); errWait != nil {
		return
	}
	credit, _, msg, ok := r.client.claimGrowthTask(ctx, creds, task.Code)
	if !ok {
		logger.add("warn", "! 任务 [%s] 领奖失败: %s", task.Name, firstNonEmpty(msg, "未知原因"))
		result.Failed++
		return
	}
	result.Earned += credit
	result.Claimed++
	logger.add("ok", "✓ 任务 [%s] 领奖成功: +%d 积分", task.Name, credit)
}

func limiterKey(creds *workBuddyCredentials) string {
	if creds == nil || creds.UID == "" {
		return "default"
	}
	return creds.UID
}

// firstN returns at most n entries, for log truncation.
func firstN(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

// sortGrowthTasks orders tasks by descending reward then code, so a truncated
// UI shows the most valuable work first.
func sortGrowthTasks(tasks []growthTask) {
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].Reward != tasks[j].Reward {
			return tasks[i].Reward > tasks[j].Reward
		}
		return tasks[i].Code < tasks[j].Code
	})
}
