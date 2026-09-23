package main

import (
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
// The host collects the returned Chunks slice, so the whole upstream stream is
// buffered here. WorkBuddy's SSE frames are forwarded verbatim, including the
// terminal "data: [DONE]" sentinel, so downstream clients see the exact
// OpenAI-compatible stream they expect.
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
		// Copy the frame: the reader reuses nothing, but being explicit keeps
		// the chunk ownership unambiguous.
		payload := make([]byte, len(frame))
		copy(payload, frame)
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: payload})
		return nil
	})
	if errStream != nil {
		// Surface the failure as a terminal chunk so the client sees an error
		// event rather than a silently truncated stream.
		errFrame, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": errStream.Error(),
				"type":    "upstream_error",
			},
		})
		chunks = append(chunks, pluginapi.ExecutorStreamChunk{
			Payload: append(append([]byte("data: "), errFrame...), []byte("\n\n")...),
		})
		return okEnvelope(map[string]any{
			"headers": filterResponseHeaders(headers),
			"chunks":  chunks,
		})
	}

	return okEnvelope(map[string]any{
		"headers": filterResponseHeaders(headers),
		"chunks":  chunks,
	})
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
