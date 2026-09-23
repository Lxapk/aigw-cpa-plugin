package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// This file implements the WorkBuddy upstream client, ported from AI 聚合网关
// 0.1.18 provider a2/b.
//
// Two endpoints matter:
//
//	listModels   GET  {q(domain)}/console/enterprises/personal/models   (a2/b.java:745 w())
//	chat         POST {q(domain)}/v2/chat/completions                   (a2/b.java:335 b())
//
// Bot hahs the same base selection (a2/b.java:716 q()):
//
//	domain == "global" ? "https://www.workbuddy.ai" : "https://copilot.tencent.com"
//
// and the same header set (a2/b.java:720 r()).
//
// Importantly, /v2/chat/completions already speaks the OpenAI Chat Completions
// protocol, so no request/response translation layer is required — the client
// body is forwarded with only the model name normalised.

const (
	// workBuddyModelsPath is the model catalogue endpoint (a2/b.java:745).
	workBuddyModelsPath = "/console/enterprises/personal/models"
	// workBuddyChatPath is the OpenAI-compatible chat endpoint (a2/b.java:335).
	workBuddyChatPath = "/v2/chat/completions"
	// workBuddyCLIAgentName is the agent whose model list acts as a whitelist.
	workBuddyCLIAgentName = "cli"
)

// workBuddyUpstreamError carries the upstream status so the executor can decide
// whether the failure is worth rotating credentials over.
type workBuddyUpstreamError struct {
	StatusCode int
	Message    string
	Body       []byte
}

func (e *workBuddyUpstreamError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("上游 HTTP %d", e.StatusCode)
}

// workBuddyModel is one entry of the model catalogue (a2/b.java w() -> q(8,...)).
type workBuddyModel struct {
	ID             string
	DisplayName    string
	MaxInputTokens int64
}

// workBuddyClient performs the upstream calls.
type workBuddyClient struct {
	httpClient *http.Client
}

func newWorkBuddyClient() *workBuddyClient {
	return &workBuddyClient{
		httpClient: &http.Client{
			// No global timeout: streaming responses are long-lived. Connection
			// establishment and header reads are bounded instead.
			Transport: &http.Transport{
				ResponseHeaderTimeout: 60 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

var workBuddyUpstream = newWorkBuddyClient()

// ---- models ---------------------------------------------------------------

// listModels ports a2/b.java w(): fetch and filter the provider model catalogue.
//
// Filtering rules, in the original order:
//
//  1. id must be non-empty
//  2. id must not already have been seen
//  3. if the "cli" agent declares a model list, id must be in it
//  4. disabled must not be true
//
// DisplayName falls back to id when the provider omits "name"; MaxInputTokens
// comes from "maxInputTokens".
func (c *workBuddyClient) listModels(ctx context.Context, creds *workBuddyCredentials) ([]workBuddyModel, error) {
	if creds == nil || creds.AccessToken == "" {
		return nil, errors.New("缺少访问令牌")
	}

	endpoint := workBuddyBaseURL(creds.Domain) + workBuddyModelsPath
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if errRequest != nil {
		return nil, errRequest
	}
	applyWorkBuddyHeaders(req.Header, creds)

	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("模型接口请求失败: %w", errDo)
	}
	defer resp.Body.Close()

	body, errRead := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if errRead != nil {
		return nil, fmt.Errorf("读取模型响应失败: %w", errRead)
	}

	// a2/b.java w(): "模型接口 HTTP <code>"
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &workBuddyUpstreamError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("模型接口 HTTP %d", resp.StatusCode),
			Body:       body,
		}
	}
	return parseWorkBuddyModels(body)
}

// parseWorkBuddyModels implements the catalogue parsing of a2/b.java w().
func parseWorkBuddyModels(body []byte) ([]workBuddyModel, error) {
	var doc struct {
		Code *int `json:"code"`
		Data *struct {
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
			Models []struct {
				ID             string `json:"id"`
				Name           string `json:"name"`
				MaxInputTokens int64  `json:"maxInputTokens"`
				Disabled       bool   `json:"disabled"`
			} `json:"models"`
		} `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal != nil {
		// a2/b.java: "模型响应不是合法 JSON"
		return nil, errors.New("模型响应不是合法 JSON")
	}
	// a2/b.java: code must be present and zero.
	if doc.Code == nil || *doc.Code != 0 {
		code := "<nil>"
		if doc.Code != nil {
			code = fmt.Sprint(*doc.Code)
		}
		return nil, errors.New("模型接口 code=" + code)
	}
	if doc.Data == nil {
		// a2/b.java returns an empty list when data is absent.
		return nil, nil
	}

	// The "cli" agent's model list is an optional whitelist.
	whitelist := make(map[string]struct{})
	for _, agent := range doc.Data.Agents {
		if agent.Name != workBuddyCLIAgentName {
			continue
		}
		for _, id := range agent.Models {
			whitelist[id] = struct{}{}
		}
	}

	seen := make(map[string]struct{})
	out := make([]workBuddyModel, 0, len(doc.Data.Models))
	for _, m := range doc.Data.Models {
		id := m.ID
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if len(whitelist) > 0 {
			if _, ok := whitelist[id]; !ok {
				continue
			}
		}
		if m.Disabled {
			continue
		}
		seen[id] = struct{}{}

		display := m.Name
		if display == "" {
			display = id
		}
		out = append(out, workBuddyModel{
			ID:             id,
			DisplayName:    display,
			MaxInputTokens: m.MaxInputTokens,
		})
	}
	return out, nil
}

// ---- chat -----------------------------------------------------------------

// normalizeWorkBuddyModel ports a2/b.java k():
//
//	String obj = trim(requested);
//	return (obj.isEmpty() || obj.equals("auto"))
//	    ? <configured default model>
//	    : obj;
//
// The configured default falls back to the first available model when the
// operator has not set one, which keeps "auto" usable.
func normalizeWorkBuddyModel(requested string, defaultModel string) string {
	trimmed := strings.TrimSpace(requested)
	if trimmed == "" || trimmed == "auto" {
		return strings.TrimSpace(defaultModel)
	}
	return trimmed
}

// rewriteChatModel replaces the "model" field of a chat-completions body while
// preserving every other field verbatim.
func rewriteChatModel(body []byte, model string) ([]byte, error) {
	if model == "" {
		return body, nil
	}
	var doc map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", errUnmarshal)
	}
	encoded, errMarshal := json.Marshal(model)
	if errMarshal != nil {
		return nil, errMarshal
	}
	doc["model"] = encoded
	return json.Marshal(doc)
}

// chatCompletions posts a chat request upstream and returns the raw
// OpenAI-shaped response.
//
// The caller gets the status code alongside the body so a non-2xx response can
// be surfaced without losing the upstream error detail.
func (c *workBuddyClient) chatCompletions(ctx context.Context, creds *workBuddyCredentials, body []byte) (int, http.Header, []byte, error) {
	if creds == nil || creds.AccessToken == "" {
		return 0, nil, nil, errors.New("缺少访问令牌")
	}

	endpoint := workBuddyBaseURL(creds.Domain) + workBuddyChatPath
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errRequest != nil {
		return 0, nil, nil, errRequest
	}
	applyWorkBuddyHeaders(req.Header, creds)

	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return 0, nil, nil, fmt.Errorf("上游请求失败: %w", errDo)
	}
	defer resp.Body.Close()

	respBody, errRead := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if errRead != nil {
		return resp.StatusCode, resp.Header, nil, fmt.Errorf("读取上游响应失败: %w", errRead)
	}
	return resp.StatusCode, resp.Header, respBody, nil
}

// chatCompletionsStream posts a streaming chat request and invokes onChunk for
// every SSE frame.
//
// The upstream already emits OpenAI-style SSE (`data: {...}` / `data: [DONE]`),
// so frames are forwarded verbatim. onChunk returning an error aborts the read.
func (c *workBuddyClient) chatCompletionsStream(
	ctx context.Context,
	creds *workBuddyCredentials,
	body []byte,
	onChunk func([]byte) error,
) (int, http.Header, error) {
	if creds == nil || creds.AccessToken == "" {
		return 0, nil, errors.New("缺少访问令牌")
	}

	endpoint := workBuddyBaseURL(creds.Domain) + workBuddyChatPath
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if errRequest != nil {
		return 0, nil, errRequest
	}
	applyWorkBuddyHeaders(req.Header, creds)
	req.Header.Set("Accept", "text/event-stream")

	resp, errDo := c.httpClient.Do(req)
	if errDo != nil {
		return 0, nil, fmt.Errorf("上游请求失败: %w", errDo)
	}
	defer resp.Body.Close()

	// Non-2xx: drain a bounded amount so the error text can be reported, then
	// hand the status back for classification.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return resp.StatusCode, resp.Header, &workBuddyUpstreamError{
			StatusCode: resp.StatusCode,
			Message:    extractUpstreamMessage(errBody, resp.StatusCode),
			Body:       errBody,
		}
	}

	reader := bufio.NewReaderSize(resp.Body, 64<<10)
	for {
		line, errRead := reader.ReadBytes('\n')
		if len(line) > 0 {
			if errChunk := onChunk(line); errChunk != nil {
				return resp.StatusCode, resp.Header, errChunk
			}
		}
		if errRead != nil {
			if errors.Is(errRead, io.EOF) {
				return resp.StatusCode, resp.Header, nil
			}
			return resp.StatusCode, resp.Header, fmt.Errorf("读取上游流失败: %w", errRead)
		}
	}
}

// extractUpstreamMessage pulls a human-readable message out of an error body,
// accepting the OpenAI shape and WorkBuddy's own {"msg":...} envelope.
func extractUpstreamMessage(body []byte, statusCode int) string {
	var doc struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Msg     string `json:"msg"`
		Message string `json:"message"`
	}
	if len(body) > 0 {
		if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal == nil {
			for _, candidate := range []string{doc.Error.Message, doc.Msg, doc.Message} {
				if strings.TrimSpace(candidate) != "" {
					return strings.TrimSpace(candidate)
				}
			}
		}
	}
	return fmt.Sprintf("上游 HTTP %d", statusCode)
}
