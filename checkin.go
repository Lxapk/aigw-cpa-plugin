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
	AuthID    string    `json:"auth_id"`
	Label     string    `json:"label"`
	UID       string    `json:"uid"`
	Domain    string    `json:"domain"`
	Success   bool      `json:"success"`
	Message   string    `json:"message"`
	Already   bool      `json:"already_checked_in"`
	Code      int       `json:"code"`
	Status    int       `json:"http_status,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`
}

// checkinRun is one pass over all accounts.
type checkinRun struct {
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at"`
	Trigger    string          `json:"trigger"` // "manual" | "auto" | "startup"
	Total      int             `json:"total"`
	Succeeded  int             `json:"succeeded"`
	Failed     int             `json:"failed"`
	Results    []checkinResult `json:"results"`
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
	return &checkinState{stopCh: make(chan struct{})}
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
func collectCheckinAccounts() ([]checkinAccount, error) {
	raw, errList := callHost("host.auth.list", map[string]any{})
	if errList != nil {
		return nil, errList
	}

	// Accept either {"auths":[...]} or a bare array.
	var entries []hostAuthEntry
	if len(raw) > 0 {
		var wrapper struct {
			Auths []hostAuthEntry `json:"auths"`
			Items []hostAuthEntry `json:"items"`
		}
		if errUnmarshal := json.Unmarshal(raw, &wrapper); errUnmarshal == nil {
			entries = wrapper.Auths
			if len(entries) == 0 {
				entries = wrapper.Items
			}
		}
		if len(entries) == 0 {
			_ = json.Unmarshal(raw, &entries)
		}
	}

	var out []checkinAccount
	for _, entry := range entries {
		if !isWorkBuddyProvider(entry.Provider) && !isWorkBuddyProvider(entry.Type) {
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
		out = append(out, checkinAccount{
			AuthID: id,
			Label:  firstNonEmpty(entry.Label, entry.Name, creds.label()),
			Creds:  creds,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].AuthID < out[j].AuthID })
	return out, nil
}

// hostAuthEntry mirrors the subset of host.auth.list / host.auth.get we need.
type hostAuthEntry struct {
	AuthIndex   string          `json:"auth_index"`
	AuthID      string          `json:"id"`
	Provider    string          `json:"provider"`
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Label       string          `json:"label"`
	Disabled    bool            `json:"disabled"`
	StorageJSON json.RawMessage `json:"storage_json"`
}

// fetchAuthStorage asks the host for one credential's stored JSON.
func fetchAuthStorage(authIndex string) json.RawMessage {
	raw, errGet := callHost("host.auth.get", map[string]any{"auth_index": authIndex})
	if errGet != nil || len(raw) == 0 {
		return nil
	}
	var entry hostAuthEntry
	if errUnmarshal := json.Unmarshal(raw, &entry); errUnmarshal != nil {
		return nil
	}
	return entry.StorageJSON
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
		if res.Success {
			run.Succeeded++
		} else {
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
	res := checkinResult{
		AuthID:    account.AuthID,
		Label:     account.Label,
		UID:       account.Creds.UID,
		Domain:    account.Creds.Domain,
		CheckedAt: time.Now(),
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

	go checkinLoop()
}

// checkinLoop wakes up periodically and runs the daily check-in when due.
//
// A short tick interval keeps the trigger responsive without needing cron;
// lastAutoDay makes the run at-most-once per calendar day.
func checkinLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// Catch-up run at startup when configured.
	settings := state.settings.get()
	if settings.Checkin.Enabled && settings.Checkin.OnStart && !checkinRanToday(settings.Checkin) {
		runCheckin("startup")
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
			runCheckin("auto")
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
