package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

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
	return rewritten, model, nil
}

// executorExecute answers executor.execute (non-streaming).
func executorExecute(request []byte) ([]byte, error) {
	req, body, creds, errDecode := decodeExecutorRequest(request)
	if errDecode != nil {
		return errorEnvelope("invalid_executor_request", errDecode.Error(), 400), nil
	}

	upstreamBody, _, errPrepare := prepareUpstreamBody(body, req.Model)
	if errPrepare != nil {
		return errorEnvelope("invalid_request", errPrepare.Error(), 400), nil
	}

	ctx := context.Background()
	status, headers, respBody, errChat := workBuddyUpstream.chatCompletions(ctx, creds, upstreamBody)
	if errChat != nil {
		return errorEnvelope("upstream_error", errChat.Error(), 502), nil
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
func executorExecuteStream(request []byte) ([]byte, error) {
	req, body, creds, errDecode := decodeExecutorRequest(request)
	if errDecode != nil {
		return errorEnvelope("invalid_executor_request", errDecode.Error(), 400), nil
	}

	upstreamBody, _, errPrepare := prepareUpstreamBody(body, req.Model)
	if errPrepare != nil {
		return errorEnvelope("invalid_request", errPrepare.Error(), 400), nil
	}

	var chunks []pluginapi.ExecutorStreamChunk
	ctx := context.Background()
	_, headers, errStream := workBuddyUpstream.chatCompletionsStream(ctx, creds, upstreamBody, func(frame []byte) error {
		payload, keep := sseFrameToBareJSON(frame)
		if !keep {
			return nil
		}
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: payload})
		return nil
	})
	if errStream != nil {
		// Surface the failure as a terminal error chunk so the client sees an
		// error event rather than a silently truncated stream.
		errFrame, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": errStream.Error(),
				"type":    "upstream_error",
			},
		})
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: errFrame})
		return okEnvelope(streamChunkEnvelope{
			Headers: filterResponseHeaders(headers),
			Chunks:  chunks,
		})
	}

	return okEnvelope(streamChunkEnvelope{
		Headers: filterResponseHeaders(headers),
		Chunks:  chunks,
	})
}

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
