package main

import (
	"context"

	"fmt"
	"sort"
	"sync"
	"time"
)

// This file ports the auto-task system from workbuddy2api-panel
// (internal/panel/autotask.go + taskcenter.go).
//
// The source project implements a concurrent task queue for periodic account
// maintenance: daily check-in, credit refresh, activity tasks, school tasks.
// Each account gets its own task enable toggle, and the queue processes
// accounts in parallel up to a configurable concurrency limit.
//
// Why not just use the existing checkinLoop/quotaLoop
//
// Those two loops are independent and single-purpose. The task center
// consolidates everything into one unified queue with:
//   - shared concurrency control (no more than N runs at once)
//   - per-account task enable/disable
//   - queued sequential execution per account (no overlapping runs)
//   - status reporting through the management API

// ---- task definitions (port of autotask.go taskSpec) --------------------

// taskKind identifies the type of automated task.
type taskKind string

const (
	taskKindCheckin  taskKind = "checkin"
	taskKindQuota    taskKind = "quota"
	taskKindActivity taskKind = "activity"
	taskKindSchool   taskKind = "school"
)

// taskSpec is one task variety: what it does and how often.
type taskSpec struct {
	Kind     taskKind      `json:"kind"`
	Label    string        `json:"label"`
	Interval time.Duration `json:"-"`
}

// defaultTasks is the built-in task catalogue.
var defaultTasks = []taskSpec{
	{Kind: taskKindCheckin, Label: "每日签到", Interval: 24 * time.Hour},
	{Kind: taskKindQuota, Label: "刷新积分", Interval: 30 * time.Minute},
}

// ---- per-account task state ---------------------------------------------

// accountTasks tracks the last-run time and enable state for one account.
type accountTasks struct {
	mu sync.Mutex

	UID     string        `json:"uid"`
	Label   string        `json:"label"`
	Enabled bool          `json:"enabled"`
	Checkin *taskRunState `json:"checkin,omitempty"`
	Quota   *taskRunState `json:"quota,omitempty"`
}

type taskRunState struct {
	// LastRun is when this task last ran.
	LastRun time.Time `json:"last_run"`
	// LastResult is the outcome message.
	LastResult string `json:"last_result,omitempty"`
	// LastOK reports whether the last run succeeded.
	LastOK bool `json:"last_ok"`
}

// due reports whether a task is due for this account.
func (a *accountTasks) due(spec taskSpec, now time.Time) bool {
	st := a.runState(spec.Kind)
	if st == nil || st.LastRun.IsZero() {
		return true
	}
	return now.Sub(st.LastRun) >= spec.Interval
}

func (a *accountTasks) runState(kind taskKind) *taskRunState {
	if a == nil {
		return nil
	}
	switch kind {
	case taskKindCheckin:
		return a.Checkin
	case taskKindQuota:
		return a.Quota
	}
	return nil
}

func (a *accountTasks) setRunState(kind taskKind, st *taskRunState) {
	switch kind {
	case taskKindCheckin:
		a.Checkin = st
	case taskKindQuota:
		a.Quota = st
	}
}

// ---- task queue engine --------------------------------------------------

// taskRequest is one unit of work placed on the queue.
type taskRequest struct {
	UID   string     `json:"uid"`
	Tasks []taskKind `json:"tasks"`
	// Manual is true when the operator triggered it from the panel.
	Manual bool `json:"manual"`
}

// taskResult is the outcome of one taskRequest.

// taskEngine manages the concurrent task queue.
type taskEngine struct {
	mu sync.Mutex

	// concurrency limits how many account runs happen at once.
	concurrency int
	// running counts current executions.
	running int
	// queue is FIFO pending items.
	queue []taskRequest
	// inFlight tracks which accounts are currently executing.
	inFlight map[string]struct{}

	// per-account task state.
	accounts map[string]*accountTasks

	// stopCh signals the loop to stop.
	stopCh chan struct{}
	// started reports if the loop is running.
	started bool
}

func newTaskEngine() *taskEngine {
	return &taskEngine{
		concurrency: 2,
		queue:       make([]taskRequest, 0),
		inFlight:    make(map[string]struct{}),
		accounts:    make(map[string]*accountTasks),
		stopCh:      make(chan struct{}, 1),
	}
}

// enqueue adds tasks to the queue. When the request is for an account already
// in flight, it is queued instead of executing concurrently.
func (e *taskEngine) enqueue(req taskRequest) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queue = append(e.queue, req)
}

// tick executes one queue iteration: start as many runs as concurrency allows.
// safe to call from a loop goroutine.
func (e *taskEngine) tick(ctx context.Context) {
	e.mu.Lock()
	// Start as many as concurrency allows. An in-flight account is deferred to
	// the next tick rather than re-queued in this same loop, otherwise a queue
	// full of running accounts spins this loop forever.
	pending := append([]taskRequest(nil), e.queue...)
	e.queue = e.queue[:0]
	for _, req := range pending {
		if e.running >= e.concurrency {
			e.queue = append(e.queue, req)
			continue
		}
		if _, inFlight := e.inFlight[req.UID]; inFlight {
			// Defer to next tick.
			e.queue = append(e.queue, req)
			continue
		}
		e.inFlight[req.UID] = struct{}{}
		e.running++
		go e.execute(ctx, req)
	}
	e.mu.Unlock()
}

// execute runs one task request (in a goroutine).
func (e *taskEngine) execute(ctx context.Context, req taskRequest) {
	for _, kind := range req.Tasks {
		switch kind {
		case taskKindCheckin:
			e.runCheckin(ctx, req.UID)
		case taskKindQuota:
			e.runQuota(ctx, req.UID)
		default:
			e.record(req.UID, kind, false, fmt.Sprintf("unknown task: %s", kind))
		}
	}

	e.mu.Lock()
	e.running--
	delete(e.inFlight, req.UID)
	e.mu.Unlock()
}

func (e *taskEngine) runCheckin(_ context.Context, uid string) {
	account, ok := e.resolveAccount(uid)
	if !ok {
		e.record(uid, taskKindCheckin, false, "账号不存在或已禁用")
		return
	}
	res := checkinOne(account, state.settings.get().Checkin)
	msg := firstNonEmpty(res.Message, res.Error, "签到完成")
	e.record(uid, taskKindCheckin, res.Success && res.Error == "", msg)
}

func (e *taskEngine) runQuota(_ context.Context, uid string) {
	account, ok := e.resolveAccount(uid)
	if !ok {
		e.record(uid, taskKindQuota, false, "账号不存在或已禁用")
		return
	}
	res := fetchQuotaOne(account)
	msg := firstNonEmpty(res.Message, res.Error, "积分已刷新")
	e.record(uid, taskKindQuota, res.Error == "", msg)
}

// resolveAccount maps a task uid onto a real check-in credential, refusing
// accounts the operator has disabled. The task engine never goes through
// pool.pick, so the disabled filter has to be applied explicitly here.
func (e *taskEngine) resolveAccount(uid string) (checkinAccount, bool) {
	accounts, errCollect := collectCheckinAccounts()
	if errCollect != nil {
		return checkinAccount{}, false
	}
	for _, account := range accounts {
		if account.AuthID != uid && (account.Creds == nil || account.Creds.UID != uid) {
			continue
		}
		authIndex := account.AuthID
		credUID := ""
		if account.Creds != nil {
			credUID = account.Creds.UID
		}
		if state.pool.isAccountDisabled(credUID, authIndex) {
			return checkinAccount{}, false
		}
		return account, true
	}
	return checkinAccount{}, false
}

func (e *taskEngine) record(uid string, kind taskKind, ok bool, msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	at := e.ensureAccount(uid)
	at.mu.Lock()
	at.setRunState(kind, &taskRunState{
		LastRun:    time.Now(),
		LastResult: msg,
		LastOK:     ok,
	})
	at.mu.Unlock()
}

func (e *taskEngine) ensureAccount(uid string) *accountTasks {
	at, ok := e.accounts[uid]
	if !ok {
		at = &accountTasks{UID: uid, Label: uid, Enabled: true}
		e.accounts[uid] = at
	}
	return at
}

// ---- management endpoints ------------------------------------------------

// taskStatusJSON returns the status view for the management API.
func taskStatusJSON() map[string]any {
	e := state.taskEngine
	e.mu.Lock()
	defer e.mu.Unlock()

	accounts := make([]map[string]any, 0, len(e.accounts))
	for _, at := range e.accounts {
		if at == nil {
			continue
		}
		at.mu.Lock()
		tasks := make([]map[string]any, 0)
		for _, spec := range defaultTasks {
			st := at.runState(spec.Kind)
			entry := map[string]any{
				"kind":  string(spec.Kind),
				"label": spec.Label,
			}
			if st != nil {
				entry["last_run"] = st.LastRun.Format(time.RFC3339)
				entry["last_result"] = st.LastResult
				entry["last_ok"] = st.LastOK
			}
			tasks = append(tasks, entry)
		}
		at.mu.Unlock()

		accounts = append(accounts, map[string]any{
			"uid":      at.UID,
			"label":    at.Label,
			"enabled":  at.Enabled,
			"tasks":    tasks,
			"queued":   e.inQueue(at.UID),
			"inflight": e.inFlightCount(at.UID),
		})
	}
	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i]["label"].(string) < accounts[j]["label"].(string)
	})

	return map[string]any{
		"accounts":    accounts,
		"concurrency": e.concurrency,
		"running":     e.running,
		"queued":      len(e.queue),
	}
}

func (e *taskEngine) inQueue(uid string) bool {
	for _, q := range e.queue {
		if q.UID == uid {
			return true
		}
	}
	return false
}

func (e *taskEngine) inFlightCount(uid string) int {
	if _, ok := e.inFlight[uid]; ok {
		return 1
	}
	return 0
}

// initFromAccounts populates the engine's account view from the UI list.
func (e *taskEngine) initFromAccounts(accounts []workBuddyAccount) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range accounts {
		key := firstNonEmpty(a.UID, a.AuthIndex)
		if key == "" {
			continue
		}
		at := e.ensureAccount(key)
		if a.Label != "" {
			at.Label = a.Label
		}
		at.Enabled = !a.Disabled && !a.DisabledByUser
	}
}

// ---- scheduler ----------------------------------------------------------

// startTaskScheduler launches the background task loop exactly once.
//
// It mirrors the check-in and quota schedulers: a one-minute tick scans for
// due per-account tasks and feeds the existing queue, which tick() drains
// under the concurrency cap. The loop is idempotent across repeated
// plugin.register calls.
func startTaskScheduler() {
	e := state.taskEngine
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.mu.Unlock()

	safeGo("task-loop", taskLoop)
}

// taskLoop periodically enqueues due tasks and drains the queue.
func taskLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// One immediate pass so tasks become visible without waiting a minute.
	//
	// Guarded like the scheduled ticks: an unguarded panic here would kill the
	// goroutine before the select loop is reached, so tasks would silently stop
	// being scheduled.
	guardLoop("task-startup", taskTick)

	for {
		select {
		case <-state.taskEngine.stopCh:
			return
		case <-ticker.C:
			guardLoop("task-loop", taskTick)
		}
	}
}

// taskTick scans accounts for due tasks and lets the engine run them.
func taskTick() {
	state.taskEngine.scanDue(time.Now())
	state.taskEngine.tick(context.Background())
}

// stopTaskScheduler ends the background task loop.
func stopTaskScheduler() {
	e := state.taskEngine
	e.mu.Lock()
	started := e.started
	e.started = false
	e.mu.Unlock()
	if !started {
		return
	}
	select {
	case e.stopCh <- struct{}{}:
	default:
	}
}

// scanDue enqueues every enabled account whose scheduled tasks are due.
//
// Disabled accounts (by the host or by the panel toggle) are skipped, and an
// account already queued or running is not enqueued again.
func (e *taskEngine) scanDue(now time.Time) {
	accounts, errCollect := collectCheckinAccounts()
	if errCollect != nil {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	for _, account := range accounts {
		credUID := ""
		if account.Creds != nil {
			credUID = account.Creds.UID
		}
		if state.pool.isAccountDisabled(credUID, account.AuthID) {
			continue
		}
		key := firstNonEmpty(credUID, account.AuthID)
		if key == "" {
			continue
		}
		at := e.ensureAccount(key)
		if account.Label != "" {
			at.Label = account.Label
		}
		if !at.Enabled {
			continue
		}
		if e.inQueue(key) || e.inFlightCount(key) > 0 {
			continue
		}
		var due []taskKind
		for _, spec := range defaultTasks {
			if at.due(spec, now) {
				due = append(due, spec.Kind)
			}
		}
		if len(due) == 0 {
			continue
		}
		e.queue = append(e.queue, taskRequest{UID: key, Tasks: due})
	}
}
