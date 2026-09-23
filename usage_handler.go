package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleUsage ports V1/o.r()'s book-keeping into CPA's UsagePlugin hook.
//
// CPA calls this after every completed request with a fully populated
// UsageRecord, which is a richer version of what the app tracked, so the
// counters line up with the management status page.
func handleUsage(request []byte) ([]byte, error) {
	var rec pluginapi.UsageRecord
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &rec); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	provider := strings.ToLower(strings.TrimSpace(rec.Provider))
	model := rec.Model
	if model == "" {
		model = rec.Alias
	}

	uid := strings.TrimSpace(rec.AuthID)
	if uid == "" {
		uid = strings.TrimSpace(rec.AuthIndex)
	}
	if rec.AuthIndex != "" {
		uid = rec.AuthIndex
	}

	if provider != "" && uid != "" {
		if rec.Failed {
			kind := failureTransient
			if rec.Failure.StatusCode > 0 {
				kind = classifyUpstream(rec.Failure.StatusCode, []byte(rec.Failure.Body)).Kind
			}
			state.pool.failure(provider, uid, kind, rec.Failure.Body, state.settings.get(), false)
		} else {
			state.pool.success(provider, uid)
		}
	}

	statusCode := 200
	errText := ""
	if rec.Failed {
		statusCode = rec.Failure.StatusCode
		if statusCode == 0 {
			statusCode = 500
		}
		errText = rec.Failure.Body
	}

	started := rec.RequestedAt
	if started.IsZero() {
		started = time.Now().Add(-rec.Latency)
	}

	state.log.add(callRecord{
		ProviderID:       provider,
		UID:              uid,
		Model:            model,
		RequestedModel:   model,
		Stream:           rec.Stream,
		StatusCode:       statusCode,
		PromptTokens:     rec.Detail.InputTokens,
		CompletionTokens: rec.Detail.OutputTokens,
		TotalTokens:      rec.Detail.TotalTokens,
		LatencyMillis:    rec.Latency.Milliseconds(),
		Error:            errText,
		StartedAt:        started,
	})

	return okEnvelope(map[string]any{})
}
