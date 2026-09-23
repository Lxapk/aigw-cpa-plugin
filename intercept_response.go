package main

import (
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// interceptResponse ports the non-streaming half of V1/o.k() step 9 together
// with the accounting of V1/o.r().
//
// Original failure path:
//
//	if resp.status >= 400 {
//	    provider.f(bodyString, status)        // classify
//	    engine.c(providerId, uid, kind, reason) // A0.s.p -> cooldown
//	    accumulate reason; rotate credential
//	}
//	...
//	engine.r(providerId, ..., usage, latency, request, response)   // book-keeping
//
// The response interceptor cannot rotate credentials (CPA owns that loop), but
// it can (a) classify the upstream failure so the credential pool applies the
// right cooldown and (b) record the call for the management status page.
func interceptResponse(request []byte) ([]byte, error) {
	var req pluginapi.ResponseInterceptRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	ctx := resolveContext(req.RequestID, req.RequestHeaders, req.Model, req.RequestedModel, req.Stream)
	statusCode := req.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}

	// --- failure classification (V1/o.k step 9) ------------------------
	if statusCode >= 400 {
		upErr := classifyUpstream(statusCode, req.Body)
		if ctx.Provider != "" {
			state.pool.failure(ctx.Provider, ctx.UID, upErr.Kind, upErr.Message, state.settings.get(), false)
		}
		state.log.add(callRecord{
			ProviderID:     ctx.Provider,
			UID:            ctx.UID,
			Label:          ctx.Label,
			Model:          ctx.Model,
			RequestedModel: ctx.RequestedModel,
			Stream:         ctx.Stream,
			StatusCode:     statusCode,
			LatencyMillis:  elapsed(ctx),
			Error:          upstreamErrText(upErr),
			StartedAt:      ctx.StartedAt,
		})
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}

	// --- success accounting (V1/o.r) -----------------------------------
	if ctx.Provider != "" {
		state.pool.success(ctx.Provider, ctx.UID)
	}
	rec := callRecord{
		ProviderID:     ctx.Provider,
		UID:            ctx.UID,
		Label:          ctx.Label,
		Model:          ctx.Model,
		RequestedModel: ctx.RequestedModel,
		Stream:         ctx.Stream,
		StatusCode:     statusCode,
		LatencyMillis:  elapsed(ctx),
		StartedAt:      ctx.StartedAt,
	}
	if usage, okUsage := extractUsage(req.Body); okUsage {
		rec.PromptTokens, rec.CompletionTokens, rec.TotalTokens = usage.normalized()
	}
	state.log.add(rec)

	return okEnvelope(pluginapi.ResponseInterceptResponse{})
}

// interceptStreamChunk ports the streaming accounting path.
//
// V1/m (the APK's stream pump) inspects every SSE frame with V1/o.p(), pulling
// out "usage", "error", and "choices[0].delta.content". Usage usually arrives
// only on the final frame, so the totals are returned to the host on the
// header-init call and accumulated per chunk.
//
// CPA calls this hook once with ChunkIndex == StreamChunkHeaderInitIndex before
// any payload, then once per chunk.
func interceptStreamChunk(request []byte) ([]byte, error) {
	var req pluginapi.StreamChunkInterceptRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	ctx := resolveContext(req.RequestID, req.RequestHeaders, req.Model, req.RequestedModel, true)

	// Header-init: nothing to inspect, but remember the correlation context.
	if req.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
		inflight.put(req.RequestID, ctx)
		return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
	}

	// Payload chunk: scan for usage and terminal errors.
	payload := req.Body
	if usage, okUsage := extractStreamUsage(payload); okUsage {
		acc := accumulatorFor(req.RequestID)
		acc.merge(usage)
	}
	if msg := extractStreamError(payload); msg != "" {
		if ctx.Provider != "" {
			upErr := classifyUpstream(http.StatusBadGateway, []byte(msg))
			state.pool.failure(ctx.Provider, ctx.UID, upErr.Kind, upErr.Message, state.settings.get(), false)
		}
	}

	return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
}

// upstreamErrText renders the same shape the app logged, e.g.
// "请求被上游拒绝（trae/acc-1）：invalid or expired token".
func upstreamErrText(upErr upstreamError) string {
	if upErr.Message == "" {
		return "上游 HTTP " + http.StatusText(upErr.StatusCode)
	}
	return upErr.Message
}

// resolveContext rebuilds the per-request context from the AIGW headers the
// request interceptor stamped, falling back to whatever CPA supplied.
func resolveContext(requestID string, headers http.Header, model, requestedModel string, stream bool) requestContext {
	var ctx requestContext
	if c, okCtx := inflight.get(requestID); okCtx {
		ctx = c
	}

	if provider := headerValue(headers, "X-AIGW-Provider"); provider != "" {
		ctx.Provider = provider
	}
	if m := headerValue(headers, "X-AIGW-Model"); m != "" {
		ctx.Model = m
	} else if ctx.Model == "" {
		ctx.Model = model
	}
	if rm := headerValue(headers, "X-AIGW-Requested-Model"); rm != "" {
		ctx.RequestedModel = rm
	} else if ctx.RequestedModel == "" {
		ctx.RequestedModel = requestedModel
	}
	if uid := headerValue(headers, "X-AIGW-Auth-Id"); uid != "" {
		ctx.UID = uid
	}
	if label := headerValue(headers, "X-AIGW-Auth-Label"); label != "" {
		ctx.Label = label
	}

	// The explicit "provider/model" form is a second, header-independent source
	// of truth (V1/o.k step 6).
	if ctx.Provider == "" && ctx.Model != "" {
		settings := state.settings.get()
		if res, okRoute := resolveRoute(ctx.Model, settings, []string{settings.DefaultProvider}); okRoute {
			ctx.Provider = res.Provider
		}
	}
	if ctx.RequestedModel == "" {
		ctx.RequestedModel = ctx.Model
	}
	ctx.Stream = stream
	if ctx.StartedAt.IsZero() {
		ctx.StartedAt = nowUTC()
	}
	return ctx
}

func elapsed(ctx requestContext) int64 {
	if ctx.StartedAt.IsZero() {
		return 0
	}
	return nowUTC().Sub(ctx.StartedAt).Milliseconds()
}

// extractUsage pulls the "usage" object out of a non-streaming response.
func extractUsage(body []byte) (usagePayload, bool) {
	if len(body) == 0 {
		return usagePayload{}, false
	}
	var doc struct {
		Usage *usagePayload `json:"usage"`
	}
	if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal != nil {
		return usagePayload{}, false
	}
	if doc.Usage == nil {
		return usagePayload{}, false
	}
	return *doc.Usage, true
}
