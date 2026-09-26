package main

import (
	"context"
	"encoding/json"
	"sort"

	"sync"
	"time"
)

// This file adds the check-in feature on top of the ported WorkBuddy client:
//
//	* manual run   : check in every WorkBuddy account now
//	* automatic run: a daily schedule, driven by config
//
// Results are kept in memory (bounded) and surfaced on the management page.

// checkinSettings is the check-in half of the plugin configuration.
type checkinSettings struct {
	// Enabled turns the automatic daily run on or off.
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Hour/Minute is the local time of day for the automatic run.
	Hour   int `json:"hour" yaml:"hour"`
	Minute int `json:"minute" yaml:"minute"`
	// OnStart also runs a catch-up pass when the plugin loads and today's run
	// has not happened yet.
	OnStart bool `json:"on_start" yaml:"on_start"`
	// RetryOnDeviceFingerprint mirrors the source app's behaviour of sleeping
	// 8s and retrying once when the upstream rejects the device fingerprint
	// (d2/C0482C.java catches code 9074 and retries).
	RetryOnDeviceFingerprint bool `json:"retry_on_device_fingerprint" yaml:"retry_on_device_fingerprint"`
}

// defaultCheckinSettings: the automatic run is off by default so the plugin
// never calls out on the operator's behalf unless asked.
func defaultCheckinSettings() checkinSettings {
	return checkinSettings{
		Enabled:                  false,
		Hour:                     9,
		Minute:                   0,
		OnStart:                  false,
		RetryOnDeviceFingerprint: true,
	}
}

// checkinResult is one account's outcome, as shown on the management page.
type checkinResult struct {
	AuthID  string `json:"auth_id"`
	Label   string `json:"label"`
	UID     string `json:"uid"`
	Domain  string `json:"domain"`
	Success bool   `json:"success"`
	// Skipped marks an account the pass deliberately did not check in, such as
	// an international credential with no check-in endpoint. It is distinct from
	// a failure so the UI does not paint it red.
	Skipped   bool      `json:"skipped,omitempty"`
	Message   string    `json:"message"`
	Already   bool      `json:"already_checked_in"`
	Code      int       `json:"code"`
	Status    int       `json:"http_status,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`
}

// checkinRun is one pass over all accounts.
type checkinRun struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Trigger    string    `json:"trigger"` // "manual" | "auto" | "startup"
	Total      int       `json:"total"`
	Succeeded  int       `json:"succeeded"`
	Failed     int       `json:"failed"`
	// Skipped counts accounts the pass intentionally did not attempt, so that
	// Total == Succeeded + Failed + Skipped always holds.
	Skipped int             `json:"skipped,omitempty"`
	Results []checkinResult `json:"results"`
}

// checkinState holds the scheduler + history.
type checkinState struct {
	mu sync.Mutex
	// history is newest-first, bounded by historyMax.
	history []checkinRun
	// lastAutoDay records the date of the last successful automatic run so the
	// daily trigger fires at most once per day.
	lastAutoDay string
	// running guards against overlapping runs.
	running bool
	// stopCh ends the background loop on shutdown.
	stopCh chan struct{}
	// started reports whether the loop is active (so we never start twice).
	started bool
}

const checkinHistoryMax = 20

func newCheckinState() *checkinState {
	return &checkinState{stopCh: make(chan struct{}, 1)}
}

// ---- account enumeration -------------------------------------------------

// checkinAccount is one credential eligible for check-in.
type checkinAccount struct {
	AuthID string
	Label  string
	Creds  *workBuddyCredentials
}

// collectCheckinAccounts reads every WorkBuddy credential CPA knows about.
//
// The host exposes the auth-file inventory through host.auth.list; each entry's
// storage is read with host.auth.get. Credentials we cannot parse are skipped
// rather than failing the whole run.
// collectCheckinAccounts enumerates the credentials this plugin owns.
//
// This is the single inventory used by the check-in pass, the quota refresh and
// the task engine, so its filter decides what "this plugin's accounts" means
// everywhere. It must therefore use the same strict rule as the panel's account
// list (isWorkBuddyAuthEntry); an earlier version tested only provider/type,
// which disagreed with the panel whenever a host exposed a credential solely
// through its file name — the account showed up in the list but was missing
// from quota totals and check-in.
func collectCheckinAccounts() ([]checkinAccount, error) {
	raw, errList := callHost("host.auth.list", map[string]any{})
	if errList != nil {
		return nil, errList
	}

	// Use the shared decoder: CPA nests the entries under "files".
	entries := decodeAuthEntries(raw)

	var out []checkinAccount
	for _, entry := range entries {
		if !isWorkBuddyAuthEntry(entry) {
			continue
		}
		storage := entry.StorageJSON
		if len(storage) == 0 && entry.AuthIndex != "" {
			// Fall back to reading the file contents.
			if fetched := fetchAuthStorage(entry.AuthIndex); len(fetched) > 0 {
				storage = fetched
			}
		}
		creds, errParse := parseWorkBuddyCredentials(storage)
		if errParse != nil {
			continue
		}
		id := entry.AuthIndex
		if id == "" {
			id = creds.authID()
		}
		// Skip accounts the operator disabled from the panel, or the host has
		// parked. The task engine and the scheduled passes share this list, so
		// filtering here keeps every caller consistent.
		if state.pool.isAccountDisabled(creds.UID, id) {
			continue
		}
		out = append(out, checkinAccount{
			AuthID: id,
			Label:  firstNonEmpty(entry.Label, entry.Name, creds.label()),
			Creds:  creds,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].AuthID < out[j].AuthID })
	return out, nil
}

// fetchAuthStorage asks the host for one credential's stored JSON.
//
// The response nests the body under "json" (rpcHostAuthGetResponse); reading a
// wrong key here leaves StorageJSON nil and the credential looks unparsable.
func fetchAuthStorage(authIndex string) json.RawMessage {
	raw, errGet := callHost("host.auth.get", map[string]any{"auth_index": authIndex})
	if errGet != nil || len(raw) == 0 {
		return nil
	}
	var resp hostAuthGetResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return nil
	}
	return resp.credentialJSON()
}

// ---- running -------------------------------------------------------------

// runCheckin performs one pass over every account. trigger is "manual",
// "auto" or "startup" and only affects reporting.
func runCheckin(trigger string) *checkinRun {
	state.checkin.mu.Lock()
	if state.checkin.running {
		state.checkin.mu.Unlock()
		return &checkinRun{
			Trigger: trigger,
			Results: []checkinResult{{Error: "已有签到任务正在运行"}},
		}
	}
	state.checkin.running = true
	state.checkin.mu.Unlock()

	defer func() {
		state.checkin.mu.Lock()
		state.checkin.running = false
		state.checkin.mu.Unlock()
	}()

	run := &checkinRun{StartedAt: time.Now(), Trigger: trigger}

	accounts, errCollect := collectCheckinAccounts()
	if errCollect != nil {
		run.FinishedAt = time.Now()
		run.Results = []checkinResult{{Error: "读取账号失败：" + errCollect.Error()}}
		state.checkin.record(run)
		return run
	}
	if len(accounts) == 0 {
		run.FinishedAt = time.Now()
		run.Results = []checkinResult{{Error: "没有可签到的 WorkBuddy 账号"}}
		state.checkin.record(run)
		return run
	}

	settings := state.settings.get()
	for _, account := range accounts {
		run.Total++
		res := checkinOne(account, settings.Checkin)
		switch {
		case res.Success:
			run.Succeeded++
		case res.Skipped:
			// Deliberately not attempted (e.g. an international credential with
			// no check-in endpoint); counting it as a failure would report a
			// red run for a correct decision.
			run.Skipped++
		default:
			run.Failed++
		}
		run.Results = append(run.Results, res)
	}

	run.FinishedAt = time.Now()
	state.checkin.record(run)
	return run
}

// checkinOne performs a single account's check-in, including the source app's
// one-shot retry when the device fingerprint is rejected.
func checkinOne(account checkinAccount, cfg checkinSettings) checkinResult {
	variant := variantForCredentials(account.Creds)
	res := checkinResult{
		AuthID:    account.AuthID,
		Label:     account.Label,
		UID:       account.Creds.UID,
		Domain:    account.Creds.Domain,
		CheckedAt: time.Now(),
	}

	// The international build has no check-in endpoint (see
	// wbVariant.hasCheckin), so sending one would only ever 404 and get
	// reported as a failure for every international account. Report it as a
	// skipped result instead of a fault.
	if !variant.hasCheckin() {
		res.Skipped = true
		res.Message = "国际版无签到功能"
		return res
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	outcome, errCheckin := workBuddyUpstream.checkin(ctx, account.Creds)
	if errCheckin != nil {
		res.Error = errCheckin.Error()
		res.Message = "签到失败"
		return res
	}

	// Retry once on a device-fingerprint rejection, as d2/C0482C.java does.
	if cfg.RetryOnDeviceFingerprint && !outcome.Success && outcome.Code == checkinCodeDeviceFingerprint {
		select {
		case <-time.After(checkinDeviceFingerprintRetryDelay):
		case <-ctx.Done():
			res.Error = ctx.Err().Error()
			return res
		}
		if retry, errRetry := workBuddyUpstream.checkin(ctx, account.Creds); errRetry == nil {
			outcome = retry
		}
	}

	res.Success = outcome.Success
	res.Message = outcome.Message
	res.Already = outcome.AlreadyCheckedIn
	res.Code = outcome.Code
	res.Status = outcome.HTTPStatus
	return res
}

const (
	// checkinCodeDeviceFingerprint is the upstream code the source app retries
	// on (d2/C0482C.java compares against 9074).
	checkinCodeDeviceFingerprint = 9074
	// checkinDeviceFingerprintRetryDelay mirrors the source's Thread.sleep(8000).
	checkinDeviceFingerprintRetryDelay = 8 * time.Second
)

// ---- history -------------------------------------------------------------

func (s *checkinState) record(run *checkinRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.history = append([]checkinRun{*run}, s.history...)
	if len(s.history) > checkinHistoryMax {
		s.history = s.history[:checkinHistoryMax]
	}
	if run.Trigger == "auto" && run.FinishedAt.After(run.StartedAt) {
		s.lastAutoDay = run.StartedAt.Format("2006-01-02")
	}
}

func (s *checkinState) snapshot(limit int) []checkinRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.history) {
		limit = len(s.history)
	}
	out := make([]checkinRun, limit)
	copy(out, s.history[:limit])
	return out
}

func (s *checkinState) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *checkinState) lastAutoDate() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAutoDay
}

// ---- scheduler -----------------------------------------------------------

// startCheckinScheduler launches the background loop exactly once.
func startCheckinScheduler() {
	state.checkin.mu.Lock()
	if state.checkin.started {
		state.checkin.mu.Unlock()
		return
	}
	state.checkin.started = true
	state.checkin.mu.Unlock()

	safeGo("checkin-loop", checkinLoop)
}

// checkinLoop wakes up periodically and runs the daily check-in when due.
//
// A short tick interval keeps the trigger responsive without needing cron;
// lastAutoDay makes the run at-most-once per calendar day.
func checkinLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// Catch-up run at startup when configured.
	//
	// Guarded like the scheduled ticks: an unguarded panic here would end the
	// goroutine before the select loop starts, so the scheduler would never run
	// again and nothing would report why.
	settings := state.settings.get()
	if settings.Checkin.Enabled && settings.Checkin.OnStart && !checkinRanToday(settings.Checkin) {
		guardLoop("checkin-startup", func() {
			runCheckin("startup")
		})
	}

	for {
		select {
		case <-state.checkin.stopCh:
			return
		case <-ticker.C:
			cfg := state.settings.get().Checkin
			if !cfg.Enabled {
				continue
			}
			if checkinRanToday(cfg) {
				continue
			}
			if !checkinDueNow(cfg) {
				continue
			}
			guardLoop("checkin-tick", func() {
				runCheckin("auto")
			})
		}
	}
}

// checkinRanToday reports whether the automatic run already happened today.
func checkinRanToday(_ checkinSettings) bool {
	return state.checkin.lastAutoDate() == time.Now().Format("2006-01-02")
}

// checkinDueNow reports whether the configured time of day has been reached.
func checkinDueNow(cfg checkinSettings) bool {
	now := time.Now()
	target := time.Date(now.Year(), now.Month(), now.Day(), clampHour(cfg.Hour), clampMinute(cfg.Minute), 0, 0, now.Location())
	return !now.Before(target)
}

func clampHour(h int) int {
	if h < 0 {
		return 0
	}
	if h > 23 {
		return 23
	}
	return h
}

func clampMinute(m int) int {
	if m < 0 {
		return 0
	}
	if m > 59 {
		return 59
	}
	return m
}

// stopCheckinScheduler ends the background loop.
func stopCheckinScheduler() {
	state.checkin.mu.Lock()
	started := state.checkin.started
	state.checkin.started = false
	state.checkin.mu.Unlock()
	if !started {
		return
	}
	// Buffered send: see stopQuotaScheduler. An unbuffered channel would drop
	// the signal whenever the loop happened to be mid-check-in, leaving a
	// running loop behind a "started = false" flag.
	select {
	case state.checkin.stopCh <- struct{}{}:
	default:
	}
}

// ---- management helpers --------------------------------------------------

// runFromManagement is the entry point used by the management HTTP handler.
func runFromManagement() *checkinRun {
	return runCheckin("manual")
}

// applyCheckinConfig validates and stores the check-in settings.
func applyCheckinConfig(cfg checkinSettings) checkinSettings {
	cfg.Hour = clampHour(cfg.Hour)
	cfg.Minute = clampMinute(cfg.Minute)
	state.settings.setCheckin(cfg)
	return cfg
}

// checkinStatusJSON is the payload for the status/summary endpoint.
func checkinStatusJSON() map[string]any {
	cfg := state.settings.get().Checkin
	return map[string]any{
		"enabled":                     cfg.Enabled,
		"hour":                        cfg.Hour,
		"minute":                      cfg.Minute,
		"on_start":                    cfg.OnStart,
		"retry_on_device_fingerprint": cfg.RetryOnDeviceFingerprint,
		"running":                     state.checkin.isRunning(),
		"last_auto_day":               state.checkin.lastAutoDate(),
		"next_run":                    nextCheckinTime(cfg),
		"history":                     state.checkin.snapshot(10),
	}
}

// nextCheckinTime renders the next scheduled run, or "" when disabled.
func nextCheckinTime(cfg checkinSettings) string {
	if !cfg.Enabled {
		return ""
	}
	now := time.Now()
	target := time.Date(now.Year(), now.Month(), now.Day(), clampHour(cfg.Hour), clampMinute(cfg.Minute), 0, 0, now.Location())
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	return target.Format(time.RFC3339)
}
