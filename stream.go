package main

import (
	"encoding/json"
	"strings"
	"sync"
)

// This file ports the SSE handling of V1/o.p() and the stream pump V1/m.
//
// V1/o.p(String line) in the APK:
//
//	if (!line.startsWith("data:")) return null
//	payload := line.substringAfter("data:").trim()
//	if (payload.isEmpty() || payload == "[DONE]") return null
//	root := JSON.parse(payload) as JsonObject
//	usage  := root["usage"]
//	error  := root["error"]["message"]
//	content:= root["choices"][0]["delta"]["content"]
//	return n(usage, error, content)
//
// V1/m.l(...) then folds usage into the running totals, forwards the frame to
// the downstream chunked writer, and on completion calls V1/o.r() to log the
// call.

// streamAccumulator accumulates usage across the frames of one stream.
type streamAccumulator struct {
	mu       sync.Mutex
	prompt   int64
	compl    int64
	total    int64
	sawUsage bool
}

var streamAccumulators = struct {
	mu    sync.Mutex
	items map[string]*streamAccumulator
}{
	items: make(map[string]*streamAccumulator),
}

// accumulatorFor returns the accumulator for a request, creating it lazily.
func accumulatorFor(requestID string) *streamAccumulator {
	streamAccumulators.mu.Lock()
	defer streamAccumulators.mu.Unlock()
	acc, ok := streamAccumulators.items[requestID]
	if !ok {
		acc = &streamAccumulator{}
		streamAccumulators.items[requestID] = acc
	}
	return acc
}

func (a *streamAccumulator) merge(u usagePayload) {
	prompt, compl, total := u.normalized()
	a.mu.Lock()
	defer a.mu.Unlock()
	// SSE providers either send a single cumulative usage frame or none at
	// all; take the max so a final authoritative frame wins.
	if prompt > a.prompt {
		a.prompt = prompt
	}
	if compl > a.compl {
		a.compl = compl
	}
	if total > a.total {
		a.total = total
	}
	if total == 0 {
		a.total = a.prompt + a.compl
	}
	a.sawUsage = true
}

func (a *streamAccumulator) snapshot() usagePayload {
	a.mu.Lock()
	defer a.mu.Unlock()
	return usagePayload{
		PromptTokens:     a.prompt,
		CompletionTokens: a.compl,
		TotalTokens:      a.total,
	}
}

// extractStreamUsage ports V1/o.p()'s usage extraction over an arbitrary chunk
// that may contain several SSE frames (or a partial one).
func extractStreamUsage(chunk []byte) (usagePayload, bool) {
	var best usagePayload
	found := false
	for _, payload := range iterSSEPayloads(chunk) {
		var doc struct {
			Usage *usagePayload `json:"usage"`
		}
		if errUnmarshal := json.Unmarshal(payload, &doc); errUnmarshal != nil {
			continue
		}
		if doc.Usage == nil {
			continue
		}
		p, c, t := doc.Usage.normalized()
		if t == 0 && p == 0 && c == 0 {
			continue
		}
		best = *doc.Usage
		found = true
	}
	return best, found
}

// extractStreamError ports V1/o.p()'s error extraction: root["error"]["message"],
// plus the Responses-API shape root["response"]["error"]["message"].
func extractStreamError(chunk []byte) string {
	for _, payload := range iterSSEPayloads(chunk) {
		var doc struct {
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
			Response *struct {
				Error *struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			} `json:"response"`
		}
		if errUnmarshal := json.Unmarshal(payload, &doc); errUnmarshal != nil {
			continue
		}
		if doc.Error != nil && doc.Error.Message != "" {
			return doc.Error.Message
		}
		if doc.Response != nil && doc.Response.Error != nil && doc.Response.Error.Message != "" {
			return doc.Response.Error.Message
		}
	}
	return ""
}

// iterSSEPayloads yields the JSON payload of each `data:` frame in a chunk,
// skipping the keep-alive comment `: keep-alive` and the `[DONE]` sentinel,
// exactly as V1/o.p() does.
func iterSSEPayloads(chunk []byte) [][]byte {
	if len(chunk) == 0 {
		return nil
	}
	var out [][]byte
	for _, rawLine := range strings.Split(string(chunk), "\n") {
		line := strings.TrimRight(rawLine, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") {
			// Blank separator or keep-alive comment.
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		out = append(out, []byte(payload))
	}
	return out
}
