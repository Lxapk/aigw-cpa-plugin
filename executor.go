package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// This file implements CPA's ProviderExecutor capability for WorkBuddy.
//
// CPA drives it through:
//
//	executor.identifier     -> stable executor id
//	executor.execute        -> non-streaming completion
//	executor.execute_stream -> streaming completion
//	executor.count_tokens   -> token counting (delegated to upstream)
//	executor.http_request   -> raw upstream bridging
//
// Because /v2/chat/completions already speaks OpenAI Chat Completions, the
// executor's job is narrow:
//
//	1. recover the credential from StorageJSON
//	2. normalise the model name (a2/b.java k())
//	3. POST the body upstream
//	4. pass the response back verbatim

// executorIdentifier answers executor.identifier.
func executorIdentifier() ([]byte, error) {
	return okEnvelope(identifierResponse{Identifier: workBuddyProviderKey})
}

// executorRequest mirrors pluginhost.rpcExecutorRequest: ExecutorRequest plus
// the correlation fields the host adds.
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// decodeExecutorRequest parses an executor call and picks the body to forward.
//
// The host may supply the original client body (OriginalRequest) or a
// pre-translated payload (Payload). Since WorkBuddy consumes OpenAI format
// directly, the original body is preferred so nothing is lost in translation.
func decodeExecutorRequest(request []byte) (executorRequest, []byte, *workBuddyCredentials, error) {
	var req executorRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return req, nil, nil, errUnmarshal
		}
	}

	body := req.OriginalRequest
	if len(body) == 0 {
		body = req.Payload
	}
	if len(body) == 0 {
		return req, nil, nil, errors.New("执行请求缺少请求体")
	}

	creds, errParse := parseWorkBuddyCredentials(req.StorageJSON)
	if errParse != nil {
		return req, body, nil, errParse
	}
	return req, body, creds, nil
}

// prepareUpstreamBody normalises the model name and returns the upstream body.
//
// Two normalisations happen, mirroring the source gateway:
//
//  1. strip a recognised "provider/" prefix  (V1/o.k step 6/8)
//  2. resolve "" and "auto" to the configured default  (a2/b.java k())
func prepareUpstreamBody(body []byte, requestedModel string) ([]byte, string, error) {
	requested := strings.TrimSpace(requestedModel)
	if requested == "" {
		if meta, okMeta := parseRequestMeta(body); okMeta {
			requested = strings.TrimSpace(meta.Model)
		}
	}

	// Drop an explicit provider prefix, but only when it addresses this plugin;
	// anything else is a routing mistake we should not silently rewrite.
	if provider, rest, ok := splitProviderPrefix(requested); ok && isWorkBuddyProvider(provider) {
		requested = rest
	}

	model := normalizeWorkBuddyModel(requested, state.settings.get().DefaultModel)
	if model == "" {
		return nil, "", errors.New("缺少 model 参数")
	}
	rewritten, errRewrite := rewriteChatModel(body, model)
	if errRewrite != nil {
		return nil, "", errRewrite
	}
	// Normalise the message array into the shape the upstream accepts. Without
	// this a client sending a "developer" role or an interrupted tool batch gets
	// "request illegal" (codes 11128 / 11148) for every turn.
	normalised, errNormalise := normaliseUpstreamBody(rewritten)
	if errNormalise != nil {
		return nil, "", errNormalise
	}
	return normalised, model, nil
}

// errUpstreamFrameError marks a stream that carried an error frame.
//
// The upstream signals a throttle inside an HTTP 200 stream, so the reader's
// transport error stays nil; this sentinel lets the caller tell "the upstream
// said no" apart from "the connection dropped".
var errUpstreamFrameError = errors.New("upstream reported an error frame")

// requestContentPhrases mark failures caused by the request's own shape rather
// than by the credential.
//
// The upstream answers these with 5xx ("server_error") just like a throttle, so
// they would otherwise accumulate as soft failures: three of them parked the
// account, even though retrying the same malformed conversation on another
// account fails identically. They are the client's to fix, and the upstream says
// so ("please start a new conversation and retry").
var requestContentPhrases = []string{
	"tool calls and tool results do not match",
	"tool_calls and tool_results do not match",
	"please start a new conversation",
	"request illegal",
	"invalid_request_error",
}

// isRequestContentFailure reports whether the message describes a malformed
// request.
func isRequestContentFailure(message string) bool {
	return containsAnyFold(message, requestContentPhrases)
}

// reportExecutorFailure records an upstream failure from inside the executor.
//
// The response interceptor is not reached when the executor itself answers with
// an error: CPA marks the exchange failed at the executor layer
// (conductor_execution.go "upstream execution failed") and never runs the
// response interceptor. Reporting from both places is therefore required, not
// redundant — without this call a throttle was classified correctly and then
// never applied, so the same throttled account was retried on every request.
//
// model scopes the cooldown: a throttle parks only that model, leaving the
// account usable for the others.
func reportExecutorFailure(creds *workBuddyCredentials, model string, statusCode int, body []byte) {
	if creds == nil {
		return
	}
	uid := strings.TrimSpace(creds.UID)
	if uid == "" {
		uid = strings.TrimSpace(creds.AuthKey())
	}
	if uid == "" {
		return
	}
	provider := workBuddyProviderKey

	upErr := classifyUpstream(statusCode, body)
	if upErr.Kind == 0 {
		return
	}
	// A malformed request is not evidence about the credential: retrying it on
	// another account fails the same way, so counting it parked every account in
	// turn and the client ended up with "no auth available" for a conversation it
	// could have fixed itself.
	if isRequestContentFailure(upErr.Message) {
		return
	}
	state.pool.failureForModel(provider, uid, model, upErr.Kind, upErr.Message,
		state.settings.get(), isPermanentFailure(statusCode, upErr))
}

// executorExecute answers executor.execute (non-streaming).
func executorExecute(request []byte) ([]byte, error) {
	req, body, creds, errDecode := decodeExecutorRequest(request)
	if errDecode != nil {
		return errorEnvelope("invalid_executor_request", errDecode.Error(), 400), nil
	}

	// The resolved model is what the cooldown must be keyed by; see the streaming
	// path for why req.Model alone is not enough.
	upstreamBody, model, errPrepare := prepareUpstreamBody(body, req.Model)
	if errPrepare != nil {
		return errorEnvelope("invalid_request", errPrepare.Error(), 400), nil
	}

	ctx := context.Background()
	status, headers, respBody, errChat := workBuddyUpstream.chatCompletions(ctx, creds, upstreamBody)
	if errChat != nil {
		// Network-level failure: no upstream body to classify.
		reportExecutorFailure(creds, model, http.StatusBadGateway, []byte(errChat.Error()))
		return errorEnvelope("upstream_error", errChat.Error(), 502), nil
	}
	if status >= 400 {
		// The interceptor will not see this exchange, so classify and park here.
		reportExecutorFailure(creds, model, status, respBody)
	}

	// The provider rejects non-streaming chat requests outright
	// ({"code":11101,"msg":"Non-stream chat request is currently not supported"}).
	// Satisfy the client by streaming upstream and folding the frames back into
	// a single chat.completion.
	if isNonStreamUnsupported(status, respBody) {
		streamBody, errForce := forceStream(upstreamBody)
		if errForce != nil {
			return errorEnvelope("invalid_request", errForce.Error(), 400), nil
		}
		var frames [][]byte
		_, streamHeaders, errStream := workBuddyUpstream.chatCompletionsStream(ctx, creds, streamBody, func(frame []byte) error {
			if payload, keep := sseFrameToBareJSON(frame); keep {
				frames = append(frames, payload)
			}
			return nil
		})
		if errStream != nil {
			return errorEnvelope("upstream_error", errStream.Error(), 502), nil
		}
		if len(frames) == 0 {
			return errorEnvelope("upstream_error", "上游未返回任何内容", 502), nil
		}
		aggregated := aggregateStreamToCompletion(frames, req.Model)
		return okEnvelope(pluginapi.ExecutorResponse{
			Payload: aggregated,
			Headers: filterResponseHeaders(streamHeaders),
			Metadata: map[string]any{
				"upstream_status": status,
				"provider":        workBuddyProviderKey,
				"aggregated":      true,
			},
		})
	}

	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: respBody,
		Headers: filterResponseHeaders(headers),
		Metadata: map[string]any{
			"upstream_status": status,
			"provider":        workBuddyProviderKey,
		},
	})
}

// executorExecuteStream answers executor.execute_stream.
//
// Chunk payload format is subtle and getting it wrong produces
// "Unexpected JSON token at offset 5: Expected EOF after parsing, but had :"
// because the outbound layer parses each chunk as bare JSON.
//
// CPA only runs its translator when the plugin's output format differs from the
// client's requested format (adapters_executors.go:552). For WorkBuddy both are
// "chat-completions", so the translator is skipped and our chunks reach the
// response writer verbatim. That writer expects **bare JSON per chunk** and adds
// the "data: " prefix and the SSE blank line itself.
//
// Therefore we strip the SSE framing ("data: " prefix, trailing newlines) and
// forward only the JSON object. The terminal "[DONE]" sentinel is dropped too:
// CPA emits it after the executor's stream ends.
// executorExecuteStream answers executor.execute_stream.
//
// Shape follows the official claude-web-search-router example
// (examples/plugin/claude-web-search-router/go/execute_stream.go):
//
//   - stream_id is required; without it the host has nowhere to route chunks.
//     Answering with an error is what that example does, rather than guessing at
//     a buffered fallback.
//   - The upstream read happens in a background goroutine, so this returns
//     immediately and the host can start draining the stream. The host buffers
//     emitted chunks in a 16-slot queue that it only starts reading after this
//     call returns (pluginhost.streamBridgeBufferSize), so a synchronous
//     implementation blocks on the 17th chunk.
//   - The response carries Content-Type: text/event-stream, matching what the
//     example returns.
//
// Chunk payloads are the provider's native frames — bare JSON objects, no "data:"
// prefix and no [DONE] sentinel. That is what the host expects: it runs the
// payload through sdktranslator.TranslateStream and writes its own
// "data: [DONE]" tail (pluginhost.executorStreamDonePayload), so a prefix added
// here would end up doubled.
func executorExecuteStream(request []byte) ([]byte, error) {
	req, body, creds, errDecode := decodeExecutorRequest(request)
	if errDecode != nil {
		return errorEnvelope("invalid_executor_request", errDecode.Error(), 400), nil
	}

	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream", 400), nil
	}

	// prepareUpstreamBody resolves the model from the request body when the host
	// did not set ExecutorRequest.Model, and that resolved name is what the
	// cooldown has to be keyed by: it is the name the router will look up on the
	// next attempt. Using req.Model alone left model empty whenever the host
	// omitted it, which silently demoted a throttle to an account-level cooldown.
	upstreamBody, model, errPrepare := prepareUpstreamBody(body, req.Model)
	if errPrepare != nil {
		return errorEnvelope("invalid_request", errPrepare.Error(), 400), nil
	}

	go pumpUpstreamStreamIntoHost(streamID, creds, upstreamBody, model)

	return okEnvelope(streamChunkEnvelope{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

// pumpUpstreamStreamIntoHost reads the upstream stream, forwards each frame to
// the client, then closes the stream.
//
// Runs in its own goroutine so executorExecuteStream can return before the first
// chunk exists; see that function for why. The recover mirrors the official
// example: a panic here would otherwise leave the stream open and the client
// waiting on a connection nobody will ever close.
func pumpUpstreamStreamIntoHost(streamID string, creds *workBuddyCredentials, upstreamBody []byte, model string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			closeHostStream(streamID, fmt.Sprintf("upstream stream panic: %v", recovered))
		}
	}()

	var emitted int
	var frameError string
	ctx := context.Background()
	_, _, errStream := workBuddyUpstream.chatCompletionsStream(ctx, creds, upstreamBody, func(frame []byte) error {
		// Check the raw frame: extractStreamError parses "data:" lines, so it
		// has to see the frame before the prefix is stripped.
		//
		// The upstream reports a throttle as a data frame inside an HTTP 200
		// stream, so the transport-level error stays nil and this is the only
		// place the failure is visible. Forwarding it as content would show the
		// error text as the model's answer.
		if message := extractStreamError(frame); message != "" {
			frameError = message
			return errUpstreamFrameError
		}
		payload, keep := sseFrameToBareJSON(frame)
		if !keep {
			return nil
		}
		if errEmit := emitStreamChunk(streamID, payload); errEmit != nil {
			// The client is gone; stop reading upstream.
			return errEmit
		}
		emitted++
		return nil
	})

	// frameError is checked first because it is the reliable signal: the upstream
	// reports a throttle as a data frame inside an HTTP 200 stream, so errStream
	// may be nil or an unrelated wrapper by the time the reader returns. Relying
	// on errors.Is alone let the throttle fall through to the generic branch,
	// where the reason became "Bad Gateway" and the cooldown was applied to the
	// whole account instead of the model.
	if frameError != "" {
		reportExecutorFailure(creds, model, http.StatusBadGateway, []byte(frameError))
		closeHostStream(streamID, frameError)
		return
	}
	if errors.Is(errStream, errUpstreamFrameError) {
		reportExecutorFailure(creds, model, http.StatusBadGateway, []byte(errStream.Error()))
		closeHostStream(streamID, errStream.Error())
		return
	}

	if errStream != nil {
		// Report before closing: the response interceptor does not run for an
		// exchange the executor itself failed, so this is the only place the
		// throttle can be classified and parked.
		reportExecutorFailure(creds, model, http.StatusBadGateway, []byte(errStream.Error()))

		message := errStream.Error()
		if emitted == 0 {
			message = "上游未返回任何内容: " + message
		}
		closeHostStream(streamID, message)
		return
	}
	if emitted == 0 {
		// A 200 with no frames is still a failure to answer.
		reportExecutorFailure(creds, model, http.StatusBadGateway, []byte("上游未返回任何内容"))
		closeHostStream(streamID, "上游未返回任何内容")
		return
	}
	closeHostStream(streamID, "")
}

// emitStreamChunk pushes one chunk to the client through the host.
//
// Mirrors emitPluginStreamChunk from the official example, including the request
// shape (rpcStreamEmitRequest).
func emitStreamChunk(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return errNoStreamID
	}
	_, errEmit := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
	})
	return errEmit
}

// closeHostStream ends a host stream, optionally attaching an error.
//
// Mirrors closePluginStream from the official example.
func closeHostStream(streamID string, errorMessage string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(errorMessage),
	})
}

// rpcStreamEmitRequest mirrors pluginhost.rpcStreamEmitRequest.
//
// Payload is []byte so encoding/json emits base64, which is how the host decodes
// it (the same shape the official examples use).
type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

// rpcStreamCloseRequest mirrors pluginhost.rpcStreamCloseRequest.
type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

// errNoStreamID reports that no host stream id was supplied with the call.
var errNoStreamID = errors.New("plugin stream id is required")

// streamChunkEnvelope mirrors pluginhost.rpcExecutorStreamResponse.
//
// pluginapi.ExecutorStreamResponse is not usable here because its Chunks field
// is a channel meant for in-process consumers; the RPC wire shape carries a
// concrete slice instead (internal/pluginhost/rpc_schema.go:55).
type streamChunkEnvelope struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

// sseFrameToBareJSON converts one upstream SSE line into the bare JSON payload
// CPA's streaming writer expects.
//
// Accepted input shapes (the upstream already speaks OpenAI SSE):
//
//	"data: {\"id\":...}\n"   -> the JSON object
//	"data: [DONE]\n"         -> dropped (CPA emits the sentinel itself)
//	": keep-alive\n"         -> dropped (comment / heartbeat)
//	"\n"                     -> dropped (frame separator)
//
// Anything that is not valid JSON after unwrapping is dropped rather than
// forwarded, because a malformed frame would abort the whole stream downstream.
func sseFrameToBareJSON(frame []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(frame)
	if len(trimmed) == 0 {
		return nil, false
	}
	// SSE comment / heartbeat.
	if bytes.HasPrefix(trimmed, []byte(":")) {
		return nil, false
	}
	// Unwrap every "data:" prefix (some providers send "data:" without a space).
	for bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 {
		return nil, false
	}
	// Terminal sentinel: CPA writes this itself after the stream ends.
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil, false
	}
	// Upstream error frames can be plain text; keep only JSON.
	if !json.Valid(trimmed) {
		return nil, false
	}
	// Copy: the reader's buffer is reused between calls.
	out := make([]byte, len(trimmed))
	copy(out, trimmed)
	return out, true
}

// executorCountTokens answers executor.count_tokens.
//
// The provider exposes no dedicated counting endpoint, so the same completion
// call is used and the usage block is returned; CPA only reads the totals.
func executorCountTokens(request []byte) ([]byte, error) {
	return executorExecute(request)
}

// executorHTTPRequest answers executor.http_request, used by CPA when it wants
// to issue a provider-shaped call itself.
func executorHTTPRequest(request []byte) ([]byte, error) {
	var req pluginapi.ExecutorHTTPRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
	}

	creds, errParse := parseWorkBuddyCredentials(req.StorageJSON)
	if errParse != nil {
		return errorEnvelope("invalid_auth", errParse.Error(), 401), nil
	}

	body := req.Body
	if len(body) > 0 {
		var doc map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal == nil {
			if _, has := doc["model"]; has {
				if rewritten, _, errPrepare := prepareUpstreamBody(body, ""); errPrepare == nil {
					body = rewritten
				}
			}
		}
	}

	status, headers, respBody, errChat := workBuddyUpstream.chatCompletions(context.Background(), creds, body)
	if errChat != nil {
		return errorEnvelope("upstream_error", errChat.Error(), 502), nil
	}
	return okEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: status,
		Headers:    filterResponseHeaders(headers),
		Body:       respBody,
	})
}

// filterResponseHeaders keeps the headers worth forwarding and drops hop-by-hop
// or length-negotiated ones that would contradict the rewritten body.
func filterResponseHeaders(src http.Header) http.Header {
	if src == nil {
		return nil
	}
	out := http.Header{}
	for _, key := range []string{"Content-Type", "Cache-Control", "X-Request-Id"} {
		if v := src.Get(key); v != "" {
			out.Set(key, v)
		}
	}
	if out.Get("Content-Type") == "" {
		out.Set("Content-Type", "application/json; charset=utf-8")
	}
	return out
}
