package main

import (
	"encoding/json"
	"strings"
)

// This file normalises an upstream chat request.
//
// Why it exists
//
// The upstream validates the message array strictly and rejects the whole
// request when it does not conform, answering "request illegal". Two shapes in
// particular are rejected, with the codes the reference implementation recorded
// against the live service:
//
//	code 11128  a "developer" role. WorkBuddy knows only
//	            system / user / assistant / tool. OpenAI's newer "developer"
//	            role (sent by the Codex CLI and current SDKs) is semantically
//	            the same as "system" but is not accepted verbatim.
//
//	code 11148  a tool result that is not adjacent to the assistant message
//	            that requested it. Parallel tool calls interleaved with any other
//	            message (an image_resize_notice injected mid batch, for one)
//	            break the pairing and the upstream rejects every later turn of
//	            that conversation.
//
// The plugin used to forward the client body almost verbatim, changing only the
// model field, so any client sending a developer role or a reordered tool batch
// failed — and the failure surfaced as an opaque "request illegal".
//
// Everything here is a rename or a reorder. No content is dropped, and the only
// message ever added is the leading system prompt the upstream expects.

// defaultSystemPrompt mirrors the reference implementation's DEFAULT_SYSTEM_PROMPT.
const defaultSystemPrompt = "You are a helpful assistant."

// normaliseUpstreamBody rewrites a chat request into the shape the upstream
// accepts. It returns the original bytes when nothing needed changing.
func normaliseUpstreamBody(body []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &doc); errUnmarshal != nil {
		// Not an object (or not JSON at all): leave it alone so the upstream
		// reports the real problem rather than a rewrite error.
		return body, nil
	}

	rawMessages, okMessages := doc["messages"]
	if !okMessages || len(rawMessages) == 0 {
		return body, nil
	}

	var messages []map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(rawMessages, &messages); errUnmarshal != nil {
		// A non-array or non-object message list is not something to repair.
		return body, nil
	}

	changed := normaliseRoles(messages)
	if repackToolResults(messages) {
		changed = true
	}
	if ensureLeadingSystemMessage(&messages) {
		changed = true
	}

	if !changed {
		return body, nil
	}

	encoded, errMarshal := json.Marshal(messages)
	if errMarshal != nil {
		return nil, errMarshal
	}
	doc["messages"] = encoded
	return json.Marshal(doc)
}

// normaliseRoles renames role values to the set the upstream accepts.
//
// Reports whether anything changed. "developer" is OpenAI's rename of "system"
// and is what modern SDKs and the Codex CLI send; verbatim it is rejected with
// code 11128.
func normaliseRoles(messages []map[string]json.RawMessage) bool {
	changed := false
	for _, message := range messages {
		rawRole, okRole := message["role"]
		if !okRole {
			continue
		}
		var role string
		if errUnmarshal := json.Unmarshal(rawRole, &role); errUnmarshal != nil {
			continue
		}
		replacement, needsRename := upstreamRoleAlias(role)
		if !needsRename {
			continue
		}
		encoded, errMarshal := json.Marshal(replacement)
		if errMarshal != nil {
			continue
		}
		message["role"] = encoded
		changed = true
	}
	return changed
}

// upstreamRoleAlias maps a client role onto the upstream's vocabulary.
func upstreamRoleAlias(role string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "developer":
		return "system", true
	case "function":
		// The legacy pre-tool_calls spelling; the upstream only knows "tool".
		return "tool", true
	}
	return "", false
}

// repackToolResults moves any message that intrudes between an assistant's
// tool_calls and its results to after the results.
//
// Only the order changes: the same results in the same relative order, with the
// intruders moved behind the batch. Reports whether anything moved.
func repackToolResults(messages []map[string]json.RawMessage) bool {
	if len(messages) < 3 {
		return false
	}
	out := make([]map[string]json.RawMessage, 0, len(messages))
	moved := false
	i := 0
	for i < len(messages) {
		message := messages[i]
		if !isAssistantWithToolCalls(message) {
			out = append(out, message)
			i++
			continue
		}

		// Collect the assistant plus the run of tool results that follow,
		// deferring anything that appears inside that run.
		batch := []map[string]json.RawMessage{message}
		var intruders []map[string]json.RawMessage
		j := i + 1
		for j < len(messages) {
			next := messages[j]
			if isToolResult(next) {
				batch = append(batch, next)
				j++
				continue
			}
			// A non-tool message ends the batch — unless it merely interrupts
			// it and more results follow, which is the case being repaired.
			if hasToolResultLater(messages, j+1) {
				intruders = append(intruders, next)
				moved = true
				j++
				continue
			}
			break
		}
		out = append(out, batch...)
		out = append(out, intruders...)
		i = j
	}
	if !moved {
		return false
	}
	copy(messages, out)
	return true
}

// isAssistantWithToolCalls reports whether a message is an assistant turn that
// requested one or more tool calls.
func isAssistantWithToolCalls(message map[string]json.RawMessage) bool {
	if roleOf(message) != "assistant" {
		return false
	}
	rawCalls, okCalls := message["tool_calls"]
	if !okCalls {
		return false
	}
	var calls []json.RawMessage
	if errUnmarshal := json.Unmarshal(rawCalls, &calls); errUnmarshal != nil {
		return false
	}
	return len(calls) > 0
}

// isToolResult reports whether a message carries a tool result.
func isToolResult(message map[string]json.RawMessage) bool {
	return roleOf(message) == "tool"
}

// hasToolResultLater reports whether any message from index onwards is a tool
// result, which is what distinguishes an interruption from the end of a batch.
func hasToolResultLater(messages []map[string]json.RawMessage, from int) bool {
	for i := from; i < len(messages); i++ {
		if isToolResult(messages[i]) {
			return true
		}
		// A new assistant turn ends the segment under repair.
		if roleOf(messages[i]) == "assistant" {
			return false
		}
	}
	return false
}

// ensureLeadingSystemMessage inserts the default prompt when the conversation
// does not already begin with a system message.
//
// The upstream expects one and the reference implementation inserts it too.
// Reports whether a message was added; the slice is replaced in place because
// the caller keeps using the same variable.
func ensureLeadingSystemMessage(messages *[]map[string]json.RawMessage) bool {
	if messages == nil {
		return false
	}
	current := *messages
	if len(current) > 0 && roleOf(current[0]) == "system" {
		return false
	}
	content, errMarshal := json.Marshal(defaultSystemPrompt)
	if errMarshal != nil {
		return false
	}
	role, errRole := json.Marshal("system")
	if errRole != nil {
		return false
	}
	prefix := map[string]json.RawMessage{"role": role, "content": content}
	*messages = append([]map[string]json.RawMessage{prefix}, current...)
	return true
}

// roleOf reads a message's role without failing on an unexpected shape.
func roleOf(message map[string]json.RawMessage) string {
	rawRole, okRole := message["role"]
	if !okRole {
		return ""
	}
	var role string
	if errUnmarshal := json.Unmarshal(rawRole, &role); errUnmarshal != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(role))
}
