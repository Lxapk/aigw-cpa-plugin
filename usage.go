package main

import (
	"sync"
	"time"
)

// callRecord ports V1.f2.C0541a, the per-request log entry written by V1/o.r().
//
// Original constructor arguments (in order):
//
//	providerID, startedAt, uid, model, label, requestedModel, stream,
//	kind(f2.b), statusCode, promptTokens, completionTokens, totalTokens,
//	latencyMillis, errorText, responsePreview(<=8192), requestPreview(<=8192)
type callRecord struct {
	ProviderID string `json:"provider_id"`
	// Variant is the supplier realm that served the call ("cn" / "ai").
	// ProviderID alone cannot distinguish them: it is the constant "codebuddy"
	// for both realms.
	Variant          string    `json:"variant,omitempty"`
	UID              string    `json:"uid"`
	Label            string    `json:"label"`
	Model            string    `json:"model"`
	RequestedModel   string    `json:"requested_model"`
	Stream           bool      `json:"stream"`
	StatusCode       int       `json:"status_code"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	LatencyMillis    int64     `json:"latency_millis"`
	Error            string    `json:"error,omitempty"`
	StartedAt        time.Time `json:"started_at"`
}

// callLog ports V1.f2.C1121t: a bounded, newest-first ring of call records
// (the APK keeps the newest 100 per provider shard and prunes older entries).
type callLog struct {
	mu   sync.Mutex
	max  int
	recs []callRecord

	totalCalls  int64
	totalFailed int64
	totalPrompt int64
	totalCompl  int64
	todayCalls  int64
	todayDate   string
}

func newCallLog(max int) *callLog {
	if max < 1 {
		max = 100
	}
	return &callLog{max: max}
}

// add ports V1.o.r()'s accounting block: append newest-first, prune the tail,
// and roll the daily counter when the day changes.
func (l *callLog) add(rec callRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.recs = append([]callRecord{rec}, l.recs...)
	if len(l.recs) > l.max {
		l.recs = l.recs[:l.max]
	}

	l.totalCalls++
	l.totalPrompt += rec.PromptTokens
	l.totalCompl += rec.CompletionTokens
	if rec.Error != "" || rec.StatusCode >= 400 {
		l.totalFailed++
	}

	day := rec.StartedAt.Format("2006-01-02")
	if day == "" {
		day = time.Now().Format("2006-01-02")
	}
	if l.todayDate != day {
		l.todayDate = day
		l.todayCalls = 0
	}
	l.todayCalls++
}

func (l *callLog) recent(limit int) []callRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.recs) {
		limit = len(l.recs)
	}
	out := make([]callRecord, limit)
	copy(out, l.recs[:limit])
	return out
}

type usageTotals struct {
	TotalCalls      int64 `json:"total_calls"`
	TotalFailed     int64 `json:"total_failed"`
	TodayCalls      int64 `json:"today_calls"`
	TotalPrompt     int64 `json:"total_prompt_tokens"`
	TotalCompletion int64 `json:"total_completion_tokens"`
}

func (l *callLog) totals() usageTotals {
	l.mu.Lock()
	defer l.mu.Unlock()
	return usageTotals{
		TotalCalls:      l.totalCalls,
		TotalFailed:     l.totalFailed,
		TodayCalls:      l.todayCalls,
		TotalPrompt:     l.totalPrompt,
		TotalCompletion: l.totalCompl,
	}
}

// usagePayload is the subset of an OpenAI-compatible usage object the gateway
// reads in V1/o.r() / V1/o.p().
type usagePayload struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	// Anthropic-style aliases, since CPA can serve /v1/messages too.
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func (u usagePayload) normalized() (prompt, completion, total int64) {
	prompt = u.PromptTokens
	if prompt == 0 {
		prompt = u.InputTokens
	}
	completion = u.CompletionTokens
	if completion == 0 {
		completion = u.OutputTokens
	}
	total = u.TotalTokens
	if total == 0 {
		total = prompt + completion
	}
	return prompt, completion, total
}

// globalState is the plugin-wide singleton set, populated on register.
type globalState struct {
	settings   *settingsStore
	pool       *credentialPool
	log        *callLog
	checkin    *checkinState
	quota      *quotaState
	accounts   *accountStore
	scheduler  *schedulerState
	taskEngine *taskEngine
	growth     *growthStore
}

var state = &globalState{
	settings:   newSettingsStore(),
	pool:       newCredentialPool(),
	log:        newCallLog(100),
	checkin:    newCheckinState(),
	quota:      newQuotaState(),
	accounts:   newAccountStore(),
	scheduler:  newSchedulerState(),
	taskEngine: newTaskEngine(),
	growth:     newGrowthStore(),
}

func shutdownPlugin() {
	stopTaskScheduler()
	stopCheckinScheduler()
	stopQuotaScheduler()
}
